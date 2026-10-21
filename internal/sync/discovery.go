// Peer discovery and the gossip engine (ROADMAP §4.2.3).
//
// LAN discovery rides mDNS via github.com/libp2p/zeroconf/v2 (pure Go, no
// stdlib equivalent — sanctioned in ARCHITECTURE.md §7 and
// config/allowed-modules.txt). v2 is the maintained fork of grandcat's
// v1: its Shutdown closes shouldShutdown, closes both UDP sockets, and
// waits on refCount before returning, so no goroutine reads server state
// the closer mutates (Issue #14 — the v1 race that made `go test -race`
// flake). Everything around the library is a seam:
// Announcer and Browser are interfaces the engine never sees past, so
// hermetic tests bind fakes and no test needs UDP 5353. When the host
// network blocks multicast, discovery degrades to offline mode — a
// warning on stderr and local-only state, never a fatal error and never
// a silent success.
//
// Service name note: ROADMAP §4.2.3 sketches `_pulse._tcp.`; the Wave 7
// grilling resolution (Issue #10) fixes `_pulse-sync._tcp.` on `local.`
// so sync advertisements are unambiguous on a LAN that also runs other
// Pulse-adjacent tooling. Recorded in ADL-0043.
package sync

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"time"

	"github.com/libp2p/zeroconf/v2"
)

// ServiceName and Domain fix the mDNS advertisement.
const (
	ServiceName   = "_pulse-sync._tcp"
	ServiceDomain = "local."
	// ProtoVersion rides the TXT record: a v2 peer that cannot read v1
	// frames refuses the gossip, not the discovery.
	ProtoVersion = "1"
)

// PeerInfo is one discovered peer: who, where, and how fresh.
type PeerInfo struct {
	NodeID string
	Host   string
	Port   int
	Clock  HLC
}

// txtRecords renders the advertisement: node id, packed HLC, proto.
func txtRecords(nodeID string, clock HLC) []string {
	return []string{"node=" + nodeID, "hlc=" + clock.Pack(), "proto=" + ProtoVersion}
}

