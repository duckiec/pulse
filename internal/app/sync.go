// P2P sync lifecycle: the composition root's binding for internal/sync.
//
// Wave 7 built the sync domain (HLC, CRDT, TOFU mTLS, zeroconf seams,
// gossip engine) deliberately unwired. This file wires it: the device
// identity becomes the node id, the redactor becomes the secret scanner,
// pins become dial policy, and the announcer/browser/listener/engine join
// the App's Boot/Close lifecycle.
//
// Every failure here degrades, never fails Boot. Sync is opportunistic
// infrastructure: a missing keychain, an empty ledger, a blocked
// multicast socket, or a taken port each produce a loud DEGRADED report
// and a local-only peer — never a process that refuses to start. The one
// exception is intent: SkipSync disables the subsystem silently, because
// an operator (or test) that asked for no sync already knows.
package app

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	stdsync "sync"
	"sync/atomic"
	"time"

	"pulse/internal/mcp"
	"pulse/internal/secrets"
	"pulse/internal/sync"
)

// DefaultSyncPort is the P2P listen/advertise port when unconfigured.
const DefaultSyncPort = 19431

// DefaultPeersFile is the TOFU ledger path when unconfigured, relative to
// the process working directory (the config/ precedent).
const DefaultPeersFile = "config/peers.txt"

// syncRoundInterval paces anti-entropy: gossip is background convergence,
// not a hot loop. Five seconds keeps a LAN convergent without chatter.
const syncRoundInterval = 5 * time.Second

// syncInboxCap bounds one peer's inbound queue. A flooding peer fills 128
// frames and then drops (counted) rather than growing memory without
// bound — gossip is loss-tolerant by construction (digests re-offer).
const syncInboxCap = 128

// SyncSubsystem is one peer's live sync state. Nil *SyncSubsystem on the
// App means sync is off (SkipSync) or degraded past usefulness; a
// non-nil subsystem with entries in App.Degradations means sync runs with
// stated limits (no announce, no dials, local-only, ...).
type SyncSubsystem struct {
	// NodeID is the stable hex peer id (first 8 bytes of the device-key hash).
	NodeID string
	// Store is the local CRDT state, filtered by the App redactor.
	Store *sync.Store
	// Engine drives anti-entropy over Link (this subsystem).
	Engine *sync.Engine

	cert     tls.Certificate
	pins     map[string]string
	listener net.Listener
	port     int

	unannounce func()
	cancel     context.CancelFunc
	ctx        context.Context
	wg         stdsync.WaitGroup

	mu    stdsync.Mutex
	conns map[string]net.Conn // node id -> live connection

	inbox   chan sync.Inbound
	dropped atomic.Int64
}

// Sync exposes the live subsystem, or nil when sync is off/degraded out.
func (a *App) Sync() *SyncSubsystem { return a.syncSub }

// setSyncSub publishes the subsystem. It runs once, synchronously inside
// New before the App is returned, and Close clears the field while holding
// closeMu — so the construction/teardown order is the synchronization and
// no further lock is needed.
func (a *App) setSyncSub(s *SyncSubsystem) { a.syncSub = s }

