package app_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pulse/internal/app"
	"pulse/internal/secrets"
)

// vaultFixture seals payload for deviceID and stores the identity in mem.
// It returns the container path and the in-memory keychain holding the privs.
func vaultFixture(t *testing.T, payload string, ids ...*secrets.Identity) (vaultPath string, store *secrets.MemoryStore) {
	t.Helper()
	if len(ids) == 0 {
		t.Fatal("vaultFixture needs at least one identity")
	}
	var pubs [][]byte
	for i, id := range ids {
		p, err := id.X25519Pub()
		if err != nil {
			t.Fatalf("identity %d X25519Pub: %v", i, err)
		}
		pubs = append(pubs, p)
	}
	signer := ed25519.NewKeyFromSeed(ids[0].Ed25519Seed)
	c, err := secrets.Seal2([]byte(payload), pubs, signer)
	if err != nil {
		t.Fatalf("Seal2: %v", err)
	}
	vaultPath = filepath.Join(t.TempDir(), ".env.vault")
	if err := os.WriteFile(vaultPath, c, 0o600); err != nil {
		t.Fatalf("write vault: %v", err)
	}
	store = secrets.NewMemoryStore()
	if err := secrets.StoreIdentity("", ids[0], store); err != nil {
		t.Fatalf("StoreIdentity: %v", err)
	}
	return vaultPath, store
}

