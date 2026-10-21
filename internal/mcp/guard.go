package mcp

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"pulse/internal/engine"
)

// The blast-radius firewall: an agent-facing policy checked before the MCP
// server dispatches a request.
//
// # Why an agent needs one
//
// `pulse mcp` hands an LLM an HTTP client. The model is not malicious, but it
// is confidently wrong: asked to tidy up a staging database, it may send
// DELETE to whatever base URL the collection happens to hold. The guard turns
// that into a readable refusal the model can act on instead of an irreversible
// side effect on a host nobody intended to touch.
//
// # Where the policy lives
//
// `.pulse-guard` is an INI file in the workspace, plain text and Git-trackable
// like every other Pulse source. It is read when the MCP server is constructed,
// not per request: a policy that changes under a running session would make a
// block unreproducible.
//
// # Defaults are the security boundary
//
// A missing file is not "no policy". It is the DEFAULT policy: every method,
// loopback hosts only. An agent that needs a remote host must be given one in
// the file, which is a commit a human reviews.
//
// # Fail closed
//
// A present but malformed file is an error, not a silent fallback to defaults.
// Guessing at a half-typed security policy is how a deny rule quietly stops
// applying; the operator fixes the file or Pulse refuses to serve.
//
// This type implements engine.PolicyGate. The check runs in the engine, after
// interpolation and before dispatch, because that is the only point where the
// real host, query and headers exist (see internal/engine/gate.go).

// GuardFile is the default policy file name, relative to the invocation
// directory.
const GuardFile = ".pulse-guard"

// Rates accepted by `rate_limit`. A bare number is rejected: "10" does not say
// whether the operator meant per second or per hour.
const (
	guardSection        = "mcp"
	guardKeyAllowMethod = "allow_methods"
	guardKeyDenyMethod  = "deny_methods"
	guardKeyAllowHost   = "allow_hosts"
	guardKeyDenyHost    = "deny_hosts"
	guardKeyRateLimit   = "rate_limit"
)

// RateLimit is one admission budget: Requests per Window.
type RateLimit struct {
	// Requests is the maximum admitted requests in Window. Zero disables the
	// limit.
	Requests int
	// Window is the sliding window length.
	Window time.Duration
}

// String renders the limit in the same form the file uses (e.g. "10/minute").
func (r RateLimit) String() string {
	if r.Requests <= 0 || r.Window <= 0 {
		return "unlimited"
	}
	unit := "second"
	switch {
	case r.Window == time.Minute:
		unit = "minute"
	case r.Window == time.Hour:
		unit = "hour"
	case r.Window == time.Second:
		unit = "second"
	default:
		return fmt.Sprintf("%d/%s", r.Requests, r.Window)
	}
	return fmt.Sprintf("%d/%s", r.Requests, unit)
}

// GuardPolicy is a parsed `.pulse-guard`.
//
// Empty slices mean "no rule of this kind". The one exception is the default
// policy, which populates AllowHosts so a missing file is restrictive rather
// than permissive.
type GuardPolicy struct {
	// AllowMethods, when non-empty, is the complete set of permitted methods.
	AllowMethods []string
	// DenyMethods is checked first and always wins.
	DenyMethods []string
	// AllowHosts, when non-empty, is the complete set of permitted hosts.
	AllowHosts []string
	// DenyHosts is checked first and always wins.
	DenyHosts []string
	// RateLimit bounds admitted requests. Zero means unlimited.
	RateLimit RateLimit
	// Path is the file the policy was read from, or "" for the default.
	Path string
}

// DefaultGuardPolicy is the policy in force when no file exists: every method,
// loopback hosts only, no rate limit.
//
// Loopback includes the hostname `localhost` and both IP literals, because a
// local dev server is reached by any of the three. A differently spelled
// loopback address (an IPv4-mapped IPv6 literal, for example) is refused: the
// default errs toward blocking, and the message names the file and key that
// permit it.
func DefaultGuardPolicy() *GuardPolicy {
	return &GuardPolicy{AllowHosts: []string{"localhost", "127.0.0.1", "::1"}}
}

// IsDefault reports whether this policy came from the missing-file baseline.
func (p *GuardPolicy) IsDefault() bool { return p == nil || p.Path == "" }

// label names the policy in agent-facing messages.
func (p *GuardPolicy) label() string {
	if p.IsDefault() {
		return "the default .pulse-guard policy"
	}
	return ".pulse-guard"
}

// LoadGuard reads a policy file. A missing file yields [DefaultGuardPolicy] and
// no error; every other failure is returned.
func LoadGuard(path string) (*GuardPolicy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultGuardPolicy(), nil
		}
		return nil, fmt.Errorf("mcp: read %s: %w", path, err)
	}
	return ParseGuard(raw, path)
}