// Send implements sync.Link: marshal one frame toward a peer. A missing
// or broken connection drops the frame (counted): gossip re-offers
// whatever mattered on the next digest round.
func (s *SyncSubsystem) Send(to string, msg sync.Frame) {
	raw, err := sync.MarshalFrame(msg)
	if err != nil {
		return
	}
	s.mu.Lock()
	c, ok := s.conns[to]
	s.mu.Unlock()
	if !ok {
		s.dropped.Add(1)
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := sync.WriteFrame(c, raw); err != nil {
		s.dropped.Add(1)
		s.dropConn(to)
	}
}

// Recv implements sync.Link: drain one queued inbound frame, non-blocking.
func (s *SyncSubsystem) Recv() (sync.Inbound, bool) {
	select {
	case in := <-s.inbox:
		return in, true
	default:
		return sync.Inbound{}, false
	}
}

// DroppedFrames counts frames lost to missing/broken connections and full
// inboxes. It is a counter, not a log: gossip is loss-tolerant, and the
// count is the honest signal, not the payloads.
func (s *SyncSubsystem) DroppedFrames() int64 { return s.dropped.Load() }

// bootSync binds identity, ledger, listener, discovery, and engine. It
// never fails: every failure path degrades with a stated reason and
// leaves either a running (possibly limited) subsystem or a nil one.
func (a *App) bootSync() {
	if a.cfg.SkipSync {
		return
	}
	id, err := secrets.LoadIdentity(a.cfg.VaultKeyID, a.keyBackend)
	if err != nil {
		a.degrade(fmt.Sprintf("sync offline: no device identity (%v); run `pulse vault keygen` to enable P2P sync", err))
		return
	}
	defer clear(id.X25519Priv)
	defer clear(id.Ed25519Seed)
	pub := ed25519.NewKeyFromSeed(id.Ed25519Seed).Public().(ed25519.PublicKey)
	nodeID := sync.NodeIDFor(pub)
	nodeBytes, err := sync.NodeBytes(nodeID)
	if err != nil {
		a.degrade(fmt.Sprintf("sync offline: %v", err))
		return
	}

	pins := a.loadSyncPins()

	cert, err := sync.IssueCert(id.Ed25519Seed, nodeID, time.Now())
	if err != nil {
		a.degrade(fmt.Sprintf("sync offline: cannot mint the peer certificate (%v)", err))
		return
	}

	// Zero selects the default port; a negative value asks the kernel for
	// an ephemeral one (tests), so parallel or repeated boots never fight
	// over a fixed port. The bound port is read back below either way.
	addr := ":" + strconv.Itoa(DefaultSyncPort)
	if a.cfg.SyncPort < 0 {
		addr = ":0"
	} else if a.cfg.SyncPort > 0 {
		addr = ":" + strconv.Itoa(a.cfg.SyncPort)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		a.degrade(fmt.Sprintf("sync offline: cannot listen on %s (%v)", addr, err))
		return
	}
	port := a.cfg.SyncPort
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		port = tcpAddr.Port
	}

	ctx, cancel := context.WithCancel(a.ctx)
	sub := &SyncSubsystem{
		NodeID:     nodeID,
		Store:      sync.NewStore(nodeBytes, nil, a.syncScanner()),
		cert:       cert,
		pins:       pins,
		listener:   ln,
		port:       port,
		unannounce: func() {},
		cancel:     cancel,
		ctx:        ctx,
		conns:      map[string]net.Conn{},
		inbox:      make(chan sync.Inbound, syncInboxCap),
	}
	peers := make([]string, 0, len(pins))
	for n := range pins {
		if n != nodeID {
			peers = append(peers, n)
		}
	}
	sort.Strings(peers)
	sub.Engine = &sync.Engine{Self: nodeID, Store: sub.Store, Link: sub, Peers: peers}
	a.setSyncSub(sub)

	sub.wg.Add(3)
	go sub.acceptLoop(ctx)
	go sub.browseLoop(ctx)
	go sub.roundLoop(ctx)

	sub.announce(ctx, a)
}

// syncScanner binds the App redactor as the CRDT secret probe: any value
// the chain resolved (keychain, .env, vault — all registered at resolve
// time) is unfit for shared state. The probe is side-effect free
// (Contains, not String), so gossip scanning never pollutes metrics.
func (a *App) syncScanner() func(string) bool {
	red := a.Redactor
	return func(v string) bool {
		if red == nil {
			return true // no redactor, no knowledge: deny (fail closed)
		}
		return red.Contains(v)
	}
}

// loadSyncPins reads the TOFU ledger. A missing file degrades to an empty
// pin set (announce-only: discoverable, dialing nobody); a malformed file
// degrades the same way rather than half-loading — a partial ledger that
// silently unpins someone is worse than an empty one.
func (a *App) loadSyncPins() map[string]string {
	path := a.cfg.SyncPeersFile
	if path == "" {
		path = DefaultPeersFile
	}
	pins, err := sync.LoadPins(path)
	if err != nil {
		// errors.Is, not os.IsNotExist: LoadPins wraps the read failure
		// with %w, and os.IsNotExist does not unwrap — it would
		// misreport every absent ledger as malformed.
		if errors.Is(err, os.ErrNotExist) {
			a.degrade(fmt.Sprintf("sync pin ledger %s is absent: announcing only, dialing nobody (pin peers to sync)", path))
		} else {
			a.degrade(fmt.Sprintf("sync pin ledger %s is malformed (%v): refusing all peers until it parses", path, err))
		}
		return map[string]string{}
	}
	if len(pins) == 0 {
		a.degrade(fmt.Sprintf("sync pin ledger %s holds no pins: announcing only, dialing nobody", path))
	}
	return pins
}

// announce publishes this peer on mDNS. Failure (blocked multicast is the
// common one) degrades: direct dials and the listener still work, and the
// ledger still gates everything.
func (s *SyncSubsystem) announce(ctx context.Context, a *App) {
	host, err := outboundIP()
	if err != nil {
		a.degrade(fmt.Sprintf("sync announce skipped (%v); direct dials still work", err))
		return
	}
	ann := &sync.ZeroconfAnnouncer{}
	unannounce, err := ann.Announce(ctx, sync.PeerInfo{
		NodeID: s.NodeID, Host: host, Port: s.port, Clock: s.Store.Clock(),
	})
	if err != nil {
		a.degrade(fmt.Sprintf("sync announce unavailable (%v); direct dials still work", err))
		return
	}
	s.mu.Lock()
	s.unannounce = unannounce
	s.mu.Unlock()
}

// outboundIP finds the preferred outbound address without sending a byte:
// connecting a UDP socket performs no handshake, it only selects the
// source address the kernel would use.
func outboundIP() (string, error) {
	c, err := net.Dial("udp", "255.255.255.255:1")
	if err != nil {
		return "", fmt.Errorf("no outbound address: %w", err)
	}
	defer c.Close()
	addr, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil {
		return "", fmt.Errorf("no outbound address")
	}
	return addr.IP.String(), nil
}

