// Package secrets implements Pulse's zero-trust secret handling: the OS keychain
// backends, the fail-closed resolution chain, and the redaction chokepoint that
// stands between a resolved secret and anything a user, a log, or the database
// can see.
//
// Three laws govern this package (AGENTS.md §3.4, §4.3, §4.4):
//
//  1. A secret is never written to disk — not the repo, not /tmp, not SQLite.
//  2. Resolution is fail-closed: keychain → ephemeral .env → hard error. There is
//     no fourth option, no placeholder, and no empty-string fallback.
//  3. A resolved value is registered with the redactor the moment it exists, so
//     it cannot leak through a payload the redactor has not seen yet.
//
// Everything here is pure Go. No cgo, no libsecret, no Security.framework, no
// advapi32 cgo binding.
package secrets

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// ServiceName is the keychain service Pulse stores and looks secrets under.
const ServiceName = "pulse"

// ErrUnresolved is the terminal error of the resolution chain. A run that hits
// it exits with code 3 (AGENTS.md §4.4 exit codes).
var ErrUnresolved = errors.New("secrets: variable unresolved")

// ErrNotFound reports that the keychain answered but holds no item under the
// requested account. It is distinct from "the keychain is unreachable"
// (a BLOCKED condition): callers use errors.Is to tell "absent" from
// "unavailable" without parsing a message. Proven live against the Secret
// Service in Wave 6.5.
var ErrNotFound = errors.New("not found in keychain")

// ExitCodeSecret is the process exit code for a secret-resolution failure.
const ExitCodeSecret = 3

// SecretResolver is a single keychain backend.
//
// Available reports whether the backend can be used at all. A backend that is
// not available is a BLOCKED condition (AGENTS.md §4.5), never a silent success.
type SecretResolver interface {
	// Available returns nil when the backend can serve lookups, or an error
	// describing why it cannot (no session bus, no keychain daemon, ...).
	Available() error
	// Resolve returns the secret stored under key, or an error. It MUST NOT
	// include the secret in any error it returns.
	Resolve(key string) (string, error)
	// Backend names the implementation, for diagnostics.
	Backend() string
} // Chain is the ordered set of sources consulted for a lookup, terminating in a
// hard error. It is the zero-trust resolution chain of ARCHITECTURE.md §6.1.
type Chain struct {
	mu       sync.RWMutex
	backend  SecretResolver    // OS keychain, may be nil
	env      map[string]string // ephemeral .env, memory only
	vault    map[string]string // decrypted .env.vault, memory only
	redactor *Redactor         // where every resolved value is registered
	order    []string          // provenance, for diagnostics
}

// ChainOption configures a Chain.
type ChainOption func(*Chain)

// WithBackend binds the OS keychain backend.
func WithBackend(b SecretResolver) ChainOption { return func(c *Chain) { c.backend = b } }

// WithEnv binds an ephemeral, in-memory variable map (parsed from a .env).
func WithEnv(env map[string]string) ChainOption { return func(c *Chain) { c.env = env } }

// WithVaultEnv binds the decrypted vault payload: the PVLT2 container opened
// with the device identity, parsed as .env, held in memory only. It ranks
// BELOW the explicit .env — a developer's own machine values override the
// team-shared vault — and above the terminal error. Values resolved here are
// registered with the redactor exactly like every other source.
func WithVaultEnv(vault map[string]string) ChainOption {
	return func(c *Chain) { c.vault = vault }
}

// WithRedactor binds the redactor that every resolved value is registered with.
func WithRedactor(r *Redactor) ChainOption { return func(c *Chain) { c.redactor = r } }

// NewChain builds the resolution chain. The order is fixed and non-negotiable:
// OS keychain first, ephemeral .env second, hard error last.
func NewChain(opts ...ChainOption) *Chain {
	c := &Chain{backend: NewBackend(), redactor: NewRedactor()}
	for _, o := range opts {
		o(c)
	}
	if c.env == nil {
		c.env = map[string]string{}
	}
	if c.vault == nil {
		c.vault = map[string]string{}
	}
	c.order = []string{}
	if c.backend != nil {
		c.order = append(c.order, "keychain:"+c.backend.Backend())
	}
	if len(c.env) > 0 {
		c.order = append(c.order, "env")
	}
	if len(c.vault) > 0 {
		c.order = append(c.order, "vault")
	}
	c.order = append(c.order, "error")
	return c
}

