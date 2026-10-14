// Package sync implements Pulse's peer-to-peer collection sync (PRD FR-20,
// ROADMAP §4.2): CRDT collection state over mDNS discovery with mTLS TOFU
// transport. Collections sync; secrets never do (AGENTS.md §3.4, §4.3).
//
// The package is transport-agnostic domain logic: HLC clocks, the item CRDT,
// the secret filter, digests, and the gossip engine all run hermetically.
// The composition root (internal/app) binds the network, the keychain
// identity, and the redactor-backed secret scanner.
package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"pulse/internal/secrets"
)

// ---------------------------------------------------------------------------
// Hybrid Logical Clock (ROADMAP §4.2.1)
// ---------------------------------------------------------------------------

// HLC is a Hybrid Logical Clock: wall-clock milliseconds plus a logical
// counter plus the node id. It guards against physical wall-clock
// drift and regression across peers — a peer whose clock jumps backwards
// (or a liar whose clock jumps forwards) cannot move the shared order
// backwards, because the counter carries monotonicity when the wall does
// not, and Observe adopts a remote wall only by ticking past it.
type HLC struct {
	WallMS  int64
	Counter uint32
	Node    [8]byte
}

// Compare orders clocks lexicographically: wall, then counter, then node.
// It returns -1, 0, or +1. A zero result means the same tick on the same
// node — the same causal instant — so a merge tie-breaks deterministically
// on value rather than reordering.
func (h HLC) Compare(o HLC) int {
	if h.WallMS != o.WallMS {
		if h.WallMS < o.WallMS {
			return -1
		}
		return 1
	}
	if h.Counter != o.Counter {
		if h.Counter < o.Counter {
			return -1
		}
		return 1
	}
	return bytes.Compare(h.Node[:], o.Node[:])
}

// Tick advances the clock for a local event and returns the stamp.
// A wall that moved forward resets the counter; a wall that stalled or
// regressed ticks the counter instead, so stamps never go backwards even
// when the machine clock does.
func (h *HLC) Tick(now time.Time) HLC {
	pt := now.UnixMilli()
	switch {
	case pt > h.WallMS:
		h.WallMS, h.Counter = pt, 0
	default:
		if h.Counter == ^uint32(0) {
			// A counter that wrapped would break monotonicity silently;
			// advancing the wall by one millisecond is the honest
			// overflow path (4 billion ticks in one millisecond is not
			// a workload, it is a bug, and this stays monotonic anyway).
			h.WallMS++
			h.Counter = 0
		} else {
			h.Counter++
		}
	}
	return *h
}

// Observe folds a remote stamp (or a digest max) into the clock for a
// receive event. The wall adopts the maximum of local, remote, and
// physical time; the counter ticks past whichever chain supplied that
// maximum, so the next local stamp sorts after everything observed.
func (h *HLC) Observe(o HLC, now time.Time) {
	pt := now.UnixMilli()
	switch {
	case o.WallMS > h.WallMS && o.WallMS >= pt:
		h.WallMS, h.Counter = o.WallMS, o.Counter+1
	case pt > h.WallMS && pt >= o.WallMS:
		h.WallMS, h.Counter = pt, 0
	case o.WallMS == h.WallMS && o.Counter >= h.Counter:
		h.Counter = o.Counter + 1
	default:
		if h.Counter == ^uint32(0) {
			h.WallMS++
			h.Counter = 0
		} else {
			h.Counter++
		}
	}
	// Counter+1 on a saturated remote counter wraps to zero, which would
	// sort BEFORE the remote stamp it was meant to succeed. Saturate
	// instead: the wall still advances on the next tick.
	if h.Counter == 0 && (o.WallMS == h.WallMS) {
		h.WallMS++
	}
}

// Pack renders the clock for a TXT record: 16 hex wall-ms, 8 hex counter,
// 16 hex node id. Unpack inverts it; anything else is an error, never a
// guess (a corrupt TXT record is a corrupt peer, not a default clock).
func (h HLC) Pack() string {
	var buf [20]byte
	binary.BigEndian.PutUint64(buf[0:8], uint64(h.WallMS))
	binary.BigEndian.PutUint32(buf[8:12], h.Counter)
	copy(buf[12:20], h.Node[:])
	return hex.EncodeToString(buf[:])
}

