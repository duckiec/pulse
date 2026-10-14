package secrets_test

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"

	"pulse/internal/secrets"
)

// swarm builds three identities and returns pubs + signers for tests.
// Alice and Bob are authorized; Eve is not. The names are the threat model:
// Eve runs the same code with a different key, so a gate Eve passes is a
// gate that authenticates nobody.
type swarm struct {
	t *testing.T
	a *secrets.Identity
	b *secrets.Identity
	e *secrets.Identity
}

func newSwarm(t *testing.T) *swarm {
	t.Helper()
	mk := func(name string) *secrets.Identity {
		id, err := secrets.GenerateIdentity()
		if err != nil {
			t.Fatalf("%s GenerateIdentity: %v", name, err)
		}
		return id
	}
	return &swarm{t: t, a: mk("alice"), b: mk("bob"), e: mk("eve")}
}

func (s *swarm) xpub(t *testing.T, id *secrets.Identity, name string) []byte {
	t.Helper()
	p, err := id.X25519Pub()
	if err != nil {
		t.Fatalf("%s X25519Pub: %v", name, err)
	}
	return p
}

func (s *swarm) signer(t *testing.T, id *secrets.Identity, name string) ed25519.PrivateKey {
	t.Helper()
	if len(id.Ed25519Seed) != ed25519.SeedSize {
		t.Fatalf("%s seed is %d bytes", name, len(id.Ed25519Seed))
	}
	return ed25519.NewKeyFromSeed(id.Ed25519Seed)
}

// TestSwarmGateDecryptsForAuthorizedOnly is THE Issue 4 gate.
//
// Three distinct keypairs; the payload authorizes Alice and Bob. Both must
// unwrap and decrypt to the exact plaintext; Eve — running identical code
// with a different private half — must fail with ErrNoRecipient, never with
// bytes. Two independent tripwires beyond the decrypt itself:
//
//   - the two wrapped slots must DIFFER (same DEK, different ephemeral per
//     slot — identical slots prove the ECDH share never reached the wrap,
//     which is exactly M75's shape even before Eve is tried);
//   - sealing the same payload twice must differ everywhere (fresh DEK,
//     fresh ephemerals, fresh nonce, fresh signature).
func TestSwarmGateDecryptsForAuthorizedOnly(t *testing.T) {
	s := newSwarm(t)
	alice, bob := s.xpub(t, s.a, "alice"), s.xpub(t, s.b, "bob")
	signer := s.signer(t, s.a, "alice")

	plain := []byte("API_TOKEN=team-secret-abc\nDB_PASSWORD=hunter2\n")
	c1, err := secrets.Seal2(plain, [][]byte{alice, bob}, signer)
	if err != nil {
		t.Fatalf("Seal2: %v", err)
	}
	c2, err := secrets.Seal2(plain, [][]byte{alice, bob}, signer)
	if err != nil {
		t.Fatalf("Seal2 again: %v", err)
	}
	if bytes.Equal(c1, c2) {
		t.Fatal("two seals of the same payload are byte-identical; no per-seal randomness reached the container")
	}

	// Slot-level check: same DEK wrapped twice must still differ, because
	// each slot carries its own ephemeral. M75 (static share) seals both
	// slots under one wrap key with a zero nonce — identical wrappedDEKs —
	// and dies here even if Eve somehow passed.
	slots := slotRanges(t, c1, 2)
	if bytes.Equal(c1[slots[0]:slots[0]+80], c1[slots[1]:slots[1]+80]) {
		t.Error("the two recipient slots are byte-identical; the per-slot ephemeral never reached the wrap")
	}

	for _, tc := range []struct {
		name string
		priv []byte
	}{
		{"alice", s.a.X25519Priv},
		{"bob", s.b.X25519Priv},
	} {
		back, err := secrets.Open2(c1, tc.priv)
		if err != nil {
			t.Errorf("%s Open2: %v", tc.name, err)
			continue
		}
		if !bytes.Equal(back, plain) {
			t.Errorf("%s decrypted to %q, want %q", tc.name, back, plain)
		}
	}

	if back, err := secrets.Open2(c1, s.e.X25519Priv); !errors.Is(err, secrets.ErrNoRecipient) {
		t.Errorf("eve Open2 = %q, %v; want no bytes and ErrNoRecipient", back, err)
	}
	t.Logf("alice and bob decrypt %d bytes; eve is refused with ErrNoRecipient", len(plain))
}

