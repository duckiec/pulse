package sync

// Unit gates for the HLC and the collection CRDT: clock monotonicity
// under drift, LWW merge rules, OR-Set liveness, the three secret-filter
// legs, and digest/canonical determinism. The multi-peer convergence
// proof lives in convergence_test.go; the adversarial TLS proof in
// tls_test.go.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testNode(b byte) [8]byte {
	var n [8]byte
	for i := range n {
		n[i] = b
	}
	return n
}

// allowAll is the test scanner: no registered secrets.
func allowAll(string) bool { return false }

func TestHLCTickIsMonotonicUnderRegression(t *testing.T) {
	var h HLC
	h.Node = testNode(1)
	base := time.UnixMilli(1_700_000_000_000)
	prev := h.Tick(base)
	// The wall falls off a cliff; stamps must still climb.
	for i := 0; i < 5; i++ {
		next := h.Tick(base.Add(-time.Hour))
		if next.Compare(prev) <= 0 {
			t.Fatalf("tick %d went backwards under regression: %+v after %+v", i, next, prev)
		}
		prev = next
	}
	// Recovery to real time resumes wall stamps above the counter chain.
	after := h.Tick(base.Add(2 * time.Hour))
	if after.Compare(prev) <= 0 {
		t.Fatalf("post-recovery tick did not advance: %+v after %+v", after, prev)
	}
	if after.Counter != 0 {
		t.Fatalf("forward wall did not reset the counter: %+v", after)
	}
}

func TestHLCObserveAdoptsAndTicksPast(t *testing.T) {
	var a, b HLC
	a.Node, b.Node = testNode(1), testNode(2)
	now := time.UnixMilli(1_700_000_000_000)
	a.Tick(now)
	remote := b.Tick(now.Add(time.Hour)) // remote wall ahead by an hour
	a.Observe(remote, now)
	if got := a.Compare(remote); got <= 0 {
		t.Fatalf("Observe did not pass the remote stamp: local %+v vs remote %+v", a, remote)
	}
	// A second observe of the SAME stamp still advances (counter chain).
	again := a
	a.Observe(remote, now)
	if a.Compare(again) <= 0 {
		t.Fatalf("re-observing the same stamp stalled: %+v after %+v", a, again)
	}
}

func TestHLCCompareTieBreaksOnNode(t *testing.T) {
	var a, b HLC
	a.Node, b.Node = testNode(1), testNode(2)
	now := time.UnixMilli(1_700_000_000_000)
	sa, sb := a.Tick(now), b.Tick(now)
	if sa.Compare(sb) >= 0 {
		t.Fatalf("same wall+counter must tie-break on node: %+v vs %+v", sa, sb)
	}
	if sb.Compare(sa) <= 0 {
		t.Fatalf("node tie-break is not antisymmetric")
	}
}

func TestHLCPackRoundTrip(t *testing.T) {
	h := HLC{WallMS: 1_700_000_000_123, Counter: 42, Node: testNode(7)}
	back, err := UnpackHLC(h.Pack())
	if err != nil {
		t.Fatalf("UnpackHLC: %v", err)
	}
	if back != h {
		t.Fatalf("pack round trip: %+v != %+v", back, h)
	}
	for _, bad := range []string{"", "zz", h.Pack()[:10]} {
		if _, err := UnpackHLC(bad); err == nil {
			t.Fatalf("UnpackHLC(%q) succeeded, want an error", bad)
		}
	}
}

func TestFieldMergeIsLWWWithDeterministicTie(t *testing.T) {
	s := NewStore(testNode(1), nil, allowAll)
	now := time.Now()
	lo := HLC{WallMS: now.UnixMilli(), Counter: 1, Node: testNode(1)}
	hi := HLC{WallMS: now.UnixMilli(), Counter: 2, Node: testNode(1)}
	a := &Item{ID: "r", Fields: map[string]FieldValue{"name": {Value: "old", Stamp: lo}}}
	b := &Item{ID: "r", Fields: map[string]FieldValue{"name": {Value: "new", Stamp: hi}}}
	a.Merge(b)
	if a.Fields["name"].Value != "new" {
		t.Fatalf("higher stamp lost: %+v", a.Fields["name"])
	}
	// Exact-stamp tie breaks toward the larger value on every peer.
	tied := HLC{WallMS: now.UnixMilli(), Counter: 9, Node: testNode(2)}
	x := &Item{ID: "r", Fields: map[string]FieldValue{"name": {Value: "aaa", Stamp: tied}}}
	y := &Item{ID: "r", Fields: map[string]FieldValue{"name": {Value: "zzz", Stamp: tied}}}
	x.Merge(y)
	y2 := &Item{ID: "r", Fields: map[string]FieldValue{"name": {Value: "aaa", Stamp: tied}}}
	y.Merge(y2) // y already holds zzz; merging aaa must not dislodge it
	if x.Fields["name"].Value != "zzz" || y.Fields["name"].Value != "zzz" {
		t.Fatalf("tie-break diverged: %q vs %q", x.Fields["name"].Value, y.Fields["name"].Value)
	}
	_ = s
}

