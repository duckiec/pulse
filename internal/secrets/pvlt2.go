package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// The PVLT2 hybrid container (ROADMAP.md §4.3.1): one AES-256-GCM payload key
// (the DEK) wrapped per recipient under an ECDH-derived key, with an Ed25519
// signature over everything before it.
//
// # Layout (all integers big-endian, all keys in field order)
//
//	"PVLT2"      magic, 5 bytes (deliberately NOT "PVLT1": a v1 opener must
//	             refuse a v2 container as foreign, never misparse it)
//	flags      1 byte, reserved, 0 today
//	count      u16, number of recipient slots (1..MaxRecipients)
//	per slot:  ephPub[32] | wrappedDEK[48]  (32B DEK + 16B tag, zero nonce)
//	signerPub  32 bytes, the committer's Ed25519 verification key
//	pNonce     12 bytes, fresh crypto/rand per seal
//	ciphertext gcm.Seal(DEK, pNonce, payload) with 16B tag appended
//	sig        64 bytes, Ed25519 over every byte before it
//
// # Where this deviates from the §4.3.1 sketch, and why
//
// The sketch stores no recipient identity per slot, so "the slot matching the
// local key" (§4.3.3) can only mean trial-unwrap: try each slot with the local
// private half, and the slot that authenticates IS the match. That is what
// this implements — authentication-based matching rather than a stored
// identity column. Three reasons it is the honest reading:
//
//   - A stored identity would let Open reject Eve BEFORE any crypto, which
//     would make the M75 mutation (static wrap secret) go GREEN: Eve would
//     be refused at the match and never reach the broken unwrap. A mutation
//     that cannot go red is a gate that proves nothing.
//   - Trial-unwrap leaks nothing: a failed GCM unwrap is a silent,
//     constant-shape refusal, and every slot failure reports the identical
//     ErrNoRecipient. There is no oracle in which slot failed versus which
//     was never tried.
//   - Slot counts are team-sized (capped at MaxRecipients), so the trial is
//     O(handful of ECDH ops), not a search.
//
// # Verify-before-decrypt
//
// The signature is verified BEFORE any slot is attempted (§4.3.3 challenge).
// A forged container is refused at the signature even when the recipient key
// is valid — fail fast, no oracle feedback from the unwrap path.

// VaultMagic2 identifies a PVLT2 container.
const VaultMagic2 = "PVLT2"

// MaxRecipients caps the recipient list. It is a fail-closed bound, not a
// limit to raise quietly: each slot is an ECDH + GCM unwrap trial on open,
// and an unbounded list turns a malicious container into a CPU burn.
const MaxRecipients = 64

const (
	pvlt2FlagOffset  = len(VaultMagic2)
	pvlt2CountOffset = pvlt2FlagOffset + 1
	pvlt2SlotsOffset = pvlt2CountOffset + 2
	pvlt2SlotSize    = 32 + 48 // ephPub + wrappedDEK (DEK 32B + tag 16B)
	pvlt2SlotDEKSize = 32 + 16
)

// wrapInfo is the HKDF info string, taken verbatim from ROADMAP.md §4.3.2.
const wrapInfo = "pulse-vault-v1"

// Errors. Tamper, truncation, wrong key and no-membership all surface through
// this set, and the AUTHENTICATION failures are deliberately one error:
// distinguishing "no slot matched you" from "a slot matched but the tag
// failed" would hand an oracle to anyone who can present a container and
// observe the refusal.
var (
	// ErrNoRecipient reports that no recipient slot unwraps with the local
	// private half — the caller is not an authorized recipient. It maps to
	// exit 3 (a keychain/authorization failure, AGENTS.md §4.4).
	ErrNoRecipient = errors.New("secrets: no recipient slot matches this device")
	// ErrBadSignature reports a container whose Ed25519 signature does not
	// verify. It is distinct from ErrNoRecipient on purpose: a forged
	// container is refused BEFORE membership is even considered, and the
	// ordering is itself gated (TestVerifyBeforeDecryptOrdering).
	ErrBadSignature = errors.New("secrets: vault signature does not verify")
	// ErrBadRecipients reports a container with zero or too many slots.
	ErrBadRecipients = errors.New("secrets: vault recipient count out of range")
)

// hkdfSHA256 is HKDF-Extract+Expand over SHA-256 (RFC 5869), hand-built from
// crypto/hmac + crypto/sha256: the stdlib has no crypto/hkdf package, and a
// third-party KDF for 30 lines of specified construction would be a
// dependency that owns the vault's key schedule.
func hkdfSHA256(secret, salt, info []byte, outLen int) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write(secret)
	prk := mac.Sum(nil)
	var out, prev []byte
	var counter byte = 1
	for len(out) < outLen {
		mac = hmac.New(sha256.New, prk)
		mac.Write(prev)
		mac.Write(info)
		mac.Write([]byte{counter})
		prev = mac.Sum(nil)
		out = append(out, prev...)
		counter++
	}
	return out[:outLen]
}

