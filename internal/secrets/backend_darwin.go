//go:build darwin

package secrets

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// The macOS backend shells out to /usr/bin/security. That is the CGO-free route
// to the Keychain: the Security.framework APIs that wrap it are cgo-only, and
// AGENTS.md §2.1 bans cgo outright (ARCHITECTURE.md §6.2).
//
//	security find-generic-password -s <service> -a <account> -w
//
// `-w` prints the password on stdout with no prompt, which is exactly what a
// non-interactive tool needs. Note the process argv therefore carries the
// account name but never the secret.

// darwinBackend is the macOS Keychain backend.
type darwinBackend struct{}

// newPlatformBackend is the darwin build of NewBackend.
func newPlatformBackend() SecretResolver { return darwinBackend{} }

// Backend implements SecretResolver.
func (darwinBackend) Backend() string { return "macos-keychain-security-cli" }

// Available reports whether the security CLI can answer a query at all.
func (d darwinBackend) Available() error {
	// `security -h` exits non-zero on some releases, so probe the specific
	// subcommand instead: a successful, empty dump means the toolchain is there.
	path, err := exec.LookPath("security")
	if err != nil {
		return fmt.Errorf("secrets: /usr/bin/security not found (macOS keychain unavailable): %w", err)
	}
	if !strings.HasPrefix(path, "/usr/bin") {
		// Prefer the system binary; a shadowed one on PATH would be a hijack.
		return fmt.Errorf("secrets: refusing non-system security binary at %s", path)
	}
	return nil
}

// Resolve reads one generic password from the login keychain.
func (d darwinBackend) Resolve(key string) (string, error) {
	if err := d.Available(); err != nil {
		return "", err
	}
	cmd := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", ServiceName, "-a", key, "-w")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// The stderr text names the item but never its value; it is safe to
		// surface for diagnosis.
		return "", fmt.Errorf("secrets: keychain lookup for %q failed: %w: %s",
			key, err, strings.TrimSpace(stderr.String()))
	}
	val := strings.TrimRight(stdout.String(), "\n")
	if val == "" {
		return "", fmt.Errorf("secrets: keychain returned an empty value for %q", key)
	}
	return val, nil
}