func TestBodyMergeIsOpaqueLWW(t *testing.T) {
	now := time.Now()
	lo := HLC{WallMS: now.UnixMilli(), Counter: 1, Node: testNode(1)}
	hi := HLC{WallMS: now.UnixMilli(), Counter: 5, Node: testNode(2)}
	a := &Item{ID: "r", Body: BodyValue{Bytes: []byte("v1"), Stamp: lo}, HasBody: true}
	b := &Item{ID: "r", Body: BodyValue{Bytes: []byte("v2-longer-but-newer"), Stamp: hi}, HasBody: true}
	a.Merge(b)
	if string(a.Body.Bytes) != "v2-longer-but-newer" {
		t.Fatalf("newer body lost regardless of size: %q", a.Body.Bytes)
	}
}

func TestORSetAddWinsOverUnseenRemove(t *testing.T) {
	// The remover kills every tag it saw; a concurrent add carries a tag
	// the remover never saw, so the item survives. This is the rule M91
	// breaks (by ignoring tombstones, deletes stop propagating instead).
	a := &Item{ID: "r", Adds: map[uint64]HLC{1: {}}, Removed: map[uint64]bool{}}
	b := &Item{ID: "r", Adds: map[uint64]HLC{1: {}, 2: {}}, Removed: map[uint64]bool{1: true}}
	a.Merge(b)
	if !a.Alive() {
		t.Fatal("concurrent add tag 2 died with removed tag 1: add-wins violated")
	}
	b.Merge(a)
	if !b.Alive() {
		t.Fatal("merge is not commutative on liveness")
	}
	// Once the remover SEES tag 2 and tombstones it, the item dies
	// everywhere: full tombstone coverage is death, deterministically.
	c := &Item{ID: "r", Adds: map[uint64]HLC{1: {}, 2: {}}, Removed: map[uint64]bool{1: true, 2: true}}
	a.Merge(c)
	if a.Alive() {
		t.Fatal("fully-tombstoned item still alive")
	}
}

func TestRemoveTombstonesObservedTags(t *testing.T) {
	s := NewStore(testNode(3), nil, allowAll)
	if err := s.PutField("r1", FieldName, "n"); err != nil {
		t.Fatal(err)
	}
	if !s.Get("r1").Alive() {
		t.Fatal("fresh item not alive")
	}
	s.Remove("r1")
	if s.Get("r1").Alive() {
		t.Fatal("removed item still alive")
	}
	// Removing the unknown records nothing (and panics nothing).
	s.Remove("never-seen")
	if s.Get("never-seen") != nil {
		t.Fatal("remove created state for an unknown id")
	}
}

func TestSecretFilterLegs(t *testing.T) {
	clean := &Item{ID: "ok", Fields: map[string]FieldValue{
		FieldName:                {Value: "login"},
		HeaderPrefix + "X-Trace": {Value: "abc"},
	}}
	if err := CheckItemClean(clean, allowAll); err != nil {
		t.Fatalf("clean item rejected: %v", err)
	}
	// Leg 1: sensitive header NAMES reject regardless of value.
	cred := &Item{ID: "c", Fields: map[string]FieldValue{HeaderPrefix + "Authorization": {Value: "Bearer totally-innocent"}}}
	err := CheckItemClean(cred, allowAll)
	if !errors.Is(err, ErrSecretRejected) {
		t.Fatalf("credential header accepted: %v", err)
	}
	if strings.Contains(err.Error(), "totally-innocent") {
		t.Fatalf("rejection error echoes the value: %q", err)
	}
	// Leg 2: .env-shaped payloads reject.
	env := &Item{ID: "e", Fields: map[string]FieldValue{FieldName: {Value: "cfg"}}}
	env.HasBody = true
	env.Body.Bytes = []byte("API_KEY=abc123\nDB_PASS=hunter2\n")
	if err := CheckItemClean(env, allowAll); !errors.Is(err, ErrSecretRejected) {
		t.Fatalf(".env body accepted: %v", err)
	}
	// One KEY=value line is a coincidence, not a file.
	one := &Item{ID: "o", Fields: map[string]FieldValue{FieldURL: {Value: "https://h/p?a=b"}}}
	if err := CheckItemClean(one, allowAll); err != nil {
		t.Fatalf("single pair rejected: %v", err)
	}
	// Leg 3: the scanner leg catches registered resolved secrets.
	scanner := func(v string) bool { return strings.Contains(v, "resolved-token-xyz") }
	leak := &Item{ID: "l", Fields: map[string]FieldValue{HeaderPrefix + "X-Trace": {Value: "id resolved-token-xyz tail"}}}
	if err := CheckItemClean(leak, scanner); !errors.Is(err, ErrSecretRejected) {
		t.Fatalf("resolved secret accepted: %v", err)
	}
	// Nil scanner fails closed: everything rejects, loudly.
	if err := CheckItemClean(clean, nil); !errors.Is(err, ErrSecretRejected) {
		t.Fatalf("nil scanner accepted a clean item: %v", err)
	}
}

