package app

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"pulse/internal/ast"
	"pulse/internal/secrets"
	"pulse/internal/sync"
)

// SyncStatusItem is one replicated item as observed locally.
type SyncStatusItem struct {
	// ID is the item id (relpath + "#" + request name).
	ID string `json:"id"`
	// Alive is false once every observed add tag is tombstoned.
	Alive bool `json:"alive"`
}

// SyncStatus is the short-lived sync readout: everything a `pulse sync
// status` needs without joining the network. Peers are live-only state
// and are deliberately absent — convergence is proven by equal digests,
// not by a connection count that flaps with the LAN.
type SyncStatus struct {
	// NodeID is the stable hex peer id, or "" when no device identity exists.
	NodeID string `json:"node_id"`
	// Fingerprint pins the device key (hex SHA-256 over the raw Ed25519
	// public key): it is the second column of config/peers.txt, printed
	// here so an operator can pin a peer without decoding key formats.
	Fingerprint string `json:"fingerprint"`
	// Digest is the hex Merkle root over (id, content-hash) pairs.
	Digest string `json:"digest"`
	// Count is the replicated item count, tombstones included.
	Count uint32 `json:"count"`
	// Items are the known ids, sorted.
	Items []SyncStatusItem `json:"items"`
}

// SyncStatus reads the persisted snapshot and reports it. It never binds
// a port and never announces: status is an observation, not a join.
// A missing identity yields an empty node with an empty store rather
// than an error — there is nothing to misreport, only nothing to show.
func (a *App) SyncStatus(ctx context.Context) (SyncStatus, error) {
	node, nodeBytes, fp, err := a.syncNodeID()
	if err != nil {
		return SyncStatus{}, nil // no identity, no sync: empty status, not a failure
	}
	tmp := sync.NewStore(nodeBytes, nil, a.syncScanner())
	raw, err := a.Store.LoadSyncSnapshot(ctx)
	if err != nil {
		return SyncStatus{}, fmt.Errorf("app: load sync snapshot: %w", err)
	}
	if err := tmp.ImportSnapshot(raw); err != nil {
		return SyncStatus{}, fmt.Errorf("app: read sync snapshot: %w", err)
	}
	// The digest describes the STORE; the identity describes the NODE.
	// Describe first, then attach the fingerprint — describing rebuilds
	// the struct, and attaching before would drop it on the floor.
	st := describeSyncStore(tmp, node)
	st.Fingerprint = fp
	return st, nil
}

// describeSyncStore renders one store's digest and item list.
func describeSyncStore(s *sync.Store, node string) SyncStatus {
	d := s.DigestOf()
	items := make([]SyncStatusItem, 0, len(s.IDs()))
	for _, id := range s.IDs() {
		alive := false
		if it := s.Get(id); it != nil {
			alive = it.Alive()
		}
		items = append(items, SyncStatusItem{ID: id, Alive: alive})
	}
	return SyncStatus{NodeID: node, Digest: hex.EncodeToString(d.Root[:]), Count: d.Count, Items: items}
}

// LiveSyncStatus renders the RUNNING subsystem's store: the same shape as
// SyncStatus, sourced live rather than from the last persist.
func (s *SyncSubsystem) LiveSyncStatus() SyncStatus {
	return describeSyncStore(s.Store, s.NodeID)
}

// syncNodeID derives the peer identity from the device key through the
// shared backend — the same derivation bootSync uses, factored so status
// and publish agree with the live subsystem about who this device is.
func (a *App) syncNodeID() (nodeID string, nodeBytes [8]byte, fingerprint string, err error) {
	id, err := secrets.LoadIdentity(a.cfg.VaultKeyID, a.keyBackend)
	if err != nil {
		return "", [8]byte{}, "", err
	}
	defer clear(id.X25519Priv)
	defer clear(id.Ed25519Seed)
	pub := ed25519.NewKeyFromSeed(id.Ed25519Seed).Public().(ed25519.PublicKey)
	nodeID = sync.NodeIDFor(pub)
	nodeBytes, err = sync.NodeBytes(nodeID)
	if err != nil {
		return "", [8]byte{}, "", err
	}
	return nodeID, nodeBytes, sync.Fingerprint(pub), nil
}

// publishDirToStore idempotently converges one store onto a directory of
// .http files: new requests are added, changed ones re-stamped, vanished
// ones tombstoned. Unchanged requests cost NOTHING — no Put, no new add
// tag — because every gratuitous stamp churns the digest and anti-entropy
// would never quiesce. Returns published ids, skipped files, and the
// first hard error (a parse failure aborts: partial intake that silently
// drops a file is worse than none).
func publishDirToStore(dir string, s *sync.Store) (published []string, skipped []string, err error) {
	var files []string
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".http") {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if walkErr != nil {
		return nil, nil, fmt.Errorf("app: walk sync dir %s: %w", dir, walkErr)
	}
	sort.Strings(files)
	node := s.Node()
	marker := hex.EncodeToString(node[:]) + ":"
	rels := make(map[string]bool, len(files))
	for _, f := range files {
		rels[relOf(f, dir)] = true
		ids, skip, perr := publishFileToStore(dir, f, s)
		if perr != nil {
			return nil, nil, perr
		}
		if skip {
			skipped = append(skipped, f)
			continue
		}
		published = append(published, ids...)
	}
	// Tombstone what this node sourced but the directory no longer holds.
	// The marker prefix is the guard: an id carrying another node's
	// marker (or none) is remote state a local delete must never kill.
	for _, id := range s.IDs() {
		it := s.Get(id)
		if it == nil {
			continue
		}
		src, ok := it.Fields[sync.FieldSource]
		if !ok || !strings.HasPrefix(src.Value, marker) {
			continue
		}
		rel, _, _ := strings.Cut(id, "#")
		if !rels[rel] {
			s.Remove(id)
		}
	}
	sort.Strings(published)
	return published, skipped, nil
}