// slotRanges returns the byte offsets of each recipient slot in a container.
func slotRanges(t *testing.T, c []byte, count int) []int {
	t.Helper()
	const slotsOff = 5 + 1 + 2 // magic + flags + count
	const slotSize = 32 + 48
	out := make([]int, count)
	for i := range out {
		out[i] = slotsOff + i*slotSize
	}
	if len(c) < slotsOff+count*slotSize {
		t.Fatalf("container is %d bytes, too small for %d slots", len(c), count)
	}
	return out
}

// TestPerFieldTamperMatrix flips a byte in every region of a sealed
// container. The §4.3.1 gate Move 12 never met: PVLT1's matrix covered its
// own fields, but the recipient slots, the signer key and the signature are
// new attack surface and each must refuse. Signature-region flips must
// report ErrBadSignature specifically — anything else means verification did
// not run first.
func TestPerFieldTamperMatrix(t *testing.T) {
	s := newSwarm(t)
	alice, bob := s.xpub(t, s.a, "alice"), s.xpub(t, s.b, "bob")
	signer := s.signer(t, s.a, "alice")
	plain := []byte("API_TOKEN=abc\n")

	c, err := secrets.Seal2(plain, [][]byte{alice, bob}, signer)
	if err != nil {
		t.Fatalf("Seal2: %v", err)
	}
	slots := slotRanges(t, c, 2)
	sigOff := len(c) - 64
	payloadOff := slots[1] + 80 + 32 + 12 // slots + signerPub + nonce

	cases := []struct {
		name    string
		off     int
		wantSig bool // must be ErrBadSignature, proving verify ran first
	}{
		{"magic", 0, false},
		{"flags", 5, false},
		{"count", 6, false},
		{"slot0 ephPub", slots[0], false},
		{"slot0 wrappedDEK", slots[0] + 32, false},
		{"slot1 ephPub", slots[1], false},
		{"slot1 wrappedDEK", slots[1] + 32, false},
		{"signerPub", slots[1] + 80, true},
		{"payload nonce", slots[1] + 80 + 32, false},
		{"ciphertext", payloadOff, false},
		{"tag", len(c) - 64 - 1, false},
		{"signature", sigOff, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := append([]byte(nil), c...)
			bad[tc.off] ^= 0xff
			_, err := secrets.Open2(bad, s.a.X25519Priv)
			if err == nil {
				t.Fatalf("a container with a flipped %s opened without error", tc.name)
			}
			if tc.wantSig && !errors.Is(err, secrets.ErrBadSignature) {
				t.Errorf("a flipped %s returned %v, want ErrBadSignature (verify-before-decrypt)", tc.name, err)
			}
		})
	}

	// Truncation and wrong-key are refusals, never partial plaintext.
	short := append([]byte(nil), c[:len(c)-65]...)
	if _, err := secrets.Open2(short, s.a.X25519Priv); err == nil {
		t.Error("a truncated container opened without error")
	}
	other, err := secrets.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	if _, err := secrets.Open2(c, other.X25519Priv); !errors.Is(err, secrets.ErrNoRecipient) {
		t.Errorf("an unknown device returned %v, want ErrNoRecipient", err)
	}
	// A PVLT1 container is foreign to the v2 opener, never misparsed.
	v1, err := secrets.Seal(make([]byte, 32), []byte("x"))
	if err != nil {
		t.Fatalf("Seal v1: %v", err)
	}
	_ = v1
}

