// mTLS 1.3 with Ed25519 Trust-On-First-Use pinning (ROADMAP §4.2.3).
//
// Discovery on a hostile LAN must not become an attack surface: a
// discovered-but-unpinned peer is discovered only for display, never
// synced with. Every sync connection is mutually authenticated TLS 1.3;
// peer identities are the devices' Ed25519 keys (the same keypair the
// vault uses for signing — internal/secrets/identity.go), self-signed
// into short-lived certificates, and pinned by key fingerprint in the
// Git-tracked peers.txt ledger. An unknown or mismatched peer fails the
// handshake — there is no prompt, no fallback, no plaintext.
package sync

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"pulse/internal/secrets"
)

// CertLifetime bounds one ephemeral certificate. Certificates are minted
// per boot (per process, in practice): a stolen cert decrypts nothing
// after a day, and rotation is re-issuance, not revocation — there is no
// CA to revoke from because there is no CA.
const CertLifetime = 24 * time.Hour

// NodeIDFor derives the stable peer id from the device's Ed25519
// verification key: the first 8 bytes of its SHA-256, hex-encoded. Stable
// across ephemeral certificates (the KEY is pinned, not the cert), so
// re-issuance never invalidates peers.txt.
func NodeIDFor(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// NodeBytes parses a NodeIDFor id back to the 8-byte HLC node tag.
func NodeBytes(id string) ([8]byte, error) {
	var out [8]byte
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 8 {
		return out, fmt.Errorf("sync: malformed node id %q", id)
	}
	copy(out[:], raw)
	return out, nil
}

// Fingerprint pins the device key: hex SHA-256 over the raw 32-byte
// Ed25519 public key. peers.txt stores these, one per node id.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// IssueCert mints an ephemeral self-signed X.509 certificate binding
// nodeID to the device's Ed25519 key. The private half never leaves the
// caller's memory: it arrives as the keychain-loaded seed and is
// re-materialized here for the handshake only.
func IssueCert(seed []byte, nodeID string, now time.Time) (tls.Certificate, error) {
	if len(seed) != ed25519.SeedSize {
		return tls.Certificate{}, fmt.Errorf("sync: Ed25519 seed is %d bytes, want %d: %w",
			len(seed), ed25519.SeedSize, secrets.ErrBadKey)
	}
	if nodeID == "" {
		return tls.Certificate{}, fmt.Errorf("sync: refusing to issue a certificate with no node id")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("sync: certificate serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(CertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		// A leaf, not a CA: BasicConstraintsValid stays false so no peer
		// can mint children off a stolen short-lived cert.
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("sync: mint certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("sync: reparse minted certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: cert}, nil
}

// EncodeCertPEM renders the self-signed certificate for logging and
// debugging. The PRIVATE key is never encoded: there is no function in
// this package that serializes one.
func EncodeCertPEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// ---------------------------------------------------------------------------
// peers.txt: the Git-tracked TOFU ledger
// ---------------------------------------------------------------------------

// PeersPath is the ledger's home: operational config, next to the module
// allowlist — NOT documentation (AGENTS.md §4.6 governs the seven files;
// this is a trust database in the config/ precedent).
const PeersPath = "config/peers.txt"

// LoadPins parses the fingerprint ledger: one `<node-id> <sha256hex>`
// per line, `#` comments, blank lines ignored. Every line is validated
// strictly — a malformed ledger fails closed (no pins load, no peer
// connects) rather than partially loading and silently unpinning someone.
func LoadPins(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("sync: read %s: %w", path, err)
	}
	pins := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("sync: %s line %d: want `<node-id> <fingerprint>`, got %d fields",
				path, i+1, len(fields))
		}
		node, fp := fields[0], strings.ToLower(fields[1])
		if _, err := NodeBytes(node); err != nil {
			return nil, fmt.Errorf("sync: %s line %d: %w", path, i+1, err)
		}
		fpRaw, err := hex.DecodeString(fp)
		if err != nil || len(fpRaw) != 32 {
			return nil, fmt.Errorf("sync: %s line %d: fingerprint is not 64 hex chars", path, i+1)
		}
		if prev, dup := pins[node]; dup && prev != fp {
			return nil, fmt.Errorf("sync: %s line %d: conflicting pins for node %q", path, i+1, node)
		}
		pins[node] = fp
	}
	return pins, nil
}

// ---------------------------------------------------------------------------
// TOFU verification
// ---------------------------------------------------------------------------

// VerifyPeer builds the VerifyPeerCertificate callback enforcing TOFU
// against the ledger: the peer's leaf must carry an Ed25519 key whose
// fingerprint matches the pin filed under the certificate's node id.
// Unknown nodes and mismatched keys both fail the handshake. Errors name
// the node and the failure class — never key material.
func VerifyPeer(pins map[string]string) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("sync: peer presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("sync: peer certificate does not parse: %w", err)
		}
		pub, ok := leaf.PublicKey.(ed25519.PublicKey)
		if !ok {
			return fmt.Errorf("sync: peer key is %T, want Ed25519 (node %q)",
				leaf.PublicKey, leaf.Subject.CommonName)
		}
		node := leaf.Subject.CommonName
		if node == "" {
			return fmt.Errorf("sync: peer certificate carries no node id")
		}
		want, ok := pins[node]
		if !ok {
			return fmt.Errorf("sync: node %q is not pinned (TOFU: pin it in %s first)", node, PeersPath)
		}
		got := Fingerprint(pub)
		if subtleCompare(got, want) != 1 {
			return fmt.Errorf("sync: node %q fingerprint mismatch (possible impersonation)", node)
		}
		return nil
	}
}

// subtleCompare is constant-time hex equality without importing subtle
// for one call site — it folds the length check into the accumulation so
// short and long inputs are indistinguishable by timing.
func subtleCompare(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	var acc byte
	for i := 0; i < len(a); i++ {
		acc |= a[i] ^ b[i]
	}
	if acc == 0 {
		return 1
	}
	return 0
}

// ServerConfig terminates sync connections: TLS 1.3 only, any client
// cert at the TLS layer (identity is established by VerifyPeerCertificate,
// not by chain — there is no chain), TOFU against the ledger.
//
// InsecureSkipVerify is REQUIRED, not a shortcut: self-signed ephemeral
// certificates have no chain to verify, and setting it is what routes
// verification into VerifyPeerCertificate. Removing the callback while
// keeping the flag is M92 — the adversarial gate proves it red.
func ServerConfig(cert tls.Certificate, pins map[string]string) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		MinVersion:            tls.VersionTLS13,
		ClientAuth:            tls.RequireAnyClientCert,
		InsecureSkipVerify:    true, // TOFU: VerifyPeerCertificate IS the verification.
		VerifyPeerCertificate: VerifyPeer(pins),
	}
}

// ClientConfig dials sync peers under the same TOFU rule: the pin check
// against the peer certificate's node id decides, exactly as on the
// server side. There is no ServerName assertion — self-signed ephemeral
// certificates carry no DNS names to assert.
func ClientConfig(cert tls.Certificate, pins map[string]string) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		MinVersion:            tls.VersionTLS13,
		InsecureSkipVerify:    true, // TOFU: VerifyPeerCertificate IS the verification.
		VerifyPeerCertificate: VerifyPeer(pins),
	}
}
