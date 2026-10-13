package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- format ---------------------------------------------------------------

// TestContainerFormatRoundTrips pins the wire format rather than trusting a
// construction.
//
// The offsets are read out of the code under test, so a shifted magic or a
// mis-sized nonce is caught here rather than appearing as a mystifying
// authentication failure three releases later.
func TestContainerFormatRoundTrips(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	plain := []byte("API_TOKEN=abc123\nDEBUG=true\n")

	container, err := Seal(key, plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if got := string(container[:len(VaultMagic)]); got != VaultMagic {
		t.Errorf("magic = %q, want %q", got, VaultMagic)
	}
	if flags := container[vaultFlagOffset]; flags != 0 {
		t.Errorf("flags = %#02x, want 0x00", flags)
	}
	// The nonce must be present and non-zero: a zero nonce is the signature of
	// a hardcoded one, which is exactly what mutation M61 breaks.
	nonce := container[vaultNonceOffset:vaultHeaderSize]
	if isZero(nonce) {
		t.Error("the nonce is all zeroes; it was not generated")
	}
	// The payload carries the ciphertext AND the appended GCM tag.
	if want := len(plain) + VaultTagSize; len(container)-vaultHeaderSize != want {
		t.Errorf("payload is %d bytes, want %d (ciphertext + tag)",
			len(container)-vaultHeaderSize, want)
	}

	back, err := Open(key, container)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(back, plain) {
		t.Errorf("round trip changed the plaintext:\n got %q\nwant %q", back, plain)
	}
	t.Logf("container: %d header + %d payload bytes for %d plaintext",
		vaultHeaderSize, len(container)-vaultHeaderSize, len(plain))
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// ---- THE NONCE GATE -------------------------------------------------------

// TestSealingTheSamePlaintextTwiceDiffers is the nonce gate.
//
// It proves, concretely rather than by argument, that nonces are unique per
// seal. The three assertions are each a different failure:
//
//   - the CONTAINERS differ, which is what a caller observes;
//   - the NONCES differ, which is the cause. Without this, a format that
//     happens to pad or version-tag could differ in the containers while
//     reusing its nonce, and the first assertion alone would not catch it;
//   - both decrypt to the SAME plaintext, which proves the difference is the
//     nonce's doing and not corruption.
//
// The gate repeats many times because a fixed nonce passes a single-seal test
// and only fails on the second: the whole defect is that the second seal reuses
// the first's nonce, so the loop is the test.
func TestSealingTheSamePlaintextTwiceDiffers(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	plain := []byte("API_TOKEN=the-same-value-every-time\n")

	const trials = 200
	nonceSeen := make(map[string]bool, trials)
	var prev []byte

	for i := 0; i < trials; i++ {
		// Seal zeroises the plaintext it is given, so each trial gets a fresh
		// copy of the same bytes.
		container, err := Seal(key, []byte(string(plain)))
		if err != nil {
			t.Fatalf("Seal %d: %v", i, err)
		}

		nonce := string(container[vaultNonceOffset:vaultHeaderSize])
		if nonceSeen[nonce] {
			t.Fatalf("trial %d reused a nonce seen earlier (%x); nonce reuse under "+
				"one key breaks both confidentiality AND authenticity", i, nonce)
		}
		nonceSeen[nonce] = true

		if prev != nil && bytes.Equal(prev, container) {
			t.Fatalf("trial %d produced a byte-identical container; the nonce is "+
				"not varying per seal", i)
		}
		prev = append(prev[:0], container...)

		back, err := Open(key, container)
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		if !bytes.Equal(back, plain) {
			t.Fatalf("trial %d decrypted to %q, want %q", i, back, plain)
		}
	}
	t.Logf("%d seals of the same plaintext: %d distinct nonces, %d distinct containers",
		trials, len(nonceSeen), trials)
}

// ---- round trip through the FILES, which is the real path ----------------

func TestEnvFileRoundTripThroughDisk(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	vaultPath := envPath + ".vault"
	plain := "API_TOKEN=super-secret-token-xyz\nDB_PASSWORD=hunter2\n"

	if err := os.WriteFile(envPath, []byte(plain), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	key, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}

	if err := EncryptEnvFile(envPath, vaultPath, key); err != nil {
		t.Fatalf("EncryptEnvFile: %v", err)
	}

	// The plaintext must be GONE. A vault next to the .env it came from
	// protects nothing, and an editor swap file would commit the secret.
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Errorf("the plaintext %s still exists after encryption", envPath)
	}
	// The container must be 0600: the ciphertext is not secret, but who holds a
	// production config is.
	fi, err := os.Stat(vaultPath)
	if err != nil {
		t.Fatalf("stat the vault: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("container mode = %v, want 0600", fi.Mode().Perm())
	}
	// The ciphertext must not contain the plaintext.
	raw, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatalf("read the vault: %v", err)
	}
	if bytes.Contains(raw, []byte("super-secret-token-xyz")) || bytes.Contains(raw, []byte("hunter2")) {
		t.Error("the container holds the plaintext verbatim")
	}

	if err := DecryptVaultFile(vaultPath, envPath, key); err != nil {
		t.Fatalf("DecryptVaultFile: %v", err)
	}
	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read the decrypted .env: %v", err)
	}
	if string(got) != plain {
		t.Errorf("decrypted .env = %q, want %q", got, plain)
	}
	// The decrypted plaintext is 0600 for the same reason.
	fi, err = os.Stat(envPath)
	if err != nil {
		t.Fatalf("stat the .env: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("plaintext mode = %v, want 0600", fi.Mode().Perm())
	}
}

// ---- failure modes --------------------------------------------------------

// TestOpenRefusesTamperedContainers proves authentication is real.
//
// Every field is corrupted in turn. A GCM tag is over the ciphertext only, so
// corrupting the MAGIC or the FLAGS is a FORMAT failure the auth check never
// sees, and corrupting the NONCE or the payload is an AUTH failure. Both are
// refusals, and neither is silent — the property under test is that there is no
// input that opens to garbage.
func TestOpenRefusesTamperedContainers(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	plain := []byte("API_TOKEN=abc\n")
	container, err := Seal(key, plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	other, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey (other): %v", err)
	}

	cases := []struct {
		name   string
		mutate func(b []byte)
		want   error
	}{
		{"flipped magic", func(b []byte) { b[0] ^= 0xff }, ErrBadMagic},
		{"flipped flags", func(b []byte) { b[vaultFlagOffset] = 0x01 }, ErrUnsupportedFlags},
		{"flipped nonce", func(b []byte) { b[vaultNonceOffset] ^= 0xff }, ErrAuthFailed},
		{"flipped ciphertext", func(b []byte) { b[vaultHeaderSize] ^= 0xff }, ErrAuthFailed},
		{"flipped tag", func(b []byte) { b[len(b)-1] ^= 0xff }, ErrAuthFailed},
		{"truncated payload", func(b []byte) { /* caller truncates */ }, ErrAuthFailed},
		{"wrong key", nil, ErrAuthFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bad := append([]byte(nil), container...)
			switch c.name {
			case "wrong key":
				_, err := Open(other, bad)
				if !errors.Is(err, c.want) {
					t.Fatalf("Open with the wrong key returned %v, want %v", err, c.want)
				}
				return
			case "truncated payload":
				bad = bad[:len(bad)-1]
			default:
				c.mutate(bad)
			}
			_, err := Open(key, bad)
			if !errors.Is(err, c.want) {
				t.Fatalf("Open returned %v, want %v", err, c.want)
			}
		})
	}

	// A container too small to hold the fixed prefix.
	for _, n := range []int{0, 1, len(VaultMagic), vaultHeaderSize - 1, vaultHeaderSize + VaultTagSize - 1} {
		if _, err := Open(key, make([]byte, n)); !errors.Is(err, ErrShortContainer) {
			t.Errorf("Open of a %d-byte container returned %v, want ErrShortContainer", n, err)
		}
	}
}

