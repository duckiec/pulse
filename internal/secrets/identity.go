package secrets

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Device identity for the PVLT2 recipient model (ROADMAP.md §4.3.1).
//
// A device owns TWO keypairs with strictly separated roles, because the two
// curves cannot share key material and the roles must not be confused:
//
//   - X25519 (crypto/ecdh) for DECRYPTION: the private half unwraps the
//     payload key from the recipient slot that authenticates it. It never
//     signs anything.
//   - Ed25519 for SIGNING: the private half signs sealed containers, and the
//     public half travels inside every container so any recipient can verify
//     before decrypting. It never decrypts anything.
//
// Both private halves live ONLY in the OS keychain (service "pulse",
// account "vault:x25519:<id>" / "vault:ed25519:<id>"), alongside the PVLT1
// symmetric key ("vault:<id>") they do not replace: PVLT1 containers keep
// opening with the symmetric key, and the account namespaces never collide.
// Public halves are shareable by design and encoded for copy-paste.

// IdentityPubPrefix marks a shareable X25519 decryption public key.
const IdentityPubPrefix = "pulse-pub-"

// IdentitySigPrefix marks a shareable Ed25519 verification public key.
const IdentitySigPrefix = "pulse-sig-"

// IdentityAccount returns the keychain account for a device private half.
//
// kind is "x25519" or "ed25519". It is part of the account rather than a
// second keychain service so all vault material stays under one service
// ("pulse") and one listing shows every key the vault owns.
func IdentityAccount(kind, keyID string) string {
	if keyID == "" {
		return "vault:" + kind + ":default"
	}
	return "vault:" + kind + ":" + keyID
}

// Identity holds a device's keypairs. Private halves are 32-byte seeds;
// public halves are derived, never stored.
type Identity struct {
	// X25519Priv decrypts recipient slots.
	X25519Priv []byte
	// Ed25519Seed signs sealed containers.
	Ed25519Seed []byte
}

// GenerateIdentity creates a fresh device identity from crypto/rand.
//
// Both curves are generated together because a device with a decryption key
// and no signing key can open containers it cannot re-seal, and a device
// with the reverse can seal containers it cannot open — either half alone
// is a broken device that fails confusingly later instead of cleanly now.
func GenerateIdentity() (*Identity, error) {
	x := make([]byte, 32)
	if _, err := rand.Read(x); err != nil {
		return nil, fmt.Errorf("secrets: generate the X25519 private half: %w", err)
	}
	// GenerateKey returns the 64-byte private key; the stored half is its
	// 32-byte seed (PrivateKey.Seed), which is the only form NewKeyFromSeed
	// accepts. Storing the 64-byte form would also work, but the seed is the
	// canonical half and every length check below asserts it.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("secrets: generate the Ed25519 signing half: %w", err)
	}
	seed := append([]byte(nil), priv.Seed()...)
	clear(priv)
	return &Identity{X25519Priv: x, Ed25519Seed: seed}, nil
}

// X25519Pub derives the decryption public key from the private half.
func (id *Identity) X25519Pub() ([]byte, error) {
	if len(id.X25519Priv) != 32 {
		return nil, fmt.Errorf("secrets: X25519 private half is %d bytes, want 32: %w",
			len(id.X25519Priv), ErrBadKey)
	}
	priv, err := ecdh.X25519().NewPrivateKey(id.X25519Priv)
	if err != nil {
		return nil, fmt.Errorf("secrets: load the X25519 private half: %w", err)
	}
	return priv.PublicKey().Bytes(), nil
}

// Ed25519Pub derives the verification public key from the seed.
func (id *Identity) Ed25519Pub() ([]byte, error) {
	if len(id.Ed25519Seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("secrets: Ed25519 seed is %d bytes, want %d: %w",
			len(id.Ed25519Seed), ed25519.SeedSize, ErrBadKey)
	}
	return ed25519.NewKeyFromSeed(id.Ed25519Seed).Public().(ed25519.PublicKey), nil
}

// EncodePub renders a public key shareable: "pulse-pub-<base64>" or
// "pulse-sig-<base64>". The prefix is the type check: feeding a signing key
// where a decryption key belongs (or the reverse) fails at parse time with
// the role named, not deep in an ECDH handshake.
func EncodePub(prefix string, raw []byte) string {
	return prefix + base64.RawStdEncoding.EncodeToString(raw)
}

// DecodePub parses a shareable public key, enforcing prefix and length.
//
// A wrong prefix is a role error (a signing key offered as a recipient);
// a bad length is a format error. Both fail closed before any crypto runs.
func DecodePub(prefix, s string) ([]byte, error) {
	if !strings.HasPrefix(s, prefix) {
		return nil, fmt.Errorf("secrets: public key %q does not start with %q: %w",
			s, prefix, ErrBadIdentity)
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil {
		return nil, fmt.Errorf("secrets: public key is not valid base64: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("secrets: public key is %d bytes, want 32: %w",
			len(raw), ErrBadIdentity)
	}
	return raw, nil
}

// ErrBadIdentity reports a malformed shareable public key.
var ErrBadIdentity = errors.New("secrets: malformed device public key")

// StoreIdentity writes both private halves to the keychain under their
// namespaced accounts. It takes a SecretStore (not a resolver) for the same
// reason VaultInit does: writing is a separate capability from reading, and
// a caller that only resolves must not silently gain it.
func StoreIdentity(keyID string, id *Identity, store SecretStore) error {
	if store == nil {
		return errors.New("secrets: no keychain write path")
	}
	if err := store.Available(); err != nil {
		return err
	}
	if len(id.X25519Priv) != 32 || len(id.Ed25519Seed) != ed25519.SeedSize {
		return fmt.Errorf("secrets: refusing to store a malformed identity: %w", ErrBadKey)
	}
	if err := store.Store(IdentityAccount("x25519", keyID), string(id.X25519Priv)); err != nil {
		return fmt.Errorf("secrets: store the X25519 private half: %w", err)
	}
	if err := store.Store(IdentityAccount("ed25519", keyID), string(id.Ed25519Seed)); err != nil {
		return fmt.Errorf("secrets: store the Ed25519 seed: %w", err)
	}
	return nil
}

// LoadIdentity reads both private halves back out of the keychain.
//
// It takes a SecretResolver (not a SecretStore): loading is reading, and
// composing the two sides keeps write-only from growing a read method.
func LoadIdentity(keyID string, r SecretResolver) (*Identity, error) {
	if r == nil {
		return nil, errors.New("secrets: no keychain read path")
	}
	xraw, err := r.Resolve(IdentityAccount("x25519", keyID))
	if err != nil {
		return nil, fmt.Errorf("secrets: load the X25519 private half: %w", err)
	}
	sraw, err := r.Resolve(IdentityAccount("ed25519", keyID))
	if err != nil {
		return nil, fmt.Errorf("secrets: load the Ed25519 seed: %w", err)
	}
	x := append([]byte(nil), []byte(xraw)...)
	s := append([]byte(nil), []byte(sraw)...)
	if len(x) != 32 || len(s) != ed25519.SeedSize {
		clear(x)
		clear(s)
		return nil, fmt.Errorf("secrets: the keychain holds a malformed identity: %w", ErrBadKey)
	}
	return &Identity{X25519Priv: x, Ed25519Seed: s}, nil
}
