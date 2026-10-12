//go:build !linux && !darwin && !windows

package secrets

import "fmt"

// On platforms with no keychain binding Pulse reports the backend as
// unavailable rather than pretending to resolve anything. The chain then falls
// through to the ephemeral .env, and beyond that to a hard error (AGENTS.md
// §4.4). Reporting BLOCKED is the honest disposition; a fake success is not
// available to us here (AGENTS.md §4.5).

// unsupportedBackend stands in for a platform with no keychain integration.
type unsupportedBackend struct{ goos string }

// newPlatformBackend is the build used on every other platform.
func newPlatformBackend() SecretResolver { return unsupportedBackend{goos: runtimeGOOS()} }

// Backend implements SecretResolver.
func (u unsupportedBackend) Backend() string { return "unsupported-" + u.goos }

// Available always reports BLOCKED, with the platform named.
func (u unsupportedBackend) Available() error {
	return fmt.Errorf("secrets: no keychain backend on %s", u.goos)
}

// Resolve always fails; it must never return a placeholder.
func (u unsupportedBackend) Resolve(key string) (string, error) {
	return "", fmt.Errorf("secrets: no keychain backend on %s (cannot resolve %q)", u.goos, key)
}