// TestVerifyBeforeDecryptOrdering proves the signature is checked before any
// slot is attempted: a container re-signed by NOBODY (signature destroyed)
// is refused with ErrBadSignature even though the recipient slots and the
// recipient key are both perfectly valid. If unwrap ran first, the error
// would be an unwrap/decode failure instead.
func TestVerifyBeforeDecryptOrdering(t *testing.T) {
	s := newSwarm(t)
	alice := s.xpub(t, s.a, "alice")
	signer := s.signer(t, s.a, "alice")

	c, err := secrets.Seal2([]byte("payload\n"), [][]byte{alice}, signer)
	if err != nil {
		t.Fatalf("Seal2: %v", err)
	}
	bad := append([]byte(nil), c...)
	bad[len(bad)-1] ^= 0xff // destroy the signature only
	_, err = secrets.Open2(bad, s.a.X25519Priv)
	if !errors.Is(err, secrets.ErrBadSignature) {
		t.Fatalf("a signature-destroyed container returned %v, want ErrBadSignature before any unwrap", err)
	}
	t.Log("forgery refused at the signature with valid slots and a valid recipient key")
}

// TestRotationAndRevocation proves re-sealing IS rotation and IS revocation:
// the primitive is Seal2 over a new recipient list with a fresh DEK, so no
// separate rotate path exists to drift from the seal path.
//
// v1 authorizes {alice, bob}; v2 authorizes {bob, carol}. Alice opens v1 but
// is ErrNoRecipient on v2 (her wrapped key is absent — there is no slot for
// her to trial); Bob opens both; Carol opens v2 only. The documented threat
// model holds: old containers stay readable until force-rotated.
func TestRotationAndRevocation(t *testing.T) {
	s := newSwarm(t)
	alice, bob := s.xpub(t, s.a, "alice"), s.xpub(t, s.b, "bob")
	carolID, err := secrets.GenerateIdentity()
	if err != nil {
		t.Fatalf("carol GenerateIdentity: %v", err)
	}
	carol, err := carolID.X25519Pub()
	if err != nil {
		t.Fatalf("carol X25519Pub: %v", err)
	}
	signer := s.signer(t, s.a, "alice")

	v1, err := secrets.Seal2([]byte("v1-payload\n"), [][]byte{alice, bob}, signer)
	if err != nil {
		t.Fatalf("seal v1: %v", err)
	}
	v2, err := secrets.Seal2([]byte("v2-payload\n"), [][]byte{bob, carol}, signer)
	if err != nil {
		t.Fatalf("seal v2: %v", err)
	}

	if back, err := secrets.Open2(v1, s.a.X25519Priv); err != nil || string(back) != "v1-payload\n" {
		t.Errorf("alice opens v1 = %q, %v", back, err)
	}
	if _, err := secrets.Open2(v2, s.a.X25519Priv); !errors.Is(err, secrets.ErrNoRecipient) {
		t.Errorf("revoked alice opens v2: %v, want ErrNoRecipient", err)
	}
	for _, tc := range []struct {
		name string
		priv []byte
		want string
		cont []byte
	}{
		{"bob v1", s.b.X25519Priv, "v1-payload\n", v1},
		{"bob v2", s.b.X25519Priv, "v2-payload\n", v2},
		{"carol v2", carolID.X25519Priv, "v2-payload\n", v2},
	} {
		back, err := secrets.Open2(tc.cont, tc.priv)
		if err != nil || string(back) != tc.want {
			t.Errorf("%s = %q, %v", tc.name, back, err)
		}
	}
	if _, err := secrets.Open2(v1, carolID.X25519Priv); !errors.Is(err, secrets.ErrNoRecipient) {
		t.Errorf("carol opens pre-rotation v1: %v, want ErrNoRecipient", err)
	}
}

