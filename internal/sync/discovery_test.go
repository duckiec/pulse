package sync

// Gates for discovery and the wire: TXT advertisement parsing (hostile
// LAN input is untrusted), frame round-trips and caps, the byte-canonical
// frame encoding, and a loopback mDNS proof that degrades to an honest
// SKIP when multicast is unavailable.

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
)

func TestTXTAdvertisementRoundTrip(t *testing.T) {
	clock := HLC{WallMS: 1_700_000_000_000, Counter: 3, Node: testNode(9)}
	txt := txtRecords("abc123", clock)
	node, back, proto, err := parseTXT(txt)
	if err != nil {
		t.Fatalf("parseTXT: %v", err)
	}
	if node != "abc123" || proto != ProtoVersion {
		t.Fatalf("node=%q proto=%q", node, proto)
	}
	if back.Compare(clock) != 0 {
		t.Fatalf("clock did not survive TXT: %+v vs %+v", back, clock)
	}
}

func TestTXTRejectsHostileShapes(t *testing.T) {
	for name, txt := range map[string][]string{
		"no node":   {"hlc=" + HLC{}.Pack(), "proto=1"},
		"no proto":  {"node=abc"},
		"bad clock": {"node=abc", "proto=1", "hlc=zzz"},
		"empty":     {},
	} {
		if _, _, _, err := parseTXT(txt); err == nil {
			t.Fatalf("%s: hostile TXT accepted", name)
		}
	}
	// Unknown keys are tolerated (newer peers advertise more); a missing
	// clock is tolerated (fresh peer, zero HLC).
	node, _, proto, err := parseTXT([]string{"node=abc", "proto=1", "future=x"})
	if err != nil || node != "abc" || proto != "1" {
		t.Fatalf("forward-compatible TXT refused: %v", err)
	}
}

func TestEntryToPeerShapes(t *testing.T) {
	mk := func(txt []string, v4, v6 string, port int) *zeroconf.ServiceEntry {
		e := &zeroconf.ServiceEntry{Port: port, Text: txt}
		if v4 != "" {
			e.AddrIPv4 = append(e.AddrIPv4, net.ParseIP(v4))
		}
		if v6 != "" {
			e.AddrIPv6 = append(e.AddrIPv6, net.ParseIP(v6))
		}
		return e
	}
	clock := HLC{WallMS: 5, Node: testNode(1)}
	txt := txtRecords("node9", clock)
	// IPv4 wins over IPv6.
	p, err := entryToPeer(mk(txt, "192.168.1.7", "::1", 8444))
	if err != nil {
		t.Fatal(err)
	}
	if p.Host != "192.168.1.7" || p.Port != 8444 || p.NodeID != "node9" {
		t.Fatalf("peer = %+v", p)
	}
	// v6-only works.
	p, err = entryToPeer(mk(txt, "", "fe80::1", 8444))
	if err != nil || p.Host != "fe80::1" {
		t.Fatalf("v6 peer = %+v err=%v", p, err)
	}
	// No address, hostile TXT, foreign proto, zero port: all refused.
	if _, err := entryToPeer(mk(txt, "", "", 8444)); err == nil {
		t.Fatal("address-less entry accepted")
	}
	if _, err := entryToPeer(mk([]string{"node=x"}, "10.0.0.1", "", 8444)); err == nil {
		t.Fatal("hostile TXT accepted")
	}
	if _, err := entryToPeer(mk([]string{"node=x", "proto=99"}, "10.0.0.1", "", 8444)); err == nil {
		t.Fatal("foreign proto accepted")
	}
	if _, err := entryToPeer(mk(txt, "10.0.0.1", "", 0)); err == nil {
		t.Fatal("zero-port entry accepted")
	}
}

func TestFrameWireRoundTrip(t *testing.T) {
	s := NewStore(testNode(1), nil, allowAll)
	if err := s.PutField("r1", FieldName, "n"); err != nil {
		t.Fatal(err)
	}
	d := s.DigestOf()
	frames := []Frame{
		{From: "aa", Type: MsgDigest, Digest: &d},
		{From: "aa", Type: MsgIDs, IDs: s.IDHashes()},
		{From: "aa", Type: MsgState, Items: s.AllStates()},
	}
	for _, f := range frames {
		raw, err := MarshalFrame(f)
		if err != nil {
			t.Fatal(err)
		}
		// Length-prefix framing carries it intact.
		var buf bytes.Buffer
		if err := WriteFrame(&buf, raw); err != nil {
			t.Fatal(err)
		}
		back, err := ReadFrame(&buf)
		if err != nil {
			t.Fatal(err)
		}
		got, err := UnmarshalFrame(back)
		if err != nil {
			t.Fatal(err)
		}
		again, err := MarshalFrame(got)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, again) {
			t.Fatalf("frame %s not byte-stable across the wire", f.Type)
		}
	}
	// Unknown type and sender-less frames refuse.
	if _, err := UnmarshalFrame([]byte(`{"from":"a","type":"evil"}`)); err == nil {
		t.Fatal("unknown frame type accepted")
	}
	if _, err := UnmarshalFrame([]byte(`{"type":"digest"}`)); err == nil {
		t.Fatal("sender-less frame accepted")
	}
	// The cap holds against a lying length prefix.
	if err := WriteFrame(&bytes.Buffer{}, make([]byte, MaxFrameBytes+1)); err == nil {
		t.Fatal("oversize frame accepted for write")
	}
	lying := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := ReadFrame(bytes.NewReader(lying)); err == nil {
		t.Fatal("lying length prefix accepted for read")
	}
}

