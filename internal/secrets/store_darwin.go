//go:build darwin

package secrets

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// The darwin write path shells out to `security add-generic-password`, for the
// same reason the read path shells out to `security find-generic-password`:
// the Security.framework APIs that wrap the Keychain are cgo-only, and
// AGENTS.md §2.1 bans cgo outright (ARCHITECTURE.md §6.2).
//
//	security add-generic-password -s <service> -a <account> -w <password>
//
// A pre-existing item is UPDATED rather than replaced, so `vault init` on a
// machine that already holds a key does not leave a stale entry behind and then
// fail to read the new one.
//
// The secret reaches the child on stdin, never in argv: `add-generic-password
// -w` with no password argument reads the password from stdin, closing the
// argv/`ps` exposure the write path previously carried (MEMORY.md ADL-0041).
// The argv (service and account names) and the binary path are built by the
// hermetic constructors in cmdbuilder.go, which the headless tests pin.
type darwinStore struct{}

func newPlatformStore() SecretStore { return darwinStore{} }

func (darwinStore) Backend() string { return "macos-keychain-security-cli" }

func (darwinStore) Available() error {
	// Pin the system binary, exactly as the read path does: a bare
	// "security" resolved via PATH is a hijack vector, and the write path
	// is the one that would hand the secret to the impostor on stdin.
	path, err := exec.LookPath("security")
	if err != nil {
		return fmt.Errorf("secrets: %w: `security` is not on PATH", ErrStoreUnavailable)
	}
	if !strings.HasPrefix(path, "/usr/bin") {
		return fmt.Errorf("secrets: refusing non-system security binary at %s", path)
	}
	return nil
}

// Store implements SecretStore.
//
// The secret travels on the child's stdin, never in argv: `-w` trailing with
// no argument tells `security` to read the password from stdin, so `ps` on a
// shared machine sees only the service and account names. (MEMORY.md
// ADL-0041; upstream provenance Apicurio/apicurio-registry#9307.)
func (s darwinStore) Store(key, value string) error {
	if err := s.Available(); err != nil {
		return err
	}
	if value == "" {
		// An empty write would store an empty password that the read side
		// rejects as an error — fail here, where the cause is obvious.
		return fmt.Errorf("secrets: refusing to store an empty value for %q", key)
	}
	var stderr bytes.Buffer
	cmd := newDarwinAddCmd(ServiceName, key, strings.NewReader(value), &stderr)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("secrets: security add-generic-password failed: %w (%s)",
			err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}