// UnpackHLC parses Pack output.
func UnpackHLC(s string) (HLC, error) {
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 20 {
		return HLC{}, fmt.Errorf("sync: malformed packed HLC %q", s)
	}
	var h HLC
	h.WallMS = int64(binary.BigEndian.Uint64(raw[0:8]))
	h.Counter = binary.BigEndian.Uint32(raw[8:12])
	copy(h.Node[:], raw[12:20])
	return h, nil
}

// ---------------------------------------------------------------------------
// Collection CRDT (ROADMAP §4.2.1)
// ---------------------------------------------------------------------------

// Field keys: metadata lives under bare keys, headers under the header:
// namespace. The namespace matters to the secret filter: a sensitive
// header NAME proves credential material regardless of the value.
const (
	FieldName   = "name"
	FieldMethod = "method"
	FieldURL    = "url"
	// HeaderPrefix namespaces header fields: "header:Authorization".
	HeaderPrefix = "header:"
)

// FieldValue is one LWW register: the value plus the stamp that won it.
type FieldValue struct {
	Value string
	Stamp HLC
}

// BodyValue is the opaque request body: content hash plus bytes, LWW by
// stamp. The body is opaque ON PURPOSE — per-line text merging would need
// a three-way merge with a human in the loop, and sync has no human.
type BodyValue struct {
	Sha256 [32]byte
	Bytes  []byte
	Stamp  HLC
}

// Item is one request (file + name). Fields merge per-field LWW, the body
// merges opaque LWW, and liveness is an OR-Set over add tags: an item is
// alive while any add tag is not covered by a tombstone.
type Item struct {
	ID      string
	Fields  map[string]FieldValue
	Body    BodyValue
	HasBody bool
	Removed map[uint64]bool
	Adds    map[uint64]HLC
	// RemovedAt stamps the last removal. It extends the ROADMAP §4.2.1
	// sketch (which lists no tombstone stamp) for a load-bearing reason:
	// digests compare (id, max-stamp) pairs, and a removal that advances
	// no stamp is invisible to anti-entropy — a remover and a peer that
	// never touched the item would carry EQUAL digests for DIVERGENT
	// states (dead vs alive) and never exchange. RemovedAt makes every
	// removal an observable update; merges take the maximum.
	RemovedAt HLC
}

// Alive reports whether any observed add tag survives the tombstones.
func (it *Item) Alive() bool {
	for tag := range it.Adds {
		if !it.Removed[tag] {
			return true
		}
	}
	return false
}

// addTag derives a unique add tag from the stamping tick: the same node
// never stamps twice (the counter ticks), so node+wall+counter is unique
// per add event, and hashing it to 64 bits keeps tags compact.
func addTag(stamp HLC) uint64 {
	h := sha256.New()
	h.Write(stamp.Node[:])
	var buf [12]byte
	binary.BigEndian.PutUint64(buf[0:8], uint64(stamp.WallMS))
	binary.BigEndian.PutUint32(buf[8:12], stamp.Counter)
	h.Write(buf[:])
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8])
}

// MaxStamp is the item's high-water mark: the maximum stamp over fields,
// body, add tags, and the removal stamp. Digests and want-lists compare
// these, so a peer that has seen every update carries the same maxima —
// including peers that have only seen a deletion.
func (it *Item) MaxStamp() HLC {
	max := HLC{}
	for _, f := range it.Fields {
		if f.Stamp.Compare(max) > 0 {
			max = f.Stamp
		}
	}
	if it.HasBody && it.Body.Stamp.Compare(max) > 0 {
		max = it.Body.Stamp
	}
	for _, s := range it.Adds {
		if s.Compare(max) > 0 {
			max = s
		}
	}
	if it.RemovedAt.Compare(max) > 0 {
		max = it.RemovedAt
	}
	return max
}

