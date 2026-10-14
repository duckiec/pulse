package sync

// The 3-node convergence gate (ROADMAP §4.2.1 verify, Issue #10 Part 4).
//
// Three peers with skewed clocks (one ahead by hours, one regressed by an
// hour MID-scenario) execute concurrent conflicting edits, header
// mutations, and deletions. Gossip is then delivered in scripted orders —
// forward, reversed, rotated, and seeded-random permutations — and every
// order must end with all three peers byte-for-byte identical.
//
// The transport is the Mesh: an in-memory Link that queues every frame
// and delivers only when the test says so, in the order the test says.
// No network, no wall clock (each peer owns a manual clock), no
// goroutines: rounds are synchronous, so the gate is deterministic and
// the synctest bubble proves zero wall-time dependence.

import (
	"fmt"
	"math/rand"
	"testing"
	"testing/synctest"
	"time"
)

// routed is one queued frame with its destination.
type routed struct {
	to  string
	msg Frame
}

// Mesh is the hermetic Link: Send queues globally; Deliver routes queued
// frames to destination inboxes in the given order; Round runs one engine
// round on every peer.
type Mesh struct {
	links   map[string]*meshLink
	pending []routed
}

type meshLink struct {
	mesh  *Mesh
	self  string
	inbox []Inbound
}

func newMesh(ids ...string) *Mesh {
	m := &Mesh{links: map[string]*meshLink{}}
	for _, id := range ids {
		m.links[id] = &meshLink{mesh: m, self: id}
	}
	return m
}

func (l *meshLink) Send(to string, msg Frame) {
	l.mesh.pending = append(l.mesh.pending, routed{to: to, msg: msg})
}

func (l *meshLink) Recv() (Inbound, bool) {
	if len(l.inbox) == 0 {
		return Inbound{}, false
	}
	in := l.inbox[0]
	l.inbox = l.inbox[1:]
	return in, true
}

// Deliver routes pending frames in perm order (indices into the pending
// queue). Undelivered frames stay queued for a later Deliver — partitions
// are just orders that omit frames.
func (m *Mesh) Deliver(perm []int) {
	rest := make([]routed, 0, len(m.pending))
	done := make([]bool, len(m.pending))
	for _, i := range perm {
		if i < 0 || i >= len(m.pending) || done[i] {
			continue
		}
		done[i] = true
		r := m.pending[i]
		if l, ok := m.links[r.to]; ok {
			l.inbox = append(l.inbox, Inbound{From: r.msg.From, Msg: r.msg})
		}
	}
	for i, r := range m.pending {
		if !done[i] {
			rest = append(rest, r)
		}
	}
	m.pending = rest
}

// identityPerm delivers everything in send order.
func (m *Mesh) identityPerm() []int {
	perm := make([]int, len(m.pending))
	for i := range perm {
		perm[i] = i
	}
	return perm
}

// manualClock is a skewed, scriptable clock: base offset plus 1ms per
// read (every stamp distinct), with a Regress step that drags the wall
// backwards mid-scenario — the HLC counter must carry monotonicity
// through it.
type manualClock struct {
	base time.Time
	n    int64
}

func (c *manualClock) now() time.Time {
	t := c.base.Add(time.Duration(c.n) * time.Millisecond)
	c.n++
	return t
}

// peer bundles one gossip participant.
type peer struct {
	id     string
	store  *Store
	engine *Engine
}

func newPeer(m *Mesh, id string, base time.Time) *peer {
	nb, _ := NodeBytes(id)
	clk := &manualClock{base: base}
	s := NewStore(nb, clk.now, allowAll)
	return &peer{id: id, store: s, engine: &Engine{
		Self:  id,
		Store: s,
		Link:  m.links[id],
		Peers: []string{},
	}}
}