// TestVaultEncryptProducesWhatTheChainReads pins the PVLT1/PVLT2 version
// skew that the bare-metal crucible caught red: `vault encrypt` sealed
// PVLT1 while the runtime chain opens PVLT2, so every encrypted vault was
// write-only. Encrypt here is through the App method (identity + recipient
// list, no keychain), decrypt through the matching method, and the payload
// must round-trip — plus the container must open with Open2 directly, which
// is what the chain calls.
func TestVaultEncryptProducesWhatTheChainReads(t *testing.T) {
	dir := t.TempDir()
	id := mkIdentity(t)
	mem := secrets.NewMemoryStore()
	if err := secrets.StoreIdentity("", id, mem); err != nil {
		t.Fatalf("StoreIdentity: %v", err)
	}
	xpub, err := id.X25519Pub()
	if err != nil {
		t.Fatal(err)
	}
	recipients := filepath.Join(dir, "pulse.recipients")
	if err := os.WriteFile(recipients, []byte(secrets.EncodePub(secrets.IdentityPubPrefix, xpub)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, "s.env")
	if err := os.WriteFile(envPath, []byte("K=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t)
	if err := a.VaultEncrypt(envPath, "", recipients, mem); err != nil {
		t.Fatalf("VaultEncrypt: %v", err)
	}
	raw, err := os.ReadFile(envPath + ".vault")
	if err != nil {
		t.Fatalf("no container written: %v", err)
	}
	if string(raw[:5]) != secrets.VaultMagic2 {
		t.Fatalf("container magic = %q, want PVLT2 %q: encrypt must produce what the chain reads", raw[:5], secrets.VaultMagic2)
	}
	out, err := a.VaultDecryptTo(envPath+".vault", filepath.Join(dir, "out.env"), "", mem)
	if err != nil {
		t.Fatalf("VaultDecryptTo: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "K=v\n" {
		t.Fatalf("round trip = %q, want the payload", got)
	}
}

func mkIdentity(t *testing.T) *secrets.Identity {
	t.Helper()
	id, err := secrets.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	return id
}

// TestSeamlessVaultResolutionIsTransparentToTheRunner is THE Issue 5 gate.
//
// The App boots with a VaultFile and NO .env. A .http execution requiring a
// vault-only variable must resolve it transparently and succeed — the runner
// never knows which tier answered. Three properties, each catching a
// different break:
//
//   - Chain.Resolve answers from the vault tier (M77 kills this: bypass the
//     vault fallback and the lookup fails closed with ErrUnresolved);
//   - the full RunTest path succeeds against a live server (wiring proof,
//     not just a unit lookup);
//   - the echoing server's secret is redacted in the report (the vault tier
//     registers with the shared redactor at boot, not on first lookup).
func TestSeamlessVaultResolutionIsTransparentToTheRunner(t *testing.T) {
	const secret = "vault-transparent-secret-8f2c"
	device := mkIdentity(t)
	vaultPath, store := vaultFixture(t, "VAULT_TOKEN="+secret+"\n", device)

	a := newTestApp(t, func(c *app.Config) {
		c.VaultFile = vaultPath
		c.Backend = store
	})
	if got, err := a.Chain.Resolve("VAULT_TOKEN"); err != nil || got != secret {
		t.Fatalf("Chain.Resolve(vault-only) = %q, %v", got, err)
	}
	// The boot path registers every vault value with the shared redactor
	// BEFORE any lookup: without that loop the secret below survives.
	if got := a.Redactor.String(secret); got != "[REDACTED]" {
		t.Fatalf("the vault value is not registered with the redactor at boot: %q", got)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	res, err := a.RunTest(context.Background(),
		dirWith(t, map[string]string{
			"v.http": fmt.Sprintf("# @name v\n# @assert status == 200\n# @assert body contains \"ok\"\nGET %s/v HTTP/1.1\nAuthorization: Bearer {{VAULT_TOKEN}}\n\n###\n", srv.URL),
		}), 2, 10*time.Second, false)
	if err != nil {
		t.Fatalf("RunTest: %v", err)
	}
	// The server only answers 200 when the vault secret arrived intact, so
	// exit 0 IS the transparency proof: no .env existed, the keychain held
	// only the identity, and the request still authenticated.
	if res.Exit != 0 {
		t.Errorf("vault-only run exited %d, want 0:\n%s", res.Exit, res.TAP)
	}
	for _, doc := range []string{res.TAP, res.JSON} {
		if strings.Contains(doc, secret) {
			t.Errorf("the vault secret survived into a report document:\n%s", doc)
		}
	}
}

// TestVaultTierYieldsToExplicitEnv pins the custody precedence: an explicit
// .env value beats the vault value for the same key. The vault is the team
// default; the local file is the developer's override. Reversing them would
// let a committed vault silently override the machine it runs on.
func TestVaultTierYieldsToExplicitEnv(t *testing.T) {
	device := mkIdentity(t)
	vaultPath, store := vaultFixture(t, "SHARED=from-vault\n", device)
	envPath := writeEnvFile(t, map[string]string{"SHARED": "from-env"})

	a := newTestApp(t, func(c *app.Config) {
		c.EnvFile = envPath
		c.VaultFile = vaultPath
		c.Backend = store
	})
	if got, err := a.Chain.Resolve("SHARED"); err != nil || got != "from-env" {
		t.Errorf("Chain.Resolve = %q, %v; want the explicit .env to win over the vault", got, err)
	}
}

// TestCloseScrubsTheVaultPayloadFromMemory proves App.Close zeroes the tiers:
// after Close, a vault-only variable fails closed with ErrUnresolved even
// though the keychain still holds the identity. Without the scrub, the
// decrypted team payload outlives the process's use of it.
func TestCloseScrubsTheVaultPayloadFromMemory(t *testing.T) {
	device := mkIdentity(t)
	vaultPath, store := vaultFixture(t, "GONE=after-close\n", device)

	a := newTestApp(t, func(c *app.Config) {
		c.VaultFile = vaultPath
		c.Backend = store
	})
	// Sanity: it resolves before Close, so the post-Close failure proves the
	// scrub rather than a wiring mistake.
	if _, err := a.Chain.Resolve("GONE"); err != nil {
		t.Fatalf("pre-Close resolve: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := a.Chain.Resolve("GONE"); !errors.Is(err, secrets.ErrUnresolved) {
		t.Errorf("post-Close resolve = %v; want ErrUnresolved (payload scrubbed)", err)
	}
}

// TestVaultRotateResealsForTheWholeList drives VaultRotate end to end: seal
// for {A,B}, rotate to {A,B,C} from the .env, and prove all three open the
// new container. M78 (silently dropping the last recipient) kills Carol —
// the test names exactly the dropped party, not just \"someone failed\".
func TestVaultRotateResealsForTheWholeList(t *testing.T) {
	aID, bID, cID := mkIdentity(t), mkIdentity(t), mkIdentity(t)
	pubOf := func(id *secrets.Identity) string {
		p, err := id.X25519Pub()
		if err != nil {
			t.Fatalf("X25519Pub: %v", err)
		}
		return secrets.EncodePub(secrets.IdentityPubPrefix, p)
	}

	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("TEAM=rotated-secret\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	recPath := filepath.Join(dir, "pulse.recipients")
	for _, pub := range []string{pubOf(aID), pubOf(bID), pubOf(cID)} {
		if err := secrets.AddRecipient(recPath, pub); err != nil {
			t.Fatalf("AddRecipient: %v", err)
		}
	}

	store := secrets.NewMemoryStore()
	if err := secrets.StoreIdentity("", aID, store); err != nil {
		t.Fatalf("StoreIdentity: %v", err)
	}
	a := newTestApp(t)
	if err := a.VaultRotate(envPath, "", recPath, store); err != nil {
		t.Fatalf("VaultRotate: %v", err)
	}
	// The .env that was read is gone (encrypt semantic); the vault stands.
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Error("the plaintext .env still exists after rotation")
	}
	vaultRaw, err := os.ReadFile(envPath + ".vault")
	if err != nil {
		t.Fatalf("read the rotated vault: %v", err)
	}
	for _, tc := range []struct {
		name string
		id   *secrets.Identity
	}{
		{"alice", aID}, {"bob", bID}, {"carol", cID},
	} {
		back, err := secrets.Open2(vaultRaw, tc.id.X25519Priv)
		if err != nil {
			t.Errorf("%s cannot open the rotated vault: %v", tc.name, err)
			continue
		}
		if !strings.Contains(string(back), "TEAM=rotated-secret") {
			t.Errorf("%s opened %q, want the rotated payload", tc.name, back)
		}
	}
}

// TestVaultRotateFromVaultOnly covers rotation after the .env is long gone:
// plaintext comes from decrypting the current container with the device key,
// and the vault is overwritten in place.
func TestVaultRotateFromVaultOnly(t *testing.T) {
	aID, bID := mkIdentity(t), mkIdentity(t)
	pubOf := func(id *secrets.Identity) string {
		p, _ := id.X25519Pub()
		return secrets.EncodePub(secrets.IdentityPubPrefix, p)
	}
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	signer := ed25519.NewKeyFromSeed(aID.Ed25519Seed)
	first, err := secrets.Seal2([]byte("V=one\n"), [][]byte{mustXPub(t, aID)}, signer)
	if err != nil {
		t.Fatalf("Seal2: %v", err)
	}
	if err := os.WriteFile(envPath+".vault", first, 0o600); err != nil {
		t.Fatalf("write vault: %v", err)
	}
	recPath := filepath.Join(dir, "pulse.recipients")
	if err := secrets.AddRecipient(recPath, pubOf(aID)); err != nil {
		t.Fatalf("add a: %v", err)
	}
	if err := secrets.AddRecipient(recPath, pubOf(bID)); err != nil {
		t.Fatalf("add b: %v", err)
	}

	store := secrets.NewMemoryStore()
	if err := secrets.StoreIdentity("", aID, store); err != nil {
		t.Fatalf("StoreIdentity: %v", err)
	}
	if err := newTestApp(t).VaultRotate(envPath, "", recPath, store); err != nil {
		t.Fatalf("VaultRotate (vault-only): %v", err)
	}
	raw, _ := os.ReadFile(envPath + ".vault")
	if back, err := secrets.Open2(raw, bID.X25519Priv); err != nil || string(back) != "V=one\n" {
		t.Errorf("bob opens the rotated vault = %q, %v", back, err)
	}
}

func mustXPub(t *testing.T, id *secrets.Identity) []byte {
	t.Helper()
	p, err := id.X25519Pub()
	if err != nil {
		t.Fatalf("X25519Pub: %v", err)
	}
	return p
}

// TestVaultDegradesWhenTheVaultCannotOpen pins the boot contract: an
// explicitly configured vault that the device cannot open is REPORTED and
// the app still serves; the vault-only variable then fails closed at
// request time (exit 3), not at boot.
func TestVaultDegradesWhenTheVaultCannotOpen(t *testing.T) {
	stranger := mkIdentity(t)
	vaultPath, _ := vaultFixture(t, "X=1\n", stranger) // sealed for nobody we hold

	// Empty store: no device identity at all.
	a := newTestApp(t, func(c *app.Config) {
		c.VaultFile = vaultPath
		c.Backend = secrets.NewMemoryStore()
	})
	found := false
	for _, d := range a.Degradations {
		if strings.Contains(d, "vault") {
			found = true
		}
	}
	if !found {
		t.Errorf("an unopenable vault produced no degradation; the app looks healthy while a tier is missing: %v", a.Degradations)
	}
	var out bytes.Buffer
	code, err := a.TestDir(context.Background(),
		dirWith(t, map[string]string{
			"v.http": "GET http://127.0.0.1:9/none\nAuthorization: Bearer {{X}}\n\n###\n",
		}), &out, 2, 2*time.Second, false)
	if err != nil {
		t.Fatalf("TestDir: %v", err)
	}
	// Exit 3, not 1 and not 0: the variable is unresolved because the vault
	// never opened, and an unresolved secret is a keychain-class failure.
	if code != 3 {
		t.Errorf("unresolvable vault-only run exited %d, want 3 (fail closed)", code)
	}
}