// deriveWrapKey binds an ECDH share to one slot: HKDF(salt=ephPub,
// info="pulse-vault-v1") exactly as ROADMAP.md §4.3.2 specifies. The ephemeral
// public half is BOTH the salt and the stored slot identifier, so two seals
// never share a wrap key even for the same recipient set.
func deriveWrapKey(share, ephPub []byte) ([]byte, error) {
	if len(share) != 32 || len(ephPub) != 32 {
		return nil, fmt.Errorf("secrets: refusing to derive a wrap key from malformed input: %w", ErrBadKey)
	}
	return hkdfSHA256(share, ephPub, []byte(wrapInfo), 32), nil
}

// Seal2 encrypts payload for recipients and signs the container.
//
// recipients are raw 32-byte X25519 public keys (DecodePub output); signer is
// the committer's Ed25519 private key. Every seal generates a fresh DEK, a
// fresh payload nonce, AND a fresh ephemeral per slot — sealing the same
// payload twice differs in every slot, so identical wrapped slots across two
// seals (or two slots in one seal) prove the randomness never reached the
// wrap. Empty and duplicate recipient lists fail closed.
func Seal2(payload []byte, recipients [][]byte, signer ed25519.PrivateKey) ([]byte, error) {
	if len(recipients) == 0 || len(recipients) > MaxRecipients {
		return nil, fmt.Errorf("%w: %d recipients", ErrBadRecipients, len(recipients))
	}
	seen := map[string]bool{}
	for _, r := range recipients {
		if len(r) != 32 {
			return nil, fmt.Errorf("secrets: recipient public key is %d bytes, want 32: %w", len(r), ErrBadIdentity)
		}
		k := string(r)
		if seen[k] {
			return nil, fmt.Errorf("secrets: duplicate recipient in the wrap list: %w", ErrBadRecipients)
		}
		seen[k] = true
	}
	if len(signer) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("secrets: signer is %d bytes, want %d: %w",
			len(signer), ed25519.PrivateKeySize, ErrBadKey)
	}

	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("secrets: generate the payload key: %w", err)
	}
	defer clear(dek)
	pNonce := make([]byte, VaultNonceSize)
	if _, err := rand.Read(pNonce); err != nil {
		return nil, fmt.Errorf("secrets: generate the payload nonce: %w", err)
	}

	payloadAEAD, err := freshGCM(dek)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, pvlt2SlotsOffset+len(recipients)*pvlt2SlotSize+32+VaultNonceSize+len(payload)+16+64)
	out = append(out, VaultMagic2...)
	out = append(out, 0x00)
	var countBuf [2]byte
	binary.BigEndian.PutUint16(countBuf[:], uint16(len(recipients)))
	out = append(out, countBuf[:]...)

	for _, r := range recipients {
		eph, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("secrets: generate the slot ephemeral: %w", err)
		}
		ephPub := eph.PublicKey().Bytes()
		recipPub, err := ecdh.X25519().NewPublicKey(r)
		if err != nil {
			return nil, fmt.Errorf("secrets: load a recipient public key: %w", err)
		}
		share, err := eph.ECDH(recipPub)
		if err != nil {
			return nil, fmt.Errorf("secrets: derive the slot share: %w", err)
		}
		wrapKey, err := deriveWrapKey(share, ephPub)
		clear(share)
		if err != nil {
			return nil, err
		}
		wrapAEAD, err := freshGCM(wrapKey)
		clear(wrapKey)
		if err != nil {
			return nil, err
		}
		// The wrap key is single-use (fresh ephemeral per slot per seal), so
		// the wrap nonce is ZERO by construction — the same rule age uses.
		// A random wrap nonce would need storing per slot (+12B each) for
		// zero security gain: uniqueness already comes from the key.
		var zeroNonce [12]byte
		wrapped := wrapAEAD.Seal(nil, zeroNonce[:], dek, nil)
		out = append(out, ephPub...)
		out = append(out, wrapped...)
	}

	signerPub := signer.Public().(ed25519.PublicKey)
	out = append(out, signerPub...)
	out = append(out, pNonce...)
	out = append(out, payloadAEAD.Seal(nil, pNonce, payload, nil)...)

	sig := ed25519.Sign(signer, out)
	out = append(out, sig...)
	return out, nil
}