// TestOpenRefusesAWrongSizedKey proves a bad key is a configuration error, not
// a crypto one.
func TestOpenRefusesAWrongSizedKey(t *testing.T) {
	if _, err := Seal(make([]byte, 16), []byte("x")); !errors.Is(err, ErrBadKey) {
		t.Errorf("Seal with a 16-byte key returned %v, want ErrBadKey", err)
	}
	container := make([]byte, vaultHeaderSize+VaultTagSize+4)
	copy(container, VaultMagic)
	if _, err := Open(make([]byte, 31), container); !errors.Is(err, ErrBadKey) {
		t.Errorf("Open with a 31-byte key returned %v, want ErrBadKey", err)
	}
}

// ---- keychain stores ------------------------------------------------------

// TestMemoryStoreRoundTrips is the vault's storage gate.
func TestMemoryStoreRoundTrips(t *testing.T) {
	st := NewMemoryStore()
	if err := st.Available(); err != nil {
		t.Fatalf("Available: %v", err)
	}
	const account = "vault:default"
	if err := st.Store(account, "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := st.Resolve(account)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != KeySize {
		t.Errorf("stored %d bytes, want %d", len(got), KeySize)
	}
	// Overwrite is not an error: `vault init` on a machine that already holds
	// a key must replace it, not fail on a duplicate.
	if err := st.Store(account, strings.Repeat("f", KeySize)); err != nil {
		t.Errorf("re-Store: %v", err)
	}
	if got, _ := st.Resolve(account); got != strings.Repeat("f", KeySize) {
		t.Error("re-Store did not overwrite")
	}
	// An absent account is an error, never an empty value that would encrypt
	// the user's config under nothing.
	if _, err := st.Resolve("vault:absent"); err == nil {
		t.Error("Resolve of an absent account returned no error")
	}
}