func TestPutRejectsBeforeStoring(t *testing.T) {
	s := NewStore(testNode(1), nil, allowAll)
	if err := s.PutField("r", HeaderPrefix+"X-Api-Key", "k"); !errors.Is(err, ErrSecretRejected) {
		t.Fatalf("dirty PutField accepted: %v", err)
	}
	if got := s.Get("r"); got != nil && len(got.Fields) > 0 {
		t.Fatalf("rejected edit lingered in state: %+v", got)
	}
	if err := s.PutBody("b", []byte("A=1\nB=2\n")); !errors.Is(err, ErrSecretRejected) {
		t.Fatalf("dirty PutBody accepted: %v", err)
	}
	if got := s.Get("b"); got != nil && got.HasBody {
		t.Fatal("rejected body lingered in state")
	}
}

func TestMergeSkipsDirtyItemsButKeepsClean(t *testing.T) {
	s := NewStore(testNode(1), nil, allowAll)
	dirty := &Item{ID: "dirty", Fields: map[string]FieldValue{HeaderPrefix + "Cookie": {Value: "s"}}}
	clean := &Item{ID: "clean", Fields: map[string]FieldValue{FieldName: {Value: "v"}}}
	s.MergeItem(dirty)
	s.MergeItem(clean)
	if s.Get("dirty") != nil {
		t.Fatal("dirty remote item entered state")
	}
	if s.Get("clean") == nil {
		t.Fatal("clean remote item lost alongside the dirty one")
	}
	if s.Rejected == 0 {
		t.Fatal("rejection counter did not move")
	}
}

func TestPreEgressSkipsWhenScannerTurns(t *testing.T) {
	// The egress check is LIVE, not write-time residue: an item clean at
	// write time is skipped at send time once the scanner condemns it.
	verdict := false
	s := NewStore(testNode(1), nil, func(v string) bool { return verdict && strings.Contains(v, "later-condemned") })
	if err := s.PutField("r", FieldName, "holds later-condemned bytes"); err != nil {
		t.Fatal(err)
	}
	if got := s.StatesFor([]string{"r"}); len(got) != 1 {
		t.Fatal("clean item skipped pre-egress")
	}
	verdict = true
	if got := s.StatesFor([]string{"r"}); len(got) != 0 {
		t.Fatal("condemned item egressed after the scanner turned")
	}
}

func TestDigestIdempotenceAndCanonicalBytes(t *testing.T) {
	a := NewStore(testNode(1), nil, allowAll)
	b := NewStore(testNode(2), nil, allowAll)
	if err := a.PutField("r1", FieldName, "n"); err != nil {
		t.Fatal(err)
	}
	if err := a.PutBody("r1", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	d0 := a.DigestOf()
	// Merging the same state twice is a no-op (idempotent ingress).
	for _, it := range a.AllStates() {
		b.MergeItem(it)
		b.MergeItem(it)
	}
	d1 := a.DigestOf()
	if !d0.Equal(d1) {
		t.Fatal("local digest moved without new updates")
	}
	if !d0.Equal(b.DigestOf()) {
		t.Fatal("identical updates diverged in digest")
	}
	ca, err := a.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	cb, err := b.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != string(cb) {
		t.Fatalf("canonical bytes differ:\n%s\n%s", ca, cb)
	}
	// Tombstones ride the digest: forgetting one would resurrect on gossip.
	a.Remove("r1")
	if a.DigestOf().Equal(d0) {
		t.Fatal("digest ignored the tombstone")
	}
}

func TestIDHashesSeeContentNotRecency(t *testing.T) {
	a := NewStore(testNode(1), nil, allowAll)
	b := NewStore(testNode(2), nil, allowAll)
	if err := a.PutField("r1", FieldName, "n"); err != nil {
		t.Fatal(err)
	}
	if err := a.PutField("r2", FieldName, "m"); err != nil {
		t.Fatal(err)
	}
	ah, bh := a.IDHashes(), b.IDHashes()
	if len(ah) != 2 || len(bh) != 0 {
		t.Fatalf("hash maps: a=%d b=%d, want 2/0", len(ah), len(bh))
	}
	for _, it := range a.AllStates() {
		b.MergeItem(it)
	}
	ah, bh = a.IDHashes(), b.IDHashes()
	for id, h := range ah {
		if bh[id] != h {
			t.Fatalf("synced peer disagrees on %s", id)
		}
	}
	// A changed byte moves exactly that id's hash — even when the
	// change carries an older wall than the shared maximum (skewed
	// clocks): hashes see content, not recency. This is the property
	// the max-stamp want-list lacked, and the property the gate proves.
	if err := a.PutField("r1", FieldName, "n2"); err != nil {
		t.Fatal(err)
	}
	ah2 := a.IDHashes()
	if ah2["r1"] == ah["r1"] {
		t.Fatal("edited item kept its content hash")
	}
	if ah2["r2"] != ah["r2"] {
		t.Fatal("unedited item moved its content hash")
	}
}
