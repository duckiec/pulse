package secrets

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
)

// The write side of the keychain.
//
// `SecretResolver` is read-only and has been since Move 4: its whole contract is
// "give me the value under this name", and three moves of resolution logic,
// degradation reporting and fail-closed error mapping are built on it. Making it
// also write would mean every resolver implementation acquired a credential-
// writing code path it does not otherwise need, and a caller holding a resolver
// to READ a token would silently be able to write one.
//
// The vault needs to write, so the write side is a separate interface with its
// own availability contract. Keeping them apart also makes the failure modes
// honest: "cannot read your keys" and "cannot store a new one" are different
// problems a user needs to diagnose differently.

// ErrStoreUnavailable reports that this platform or environment has no keychain
// write path.
//
// It is a typed BLOCKED condition, not a generic failure: on a headless Linux
// box, on an unsupported OS, and on a Windows host without the credential
// manager, `vault init` must say which of those it is, because the remedy is
// different in each case.
var ErrStoreUnavailable = errors.New("secrets: this platform or environment has no keychain write path")

// VaultAccount returns the keychain account attribute for a vault key.
//
// It is a function rather than a constant so the account shape is discoverable
// and testable, and so a future key-id form (multiple vault keys on one machine)
// extends without changing callers.
func VaultAccount(keyID string) string {
	if keyID == "" {
		return "vault:default"
	}
	return "vault:" + keyID
}

// SecretStore is the keychain's write side.
type SecretStore interface {
	// Store writes value under the given key, replacing any existing value.
	//
	// It returns ErrStoreUnavailable when the platform or environment has no
	// write path, and a platform error otherwise. It must NOT fall back to a
	// file, a temp dir, or a plaintext store: AGENTS.md §4.3 forbids a
	// credential ever landing on disk, and a fallback that "works" on a machine
	// without a keychain is the worst possible behaviour here.
	Store(key, value string) error

	// Available reports whether Store can be attempted at all, with a reason
	// when it cannot.
	Available() error

	// Backend names the implementation, for diagnostics.
	Backend() string
}

// noStore is the implementation on platforms with no keychain write path.
//
// It is a distinct type rather than nil so a caller can type-assert on it and
// so the error is informative instead of a nil-pointer panic.
type noStore struct{}

func (noStore) Store(_, _ string) error { return ErrStoreUnavailable }
func (noStore) Available() error        { return ErrStoreUnavailable }
func (noStore) Backend() string         { return "unsupported-" + runtime.GOOS }

// NewStore returns this platform's keychain write side.
//
// It never returns nil: a platform with no write path gets noStore, whose
// Available explains itself. Returning nil would push the check onto every
// caller.
func NewStore() SecretStore { return newPlatformStore() }

// StoreAvailable reports whether the keychain can be written to on this host,
// with the reason it cannot be when the answer is no.
func StoreAvailable() error { return NewStore().Available() }

// MemoryStore is an in-process keychain used by tests and by callers that need
// a deterministic store without depending on an OS service.
//
// It exists because the keychain is an OS service, and a test suite that needs
// one to be present is a test suite that is BLOCKED on most machines. It is NOT
// a fallback the production path may use: `vault init` calls [NewStore], which
// returns noStore where there is no keychain, and fails closed there.
//
// It is mutex-guarded because the composition root's goroutines resolve and
// write concurrently, and a data race on a credentials map is a defect that
// `-race` would rightly call a release blocker.
type MemoryStore struct {
	mu    sync.Mutex
	data  map[string]string
	avail error
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: map[string]string{}}
}

// Store writes value under key, overwriting.
func (m *MemoryStore) Store(key, value string) error {
	if m.avail != nil {
		return m.avail
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

// Resolve reads a value back.
//
// It is here so a caller that only has a MemoryStore can still read what it
// wrote, and so tests can round-trip without a resolver.
func (m *MemoryStore) Resolve(key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return "", fmt.Errorf("secrets: no entry for %q", key)
	}
	return v, nil
}

// Available reports whether this store accepts writes.
func (m *MemoryStore) Available() error { return m.avail }

// Backend names the implementation.
func (m *MemoryStore) Backend() string { return "memory" }

// Fail makes the store reject everything with err, for gates that need a store
// which is present but unusable.
func (m *MemoryStore) Fail(err error) *MemoryStore { m.avail = err; return m }

// Len reports how many entries are held.
func (m *MemoryStore) Len() int { return len(m.data) }
