package app

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pulse/internal/secrets"
)

// This file is Phase 2.3's one-step vaulting: `pulse vault set/list/unset`
// over a PVLT2 `.pulse.vault` key-value store in the workspace.
//
// The store holds a `.env`-shaped payload (KEY=value lines) sealed with
// Seal2 for the device identity, so the SAME container the runtime chain
// opens at boot (openVaultTier) is the one the CLI edits — there is no
// second format to drift. Keys are sorted on every write so seals diff
// cleanly; values are never printed, never logged, and registered with
// the chain redactor when one is present.

// DefaultVaultName is the workspace vault filename the directive names.
// PULSE_VAULT_FILE overrides it (consistent with appConfig); every vault
// subcommand and `pulse doctor` resolve through ResolveVaultPath so they
// cannot disagree about which file they mean.
const DefaultVaultName = ".pulse.vault"

// ResolveVaultPath returns the vault container path: PULSE_VAULT_FILE when
// set, otherwise DefaultVaultName in the current directory.
func ResolveVaultPath() string {
	if v := os.Getenv("PULSE_VAULT_FILE"); v != "" {
		return v
	}
	return DefaultVaultName
}

// isVaultKey mirrors secrets' key grammar (letters, digits, '_', '.', '-'):
// the payload parses through secrets.LoadEnv, which rejects anything else,
// so an early refusal here names the key instead of failing deep in a seal.
func isVaultKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c == '_' || c == '.' || c == '-' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// encodeVaultEnv renders a key map as `.env` text with sorted keys and a
// trailing newline per entry. Values carrying newlines, quotes, or a `#`
// comment opener are double-quoted with \\, \", \n, \t escapes — the exact
// forms secrets.LoadEnv unquotes — so the encoding round-trips byte-exact
// through the parser the chain reads with.
func encodeVaultEnv(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v := m[k]
		if strings.ContainsAny(v, "\n\r\"\\") || strings.Contains(v, " #") || strings.HasPrefix(strings.TrimSpace(v), "#") {
			r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
			b.WriteString(k + `="` + r.Replace(v) + "\"\n")
			continue
		}
		b.WriteString(k + "=" + v + "\n")
	}
	return []byte(b.String())
}