// Redactor exposes the chain's redactor so callers can scrub before the value
// is resolved anywhere else.
func (c *Chain) Redactor() *Redactor { return c.redactor }

// Sources returns the ordered provenance of the chain, for diagnostics.
func (c *Chain) Sources() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, len(c.order))
	copy(out, c.order)
	return out
}

// Available reports whether the chain can serve lookups: either the keychain is
// usable, or the ephemeral env holds at least one variable. It is the honest
// "can this chain ever resolve something" probe.
func (c *Chain) Available() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.backend != nil {
		if err := c.backend.Available(); err == nil {
			return nil
		}
	}
	if len(c.env) > 0 {
		return nil
	}
	return fmt.Errorf("secrets: no usable source (keychain: %v, env: empty)", c.backendStatusLocked())
}

func (c *Chain) backendStatusLocked() string {
	if c.backend == nil {
		return "none"
	}
	if err := c.backend.Available(); err != nil {
		return err.Error()
	}
	return "ok"
}

// Resolve walks the chain and returns the first hit. On a miss it returns an
// error wrapping ErrUnresolved, which callers map to exit code 3.
//
// Every value this returns is registered with the redactor before it is handed
// back, so it can never escape un-scrubbed.
func (c *Chain) Resolve(key string) (string, error) {
	c.mu.RLock()
	backend, env, vault, redactor := c.backend, c.env, c.vault, c.redactor
	c.mu.RUnlock()

	var lastErr error
	// 1. OS keychain.
	if backend != nil {
		if err := backend.Available(); err == nil {
			if v, err := backend.Resolve(key); err == nil {
				if redactor != nil {
					redactor.Register(v)
				}
				return v, nil
			} else {
				lastErr = err
			}
		} else {
			lastErr = fmt.Errorf("keychain unavailable: %w", err)
		}
	}
	// 2. Ephemeral .env.
	if v, ok := env[key]; ok {
		if redactor != nil {
			redactor.Register(v)
		}
		return v, nil
	}
	// 3. Decrypted .env.vault payload (memory only, opened at boot).
	if v, ok := vault[key]; ok {
		if redactor != nil {
			redactor.Register(v)
		}
		return v, nil
	}
	// 4. Hard error. No placeholder, no empty string.
	if lastErr != nil {
		return "", fmt.Errorf("%w: %q (%v)", ErrUnresolved, key, lastErr)
	}
	return "", fmt.Errorf("%w: %q (keychain, env, vault)", ErrUnresolved, key)
}

// ResolveAll resolves every key, failing closed on the first miss. It is used to
// pre-resolve a request before it is sent so a mid-stream secret cannot leak a
// partially-sent request.
func (c *Chain) ResolveAll(keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		v, err := c.Resolve(k)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// Lookup satisfies the consumer-side resolver contract (`Lookup(name) (string,
// error)`), so a Chain can be handed straight to the engine's interpolation
// hook without an adapter. It is Resolve under a second name and nothing more.
func (c *Chain) Lookup(name string) (string, error) { return c.Resolve(name) }

// Seal installs one ephemeral variable at runtime: the in-TUI vault helper's
// memory-only tier. It ranks exactly where the loaded .env ranks (below the
// keychain, above the vault payload and the terminal error), and the value is
// registered with the redactor before Seal returns, so no sealed value exists
// un-scrubbed. It never touches disk: the composition root tries the OS
// keychain write side first and calls Seal only when no write path exists.
func (c *Chain) Seal(name, value string) error {
	if !validKey(name) {
		return fmt.Errorf("secrets: seal: invalid key %q", name)
	}
	if value == "" {
		return fmt.Errorf("secrets: seal: refusing an empty value for %q (fail closed, never a dummy)", name)
	}
	c.mu.Lock()
	if c.env == nil {
		c.env = map[string]string{}
	}
	c.env[name] = value
	redactor := c.redactor
	c.mu.Unlock()
	if redactor != nil {
		redactor.Register(value)
	}
	return nil
}

// ExitCode maps an error to a process exit code. A secret failure is 3; every
// other failure is 1 (AGENTS.md §4.4).
func ExitCode(err error) int {
	if errors.Is(err, ErrUnresolved) {
		return ExitCodeSecret
	}
	return 1
}

// ---- ephemeral .env ---------------------------------------------------------

// LoadEnv parses a .env-style stream into memory. It NEVER writes to disk and
// NEVER persists the values beyond the returned map, which lives only as long
// the caller holds it. Supported forms: KEY=value, export KEY=value, # comments,
// quoted values ('single', "double" with \\n and \\" escapes), and blank lines.
func LoadEnv(r io.Reader) (map[string]string, error) {
	env := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		eq := strings.IndexByte(text, '=')
		if eq <= 0 {
			// A malformed line is an error, never a silently-ignored one: a
			// typo'd secret name must not read as "unresolved" much later.
			return nil, fmt.Errorf("secrets: .env line %d: expected KEY=value", line)
		}
		key := strings.TrimSpace(text[:eq])
		if !validKey(key) {
			return nil, fmt.Errorf("secrets: .env line %d: invalid key %q", line, key)
		}
		val := strings.TrimSpace(text[eq+1:])
		val = unquote(val)
		env[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("secrets: read .env: %w", err)
	}
	return env, nil
}

// LoadEnvFile reads path into memory. The file is read, never written, and never
// copied anywhere else.
func LoadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: open %s: %w", path, err)
	}
	defer f.Close()
	env, err := LoadEnv(f)
	if err != nil {
		return nil, err
	}
	return env, nil
}