// parseTXT inverts an advertisement. Unknown keys are ignored (a newer
// peer may advertise more); missing node/proto fail the entry — a peer
// that cannot name itself or its protocol is not a sync candidate.
func parseTXT(txt []string) (node string, clock HLC, proto string, err error) {
	vals := map[string]string{}
	for _, kv := range txt {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				vals[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	node = vals["node"]
	proto = vals["proto"]
	if node == "" {
		return "", HLC{}, "", fmt.Errorf("sync: advertisement without node id")
	}
	if proto == "" {
		return "", HLC{}, "", fmt.Errorf("sync: advertisement without proto version")
	}
	if raw, ok := vals["hlc"]; ok && raw != "" {
		if clock, err = UnpackHLC(raw); err != nil {
			return "", HLC{}, "", err
		}
	}
	return node, clock, proto, nil
}

// ---------------------------------------------------------------------------
// Seams: Announcer and Browser
// ---------------------------------------------------------------------------

// Announcer publishes this peer's presence. The returned function
// withdraws the advertisement.
type Announcer interface {
	Announce(ctx context.Context, self PeerInfo) (unannounce func(), err error)
}

// Browser watches for peers, delivering them until ctx ends.
type Browser interface {
	Browse(ctx context.Context) (<-chan PeerInfo, error)
}

// ErrOffline reports graceful degradation: multicast is unavailable, so
// this peer runs local-only. It is a typed condition so callers (and the
// TUI, later) can display "offline" instead of failing the process.
type ErrOffline struct{ Reason string }

func (e *ErrOffline) Error() string { return "sync: discovery offline: " + e.Reason }

// warnOffline is the degradation path: one stderr line, prefixed, never
// stdout (AGENTS.md §3.3 reserves stdout in MCP mode; discovery runs in
// both modes, so it never touches stdout at all).
func warnOffline(reason string) {
	fmt.Fprintf(os.Stderr, "pulse: sync discovery offline (%s); running local-only\n", reason)
}

// ZeroconfAnnouncer is the production Announcer: mDNS registration.
type ZeroconfAnnouncer struct {
	// Ifaces restricts multicast to interfaces (nil = system default).
	Ifaces []net.Interface
}

// Announce registers instance=nodeID on port self.Port.
func (a *ZeroconfAnnouncer) Announce(ctx context.Context, self PeerInfo) (func(), error) {
	var ifaces []net.Interface
	if a.Ifaces != nil {
		ifaces = a.Ifaces
	} else if all, err := net.Interfaces(); err == nil {
		ifaces = all
	}
	srv, err := zeroconf.Register(self.NodeID, ServiceName, ServiceDomain,
		self.Port, txtRecords(self.NodeID, self.Clock), ifaces)
	if err != nil {
		warnOffline(err.Error())
		return func() {}, &ErrOffline{Reason: err.Error()}
	}
	go func() {
		<-ctx.Done()
		srv.Shutdown()
	}()
	return srv.Shutdown, nil
}

// ZeroconfBrowser is the production Browser: mDNS browsing.
//
// Self names this peer so its own advertisement is filtered at the
// seam, not by every caller. It is set at construction — the Browser
// interface takes only ctx, so self-filtering must live inside the
// module to keep the adapter satisfying the interface.
type ZeroconfBrowser struct {
	Ifaces []net.Interface
	// Self filters this peer's own advertisement. Empty disables
	// self-filtering (tests that observe loopback).
	Self string
	// Timeout bounds one browse sweep for tests; zero selects no timeout
	// (browse until ctx ends, the production shape).
	Timeout time.Duration
}

var _ Browser = (*ZeroconfBrowser)(nil)

// Browse resolves service entries into PeerInfos. Entries that do not
// parse, speak another proto version, or name this peer are skipped —
// discovery is untrusted input, and a hostile LAN advertises garbage.
//
// libp2p/zeroconf/v2's Browse blocks and owns the entries channel: it
// closes entries (params.done) as it leaves its mainloop, on cancel,
// timeout, or query error. It sends entries WITHOUT a ctx select, though,
// so a consumer that walks away while the buffer is full leaves the
// library blocked on send. The pump therefore drains entries after
// cancellation until either the library closes it or Browse returns.
//
// Channel ownership: the sweep goroutine is the SOLE caller of the
// library and the sole closer of sweepsDone. The pump goroutine is the
// SOLE producer on out and its sole closer; out is closed only after the
// pump has stopped sending, so a fast cancel can never send on a closed
// channel.
func (b *ZeroconfBrowser) Browse(ctx context.Context) (<-chan PeerInfo, error) {
	entries := make(chan *zeroconf.ServiceEntry, 16)
	out := make(chan PeerInfo, 16)
	bctx := ctx
	cancel := context.CancelFunc(func() {})
	if b.Timeout > 0 {
		bctx, cancel = context.WithTimeout(ctx, b.Timeout)
	}
	selfNode := b.Self
	sweepsDone := make(chan struct{})
	// Sweep side: Browse blocks until bctx ends; returning means the
	// library can no longer send on entries.
	go func() {
		defer close(sweepsDone)
		if err := zeroconf.Browse(bctx, ServiceName, ServiceDomain, entries); err != nil {
			warnOffline(err.Error())
		}
	}()
	// Pump side: sole producer and sole closer of out.
	go func() {
		defer close(out)
		defer cancel()
		drain := func() {
			for {
				select {
				case _, ok := <-entries:
					if !ok {
						return
					}
				case <-sweepsDone:
					return
				}
			}
		}
		for {
			select {
			case <-bctx.Done():
				drain()
				return
			case e, ok := <-entries:
				if !ok {
					return
				}
				peer, err := entryToPeer(e)
				if err != nil {
					continue
				}
				if peer.NodeID == selfNode || peer.Port <= 0 {
					continue
				}
				select {
				case out <- peer:
				case <-bctx.Done():
					drain()
					return
				}
			}
		}
	}()
	return out, nil
}

// entryToPeer converts one mDNS entry: first usable IP wins, IPv4 before
// IPv6 (a LAN peer behind NAT64 is not this release's problem). A zero
// port is refused here — not at the browser — so every entryToPeer caller
// shares the validation (the browser's extra check is a backstop, not
// the gate).
func entryToPeer(e *zeroconf.ServiceEntry) (PeerInfo, error) {
	node, clock, proto, err := parseTXT(e.Text)
	if err != nil {
		return PeerInfo{}, err
	}
	if proto != ProtoVersion {
		return PeerInfo{}, fmt.Errorf("sync: proto %q from node %q, want %q", proto, node, ProtoVersion)
	}
	if e.Port <= 0 {
		return PeerInfo{}, fmt.Errorf("sync: node %q advertised port %d", node, e.Port)
	}
	var host string
	if len(e.AddrIPv4) > 0 {
		host = e.AddrIPv4[0].String()
	} else if len(e.AddrIPv6) > 0 {
		host = e.AddrIPv6[0].String()
	} else {
		return PeerInfo{}, fmt.Errorf("sync: node %q advertised no address", node)
	}
	return PeerInfo{NodeID: node, Host: host, Port: e.Port, Clock: clock}, nil
}

// ---------------------------------------------------------------------------
// Wire framing: length-prefixed frames over the mTLS connection
// ---------------------------------------------------------------------------

// MaxFrameBytes caps one gossip frame: a malicious peer that announces a
// gigabyte length gets an error, not an allocation.
const MaxFrameBytes = 8 << 20

// WriteFrame sends one length-prefixed frame (4-byte big-endian length).
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxFrameBytes {
		return fmt.Errorf("sync: frame is %d bytes, over the %d cap", len(payload), MaxFrameBytes)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("sync: write frame header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("sync: write frame body: %w", err)
	}
	return nil
}

// ReadFrame receives one length-prefixed frame.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("sync: read frame header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameBytes {
		return nil, fmt.Errorf("sync: frame announces %d bytes, over the %d cap", n, MaxFrameBytes)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("sync: read frame body: %w", err)
	}
	return payload, nil
}