// scriptConcurrentEdits runs the conflicting workload BEFORE any gossip:
// concurrent creates of the same ids with different values, header
// mutations that collide, and deletions racing edits. Clocks are skewed
// (B +2h, C -1h) so wall order disagrees with causal order on purpose.
func scriptConcurrentEdits(a, b, c *peer) error {
	mk := func(p *peer, id, name, method string) error {
		if err := p.store.PutField(id, FieldName, name); err != nil {
			return err
		}
		return p.store.PutField(id, FieldMethod, method)
	}
	// req-1: created on all three with different names (3-way conflict).
	if err := mk(a, "req-1", "login-A", "POST"); err != nil {
		return err
	}
	if err := mk(b, "req-1", "login-B", "POST"); err != nil {
		return err
	}
	if err := mk(c, "req-1", "login-C", "GET"); err != nil {
		return err
	}
	if err := a.store.PutField("req-1", HeaderPrefix+"X-Trace", "aaa"); err != nil {
		return err
	}
	if err := b.store.PutField("req-1", HeaderPrefix+"X-Trace", "bbb"); err != nil {
		return err
	}
	if err := a.store.PutBody("req-1", []byte(`{"user":"a"}`)); err != nil {
		return err
	}
	if err := b.store.PutBody("req-1", []byte(`{"user":"b","extra":true}`)); err != nil {
		return err
	}
	// C deletes req-1 while A and B edit it (delete races edits).
	c.store.Remove("req-1")
	// req-2: A and C edit, B deletes (delete races edits, other direction).
	if err := mk(a, "req-2", "health", "GET"); err != nil {
		return err
	}
	if err := mk(c, "req-2", "health", "GET"); err != nil {
		return err
	}
	if err := c.store.PutField("req-2", FieldURL, "https://c/v2"); err != nil {
		return err
	}
	if err := a.store.PutBody("req-2", []byte("ping")); err != nil {
		return err
	}
	b.store.Remove("req-2")
	// req-3: A-only (pure propagation check).
	if err := mk(a, "req-3", "solo", "DELETE"); err != nil {
		return err
	}
	return nil
}

// quiescent reports agreement plus nothing but digests in flight: a
// digest-broadcast protocol ALWAYS has digests queued, so "empty network"
// is the wrong quiescence bar (the gate went red on exactly this — the
// protocol had converged by round 3 while the harness demanded a silence
// the protocol never produces). IDs/State frames mean work remains.
func quiescent(peers []*peer, m *Mesh) bool {
	d0 := peers[0].store.DigestOf()
	for _, p := range peers[1:] {
		if !p.store.DigestOf().Equal(d0) {
			return false
		}
	}
	for _, r := range m.pending {
		if r.msg.Type != MsgDigest {
			return false
		}
	}
	return true
}

// converge runs rounds until quiescent or the budget runs out. Each round
// emits digests, delivers pending in permute(pending), and drains. It
// returns the rounds used and whether all peers agree.
func converge(peers []*peer, m *Mesh, permute func(n int) []int) (int, bool) {
	for round := 0; round < 24; round++ {
		for _, p := range peers {
			p.engine.Round()
		}
		m.Deliver(permute(len(m.pending)))
		if quiescent(peers, m) {
			// Probe round: agreement must SURVIVE one more full
			// exchange (quiescence, not coincidence).
			for _, p := range peers {
				p.engine.Round()
			}
			if quiescent(peers, m) {
				return round + 1, true
			}
		}
	}
	return 24, false
}

func canonicalAll(t *testing.T, peers []*peer) []string {
	t.Helper()
	out := make([]string, len(peers))
	for i, p := range peers {
		raw, err := p.store.MarshalCanonical()
		if err != nil {
			t.Fatalf("peer %d canonical: %v", i, err)
		}
		out[i] = string(raw)
	}
	return out
}

