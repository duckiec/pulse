package sync

// Gates for the mTLS 1.3 + Ed25519 TOFU boundary: certificate issuance,
// the pin-file pinning flow (pin → connect → unpin → refuse), and the
// adversarial proof — an unpinned peer with a perfectly valid self-signed
// certificate is refused at the handshake. Key material is compared,
// never logged.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pulse/internal/secrets"
)

// testIdentity generates a device identity and returns seed + pub.
func testIdentity(t *testing.T) ([]byte, ed25519.PublicKey) {
	t.Helper()
	id, err := secrets.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	pub, ok := ed25519.NewKeyFromSeed(id.Ed25519Seed).Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("Ed25519 public key has the wrong type")
	}
	return id.Ed25519Seed, pub
}

func TestIssueCertBindsNodeAndKey(t *testing.T) {
	seed, pub := testIdentity(t)
	node := NodeIDFor(pub)
	cert, err := IssueCert(seed, node, time.Now())
	if err != nil {
		t.Fatalf("IssueCert: %v", err)
	}
	if cert.Leaf.Subject.CommonName != node {
		t.Fatalf("CN = %q, want node %q", cert.Leaf.Subject.CommonName, node)
	}
	leafPub, ok := cert.Leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatalf("leaf key is %T, want Ed25519", cert.Leaf.PublicKey)
	}
	if string(leafPub) != string(pub) {
		t.Fatal("leaf binds a different key than the device identity")
	}
	// NotBefore is backdated one minute (skew tolerance), so the span is
	// the lifetime plus that minute — pinned exactly, not approximately.
	if span := cert.Leaf.NotAfter.Sub(cert.Leaf.NotBefore); span != CertLifetime+time.Minute {
		t.Fatalf("lifetime span = %v, want %v", span, CertLifetime+time.Minute)
	}
	// Malformed issuance fails closed.
	if _, err := IssueCert([]byte("short"), node, time.Now()); err == nil {
		t.Fatal("short seed issued a certificate")
	}
	if _, err := IssueCert(seed, "", time.Now()); err == nil {
		t.Fatal("empty node id issued a certificate")
	}
}

func TestNodeIDIsStableAcrossReissue(t *testing.T) {
	seed, pub := testIdentity(t)
	node := NodeIDFor(pub)
	for i := 0; i < 3; i++ {
		cert, err := IssueCert(seed, node, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		leafPub := cert.Leaf.PublicKey.(ed25519.PublicKey)
		if NodeIDFor(leafPub) != node {
			t.Fatal("node id moved across re-issuance: pins would flap")
		}
		if Fingerprint(leafPub) != Fingerprint(pub) {
			t.Fatal("fingerprint moved across re-issuance")
		}
	}
}

// handshake runs a full TLS 1.3 handshake over TCP loopback and returns
// both sides' errors. nil+nil means the peers accepted each other.
// Loopback sockets (not net.Pipe): a refused handshake must surface the
// peer's alert/close as a fast error, and pipe rendezvous deadlocks that
// path — the refusal arrived as a 15s i/o timeout instead of a verdict.
func handshake(t *testing.T, serverCfg, clientCfg *tls.Config) (serverErr, clientErr error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopback listen: %v", err)
	}
	defer ln.Close()
	sdone := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			sdone <- err
			return
		}
		defer c.Close()
		sdone <- tls.Server(c, serverCfg).Handshake()
	}()
	d := &net.Dialer{Timeout: 10 * time.Second}
	c, err := d.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("loopback dial: %v", err)
	}
	defer c.Close()
	clientErr = tls.Client(c, clientCfg).Handshake()
	serverErr = <-sdone
	return serverErr, clientErr
}

func pinsFor(t *testing.T, node string, pub ed25519.PublicKey) map[string]string {
	t.Helper()
	return map[string]string{node: Fingerprint(pub)}
}