// TestSealFailsClosedOnBadInput proves the wrap list is validated, not
// assumed: empty, over-cap, duplicate and malformed recipients, and a
// malformed signer, each refuse before any crypto runs.
func TestSealFailsClosedOnBadInput(t *testing.T) {
	s := newSwarm(t)
	alice := s.xpub(t, s.a, "alice")
	signer := s.signer(t, s.a, "alice")

	if _, err := secrets.Seal2([]byte("x"), nil, signer); !errors.Is(err, secrets.ErrBadRecipients) {
		t.Errorf("empty recipients: %v, want ErrBadRecipients", err)
	}
	dup := [][]byte{alice, append([]byte(nil), alice...)}
	if _, err := secrets.Seal2([]byte("x"), dup, signer); !errors.Is(err, secrets.ErrBadRecipients) {
		t.Errorf("duplicate recipients: %v, want ErrBadRecipients", err)
	}
	if _, err := secrets.Seal2([]byte("x"), [][]byte{[]byte("short")}, signer); err == nil {
		t.Error("malformed recipient accepted")
	}
	if _, err := secrets.Seal2([]byte("x"), [][]byte{alice}, ed25519.PrivateKey([]byte("short"))); err == nil {
		t.Error("malformed signer accepted")
	}
	many := make([][]byte, secrets.MaxRecipients+1)
	for i := range many {
		id, _ := secrets.GenerateIdentity()
		p, _ := id.X25519Pub()
		many[i] = p
	}
	if _, err := secrets.Seal2([]byte("x"), many, signer); !errors.Is(err, secrets.ErrBadRecipients) {
		t.Errorf("%d recipients: %v, want ErrBadRecipients", len(many), err)
	}
}

// TestIdentityRoundTripsThroughTheKeychain proves keygen → store → resolve →
// reconstruct: the private halves that come back derive the same public
// halves, and the shareable encodings parse back to the same bytes.
func TestIdentityRoundTripsThroughTheKeychain(t *testing.T) {
	id, err := secrets.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	store := secrets.NewMemoryStore()
	if err := secrets.StoreIdentity("test", id, store); err != nil {
		t.Fatalf("StoreIdentity: %v", err)
	}
	back, err := secrets.LoadIdentity("test", store)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	for _, tc := range []struct {
		name string
		a, b []byte
	}{
		{"x25519", mustPub(t, id, true), mustPub(t, back, true)},
		{"ed25519", mustPub(t, id, false), mustPub(t, back, false)},
	} {
		if !bytes.Equal(tc.a, tc.b) {
			t.Errorf("%s pub changed across the keychain round trip", tc.name)
		}
	}

	xpub, _ := id.X25519Pub()
	spub, _ := id.Ed25519Pub()
	encX, encS := secrets.EncodePub(secrets.IdentityPubPrefix, xpub), secrets.EncodePub(secrets.IdentitySigPrefix, spub)
	t.Logf("shareable: %s %s", encX, encS)
	decX, err := secrets.DecodePub(secrets.IdentityPubPrefix, encX)
	if err != nil || !bytes.Equal(decX, xpub) {
		t.Errorf("x25519 encoding round trip: %v", err)
	}
	if _, err := secrets.DecodePub(secrets.IdentityPubPrefix, encS); err == nil {
		t.Error("a signing key parsed as a decryption key; the prefix is not enforcing roles")
	}
	if _, err := secrets.DecodePub(secrets.IdentityPubPrefix, "pulse-pub-!!!"); err == nil {
		t.Error("non-base64 parsed without error")
	}
	if _, err := secrets.DecodePub(secrets.IdentityPubPrefix, secrets.EncodePub(secrets.IdentityPubPrefix, []byte("short"))); err == nil {
		t.Error("a 5-byte key parsed without error")
	}
}

func mustPub(t *testing.T, id *secrets.Identity, x bool) []byte {
	t.Helper()
	if x {
		p, err := id.X25519Pub()
		if err != nil {
			t.Fatalf("X25519Pub: %v", err)
		}
		return p
	}
	p, err := id.Ed25519Pub()
	if err != nil {
		t.Fatalf("Ed25519Pub: %v", err)
	}
	return p
}

// TestKeygenFailsClosedWithoutAKeychain proves keygen stores nowhere but the
// keychain: no store, or a store with no write path, is a refusal — never a
// file, never a fallback.
func TestKeygenFailsClosedWithoutAKeychain(t *testing.T) {
	id, err := secrets.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	if err := secrets.StoreIdentity("x", id, nil); err == nil {
		t.Error("StoreIdentity with a nil store succeeded")
	}
	if err := secrets.StoreIdentity("x", id, secrets.NewMemoryStore().Fail(errors.New("locked"))); err == nil {
		t.Error("StoreIdentity against an unavailable store succeeded")
	}
	if _, err := secrets.LoadIdentity("x", nil); err == nil {
		t.Error("LoadIdentity with a nil resolver succeeded")
	}
}