// ParseGuard parses the INI surface. It is total: every malformed line is an
// error carrying its line number, and an unknown section or key is refused
// rather than ignored (a misspelled `allow_host` would otherwise read as "no
// restriction").
func ParseGuard(raw []byte, path string) (*GuardPolicy, error) {
	p := &GuardPolicy{Path: path}
	sawSection := false
	seen := map[string]bool{}

	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	for i, line := range lines {
		num := i + 1
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, ";") {
			continue
		}
		if strings.HasPrefix(text, "[") {
			if !strings.HasSuffix(text, "]") {
				return nil, fmt.Errorf("mcp: %s:%d: section header is not closed", path, num)
			}
			name := strings.TrimSpace(text[1 : len(text)-1])
			if name != guardSection {
				return nil, fmt.Errorf("mcp: %s:%d: unknown section %q (only [%s] is defined)",
					path, num, name, guardSection)
			}
			sawSection = true
			continue
		}
		eq := strings.IndexByte(text, '=')
		if eq < 0 {
			return nil, fmt.Errorf("mcp: %s:%d: expected key = value", path, num)
		}
		key := strings.TrimSpace(text[:eq])
		val := strings.TrimSpace(text[eq+1:])
		if !sawSection {
			return nil, fmt.Errorf("mcp: %s:%d: %q appears before the [%s] section",
				path, num, key, guardSection)
		}
		if seen[key] {
			return nil, fmt.Errorf("mcp: %s:%d: %q is declared twice", path, num, key)
		}
		seen[key] = true
		switch key {
		case guardKeyAllowMethod:
			p.AllowMethods = parseMethods(val)
		case guardKeyDenyMethod:
			p.DenyMethods = parseMethods(val)
		case guardKeyAllowHost:
			hosts, err := parseHosts(val)
			if err != nil {
				return nil, fmt.Errorf("mcp: %s:%d: %w", path, num, err)
			}
			p.AllowHosts = hosts
		case guardKeyDenyHost:
			hosts, err := parseHosts(val)
			if err != nil {
				return nil, fmt.Errorf("mcp: %s:%d: %w", path, num, err)
			}
			p.DenyHosts = hosts
		case guardKeyRateLimit:
			rate, err := parseRateLimit(val)
			if err != nil {
				return nil, fmt.Errorf("mcp: %s:%d: %w", path, num, err)
			}
			p.RateLimit = rate
		default:
			return nil, fmt.Errorf("mcp: %s:%d: unknown key %q (known: %s, %s, %s, %s, %s)",
				path, num, key, guardKeyAllowMethod, guardKeyDenyMethod,
				guardKeyAllowHost, guardKeyDenyHost, guardKeyRateLimit)
		}
	}
	if !sawSection {
		return nil, fmt.Errorf("mcp: %s: no [%s] section", path, guardSection)
	}
	return p, nil
}