// mergeField applies one LWW register: the higher stamp wins; an exact
// stamp tie (same tick, same node — only possible across a node-id
// collision, but the rule must still be total) breaks toward the
// lexicographically larger value so every peer picks the same winner.
func mergeField(cur FieldValue, v string, stamp HLC) FieldValue {
	switch c := stamp.Compare(cur.Stamp); {
	case c > 0:
		return FieldValue{Value: v, Stamp: stamp}
	case c == 0 && v > cur.Value:
		return FieldValue{Value: v, Stamp: stamp}
	default:
		return cur
	}
}

// Merge folds a remote item into the local one. It is commutative,
// associative, and idempotent: delivery order, duplication, and repetition
// change nothing, which is the property the convergence gate proves.
// Fields and body merge LWW; add tags union with per-tag maximum stamps;
// tombstones union. A merge never deletes a tag the remover never saw —
// that is the OR-Set add-wins rule, and ignoring tombstones (M91) breaks
// exactly this.
func (it *Item) Merge(o *Item) {
	if it.Fields == nil {
		it.Fields = map[string]FieldValue{}
	}
	for k, f := range o.Fields {
		it.Fields[k] = mergeField(it.Fields[k], f.Value, f.Stamp)
	}
	if o.HasBody {
		if !it.HasBody {
			it.Body, it.HasBody = o.Body, true
		} else {
			switch c := o.Body.Stamp.Compare(it.Body.Stamp); {
			case c > 0:
				it.Body = o.Body
			case c == 0 && string(o.Body.Bytes) > string(it.Body.Bytes):
				it.Body = o.Body
			}
		}
	}
	if it.Adds == nil {
		it.Adds = map[uint64]HLC{}
	}
	for tag, s := range o.Adds {
		if cur, ok := it.Adds[tag]; !ok || s.Compare(cur) > 0 {
			it.Adds[tag] = s
		}
	}
	if it.Removed == nil {
		it.Removed = map[uint64]bool{}
	}
	for tag := range o.Removed {
		it.Removed[tag] = true
	}
	if o.RemovedAt.Compare(it.RemovedAt) > 0 {
		it.RemovedAt = o.RemovedAt
	}
}

// ---------------------------------------------------------------------------
// Secret filter (PRD FR-20: collections-only-never-secrets)
// ---------------------------------------------------------------------------

// ErrSecretRejected reports that an item carries resolved secret material
// or a .env payload and was refused before entering CRDT state or the
// wire. The error names the item and the field, NEVER the value: echoing
// the value into an error would leak the secret the filter exists to stop.
var ErrSecretRejected = errors.New("sync: item carries secret material")

// CheckItemClean enforces the strict secret filter on one item. Three
// legs, any of which rejects:
//
//  1. Sensitive header names (Authorization, Cookie, X-Api-Key, ... via
//     secrets.IsSensitiveHeader): such a field carries credential
//     material BY CONSTRUCTION, value unknown or not.
//  2. .env-shaped payloads: two or more KEY=value lines in any value —
//     the shape of a committed plaintext env file, which must never
//     enter shared state.
//  3. The scanner (the composition root binds the redactor probe):
//     any value containing a REGISTERED resolved secret. A nil scanner
//     fails closed and rejects everything — sync without a scanner is
//     sync without its third leg, and that state must be loud, not
//     silently open.
func CheckItemClean(it *Item, scanner func(string) bool) error {
	denyAll := scanner == nil
	scan := scanner
	if scan == nil {
		scan = func(string) bool { return true }
	}
	check := func(field, value string) error {
		if name, ok := strings.CutPrefix(field, HeaderPrefix); ok {
			if secrets.IsSensitiveHeader(name) {
				return fmt.Errorf("%w: item %q field %q is a credential header",
					ErrSecretRejected, it.ID, field)
			}
		}
		if looksLikeDotEnv(value) {
			return fmt.Errorf("%w: item %q field %q holds a .env payload",
				ErrSecretRejected, it.ID, field)
		}
		if scan(value) {
			if denyAll {
				return fmt.Errorf("%w: item %q field %q (no secret scanner bound)",
					ErrSecretRejected, it.ID, field)
			}
			return fmt.Errorf("%w: item %q field %q holds a resolved secret",
				ErrSecretRejected, it.ID, field)
		}
		return nil
	}
	for k, f := range it.Fields {
		if err := check(k, f.Value); err != nil {
			return err
		}
	}
	if it.HasBody {
		if err := check("body", string(it.Body.Bytes)); err != nil {
			return err
		}
	}
	return nil
}

