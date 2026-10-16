package app_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pulse/internal/app"
	"pulse/internal/secrets"
)

// TestVaultSetBootstrapsIdentityAndVault pins the transparent
// auto-bootstrap: on a fresh machine (empty keychain, no vault file) a
// single Set mints the device identity, seals the entry, and lands the
// container — all without a prior keygen or init.
func TestVaultSetBootstrapsIdentityAndVault(t *testing.T) {
	dir := t.TempDir()
	vaultPath := filepath.Join(dir, app.DefaultVaultName)
	mem := secrets.NewMemoryStore()
	a := newTestApp(t)

	if err := a.VaultSet(vaultPath, "bootstrap-1", "DEMO_TOKEN", "s3cr3t", mem, mem); err != nil {
		t.Fatalf("VaultSet (bootstrap): %v", err)
	}
	if _, err := os.Stat(vaultPath); err != nil {
		t.Fatalf("no vault file after bootstrap set: %v", err)
	}
	raw, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[:5]) != secrets.VaultMagic2 {
		t.Fatalf("container magic = %q, want PVLT2: set must produce what the chain reads", raw[:5])
	}
	if strings.Contains(string(raw), "s3cr3t") {
		t.Fatal("the sealed vault contains the plaintext value")
	}

	keys, err := a.VaultList(vaultPath, "bootstrap-1", mem)
	if err != nil {
		t.Fatalf("VaultList: %v", err)
	}
	if len(keys) != 1 || keys[0] != "DEMO_TOKEN" {
		t.Fatalf("VaultList = %q, want [DEMO_TOKEN]", keys)
	}

	// The bootstrapped identity opens the container through the same path
	// the runtime chain uses.
	id, err := secrets.LoadIdentity("bootstrap-1", mem)
	if err != nil {
		t.Fatalf("LoadIdentity after bootstrap: %v", err)
	}
	defer clear(id.X25519Priv)
	defer clear(id.Ed25519Seed)
	plain, err := secrets.Open2(raw, id.X25519Priv)
	if err != nil {
		t.Fatalf("Open2 with the bootstrapped identity: %v", err)
	}
	defer clear(plain)
	if !strings.Contains(string(plain), "DEMO_TOKEN=s3cr3t") {
		t.Fatalf("vault payload = %q, want the sealed entry", plain)
	}
}

// TestVaultSetListUnsetRoundTrip pins the CRUD cycle: two inserts list
// sorted, unset drops exactly one, and unsetting the last key leaves an
// openable (empty) container rather than deleting the file.
func TestVaultSetListUnsetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	vaultPath := filepath.Join(dir, app.DefaultVaultName)
	mem := secrets.NewMemoryStore()
	a := newTestApp(t)

	for _, kv := range [][2]string{{"ZEBRA", "z"}, {"APPLE", "a"}, {"MANGO", "m"}} {
		if err := a.VaultSet(vaultPath, "", kv[0], kv[1], mem, mem); err != nil {
			t.Fatalf("VaultSet %s: %v", kv[0], err)
		}
	}
	keys, err := a.VaultList(vaultPath, "", mem)
	if err != nil {
		t.Fatalf("VaultList: %v", err)
	}
	if strings.Join(keys, ",") != "APPLE,MANGO,ZEBRA" {
		t.Fatalf("VaultList = %q, want sorted names", keys)
	}

	if err := a.VaultUnset(vaultPath, "", "MANGO", mem); err != nil {
		t.Fatalf("VaultUnset: %v", err)
	}
	keys, err = a.VaultList(vaultPath, "", mem)
	if err != nil {
		t.Fatalf("VaultList after unset: %v", err)
	}
	if strings.Join(keys, ",") != "APPLE,ZEBRA" {
		t.Fatalf("VaultList after unset = %q, want [APPLE ZEBRA]", keys)
	}

	if err := a.VaultUnset(vaultPath, "", "MANGO", mem); err == nil {
		t.Fatal("unsetting an absent key succeeded; want a fail-closed error")
	}
	if err := a.VaultUnset(vaultPath, "", "APPLE", mem); err != nil {
		t.Fatalf("VaultUnset APPLE: %v", err)
	}
	if err := a.VaultUnset(vaultPath, "", "ZEBRA", mem); err != nil {
		t.Fatalf("VaultUnset ZEBRA: %v", err)
	}
	keys, err = a.VaultList(vaultPath, "", mem)
	if err != nil {
		t.Fatalf("VaultList of the emptied vault: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("emptied vault lists %q, want no keys", keys)
	}
}