func parseMethods(val string) []string {
	var out []string
	for _, part := range strings.Split(val, ",") {
		m := strings.ToUpper(strings.TrimSpace(part))
		if m == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

func parseHosts(val string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(val, ",") {
		h := strings.TrimSpace(part)
		if h == "" {
			continue
		}
		if strings.ContainsAny(h, "/ ") {
			return nil, fmt.Errorf("host pattern %q is not a host name (no scheme or path)", h)
		}
		out = append(out, normalizeHostPattern(h))
	}
	return out, nil
}

func parseRateLimit(val string) (RateLimit, error) {
	if strings.TrimSpace(val) == "" {
		return RateLimit{}, nil
	}
	parts := strings.SplitN(val, "/", 2)
	if len(parts) != 2 {
		return RateLimit{}, fmt.Errorf("rate_limit %q must be <count>/<second|minute|hour>", val)
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || n <= 0 {
		return RateLimit{}, fmt.Errorf("rate_limit %q has no positive count", val)
	}
	var window time.Duration
	switch strings.ToLower(strings.TrimSpace(parts[1])) {
	case "second", "seconds":
		window = time.Second
	case "minute", "minutes":
		window = time.Minute
	case "hour", "hours":
		window = time.Hour
	default:
		return RateLimit{}, fmt.Errorf("rate_limit %q has an unknown unit (use second, minute or hour)", val)
	}
	return RateLimit{Requests: n, Window: window}, nil
}

// normalizeHostPattern lowercases a pattern and strips a port or IPv6 brackets,
// so `localhost:8080` matches the host `localhost` and `[::1]` matches `::1`.
func normalizeHostPattern(p string) string {
	p = strings.TrimSpace(strings.ToLower(p))
	if strings.HasPrefix(p, "[") {
		if i := strings.IndexByte(p, ']'); i > 0 {
			return p[1:i]
		}
	}
	if h, _, err := net.SplitHostPort(p); err == nil {
		return h
	}
	return p
}

// matchHostPattern matches one pattern against a host.
//
// `*` alone permits every host (an explicit opt-in), `*.example.com` matches
// subdomains but not the apex, and an exact pattern is case-insensitive.
func matchHostPattern(pattern, host string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	return pattern == host
}

// Guard is the stateful gate: the immutable policy plus the in-memory rate
// window. One Guard per server, shared by concurrent tool calls.
type Guard struct {
	policy       *GuardPolicy
	allowMethods map[string]bool
	denyMethods  map[string]bool
	allowHosts   []string
	denyHosts    []string

	mu   sync.Mutex
	hits []time.Time
	now  func() time.Time
}

// NewGuard compiles a policy into a gate. A nil policy selects the default, so
// a caller that forgot to load one still gets the restrictive baseline.
func NewGuard(p *GuardPolicy) *Guard {
	if p == nil {
		p = DefaultGuardPolicy()
	}
	g := &Guard{
		policy:       p,
		allowMethods: make(map[string]bool, len(p.AllowMethods)),
		denyMethods:  make(map[string]bool, len(p.DenyMethods)),
		now:          time.Now,
	}
	for _, m := range p.AllowMethods {
		g.allowMethods[strings.ToUpper(m)] = true
	}
	for _, m := range p.DenyMethods {
		g.denyMethods[strings.ToUpper(m)] = true
	}
	// Host patterns are normalized here as well as in ParseGuard: a policy may
	// be built programmatically (a test, a future config source), and an
	// un-normalized `[::1]` or `localhost:8080` would silently never match.
	for _, h := range p.AllowHosts {
		g.allowHosts = append(g.allowHosts, normalizeHostPattern(h))
	}
	for _, h := range p.DenyHosts {
		g.denyHosts = append(g.denyHosts, normalizeHostPattern(h))
	}
	return g
}

// Policy returns the compiled policy, for diagnostics and tests.
func (g *Guard) Policy() *GuardPolicy { return g.policy }

// Check implements engine.PolicyGate.
func (g *Guard) Check(req engine.GateRequest) error {
	if g == nil || g.policy == nil {
		return nil
	}
	p := g.policy
	method := strings.ToUpper(strings.TrimSpace(req.Method))

	if g.denyMethods[method] {
		return engine.NewPolicyBlockedError(fmt.Sprintf(
			"Method %s is explicitly denied by %s.", method, p.label()))
	}
	if len(g.allowMethods) > 0 && !g.allowMethods[method] {
		return engine.NewPolicyBlockedError(fmt.Sprintf(
			"Method %s is not in allow_methods in %s.", method, p.label()))
	}
	if req.URL == nil {
		return engine.NewPolicyBlockedError(
			"the request target could not be parsed, so no host policy can be evaluated.")
	}
	host := strings.ToLower(req.URL.Hostname())
	if host == "" {
		return engine.NewPolicyBlockedError(
			"the request target has no host, so no host policy can be evaluated.")
	}
	for _, pattern := range g.denyHosts {
		if matchHostPattern(pattern, host) {
			return engine.NewPolicyBlockedError(fmt.Sprintf(
				"Host %s is denied by deny_hosts in %s.", host, p.label()))
		}
	}
	if len(g.allowHosts) > 0 {
		allowed := false
		for _, pattern := range g.allowHosts {
			if matchHostPattern(pattern, host) {
				allowed = true
				break
			}
		}
		if !allowed {
			if p.IsDefault() {
				return engine.NewPolicyBlockedError(fmt.Sprintf(
					"Host %s is not permitted: no %s exists, so the default policy allows "+
						"loopback hosts only. Add allow_hosts = %s to %s to permit it.",
					host, GuardFile, host, GuardFile))
			}
			return engine.NewPolicyBlockedError(fmt.Sprintf(
				"Host %s is not in allow_hosts in %s.", host, p.label()))
		}
	}
	if err := g.admit(); err != nil {
		return err
	}
	return nil
}

// admit applies the sliding-window rate limit. It is called only for a request
// the method and host rules already allowed, so a denied request never consumes
// the agent's budget.
func (g *Guard) admit() error {
	limit := g.policy.RateLimit
	if limit.Requests <= 0 || limit.Window <= 0 {
		return nil
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()

	cutoff := now.Add(-limit.Window)
	kept := g.hits[:0]
	for _, t := range g.hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	g.hits = kept
	if len(g.hits) >= limit.Requests {
		return engine.NewPolicyBlockedError(fmt.Sprintf(
			"Rate limit of %s is exceeded; no request was sent. Wait before retrying.", limit))
	}
	g.hits = append(g.hits, now)
	return nil
}
