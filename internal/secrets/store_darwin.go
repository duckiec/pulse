//go:build darwin

package secrets

import (
	"bytes"
	"fmt"
	"os/exec"
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
// The secret is passed as an argv element to a short-lived child process. That
// is a real, documented weakness -- argv is world-readable via /proc on Linux
// and via `ps` on every platform -- and it is a property of the CGO-free
// constraint, not an oversight: the alternative is an unsupported platform. It
// is mitigated in practice by the fact that the vault key lives in the keychain
// for the lifetime of the machine, so it is written once per `vault init`
// rather than per request.
type darwinStore struct{}

func newPlatformStore() SecretStore { return darwinStore{} }

func (darwinStore) Backend() string { return "macos-keychain-security-cli" }

func (darwinStore) Available() error {
	if _, err := exec.LookPath("security"); err != nil {
		return fmt.Errorf("secrets: %w: `security` is not on PATH", ErrStoreUnavailable)
	}
	return nil
}

// Store implements SecretStore.
func (s darwinStore) Store(key, value string) error {
	if err := s.Available(); err != nil {
		return err
	}
	// -U updates an existing item instead of failing on a duplicate.
	cmd := exec.Command("security", "add-generic-password",
		"-s", ServiceName, "-a", key, "-w", value, "-U")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("secrets: security add-generic-password failed: %w (%s)",
			err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}