func TestOfflineErrorIsTyped(t *testing.T) {
	err := &ErrOffline{Reason: "no multicast"}
	if err.Error() == "" {
		t.Fatal("empty offline error")
	}
	var target *ErrOffline
	if !errors.As(err, &target) {
		t.Fatal("offline error lost its type")
	}
}

// TestLoopbackDiscovery proves the real zeroconf path on this host: one
// announcer, one browser, same service name. Multicast is host-network
// dependent (blocked in many CI sandboxes and some WSL configs), so
// failure to observe the advertisement is an honest SKIP with reason —
// never a pass, never a failure. The hermetic seam (fake Announcer/
// Browser in convergence) carries the hard gates either way.
func TestLoopbackDiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	self := PeerInfo{NodeID: "loopback-probe", Host: "127.0.0.1", Port: 19431}
	ann := &ZeroconfAnnouncer{}
	unannounce, err := ann.Announce(ctx, self)
	if err != nil {
		t.Skipf("SKIP (reason: mDNS announce unavailable: %v)", err)
	}
	defer unannounce()
	b := &ZeroconfBrowser{Self: "other-node", Timeout: 6 * time.Second}
	ch, err := b.Browse(ctx)
	if err != nil {
		t.Skipf("SKIP (reason: mDNS browse unavailable: %v)", err)
	}
	deadline := time.After(7 * time.Second)
	for {
		select {
		case p, ok := <-ch:
			if !ok {
				t.Skip("SKIP (reason: mDNS browse channel closed without our advertisement)")
			}
			if p.NodeID == self.NodeID && p.Port == self.Port {
				return // observed ourselves on the loopback
			}
		case <-deadline:
			t.Skip("SKIP (reason: no mDNS advertisement observed in 7s; multicast likely blocked)")
		case <-ctx.Done():
			t.Skip("SKIP (reason: context ended before mDNS observation)")
		}
	}
}

// TestBrowseFastCancelNeverPanics is the regression gate for the Stage-4
// send-on-closed-channel panic: close(out) lived in the resolver goroutine
// while the pump goroutine still sent to out, so a fast cancel or a
// multicast failure closed the channel under a live sender.
//
// The fix binds out's lifetime strictly to its sole producer (the pump):
// the resolver owns entries only, the pump owns out only, and close(out)
// runs only after the pump's sends deterministically ceased. This gate
// forces fast cancellation (and a 1ms sweep timeout, the multicast-failure
// shape) across 50 iterations under -race: any send-on-close is a fatal
// panic, and a leaked non-closed channel is a failure. It needs no UDP
// 5353 — NewResolver succeeds without multicast; Browse blocks, cancel
// fires, and the lifetime invariant is what is asserted.
func TestBrowseFastCancelNeverPanics(t *testing.T) {
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		b := &ZeroconfBrowser{Self: "self", Timeout: time.Millisecond}
		ch, err := b.Browse(ctx)
		if err != nil {
			cancel()
			// Offline (no resolver) is honest degradation, not a panic:
			// the gate is that Browse never panics, never leaks a send.
			continue
		}
		// Cancel fast — before, during, and after the 1ms sweep — then
		// drain until close. A send-on-closed-channel panics here under
		// -race; a non-closing channel fails the gate below.
		cancel()
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			for range ch {
			}
		}()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatalf("iter %d: browse channel never closed after fast cancel", i)
		}
	}
}

// TestBrowserInterfaceSatisfied pins Q4: the production adapter satisfies
// the Browser seam with ctx only. Self lives at construction; callers
// never pass it per-call. If the signature drifts again this fails at
// build time, not at Stage-4 runtime.
func TestBrowserInterfaceSatisfied(t *testing.T) {
	var _ Browser = (*ZeroconfBrowser)(nil)
	b := &ZeroconfBrowser{Self: "abc"}
	if b.Self != "abc" {
		t.Fatal("Self not retained at construction")
	}
}
