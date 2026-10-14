package secrets

// Hermetic constructors for the macOS `security` CLI invocations.
//
// The Darwin backend shells out to /usr/bin/security (the Security.framework
// APIs are cgo-only; AGENTS.md §2.1 bans cgo). Everything about those child
// processes that can be decided without a Mac — the binary path, the argv,
// where the secret travels — is decided HERE, in platform-independent code,
// so the OS-boundary logic is unit-testable at 100% statement coverage on any
// host, with no keychain daemon. The platform-tagged wrappers
// (backend_darwin.go, store_darwin.go) only attach stdio and call Run.
//
// The security model, in one place so it can be audited in one place:
//
//   - The binary is the absolute /usr/bin/security. A bare "security"
//     resolved via PATH is a hijack vector (a shadowed binary earlier on PATH
//     receives the secret on its stdin); the read and write paths both pin
//     the system binary.
//   - The secret NEVER appears in argv. `add-generic-password -w` with no
//     password argument reads the password from stdin, so the write argv
//     carries only the service and account names — neither of which is
//     secret — and the value travels over a pipe invisible to `ps`.
//     (Upstream provenance: Apicurio/apicurio-registry#9307, which fixed the
//     identical argv exposure in another project's macOS provider.)
//   - `-w` is TRAILING. If `-U` (or any flag) followed `-w`, the CLI parser
//     could swallow it as the password argument and fall back to an
//     interactive prompt — hanging a headless process. Trailing `-w` with
//     nothing after it unambiguously means "read stdin".

import (
	"io"
	"os/exec"
	"strings"
)

// darwinSecurityPath is the only `security` binary Pulse will execute.
const darwinSecurityPath = "/usr/bin/security"

// darwinFindArgs builds the read argv: find-generic-password prints the
// password on stdout (`-w`), so the secret never needs to be an argument.
// The account name rides in argv; it is a lookup key, not a secret.
func darwinFindArgs(service, account string) []string {
	return []string{
		"find-generic-password",
		"-s", service,
		"-a", account,
		"-w",
	}
}

// darwinAddArgs builds the write argv WITHOUT the secret: `-w` trailing with
// no argument means "read the password from stdin". The `-U` flag updates a
// pre-existing item instead of failing on a duplicate, so `vault init` is
// idempotent.
func darwinAddArgs(service, account string) []string {
	return []string{
		"add-generic-password",
		"-s", service,
		"-a", account,
		"-U",
		"-w",
	}
}

// newDarwinFindCmd wires a read invocation: absolute binary, fixed argv,
// caller-provided stdout/stderr buffers.
func newDarwinFindCmd(service, account string, stdout, stderr io.Writer) *exec.Cmd {
	cmd := exec.Command(darwinSecurityPath, darwinFindArgs(service, account)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd
}

// newDarwinAddCmd wires a write invocation: absolute binary, secret-free
// argv, and the secret as the child's stdin — never in `ps` output.
func newDarwinAddCmd(service, account string, secret io.Reader, stderr io.Writer) *exec.Cmd {
	cmd := exec.Command(darwinSecurityPath, darwinAddArgs(service, account)...)
	cmd.Stdin = secret
	cmd.Stderr = stderr
	return cmd
}

// darwinArgsCarrySecret reports whether any argv element contains the secret.
// The hermetic tests assert this is false for every constructed invocation;
// it exists so the invariant is checked by code, not by eyeballing.
func darwinArgsCarrySecret(cmd *exec.Cmd, secret string) bool {
	if secret == "" {
		return false
	}
	for _, arg := range cmd.Args[1:] {
		if strings.Contains(arg, secret) {
			return true
		}
	}
	return false
}