// writeAtomic0600 lands data at path via a temp file in the same directory
// plus rename, so a crash never leaves a truncated vault. The file is 0600:
// the ciphertext is not secret, but the vault's existence and mtime are.
func writeAtomic0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("vault: create the vault directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".pulse-vault-*")
	if err != nil {
		return fmt.Errorf("vault: stage the sealed vault: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("vault: protect the staged vault: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("vault: write the staged vault: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("vault: sync the staged vault: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("vault: close the staged vault: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("vault: publish the sealed vault: %w", err)
	}
	return nil
}

// sealRecipients returns the recipient list for a (re-)seal: the device's
// own decryption public key, plus every key in pulse.recipients beside the
// vault when that file exists. Sealing for self alone on a team checkout
// would silently drop every teammate at the next set; sealing for a list
// that omits self would lock the sealer out of its own vault. Both fail
// closed here instead.
func sealRecipients(vaultPath string, id *secrets.Identity) ([][]byte, error) {
	xpub, err := id.X25519Pub()
	if err != nil {
		return nil, err
	}
	recipients := [][]byte{xpub}
	sibling := filepath.Join(filepath.Dir(vaultPath), secrets.DefaultRecipientsFile)
	if _, statErr := os.Stat(sibling); statErr != nil {
		return recipients, nil
	}
	listed, err := secrets.ParseRecipients(sibling)
	if err != nil {
		return nil, err
	}
	for _, p := range listed {
		dup := false
		for _, have := range recipients {
			if string(have) == string(p) {
				dup = true
				break
			}
		}
		if !dup {
			recipients = append(recipients, p)
		}
	}
	return recipients, nil
}

// sealAndPublish seals payload for recipients signed by id and lands it
// atomically at vaultPath. Payload, container, and signer are zeroised
// before return; the caller's map is untouched.
func sealAndPublish(vaultPath string, payload []byte, recipients [][]byte, id *secrets.Identity) error {
	signer := ed25519.NewKeyFromSeed(id.Ed25519Seed)
	res, err := secrets.Seal2(payload, recipients, signer)
	clear(signer)
	clear(payload)
	if err != nil {
		clear(res)
		return err
	}
	defer clear(res)
	return writeAtomic0600(vaultPath, res)
}

// openVaultMap decrypts vaultPath with the device identity behind resolver
// and parses the payload as `.env` into memory. It returns the identity
// (caller zeroises), the map, and the recipient list to re-seal with.
func openVaultMap(vaultPath, keyID string, resolver secrets.SecretResolver) (*secrets.Identity, map[string]string, [][]byte, error) {
	id, err := secrets.LoadIdentity(keyID, resolver)
	if err != nil {
		return nil, nil, nil, &VaultError{Kind: VaultErrResolve, msg: err.Error(), err: err}
	}
	container, err := os.ReadFile(vaultPath)
	if err != nil {
		clear(id.X25519Priv)
		clear(id.Ed25519Seed)
		return nil, nil, nil, fmt.Errorf("vault: read %s: %w", vaultPath, err)
	}
	plain, err := secrets.Open2(container, id.X25519Priv)
	clear(container)
	if err != nil {
		clear(id.X25519Priv)
		clear(id.Ed25519Seed)
		return nil, nil, nil, &VaultError{Kind: kindForDecrypt(err), msg: err.Error(), err: err}
	}
	env, err := secrets.LoadEnv(strings.NewReader(string(plain)))
	clear(plain)
	if err != nil {
		clear(id.X25519Priv)
		clear(id.Ed25519Seed)
		return nil, nil, nil, &VaultError{Kind: VaultErrFormat, msg: fmt.Sprintf("the vault payload is not .env-shaped: %v", err), err: err}
	}
	recipients, err := sealRecipients(vaultPath, id)
	if err != nil {
		clear(id.X25519Priv)
		clear(id.Ed25519Seed)
		return nil, nil, nil, err
	}
	return id, env, recipients, nil
}

// VaultSet inserts or replaces one key in the workspace vault, creating the
// device identity and the vault itself when neither exists (transparent
// auto-bootstrap for a first `set` on a fresh machine).
//
// Bootstrap matrix (fail-closed where overwriting would orphan data):
//   - no identity + no vault → generate identity, seal {key:value} for self;
//   - identity + no vault    → seal {key:value} for self (no regen);
//   - no identity + vault    → refuse (a new key cannot open the old vault);
//   - identity + vault       → decrypt, insert, re-seal atomically.
func (a *App) VaultSet(vaultPath, keyID, key, value string, store secrets.SecretStore, resolver secrets.SecretResolver) error {
	if !isVaultKey(key) {
		return &VaultError{Kind: VaultErrFormat, msg: fmt.Sprintf("refusing an invalid vault key %q (letters, digits, '_', '.', '-' only)", key)}
	}
	if value == "" {
		return &VaultError{Kind: VaultErrFormat, msg: fmt.Sprintf("refusing an empty value for %q (fail closed, never a dummy)", key)}
	}
	if store == nil || resolver == nil {
		return &VaultError{Kind: VaultErrNoStore, msg: "no keychain path"}
	}

	_, idErr := secrets.LoadIdentity(keyID, resolver)
	_, vaultErr := os.Stat(vaultPath)

	switch {
	case idErr != nil && os.IsNotExist(vaultErr):
		// Fresh machine: mint the device identity, then seal the first entry.
		id, err := secrets.GenerateIdentity()
		if err != nil {
			return err
		}
		defer clear(id.X25519Priv)
		defer clear(id.Ed25519Seed)
		if err := secrets.StoreIdentity(keyID, id, store); err != nil {
			return &VaultError{Kind: VaultErrStore, msg: err.Error(), err: err}
		}
		recipients, err := sealRecipients(vaultPath, id)
		if err != nil {
			return err
		}
		if err := sealAndPublish(vaultPath, encodeVaultEnv(map[string]string{key: value}), recipients, id); err != nil {
			return err
		}
	case idErr != nil:
		// A vault exists but this device has no identity: a fresh key would
		// orphan the container. Refuse with the remedy named.
		return &VaultError{Kind: VaultErrResolve, msg: "this device holds no vault identity but " + vaultPath + " exists; run `pulse vault keygen` on the owning device and share its public half instead of overwriting"}
	case os.IsNotExist(vaultErr):
		// Identity without a vault: seal the first entry for self.
		id, env, recipients, err := openVaultMapForNew(vaultPath, keyID, resolver)
		if err != nil {
			return err
		}
		defer clear(id.X25519Priv)
		defer clear(id.Ed25519Seed)
		env[key] = value
		if err := sealAndPublish(vaultPath, encodeVaultEnv(env), recipients, id); err != nil {
			return err
		}
	default:
		if vaultErr != nil {
			return fmt.Errorf("vault: stat %s: %w", vaultPath, vaultErr)
		}
		id, env, recipients, err := openVaultMap(vaultPath, keyID, resolver)
		if err != nil {
			return err
		}
		defer clear(id.X25519Priv)
		defer clear(id.Ed25519Seed)
		env[key] = value
		if err := sealAndPublish(vaultPath, encodeVaultEnv(env), recipients, id); err != nil {
			return err
		}
	}
	if a != nil && a.Chain != nil && a.Chain.Redactor() != nil {
		a.Chain.Redactor().Register(value)
	}
	return nil
}

// openVaultMapForNew loads the identity and recipient list when no vault
// file exists yet: there is nothing to decrypt, so the map starts empty.
func openVaultMapForNew(vaultPath, keyID string, resolver secrets.SecretResolver) (*secrets.Identity, map[string]string, [][]byte, error) {
	id, err := secrets.LoadIdentity(keyID, resolver)
	if err != nil {
		return nil, nil, nil, &VaultError{Kind: VaultErrResolve, msg: err.Error(), err: err}
	}
	recipients, err := sealRecipients(vaultPath, id)
	if err != nil {
		clear(id.X25519Priv)
		clear(id.Ed25519Seed)
		return nil, nil, nil, err
	}
	return id, map[string]string{}, recipients, nil
}

// VaultList returns the sorted key names sealed in the workspace vault.
// Values are never returned, never logged: listing is a name-only operation.
func (a *App) VaultList(vaultPath, keyID string, resolver secrets.SecretResolver) ([]string, error) {
	if resolver == nil {
		return nil, &VaultError{Kind: VaultErrNoStore, msg: "no keychain read path"}
	}
	if _, err := os.Stat(vaultPath); err != nil {
		if os.IsNotExist(err) {
			return nil, &VaultError{Kind: VaultErrFormat, msg: fmt.Sprintf("no vault at %s; run `pulse vault set <KEY> <VALUE>` to create one", vaultPath)}
		}
		return nil, fmt.Errorf("vault: stat %s: %w", vaultPath, err)
	}
	id, env, _, err := openVaultMap(vaultPath, keyID, resolver)
	if err != nil {
		return nil, err
	}
	defer clear(id.X25519Priv)
	defer clear(id.Ed25519Seed)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// VaultUnset removes one key from the workspace vault and re-seals
// atomically. An absent key or an absent vault fails closed; unsetting the
// last key leaves an empty (still openable) container rather than deleting
// the file out from under the runtime chain.
func (a *App) VaultUnset(vaultPath, keyID, key string, resolver secrets.SecretResolver) error {
	if !isVaultKey(key) {
		return &VaultError{Kind: VaultErrFormat, msg: fmt.Sprintf("refusing an invalid vault key %q", key)}
	}
	if resolver == nil {
		return &VaultError{Kind: VaultErrNoStore, msg: "no keychain read path"}
	}
	if _, err := os.Stat(vaultPath); err != nil {
		if os.IsNotExist(err) {
			return &VaultError{Kind: VaultErrFormat, msg: fmt.Sprintf("no vault at %s", vaultPath)}
		}
		return fmt.Errorf("vault: stat %s: %w", vaultPath, err)
	}
	id, env, recipients, err := openVaultMap(vaultPath, keyID, resolver)
	if err != nil {
		return err
	}
	defer clear(id.X25519Priv)
	defer clear(id.Ed25519Seed)
	if _, ok := env[key]; !ok {
		return &VaultError{Kind: VaultErrFormat, msg: fmt.Sprintf("the vault holds no key %q", key)}
	}
	delete(env, key)
	return sealAndPublish(vaultPath, encodeVaultEnv(env), recipients, id)
}

// VaultKeyNames reads the sorted key names from a vault file for `pulse
// doctor`'s cross-check without an App: same open path, no writes.
func VaultKeyNames(vaultPath, keyID string, resolver secrets.SecretResolver) ([]string, map[string]bool, error) {
	id, env, _, err := openVaultMap(vaultPath, keyID, resolver)
	if err != nil {
		return nil, nil, err
	}
	defer clear(id.X25519Priv)
	defer clear(id.Ed25519Seed)
	set := make(map[string]bool, len(env))
	keys := make([]string, 0, len(env))
	for k := range env {
		set[k] = true
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, set, nil
}