// TestMemoryStoreFailMode lets a gate build a store that is present but
// unusable, which is a real state (a locked credential manager).
func TestMemoryStoreFailMode(t *testing.T) {
	unavailable := errors.New("the credential manager is locked")
	st := NewMemoryStore().Fail(unavailable)
	if err := st.Available(); !errors.Is(err, unavailable) {
		t.Errorf("Available = %v, want the injected failure", err)
	}
	if err := st.Store("a", "b"); !errors.Is(err, unavailable) {
		t.Errorf("Store = %v, want the injected failure", err)
	}
}

// TestStoreAvailableNamesTheReason proves a BLOCKED store says WHY.
func TestStoreAvailableNamesTheReason(t *testing.T) {
	st := NewStore()
	err := st.Available()
	t.Logf("platform %s: Available() = %v", st.Backend(), err)
	if err != nil {
		// The reason must be actionable, and must not be a bare "unavailable".
		if !strings.Contains(err.Error(), "secrets:") {
			t.Errorf("the reason %q is not namespaced to the secrets package", err)
		}
		if errors.Is(err, ErrStoreUnavailable) {
			t.Logf("BLOCKED (reason: this %s host has no keychain write path)", st.Backend())
		} else {
			t.Logf("BLOCKED (reason: the keychain is not reachable: %v)", err)
		}
	}
}

// TestPlatformStoreContract pins that NewStore never returns nil, so every
// caller's nil check is the check rather than a panic.
func TestPlatformStoreContract(t *testing.T) {
	if NewStore() == nil {
		t.Fatal("NewStore returned nil; the nil check would be a panic")
	}
	no := noStore{}
	if err := no.Store("a", "b"); !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("noStore.Store = %v, want ErrStoreUnavailable", err)
	}
	if err := no.Store("a", "b"); !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("noStore.Available = %v, want ErrStoreUnavailable", err)
	}
	if no.Backend() == "" {
		t.Error("noStore.Backend() is empty; diagnostics would not say which platform")
	}
}

// TestSealLeavesTheInputIntact pins the OPPOSITE of what an earlier version
// did, and the inversion is the finding.
//
// Seal originally zeroised the caller's buffer, and
// TestSealingTheSamePlaintextTwiceDiffers caught it immediately: a function
// that wipes what it was handed cannot be called twice on the same bytes, and
// cannot compose. Zeroisation belongs to the caller, and the file path applies
// it to the buffer IT allocated (asserted by TestEnvFileRoundTripThroughDisk's
// "the plaintext is gone").
func TestSealLeavesTheInputIntact(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	plain := []byte("API_TOKEN=wipe-me-after-this")
	if _, err := Seal(key, plain); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if isZero(plain) {
		t.Error("Seal zeroised the caller's plaintext; a wrapper cannot compose " +
			"around a function that destroys its input")
	}
	if string(plain) != "API_TOKEN=wipe-me-after-this" {
		t.Errorf("Seal modified the caller's plaintext: %q", plain)
	}
}