// ---------------------------------------------------------------------------
// Gossip engine (ROADMAP §4.2.2)
// ---------------------------------------------------------------------------

// Frame types on the wire.
const (
	MsgDigest = "digest" // {Digest}
	MsgIDs    = "ids"    // {IDs: id->content-hash}
	MsgState  = "state"  // {Items: full states}
)

// Frame is one gossip message. Encoding is canonical JSON (sorted keys),
// so a frame built on any peer byte-compares with the same frame built
// anywhere else.
type Frame struct {
	From   string              `json:"from"`
	Type   string              `json:"type"`
	Digest *Digest             `json:"digest,omitempty"`
	IDs    map[string][32]byte `json:"ids,omitempty"`
	Items  []*Item             `json:"items,omitempty"`
}

// MarshalFrame renders a gossip frame for the wire: canonical JSON under
// the length-prefixed framing. UnmarshalFrame inverts it; trailing garbage
// after the frame is an error, never ignored.
func MarshalFrame(f Frame) ([]byte, error) {
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("sync: encode frame: %w", err)
	}
	return raw, nil
}

// UnmarshalFrame parses one received frame payload.
func UnmarshalFrame(raw []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(raw, &f); err != nil {
		return Frame{}, fmt.Errorf("sync: decode frame: %w", err)
	}
	if f.From == "" {
		return Frame{}, fmt.Errorf("sync: frame without sender")
	}
	switch f.Type {
	case MsgDigest, MsgIDs, MsgState:
		return f, nil
	default:
		return Frame{}, fmt.Errorf("sync: unknown frame type %q", f.Type)
	}
}

// Inbound pairs a received frame with its sender.
type Inbound struct {
	From string
	Msg  Frame
}

// Link is the injectable transport seam: queue a frame for a peer, drain
// queued inbound. The production link runs frames over the mTLS
// connection (WriteFrame/ReadFrame); hermetic tests bind the Mesh, which
// delivers in scripted orders.
type Link interface {
	Send(to string, msg Frame)
	// Recv drains one queued inbound frame, or false when empty.
	Recv() (Inbound, bool)
}

// Engine drives one peer's anti-entropy: Round broadcasts the digest and
// drains every queued inbound; Handle answers each message type. All
// merging passes through Store.MergeItem (filtered pre-merge) and all
// emission through the filtered StatesFor/IDStamps legs (pre-egress).
type Engine struct {
	Self  string
	Store *Store
	Link  Link
	Peers []string
}

// Round performs one anti-entropy exchange: broadcast the digest, then
// drain inbound until empty. Rounds are synchronous and deterministic —
// the convergence gate scripts delivery orders between rounds.
func (e *Engine) Round() {
	d := e.Store.DigestOf()
	for _, p := range e.Peers {
		if p == e.Self {
			continue
		}
		e.Link.Send(p, Frame{From: e.Self, Type: MsgDigest, Digest: &d})
	}
	for {
		in, ok := e.Link.Recv()
		if !ok {
			return
		}
		e.Handle(in)
	}
}

// Handle answers one inbound frame.
func (e *Engine) Handle(in Inbound) {
	msg := in.Msg
	switch msg.Type {
	case MsgDigest:
		if msg.Digest == nil {
			return
		}
		mine := e.Store.DigestOf()
		if mine.Equal(*msg.Digest) {
			return
		}
		e.Link.Send(in.From, Frame{From: e.Self, Type: MsgIDs, IDs: e.Store.IDHashes()})
	case MsgIDs:
		if msg.IDs == nil {
			return
		}
		// Content hashes on both sides: send my state for every LOCAL
		// id whose bytes differ anywhere (absent remotely counts).
		// Hash equality is byte-identity, so whatever is left out is
		// already agreed — no follow-up leg is needed and the
		// exchange terminates: merges only grow the join. (Comparing
		// in the wrong direction — "what I want from them" —
		// starves items only I hold; the gate caught that red too.)
		mine := e.Store.IDHashes()
		var ids []string
		for id, mh := range mine {
			if th, ok := msg.IDs[id]; !ok || th != mh {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		if len(ids) > 0 {
			e.Link.Send(in.From, Frame{From: e.Self, Type: MsgState, Items: e.Store.StatesFor(ids)})
		}
	case MsgState:
		for _, it := range msg.Items {
			e.Store.MergeItem(it)
		}
	}
}