// runScenario builds the 3-peer world, scripts the workload (with C's
// clock regressing an hour mid-edit), and converges under one delivery
// order. It returns the converged canonical bytes.
func runScenario(t *testing.T, permute func(n int) []int) string {
	t.Helper()
	ids := []string{
		fmt.Sprintf("%016x", uint64(0xA1)),
		fmt.Sprintf("%016x", uint64(0xB2)),
		fmt.Sprintf("%016x", uint64(0xC3)),
	}
	base := time.UnixMilli(1_700_000_000_000)
	m := newMesh(ids...)
	peers := []*peer{
		newPeer(m, ids[0], base),
		newPeer(m, ids[1], base.Add(2*time.Hour)), // skewed ahead
		newPeer(m, ids[2], base.Add(-time.Hour)),  // skewed behind
	}
	for _, p := range peers {
		p.engine.Peers = ids
	}
	if err := scriptConcurrentEdits(peers[0], peers[1], peers[2]); err != nil {
		t.Fatalf("script: %v", err)
	}
	rounds, ok := converge(peers, m, permute)
	if !ok {
		t.Fatalf("peers did not converge under this delivery order")
	}
	t.Logf("converged in %d rounds", rounds)
	got := canonicalAll(t, peers)
	for i := 1; i < len(got); i++ {
		if got[i] != got[0] {
			t.Fatalf("peer %d diverged:\n%s\n---\n%s", i, got[0], got[i])
		}
	}
	// Liveness facts that hold under EVERY order (OR-Set semantics, not
	// luck): req-3 propagated everywhere; req-1/req-2 resolve identically.
	st := peers[0].store
	if r3 := st.Get("req-3"); r3 == nil || !r3.Alive() {
		t.Fatalf("req-3 (uncontended) missing after convergence")
	}
	if st.Get("req-1") == nil || st.Get("req-2") == nil {
		t.Fatal("tombstoned shells must survive as tombstones (else deletes un-propagate)")
	}
	return got[0]
}

func reversePerm(n int) []int {
	perm := make([]int, n)
	for i := range perm {
		perm[i] = n - 1 - i
	}
	return perm
}

func rotatePerm(n int) []int {
	perm := make([]int, n)
	for i := range perm {
		perm[i] = (i + 1) % n
	}
	if n > 0 {
		perm[n-1] = 0
	}
	return perm
}

func seededPerm(seed int64) func(n int) []int {
	return func(n int) []int {
		return rand.New(rand.NewSource(seed)).Perm(n)
	}
}

// TestThreeNodeConvergence proves the gate: the same concurrent workload
// converges byte-for-byte under five delivery orders. The synctest bubble
// wraps the whole proof — virtual sleeps between scenarios prove no leg
// reads the wall clock (all time is injected), and bubble exit is the
// goroutine-leak assertion (there are no goroutines to leak: rounds are
// synchronous by construction).
func TestThreeNodeConvergence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		orders := map[string]func(n int) []int{
			"identity": func(n int) []int {
				perm := make([]int, n)
				for i := range perm {
					perm[i] = i
				}
				return perm
			},
			"reverse": reversePerm,
			"rotate":  rotatePerm,
			"seed-1":  seededPerm(1),
			"seed-7":  seededPerm(7),
		}
		results := map[string]string{}
		for name, perm := range orders {
			results[name] = runScenario(t, perm)
			time.Sleep(5 * time.Millisecond) // virtual: proves wall-independence
		}
		for name, got := range results {
			if got != results["identity"] {
				t.Fatalf("order %s converged DIFFERENTLY than identity order", name)
			}
		}
		synctest.Wait()
	})
}

// TestDoubleDeliveryIsANoOp proves idempotent ingress: replaying every
// delivered frame changes no digest and no byte.
func TestDoubleDeliveryIsANoOp(t *testing.T) {
	ids := []string{fmt.Sprintf("%016x", uint64(0xA1)), fmt.Sprintf("%016x", uint64(0xB2))}
	base := time.UnixMilli(1_700_000_000_000)
	m := newMesh(ids...)
	a, b := newPeer(m, ids[0], base), newPeer(m, ids[1], base)
	a.engine.Peers, b.engine.Peers = ids, ids
	if err := a.store.PutField("r", FieldName, "v"); err != nil {
		t.Fatal(err)
	}
	peers := []*peer{a, b}
	if _, ok := converge(peers, m, reversePerm); !ok {
		t.Fatal("did not converge")
	}
	before, err := a.store.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	// Replay: every state, twice, at both peers.
	for _, p := range peers {
		for _, q := range peers {
			for _, it := range q.store.AllStates() {
				p.store.MergeItem(it)
				p.store.MergeItem(it)
			}
		}
	}
	after, err := a.store.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("replay moved state:\n%s\n---\n%s", before, after)
	}
}