func validKey(k string) bool {
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

func unquote(v string) string {
	if len(v) >= 2 {
		if v[0] == '\'' && v[len(v)-1] == '\'' {
			return v[1 : len(v)-1]
		}
		if v[0] == '"' && v[len(v)-1] == '"' {
			inner := v[1 : len(v)-1]
			inner = strings.ReplaceAll(inner, `\n`, "\n")
			inner = strings.ReplaceAll(inner, `\t`, "\t")
			inner = strings.ReplaceAll(inner, `\"`, `"`)
			inner = strings.ReplaceAll(inner, `\\`, `\`)
			return inner
		}
	}
	// Strip a trailing inline comment on an unquoted value.
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// ExtractVariables returns the distinct `{{name}}` anchors appearing in s, in
// first-appearance order. It is how a caller learns which secrets a request
// needs before any of them is resolved.
func ExtractVariables(s string) []string {
	var out []string
	seen := map[string]bool{}
	rest := s
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			return out
		}
		rest = rest[i+2:]
		j := strings.Index(rest, "}}")
		if j < 0 {
			return out
		}
		name := rest[:j]
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		rest = rest[j+2:]
	}
}

// Clear zeroes the in-memory env AND vault maps. It is called on teardown
// (App.Close) so neither the developer's values nor the team's decrypted
// payload outlive the process's use of them. Zeroing precedes dropping: the
// maps are reallocated, but the old backing arrays are scrubbed first.
func (c *Chain) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.env {
		c.env[k] = ""
	}
	c.env = map[string]string{}
	for k := range c.vault {
		c.vault[k] = ""
	}
	c.vault = map[string]string{}
}

// ---- keychain backend selection --------------------------------------------

// NewBackend returns the SecretResolver for the current platform. On a platform
// with no backend it returns a backend whose Available always errors, which the
// chain treats as "keychain unavailable" rather than as a failure to construct.
func NewBackend() SecretResolver { return newPlatformBackend() }

// Available reports whether the platform keychain can be used, with a
// BLOCKED-style reason suitable for CI output.
func Available() error { return NewBackend().Available() }

// ContextKeychainResolve is the process entry point used by the headless/MCP
// modes to resolve one variable and map failure to an exit code. It exists so
// the CLI wiring stays in one place (AGENTS.md §4.4 exit codes).
func ContextKeychainResolve(ctx context.Context, chain *Chain, key string) (string, int, error) {
	v, err := chain.Resolve(key)
	if err != nil {
		return "", ExitCode(err), err
	}
	select {
	case <-ctx.Done():
		return "", 1, ctx.Err()
	default:
	}
	return v, 0, nil
}

// SortedKeys returns the env keys in sorted order, for stable diagnostics.
func SortedKeys(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runtimeGOOS is a seam for tests that assert backend selection per platform
// without needing the other OS.
func runtimeGOOS() string { return runtime.GOOS }