// TestVaultSetRefusesToOrphanAnExistingVault pins the bootstrap matrix's
// fail-closed corner: a vault sealed for another device plus no local
// identity must refuse — minting a fresh identity and overwriting would
// lock out the vault's real owners.
func TestVaultSetRefusesToOrphanAnExistingVault(t *testing.T) {
	owner := mkIdentity(t)
	ownerMem := secrets.NewMemoryStore()
	if err := secrets.StoreIdentity("", owner, ownerMem); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	vaultPath := filepath.Join(dir, app.DefaultVaultName)
	a := newTestApp(t)
	if err := a.VaultSet(vaultPath, "", "OWNER_KEY", "owner-value", ownerMem, ownerMem); err != nil {
		t.Fatalf("owner VaultSet: %v", err)
	}

	stranger := secrets.NewMemoryStore() // no identity, same vault path
	if err := a.VaultSet(vaultPath, "", "INTRUDER", "x", stranger, stranger); err == nil {
		t.Fatal("set with no identity over another device's vault succeeded; want a refusal")
	}
	// The owner's entry survives the refused overwrite.
	keys, err := a.VaultList(vaultPath, "", ownerMem)
	if err != nil {
		t.Fatalf("VaultList after refused set: %v", err)
	}
	if len(keys) != 1 || keys[0] != "OWNER_KEY" {
		t.Fatalf("vault after refused set lists %q, want the owner's entry intact", keys)
	}
}

// TestVaultSetFailsClosedOnBadInput pins the input contract: invalid key
// characters and empty values never reach the keychain or the disk.
func TestVaultSetFailsClosedOnBadInput(t *testing.T) {
	dir := t.TempDir()
	vaultPath := filepath.Join(dir, app.DefaultVaultName)
	mem := secrets.NewMemoryStore()
	a := newTestApp(t)

	if err := a.VaultSet(vaultPath, "", "HAS SPACE", "v", mem, mem); err == nil {
		t.Fatal("a key with a space was accepted")
	}
	if err := a.VaultSet(vaultPath, "", "OK_KEY", "", mem, mem); err == nil {
		t.Fatal("an empty value was accepted; want fail-closed")
	}
	if _, err := os.Stat(vaultPath); !os.IsNotExist(err) {
		t.Fatalf("a rejected set left a vault file behind: %v", err)
	}
	if mem.Len() != 0 {
		t.Fatal("a rejected set stored keychain material")
	}
}

// TestVaultListOnMissingVaultNamesTheRemedy pins the fresh-workspace error:
// listing with no vault file fails closed and tells the user the one
// command that creates it.
func TestVaultListOnMissingVaultNamesTheRemedy(t *testing.T) {
	a := newTestApp(t)
	_, err := a.VaultList(filepath.Join(t.TempDir(), app.DefaultVaultName), "", secrets.NewMemoryStore())
	if err == nil {
		t.Fatal("listing a missing vault succeeded")
	}
	if !strings.Contains(err.Error(), "pulse vault set") {
		t.Fatalf("missing-vault error = %q, want it to name `pulse vault set`", err)
	}
}

// TestVaultKeyNamesNeverReturnsValues is the name-only companion: the
// doctor cross-check reads names, and names are all it may ever observe.
func TestVaultKeyNamesNeverReturnsValues(t *testing.T) {
	dir := t.TempDir()
	vaultPath := filepath.Join(dir, app.DefaultVaultName)
	mem := secrets.NewMemoryStore()
	a := newTestApp(t)
	if err := a.VaultSet(vaultPath, "", "TOKEN_A", "value-a", mem, mem); err != nil {
		t.Fatal(err)
	}
	keys, set, err := app.VaultKeyNames(vaultPath, "", mem)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !set["TOKEN_A"] {
		t.Fatalf("VaultKeyNames = %q, want {TOKEN_A}", keys)
	}
	for _, k := range keys {
		if strings.Contains(k, "value-a") {
			t.Fatalf("a key name carries a value: %q", k)
		}
	}
}