func TestPinnedHandshakeSucceedsBothDirections(t *testing.T) {
	seedA, pubA := testIdentity(t)
	seedB, pubB := testIdentity(t)
	nodeA, nodeB := NodeIDFor(pubA), NodeIDFor(pubB)
	pins := map[string]string{nodeA: Fingerprint(pubA), nodeB: Fingerprint(pubB)}
	certA, err := IssueCert(seedA, nodeA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	certB, err := IssueCert(seedB, nodeB, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	serr, cerr := handshake(t, ServerConfig(certA, pins), ClientConfig(certB, pins))
	if serr != nil || cerr != nil {
		t.Fatalf("pinned handshake failed: server=%v client=%v", serr, cerr)
	}
}

func TestAdversarialUnpinnedPeerRefused(t *testing.T) {
	// Eve holds a perfectly valid self-signed Ed25519 certificate for
	// her own node id — but nobody pinned her. Both directions must
	// refuse: her dialing us, and us dialing her.
	seedA, pubA := testIdentity(t)
	seedEve, pubEve := testIdentity(t)
	nodeA, nodeEve := NodeIDFor(pubA), NodeIDFor(pubEve)
	pins := pinsFor(t, nodeA, pubA) // Eve NOT pinned
	certA, err := IssueCert(seedA, nodeA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	certEve, err := IssueCert(seedEve, nodeEve, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Eve dials us: OUR server must refuse with its own TOFU verdict.
	// (The dialer accepts our cert so the refusal is isolated to the
	// server side — otherwise both alerts race and the verdict names
	// the wrong direction.)
	bothPins := map[string]string{nodeA: Fingerprint(pubA), nodeEve: Fingerprint(pubEve)}
	serr, _ := handshake(t, ServerConfig(certA, pins), ClientConfig(certEve, bothPins))
	if serr == nil {
		t.Fatal("server accepted an UNPINNED peer: TOFU is open (M92 shape)")
	}
	if !strings.Contains(serr.Error(), nodeEve) {
		t.Fatalf("refusal does not name the node: %v", serr)
	}
	// We dial Eve: OUR client must refuse with its own TOFU verdict
	// (her server would accept us; our client rejects her first).
	_, cerr := handshake(t, ServerConfig(certEve, bothPins), ClientConfig(certA, pins))
	if cerr == nil {
		t.Fatal("client accepted an UNPINNED peer: TOFU is open (M92 shape)")
	}
	if !strings.Contains(cerr.Error(), nodeEve) {
		t.Fatalf("refusal does not name the node: %v", cerr)
	}
}

func TestMismatchedPinRefused(t *testing.T) {
	// The node id is pinned, but to a DIFFERENT key (rotated device, or
	// impersonation): refuse. This is the rotation story — re-pinning is
	// a ledger edit, never an automatic acceptance.
	seedA, pubA := testIdentity(t)
	seedA2, pubA2 := testIdentity(t)
	nodeA := NodeIDFor(pubA)
	if NodeIDFor(pubA2) == nodeA {
		t.Skip("collision in test node ids (2^-64); rerun")
	}
	pins := map[string]string{nodeA: Fingerprint(pubA)}
	// Attacker presents nodeA's id with their own key.
	attackerCert, err := IssueCert(seedA2, nodeA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The attacker's handshake against our server refuses at the TLS
	// layer: CN=nodeA is pinned to the true key, not theirs.
	serr, _ := handshake(t,
		ServerConfig(mustCert(t, seedA, nodeA), pins),
		ClientConfig(attackerCert, map[string]string{nodeA: Fingerprint(pubA2)}))
	if serr == nil {
		t.Fatal("server accepted an impersonated node id")
	}
	// Direct unit proof (no timing dependence): the callback refuses.
	cb := VerifyPeer(pins)
	if err := cb(attackerCert.Certificate, nil); err == nil {
		t.Fatal("VerifyPeer accepted a mismatched key for a pinned node")
	} else if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("wrong refusal class: %v", err)
	}
	// And accepts the true key.
	good, err := IssueCert(seedA, nodeA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := cb(good.Certificate, nil); err != nil {
		t.Fatalf("VerifyPeer refused the pinned key: %v", err)
	}
}

func mustCert(t *testing.T, seed []byte, node string) tls.Certificate {
	t.Helper()
	c, err := IssueCert(seed, node, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVerifyPeerRejectsShapes(t *testing.T) {
	_, pub := testIdentity(t)
	node := NodeIDFor(pub)
	pins := pinsFor(t, node, pub)
	cb := VerifyPeer(pins)
	if err := cb(nil, nil); err == nil {
		t.Fatal("empty chain accepted")
	}
	// Non-Ed25519 leaf: mint an RSA self-signed cert and offer it.
	if err := cb(rsaLeaf(t, node), nil); err == nil {
		t.Fatal("RSA leaf accepted on an Ed25519-only boundary")
	}
	// Garbage DER.
	if err := cb([][]byte{[]byte("not-a-cert")}, nil); err == nil {
		t.Fatal("garbage DER accepted")
	}
	// Unknown node with a valid cert shape.
	seed2, pub2 := testIdentity(t)
	node2 := NodeIDFor(pub2)
	cert2, err := IssueCert(seed2, node2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := cb(cert2.Certificate, nil); err == nil {
		t.Fatal("unknown node accepted")
	} else if !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("wrong refusal class: %v", err)
	}
}

func TestPeersTxtPinningFlow(t *testing.T) {
	// The ledger flow end to end: write pin → load → connect ok →
	// remove pin → refuse again. Uses a temp dir (never the repo file).
	seedA, pubA := testIdentity(t)
	seedB, pubB := testIdentity(t)
	nodeA, nodeB := NodeIDFor(pubA), NodeIDFor(pubB)
	dir := t.TempDir()
	ledger := filepath.Join(dir, "peers.txt")
	writeLedger := func(pins map[string]string) {
		var sb strings.Builder
		sb.WriteString("# test ledger\n")
		for n, f := range pins {
			sb.WriteString(n + " " + f + "\n")
		}
		if err := os.WriteFile(ledger, []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	certA := mustCert(t, seedA, nodeA)
	certB := mustCert(t, seedB, nodeB)
	// Pin both → handshake succeeds.
	writeLedger(map[string]string{nodeA: Fingerprint(pubA), nodeB: Fingerprint(pubB)})
	pins, err := LoadPins(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if serr, cerr := handshake(t, ServerConfig(certA, pins), ClientConfig(certB, pins)); serr != nil || cerr != nil {
		t.Fatalf("pinned ledger handshake failed: %v %v", serr, cerr)
	}
	// Unpin B → the same handshake now refuses.
	writeLedger(map[string]string{nodeA: Fingerprint(pubA)})
	pins, err = LoadPins(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if serr, _ := handshake(t, ServerConfig(certA, pins), ClientConfig(certB, pins)); serr == nil {
		t.Fatal("unpinned peer still connects after ledger removal")
	}
}

func TestLoadPinsRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"three fields": "node aaa bbb\n",
		"one field":    "node\n",
		"bad node":     "nothex fp\n",
		"short fp":     "aaaaaaaaaaaaaaaa 1234\n",
		"nonhex fp":    "aaaaaaaaaaaaaaaa zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz\n",
		"conflict":     "aaaaaaaaaaaaaaaa 0000000000000000000000000000000000000000000000000000000000000000\naaaaaaaaaaaaaaaa 1111111111111111111111111111111111111111111111111111111111111111\n",
	}
	for name, body := range cases {
		p := filepath.Join(dir, "peers.txt")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPins(p); err == nil {
			t.Fatalf("%s: malformed ledger loaded", name)
		}
	}
	if _, err := LoadPins(filepath.Join(dir, "missing.txt")); err == nil {
		t.Fatal("missing ledger loaded")
	}
	// Comments and blanks are fine; fingerprints normalize case.
	p := filepath.Join(dir, "ok.txt")
	os.WriteFile(p, []byte("# ledger\n\nAAAAAAAAAAAAAAAA 0000000000000000000000000000000000000000000000000000000000000000\n"), 0o644)
	pins, err := LoadPins(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 {
		t.Fatalf("ledger holds %d pins, want 1", len(pins))
	}
}

// rsaLeaf mints a non-Ed25519 leaf: the boundary must refuse key types
// it never issues, even when the certificate is otherwise well-formed.
func rsaLeaf(t *testing.T, node string) [][]byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: node},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
	}, &x509.Certificate{}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return [][]byte{der}
}