// looksLikeDotEnv reports the shape of a plaintext env payload: two or
// more lines of UPPER_SNAKE=nonempty. One KEY=value line is a coincidence
// (a query string, a header fragment); two is a file being smuggled. Keys
// are strict capitals/digits/underscore (no lowercase, no dots), which is
// what keeps ordinary prose and URLs out; there is no minimum key length
// because the two-line bar already rejects coincidences and the filter is
// fail-closed by mandate (FR-20: never secrets).
func looksLikeDotEnv(s string) bool {
	hits := 0
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := line[:eq]
		if !isEnvKey(key) {
			continue
		}
		rest := strings.TrimSpace(line[eq+1:])
		if rest == "" {
			continue
		}
		hits++
		if hits >= 2 {
			return true
		}
	}
	return false
}

func isEnvKey(k string) bool {
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Store: local state, local edits, merge ingress, digest, egress
// ---------------------------------------------------------------------------

// Store is one peer's CRDT state. Local edits stamp with the node's HLC;
// remote states enter through MergeItem (filtered pre-merge); frames leave
// through EncodeState (filtered pre-egress). A Store is NOT safe for
// concurrent use: the gossip engine drives it from one goroutine.
type Store struct {
	node    [8]byte
	clock   HLC
	now     func() time.Time
	scanner func(string) bool
	items   map[string]*Item
	// Rejected counts items refused by the secret filter (ingress skips
	// and egress skips alike). It is a counter, not a log: the VALUES
	// must not be recorded anywhere.
	Rejected int
}

// NewStore builds a peer's state. node must be this device's 8-byte id;
// now defaults to time.Now (tests inject a manual clock); scanner is the
// redactor probe — nil fails closed (every item rejected until bound).
func NewStore(node [8]byte, now func() time.Time, scanner func(string) bool) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{node: node, now: now, scanner: scanner, items: map[string]*Item{}}
}

// Node returns the peer id.
func (s *Store) Node() [8]byte { return s.node }

// Clock returns the current HLC (for TXT advertisement).
func (s *Store) Clock() HLC { return s.clock }

// stamp ticks the node clock and tags it with the node id.
func (s *Store) stamp() HLC {
	s.clock.Node = s.node
	return s.clock.Tick(s.now())
}

// item returns the live-or-tombstoned item, creating the shell if absent.
func (s *Store) item(id string) *Item {
	it, ok := s.items[id]
	if !ok {
		it = &Item{ID: id}
		s.items[id] = it
	}
	return it
}

// PutField sets one metadata/header field with a fresh stamp. A dirty
// resulting item is rejected BEFORE it enters state: the edit fails and
// nothing is stored.
func (s *Store) PutField(id, field, value string) error {
	stamp := s.stamp()
	it := s.item(id)
	prev, had := it.Fields[field]
	if it.Fields == nil {
		it.Fields = map[string]FieldValue{}
	}
	it.Fields[field] = FieldValue{Value: value, Stamp: stamp}
	if it.Adds == nil {
		it.Adds = map[uint64]HLC{}
	}
	tag := addTag(stamp)
	it.Adds[tag] = stamp
	if err := CheckItemClean(it, s.scanner); err != nil {
		// Roll back the edit: a rejected item must not linger half-added.
		if had {
			it.Fields[field] = prev
		} else {
			delete(it.Fields, field)
		}
		delete(it.Adds, tag)
		if len(it.Fields) == 0 && len(it.Adds) == 0 && !it.HasBody {
			delete(s.items, id)
		}
		return err
	}
	return nil
}