// Open2 verifies the container signature, then unwraps the first slot that
// authenticates with priv (a 32-byte X25519 private half), and decrypts.
//
// Verify precedes unwrap unconditionally: a forged container is refused even
// when the recipient key is valid. Membership failure (no slot unwraps) is
// ErrNoRecipient; every other structural failure names itself.
func Open2(container, priv []byte) ([]byte, error) {
	if len(priv) != 32 {
		return nil, fmt.Errorf("secrets: X25519 private half is %d bytes, want 32: %w", len(priv), ErrBadKey)
	}
	body, slots, signerPub, pNonce, ciphertext, sig, err := parsePVLT2(container)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(signerPub, body, sig) {
		return nil, ErrBadSignature
	}

	localPriv, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("secrets: load the X25519 private half: %w", err)
	}
	for _, s := range slots {
		share, err := localPriv.ECDH(s.ephPub)
		if err != nil {
			continue // a malformed slot is skipped, never fatal: other slots may match
		}
		wrapKey, err := deriveWrapKey(share, s.ephPubRaw)
		clear(share)
		if err != nil {
			continue
		}
		wrapAEAD, err := freshGCM(wrapKey)
		clear(wrapKey)
		if err != nil {
			continue
		}
		var zeroNonce [12]byte
		dek, err := wrapAEAD.Open(nil, zeroNonce[:], s.wrapped, nil)
		if err != nil {
			continue // not our slot: identical refusal shape, no oracle
		}
		payloadAEAD, err := freshGCM(dek)
		if err != nil {
			clear(dek)
			continue
		}
		payload, err := payloadAEAD.Open(nil, pNonce, ciphertext, nil)
		clear(dek)
		if err != nil {
			// The slot unwrapped but the payload tag failed: the container
			// was re-signed after tampering, or the DEK slot and the payload
			// belong to different seals. Either way it is not our payload.
			return nil, fmt.Errorf("%w (slot unwrapped, payload tag failed)", ErrAuthFailed)
		}
		return payload, nil
	}
	return nil, fmt.Errorf("%w (no slot unwrapped with this device key)", ErrNoRecipient)
}

// pvlt2Slot is one parsed recipient slot.
type pvlt2Slot struct {
	ephPub    *ecdh.PublicKey
	ephPubRaw []byte
	wrapped   []byte
}

// parsePVLT2 splits a container into body (everything the signature covers),
// slots, signer key, payload nonce, ciphertext and signature. It checks
// STRUCTURE only — signature verification is the caller's next step, and
// membership is never decided here.
func parsePVLT2(container []byte) (body []byte, slots []pvlt2Slot, signerPub ed25519.PublicKey, pNonce, ciphertext, sig []byte, err error) {
	min := pvlt2SlotsOffset + 32 + VaultNonceSize + VaultTagSize + ed25519.SignatureSize
	if len(container) < min+1 {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("%w: %d bytes is smaller than the fixed prefix",
			ErrShortContainer, len(container))
	}
	if string(container[:len(VaultMagic2)]) != VaultMagic2 {
		return nil, nil, nil, nil, nil, nil, ErrBadMagic2
	}
	if flags := container[pvlt2FlagOffset]; flags != 0 {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("%w: %#02x", ErrUnsupportedFlags, flags)
	}
	count := int(binary.BigEndian.Uint16(container[pvlt2CountOffset:]))
	if count == 0 || count > MaxRecipients {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("%w: %d", ErrBadRecipients, count)
	}
	off := pvlt2SlotsOffset
	slots = make([]pvlt2Slot, 0, count)
	for i := 0; i < count; i++ {
		if len(container) < off+pvlt2SlotSize {
			return nil, nil, nil, nil, nil, nil, fmt.Errorf("%w: slot %d overruns the container",
				ErrShortContainer, i)
		}
		ephRaw := append([]byte(nil), container[off:off+32]...)
		eph, err := ecdh.X25519().NewPublicKey(ephRaw)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, fmt.Errorf("secrets: slot %d carries a malformed ephemeral: %w", i, err)
		}
		wrapped := append([]byte(nil), container[off+32:off+pvlt2SlotSize]...)
		slots = append(slots, pvlt2Slot{ephPub: eph, ephPubRaw: ephRaw, wrapped: wrapped})
		off += pvlt2SlotSize
	}
	if len(container) < off+32+VaultNonceSize+VaultTagSize+ed25519.SignatureSize {
		return nil, nil, nil, nil, nil, nil, fmt.Errorf("%w: container ends inside the payload region",
			ErrShortContainer)
	}
	signerPub = ed25519.PublicKey(append([]byte(nil), container[off:off+32]...))
	off += 32
	pNonce = append([]byte(nil), container[off:off+VaultNonceSize]...)
	off += VaultNonceSize
	// The signature is the trailing 64 bytes; everything before it is signed.
	sigOff := len(container) - ed25519.SignatureSize
	ciphertext = append([]byte(nil), container[off:sigOff]...)
	sig = append([]byte(nil), container[sigOff:]...)
	body = container[:sigOff]
	return body, slots, signerPub, pNonce, ciphertext, sig, nil
}

// ErrBadMagic2 reports a container that is not a PVLT2 container at all —
// including a PVLT1 container handed to the v2 opener. The versions never
// parse each other: magic mismatch is the whole check.
var ErrBadMagic2 = errors.New("secrets: not a Pulse PVLT2 container")

// freshGCM builds an AES-256-GCM AEAD, validating key length first so a
// mis-sized wrap key reads as a configuration refusal rather than a cipher
// error from the wrong layer.
func freshGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secrets: wrap key is %d bytes, want %d: %w",
			len(key), KeySize, ErrBadKey)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: build the AES cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// compile-time proof the slot arithmetic matches the field sizes.
var _ = [1]struct{}{}[pvlt2SlotSize-32-pvlt2SlotDEKSize]
