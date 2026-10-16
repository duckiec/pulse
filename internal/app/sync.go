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

	// persist durably records the store after every round; nil when the
	// App has no store handle (tests that skip persistence).
	persist func()
	// publishDir, when set, is the .http collection converged into the
	// store whenever its fingerprint moves. lastPrint tracks the last
	// converged state so steady state costs no parse. resync runs the
	// check; both are set by bootSync, and resync is nil when no dir
	// is configured.
	publishDir string
	lastPrint  string
	resync     func()
	// static maps pinned node ids to fixed addresses for the manual
	// backend. Dialed through the same pin-gated maybeDial as
	// discovered peers, re-attempted every round until connected.
	static map[string]string

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
	sub.publishDir = a.cfg.SyncPublishDir
	sub.persist = func() { a.persistSyncSnapshot(sub) }
	sub.resync = func() { a.resyncPublishDir(sub) }
	sub.static = a.cfg.SyncStaticPeers
	a.loadSyncSnapshot(sub)
	if sub.publishDir != "" {
		a.resyncPublishDir(sub)
	}
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
	b := &sync.ZeroconfBrowser{Self: s.NodeID}
	ch, err := b.Browse(ctx)
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
		// Logged, not silent: a peer that refuses every handshake is the
		// observable half of a TOFU rejection (the other half is the
		// server's failed VerifyPeerCertificate). Next browse retries.
		mcp.Logf("sync: dial %s (%s): %v", peer.NodeID, addr, err)
		return
	}
	// Keep-first on the post-dial recheck: the pre-dial check above runs
	// without the lock held (the dial itself takes seconds), so a round
	// tick and an mDNS hit for the same peer race every 5s. The loser must
	// close its conn, never overwrite the map: an overwritten conn leaves
	// a readLoop parked on a socket closeSync no longer knows, and Close
	// hangs in wg.Wait forever. The first conn is stable by construction.
	s.mu.Lock()
	if old, ok := s.conns[peer.NodeID]; ok && old != c {
		s.mu.Unlock()
		_ = c.Close()
		return
	}
	s.conns[peer.NodeID] = c
	s.mu.Unlock()
	s.wg.Add(1)
	go s.readLoop(s.ctx, c, peer.NodeID)
}

// dialStatic dials pinned static peers this subsystem is not yet
// connected to. The pin gate inside maybeDial still applies: a static
// address for an unpinned node is never dialed.
func (s *SyncSubsystem) dialStatic() {
	for node, addr := range s.static {
		if node == s.NodeID {
			continue
		}
		host, port, ok := splitHostPort(addr)
		if !ok {
			continue // misconfigured once, skipped every round, never fatal
		}
		s.maybeDial(sync.PeerInfo{NodeID: node, Host: host, Port: port})
	}
}

// splitHostPort parses a "host:port" static peer address. A misconfigured
// entry parses to not-ok and is skipped every round, never fatal: one bad
// line must not wedge the peers that parsed.
func splitHostPort(addr string) (host string, port int, ok bool) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil || h == "" {
		return "", 0, false
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 || n > 65535 {
		return "", 0, false
	}
	return h, n, true
}

// roundLoop paces anti-entropy, then makes it durable: the collection
// dir is re-converged when it moved, and the snapshot persisted, every
// round. Both are synchronous in this goroutine — the only writer — so
// no lock guards the store.
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
			s.dialStatic()
			if s.resync != nil {
				s.resync()
			}
			if s.persist != nil {
				s.persist()
			}
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
	frames := 0
	for {
		raw, err := sync.ReadFrame(c)
		if err != nil {
			if owner != "" {
				if frames == 0 {
					// Dialed peer, zero frames delivered: the TCP+TLS
					// dial completed but the peer refused us before any
					// gossip flowed — the TOFU handshake rejection
					// (TLS 1.3 delivers the server's alert to our first
					// Read, after our Dial already succeeded: there is
					// no dial error to log, so this Read error IS the
					// observable half of the rejection). Without it an
					// unpinned peer's exclusion is silent and no
					// operator can tell refusal from misconfiguration.
					// Later deaths (frames > 0) stay silent: gossip
					// is loss-tolerant and the count, not the log, is
					// the signal there. The error carries alert text
					// and node ids only — never key material — so it
					// is safe for stderr under the redaction rules.
					mcp.Logf("sync: dial %s: connection failed before first frame: %v", owner, err)
				}
				s.dropConn(owner)
			}
			return
		}
		frames++ // a frame arrived: the peer is alive and talking
		msg, err := sync.UnmarshalFrame(raw)
		if err != nil {
			continue // hostile garbage: drop the frame, keep the peer
		}
		if owner == "" {
			owner = msg.From
			s.mu.Lock()
			// Keep-first, mirroring maybeDial's post-dial recheck: a
			// dialed conn and an accepted conn for the same peer
			// race routinely (static redial vs inbound accept), and
			// overwriting the map leaks the loser out of closeSync's
			// reach — a readLoop parked forever, a Close hung forever.
			// The deferred Close below releases the loser; the first
			// conn stays the stable one.
			if old, ok := s.conns[owner]; ok && old != c {
				s.mu.Unlock()
				return
			}
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
	// Withdraw the advertisement before tearing down the rest, but do it in
	// a way the race detector cannot confuse with a concurrent mainloop read.
	// closeSync is the sole caller of unannounce after boot, and the returned
	// function is the ZeroconfAnnouncer's captured srv.Shutdown, so the only
	// real concurrency here is cancel vs mainloop and listener.Close vs Accept.
	// The race detector flags zeroconf.Register's Shutdown path as racy today
	// under -race even when the happens-before is correct (cancel fires first,
	// then unannounce, then the listener/conns, then wg.Wait), so admit the
	// shutdown Ordering here as best-effort under -race rather than try to
	// double-close the advertisement.
	if unannounce != nil {
		unannounce()
	}
	// Cancel first so the loops and the defer shutdown in Announce unwind
	// through context rather than through tear-down-under-them.
	if s.cancel != nil {
		s.cancel()
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
	// Final persist after the loops drain: the last round's merges must
	// survive the restart, or the next boot refetches them.
	if s.persist != nil {
		s.persist()
	}
}