// PutBody replaces the opaque body with a fresh stamp. Same reject-before-
// store rule as PutField.
func (s *Store) PutBody(id string, body []byte) error {
	stamp := s.stamp()
	it := s.item(id)
	prev, hadBody := it.Body, it.HasBody
	sum := sha256.Sum256(body)
	it.Body = BodyValue{Sha256: sum, Bytes: append([]byte(nil), body...), Stamp: stamp}
	it.HasBody = true
	if it.Adds == nil {
		it.Adds = map[uint64]HLC{}
	}
	tag := addTag(stamp)
	it.Adds[tag] = stamp
	if err := CheckItemClean(it, s.scanner); err != nil {
		it.Body, it.HasBody = prev, hadBody
		delete(it.Adds, tag)
		if len(it.Fields) == 0 && len(it.Adds) == 0 && !it.HasBody {
			delete(s.items, id)
		}
		return err
	}
	return nil
}

// Remove tombstones every add tag observed so far and stamps the removal
// so digests observe it (see RemovedAt). Concurrent adds the remover never
// saw (unseen tags) survive — the OR-Set add-wins rule. Removing an
// unknown id still records nothing: there is nothing to kill.
func (s *Store) Remove(id string) {
	stamp := s.stamp()
	it, ok := s.items[id]
	if !ok {
		return
	}
	if it.Removed == nil {
		it.Removed = map[uint64]bool{}
	}
	for tag := range it.Adds {
		it.Removed[tag] = true
	}
	it.RemovedAt = stamp
}

// Get returns the item shell (live or tombstoned), or nil if unknown.
func (s *Store) Get(id string) *Item { return s.items[id] }

// MergeItem folds one remote item into local state. Dirty remote items
// are SKIPPED (counted, never merged): one poisoned item must not veto a
// whole gossip batch, and must never enter state. Malformed (empty-id)
// items are skipped the same way.
func (s *Store) MergeItem(o *Item) {
	if o == nil || o.ID == "" {
		s.Rejected++
		return
	}
	if err := CheckItemClean(o, s.scanner); err != nil {
		s.Rejected++
		return
	}
	for _, stamp := range o.Adds {
		s.clock.Observe(stamp, s.now())
	}
	for _, f := range o.Fields {
		s.clock.Observe(f.Stamp, s.now())
	}
	if o.HasBody {
		s.clock.Observe(o.Body.Stamp, s.now())
	}
	it := s.item(o.ID)
	it.Merge(o)
}

// MaxHLC is the state's high-water mark: the maximum stamp over ITEMS
// only — never the volatile local clock. Digests must be a pure function
// of replicated state: two peers holding the same items must carry equal
// digests even though their local clocks ticked a different number of
// times. (Including the clock made equal states digest-unequal forever —
// anti-entropy never quiesced. Found by the convergence gate going red.)
func (s *Store) MaxHLC() HLC {
	max := HLC{}
	for _, it := range s.items {
		if m := it.MaxStamp(); m.Compare(max) > 0 {
			max = m
		}
	}
	return max
}

// ---------------------------------------------------------------------------
// Digest and anti-entropy (ROADMAP §4.2.2)
// ---------------------------------------------------------------------------

// Digest is the anti-entropy summary: a Merkle-style root over the sorted
// (id, item-content-hash) pairs, the item count, and the store high-water
// mark. Equal digests mean equal states (modulo hash collision); differing
// digests route to the id-set exchange, then states for differing ids.
//
// The root hashes FULL item content, not max stamps: with skewed clocks a
// concurrent update can carry an OLDER wall than the shared maximum, so
// two peers can agree on every (id, max-stamp) pair while holding
// different tag sets, tombstones, or fields. A max-stamp digest is blind
// to exactly the concurrent updates sync exists to reconcile — the
// convergence gate caught this red. Content hashing closes it.
type Digest struct {
	Root   [32]byte
	Count  uint32
	MaxHLC HLC
}

// IDHashes returns every known id with its content hash: the want-list
// leg. Tombstoned ids are included so tombstones propagate. Hashes —
// not max stamps — because skewed clocks let concurrent updates share a
// maximum while differing below it; a max-stamp want-list deadlocks with
// both sides certain the other is not newer (caught red by the gate).
func (s *Store) IDHashes() map[string][32]byte {
	out := make(map[string][32]byte, len(s.items))
	for id, it := range s.items {
		out[id] = sha256.Sum256(marshalItemCanonical(it))
	}
	return out
}