// acceptLoop serves pinned peers: the TLS layer enforces the ledger, so
// any completed handshake is already TOFU-authorized. An accepted conn
// learns its owner from the first frame's From; a dialed conn arrives
// with its expected node as owner, and readLoop drops the connection if
// a frame speaks as anyone else.
func (s *SyncSubsystem) acceptLoop(ctx context.Context) {
	defer s.wg.Done()
	tlsLn := tls.NewListener(s.listener, sync.ServerConfig(s.cert, s.pins))
	for {
		c, err := tlsLn.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// Transient accept error: back off rather than hot-spin.
			// (A closed listener also lands here, but closeSync
			// cancels first, so the next select exits.)
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}
		s.wg.Add(1)
		go s.readLoop(ctx, c, "")
	}
}

// browseLoop dials DISCOVERED+BUT-PINNED peers only. Discovery without a
// pin is display, never a dial (ROADMAP §4.2.3): the pins map gates every
// connection this peer initiates.
func (s *SyncSubsystem) browseLoop(ctx context.Context) {
	defer s.wg.Done()
	b := &sync.ZeroconfBrowser{}
	ch, err := b.Browse(ctx, s.NodeID)
	if err != nil {
		mcp.Logf("sync browse unavailable (%v); pinned direct dials still work", err)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case peer, ok := <-ch:
			if !ok {
				return
			}
			s.maybeDial(peer)
		}
	}
}

// maybeDial opens one mTLS connection to a discovered peer the ledger
// files. Unknown nodes, our own advertisement, and already-connected
// nodes are skipped without a byte sent.
func (s *SyncSubsystem) maybeDial(peer sync.PeerInfo) {
	if _, ok := s.pins[peer.NodeID]; !ok {
		return
	}
	if peer.NodeID == s.NodeID || peer.Port <= 0 || peer.Host == "" {
		return
	}
	s.mu.Lock()
	if _, ok := s.conns[peer.NodeID]; ok {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	addr := net.JoinHostPort(peer.Host, strconv.Itoa(peer.Port))
	d := &net.Dialer{Timeout: 10 * time.Second}
	c, err := tls.DialWithDialer(d, "tcp", addr, sync.ClientConfig(s.cert, s.pins))
	if err != nil {
		return // peer down or refusing: next browse will retry
	}
	s.mu.Lock()
	s.conns[peer.NodeID] = c
	s.mu.Unlock()
	s.wg.Add(1)
	go s.readLoop(s.ctx, c, peer.NodeID)
}

// roundLoop paces anti-entropy.
func (s *SyncSubsystem) roundLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(syncRoundInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Engine.Round()
		}
	}
}

// readLoop pumps frames from one connection into the engine inbox and
// answers nothing inline: replies are emitted by Engine.Round and by
// Handle's immediate answers, both through Send. owner is the expected
// node for dialed conns, or "" to learn from the first frame.
func (s *SyncSubsystem) readLoop(ctx context.Context, c net.Conn, owner string) {
	defer s.wg.Done()
	defer c.Close()
	for {
		raw, err := sync.ReadFrame(c)
		if err != nil {
			if owner != "" {
				s.dropConn(owner)
			}
			return
		}
		msg, err := sync.UnmarshalFrame(raw)
		if err != nil {
			continue // hostile garbage: drop the frame, keep the peer
		}
		if owner == "" {
			owner = msg.From
			s.mu.Lock()
			s.conns[owner] = c
			s.mu.Unlock()
		} else if msg.From != owner {
			// A pinned peer speaking as another pinned peer: drop the
			// frame AND the connection, loudly. The TLS layer proved a
			// pinned key; it did not prove WHICH pinned identity may
			// use this socket.
			mcp.Logf("sync: peer %q spoke as %q; dropping the connection", owner, msg.From)
			s.dropConn(owner)
			return
		}
		select {
		case s.inbox <- sync.Inbound{From: owner, Msg: msg}:
		default:
			s.dropped.Add(1)
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// dropConn closes and forgets one peer connection. It is idempotent:
// read-loop exits and failed sends converge here from all sides.
func (s *SyncSubsystem) dropConn(node string) {
	s.mu.Lock()
	c, ok := s.conns[node]
	if ok {
		delete(s.conns, node)
	}
	s.mu.Unlock()
	if ok {
		_ = c.Close()
	}
}

// closeSync shuts the subsystem down: stop discovery and rounds, withdraw
// the advertisement, close the listener and every peer connection, then
// wait for the loops to exit. It runs BEFORE the store closes (no store
// dependency, but gossip must be silent before secret tiers are scrubbed)
// and it is safe to call with a nil subsystem.
func (a *App) closeSync() {
	s := a.syncSub
	if s == nil {
		return
	}
	a.syncSub = nil
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Lock()
	unannounce := s.unannounce
	s.mu.Unlock()
	if unannounce != nil {
		unannounce()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.mu.Lock()
	for node, c := range s.conns {
		delete(s.conns, node)
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