// relOf renders path relative to dir, falling back to path itself.
func relOf(path, dir string) string {
	if r, err := filepath.Rel(dir, path); err == nil {
		return r
	}
	return path
}

// publishFileToStore converges one file's requests into the store.
// skip is true when a resolved secret blocks the file: the file is left
// out and named, never half-merged — a partial intake that silently
// drops one request is worse than a named skip the operator can see.
func publishFileToStore(dir, path string, s *sync.Store) (ids []string, skip bool, err error) {
	doc, err := ast.ParseFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("app: publish %s: %w", path, err)
	}
	defer doc.Release()
	rel := relOf(path, dir)
	idx := 0
	for req := range doc.Requests() {
		name := doc.Str(req.NameStart, req.NameEnd)
		if name == "" {
			name = fmt.Sprintf("request #%d", idx)
		}
		id := rel + "#" + name
		if perr := publishRequestToStore(s, id, doc, req); perr != nil {
			return nil, true, nil // secret-blocked: skip the file, name it
		}
		ids = append(ids, id)
		idx++
	}
	return ids, false, nil
}

// publishRequestToStore writes one request idempotently: fields and body
// land only when they differ, so steady state costs zero stamps. The
// source marker lands write-once (first publisher wins): two nodes serving
// the same file must not flap it back and forth forever.
func publishRequestToStore(s *sync.Store, id string, doc *ast.Document, req *ast.Request) error {
	rel, _, _ := strings.Cut(id, "#")
	cur := s.Get(id)
	put := func(field, value string) error {
		if cur != nil {
			if fv, ok := cur.Fields[field]; ok && fv.Value == value {
				return nil
			}
		}
		return s.PutField(id, field, value)
	}
	if err := put(sync.FieldName, doc.Str(req.NameStart, req.NameEnd)); err != nil {
		return err
	}
	if err := put(sync.FieldMethod, req.Method.String()); err != nil {
		return err
	}
	if err := put(sync.FieldURL, doc.Str(req.URLStart, req.URLEnd)); err != nil {
		return err
	}
	if cur == nil {
		cur = s.Get(id)
	}
	if _, ok := cur.Fields[sync.FieldSource]; !ok {
		node := s.Node()
		if err := s.PutField(id, sync.FieldSource, hex.EncodeToString(node[:])+":"+rel); err != nil {
			return err
		}
	}
	body := doc.BodyText(req.Body)
	if len(body) == 0 {
		return nil
	}
	if cur != nil && cur.HasBody && cur.Body.Sha256 == sha256.Sum256(body) {
		return nil
	}
	return s.PutBody(id, body)
}

// resyncPublishDir converges the configured collection dir into the live
// store when its fingerprint moved since the last convergence. Skipped
// (secret-blocked) files degrade loudly: the operator must know a file
// is not replicating, and silence would present a partial collection as
// whole.
func (a *App) resyncPublishDir(s *SyncSubsystem) {
	if s.publishDir == "" {
		return
	}
	fp, err := dirFingerprint(s.publishDir)
	if err != nil {
		a.degrade(fmt.Sprintf("sync publish dir %s unreadable (%v)", s.publishDir, err))
		return
	}
	if fp == s.lastPrint {
		return
	}
	_, skipped, err := publishDirToStore(s.publishDir, s.Store)
	if err != nil {
		a.degrade(fmt.Sprintf("sync publish: %v", err))
		return
	}
	s.lastPrint = fp
	for _, f := range skipped {
		a.degrade(fmt.Sprintf("sync publish skipped %s: holds a resolved secret; not replicated", f))
	}
}

// loadSyncSnapshot restores the persisted gossip state into a fresh
// subsystem store. A corrupt snapshot degrades to an empty store, loudly:
// gossip refills it from peers, while refusing to boot would strand a
// peer over a disk blemish — and silently starting empty would hide the
// blemish instead of reporting it.
func (a *App) loadSyncSnapshot(s *SyncSubsystem) {
	if a.Store == nil {
		return
	}
	raw, err := a.Store.LoadSyncSnapshot(a.ctx)
	if err != nil {
		a.degrade(fmt.Sprintf("sync snapshot unreadable (%v); starting empty, gossip will refill", err))
		return
	}
	if err := s.Store.ImportSnapshot(raw); err != nil {
		a.degrade(fmt.Sprintf("sync snapshot corrupt (%v); starting empty, gossip will refill", err))
	}
}

// persistSyncSnapshot durably records the live store. Errors degrade (and
// are counted in the subsystem's dropped counter's spirit — a log line,
// not a crash): gossip is loss-tolerant, and killing a peer over a disk
// stall turns a hiccup into an outage.
func (a *App) persistSyncSnapshot(s *SyncSubsystem) {
	if a.Store == nil {
		return
	}
	raw, err := s.Store.ExportSnapshot()
	if err != nil {
		a.degrade(fmt.Sprintf("sync snapshot export failed (%v)", err))
		return
	}
	if err := a.Store.SaveSyncSnapshot(context.Background(), raw); err != nil {
		a.degrade(fmt.Sprintf("sync snapshot persist failed (%v)", err))
	}
}

// dirFingerprint stats every .http file under dir: the serve loop
// re-publishes only when the fingerprint moves, so steady state costs a
// stat walk, never a parse.
func dirFingerprint(dir string) (string, error) {
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".http") {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s:%d:%d\n", p, fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), nil
}