// DigestOf summarizes the store: the Merkle root over the sorted
// (id, content-hash) pairs plus the high-water mark. Tombstoned items
// are included WITH their tombstones: forgetting one would resurrect
// the item on the next gossip round, which is exactly the M91 failure.
func (s *Store) DigestOf() Digest {
	hashes := s.IDHashes()
	ids := make([]string, 0, len(hashes))
	for id := range hashes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	h := sha256.New()
	max := HLC{}
	for _, id := range ids {
		if m := s.items[id].MaxStamp(); m.Compare(max) > 0 {
			max = m
		}
		h.Write([]byte(id))
		h.Write([]byte{0})
		// Map index expressions are not addressable: copy out first.
		hh := hashes[id]
		h.Write(hh[:])
	}
	var root [32]byte
	copy(root[:], h.Sum(nil))
	return Digest{Root: root, Count: uint32(len(ids)), MaxHLC: max}
}

// Equal reports digest equality.
func (d Digest) Equal(o Digest) bool {
	return d.Root == o.Root && d.Count == o.Count && d.MaxHLC.Compare(o.MaxHLC) == 0
}

// StatesFor returns clean states for the requested ids. Dirty items are
// skipped pre-egress (counted): secrets never hit the network, even when
// a peer explicitly asks for the id.
func (s *Store) StatesFor(ids []string) []*Item {
	var out []*Item
	for _, id := range ids {
		it, ok := s.items[id]
		if !ok {
			continue
		}
		if err := CheckItemClean(it, s.scanner); err != nil {
			s.Rejected++
			continue
		}
		out = append(out, it)
	}
	return out
}

// AllStates returns every clean item: the full-state leg and the
// canonical-encoding source.
func (s *Store) AllStates() []*Item {
	ids := make([]string, 0, len(s.items))
	for id := range s.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return s.StatesFor(ids)
}

// marshalItemCanonical renders one item deterministically: fields sorted
// by key, tag sets sorted numerically, fixed struct order. It is the
// byte-equality unit — the store form joins these, and the digest hashes
// them — so one function serves convergence comparison and anti-entropy.
func marshalItemCanonical(it *Item) []byte {
	type field struct {
		Key   string
		Value string
		Stamp HLC
	}
	type item struct {
		ID        string
		Fields    []field
		Body      []byte
		HasBody   bool
		Adds      []uint64
		Removed   []uint64
		RemovedAt HLC
	}
	keys := make([]string, 0, len(it.Fields))
	for k := range it.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fs := make([]field, 0, len(keys))
	for _, k := range keys {
		fv := it.Fields[k]
		fs = append(fs, field{Key: k, Value: fv.Value, Stamp: fv.Stamp})
	}
	adds := make([]uint64, 0, len(it.Adds))
	for t := range it.Adds {
		adds = append(adds, t)
	}
	sort.Slice(adds, func(i, j int) bool { return adds[i] < adds[j] })
	rem := make([]uint64, 0, len(it.Removed))
	for t := range it.Removed {
		rem = append(rem, t)
	}
	sort.Slice(rem, func(i, j int) bool { return rem[i] < rem[j] })
	var body []byte
	if it.HasBody {
		body = it.Body.Bytes
	}
	// The types above are JSON-total (no maps, no channels, no funcs), so
	// Marshal cannot fail; a nil return on error keeps the digest moving
	// rather than hashing a partial rendering.
	raw, err := json.Marshal(item{ID: it.ID, Fields: fs, Body: body, HasBody: it.HasBody, Adds: adds, Removed: rem, RemovedAt: it.RemovedAt})
	if err != nil {
		return nil
	}
	return raw
}

// MarshalCanonical renders the full clean state deterministically: items
// sorted by id over the per-item canonical form. Byte-equal output on
// every peer means converged.
func (s *Store) MarshalCanonical() ([]byte, error) {
	states := s.AllStates()
	out := make([]json.RawMessage, 0, len(states))
	for _, it := range states {
		out = append(out, marshalItemCanonical(it))
	}
	return json.Marshal(out)
}
