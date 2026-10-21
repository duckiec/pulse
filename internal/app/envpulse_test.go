package app_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pulse/internal/app"
	"pulse/internal/env"
	"pulse/internal/secrets"
)

// gitRepo creates a real repository at a known branch. A fake branch would not
// prove the git interop the feature is built on.
func gitRepo(t *testing.T, branch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("SKIP (reason: git is not installed on this host)")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=pulse-test", "GIT_AUTHOR_EMAIL=pulse@example.invalid",
			"GIT_COMMITTER_NAME=pulse-test", "GIT_COMMITTER_EMAIL=pulse@example.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("checkout", "-q", "-b", branch)
	run("commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

const pulseFile = `[default]
BASE_URL = http://localhost:8080
ONLY_DEFAULT = yes

[branch:feature/snapshots]
BASE_URL = https://preview.internal.net
`

// newPulseApp boots an App with the branch-aware layer pointed at file and a
// hermetic keychain. EnvPulseFile is explicit because the production default
// is resolved by cmd/pulse, not by Config.
func newPulseApp(t *testing.T, mutate func(*app.Config)) *app.App {
	t.Helper()
	cfg := app.Config{
		Home:              t.TempDir(),
		SkipKeychainProbe: true,
		SkipSync:          true,
		Backend:           secrets.NewMemoryStore(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := app.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return a
}

// TestBranchBlockWinsInTheChain is the feature's end-to-end proof inside the
// composition root: the branch block is selected, the default fills the keys
// the block omits, and {{GIT_BRANCH}} resolves.
func TestBranchBlockWinsInTheChain(t *testing.T) {
	repo := gitRepo(t, "feature/snapshots")
	path := filepath.Join(repo, env.FileName)
	if err := os.WriteFile(path, []byte(pulseFile), 0o600); err != nil {
		t.Fatal(err)
	}

	a := newPulseApp(t, func(c *app.Config) { c.EnvPulseFile = path })
	cases := map[string]string{
		"BASE_URL":     "https://preview.internal.net",
		"ONLY_DEFAULT": "yes",
		"GIT_BRANCH":   "feature/snapshots",
	}
	for key, want := range cases {
		got, err := a.Chain.Lookup(key)
		if err != nil {
			t.Errorf("Lookup(%q): %v", key, err)
			continue
		}
		if got != want {
			t.Errorf("Lookup(%q) = %q, want %q", key, got, want)
		}
	}
	if sources := a.Chain.Sources(); !contains(sources, "env.pulse") {
		t.Errorf("chain sources = %v, want env.pulse", sources)
	}
}

// TestKeychainStillWinsOverBranchEnv pins the documented precedence: a secret
// overrides configuration, never the other way round.
func TestKeychainStillWinsOverBranchEnv(t *testing.T) {
	repo := gitRepo(t, "feature/snapshots")
	path := filepath.Join(repo, env.FileName)
	if err := os.WriteFile(path, []byte(pulseFile), 0o600); err != nil {
		t.Fatal(err)
	}
	store := secrets.NewMemoryStore()
	if err := store.Store("BASE_URL", "https://from-keychain"); err != nil {
		t.Fatal(err)
	}
	a := newPulseApp(t, func(c *app.Config) {
		c.EnvPulseFile = path
		c.Backend = store
	})
	if got, err := a.Chain.Lookup("BASE_URL"); err != nil || got != "https://from-keychain" {
		t.Fatalf("Lookup(BASE_URL) = %q, %v; want the keychain value", got, err)
	}
}

// TestEnvPulseOutranksPlainEnv pins the intra-tier rule: the branch-specific
// file is more specific than the developer's .env.
func TestEnvPulseOutranksPlainEnv(t *testing.T) {
	repo := gitRepo(t, "feature/snapshots")
	pulsePath := filepath.Join(repo, env.FileName)
	if err := os.WriteFile(pulsePath, []byte(pulseFile), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("BASE_URL=http://from-dotenv\nONLY_ENV=e\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newPulseApp(t, func(c *app.Config) {
		c.EnvPulseFile = pulsePath
		c.EnvFile = envPath
	})
	if got, err := a.Chain.Lookup("BASE_URL"); err != nil || got != "https://preview.internal.net" {
		t.Fatalf("Lookup(BASE_URL) = %q, %v; want the .env.pulse branch value", got, err)
	}
	if got, err := a.Chain.Lookup("ONLY_ENV"); err != nil || got != "e" {
		t.Fatalf("Lookup(ONLY_ENV) = %q, %v; want the plain .env", got, err)
	}
}

// TestOutsideGitFallsBackToDefaultAndOmitsGITBRANCH pins both halves of "not
// a repository": [default] still applies, and the reserved variable is ABSENT
// so a request that needs it fails closed instead of sending an empty host.
func TestOutsideGitFallsBackToDefaultAndOmitsGITBRANCH(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, env.FileName)
	body := `[default]
BASE_URL = http://localhost:8080
PREVIEW = https://x/{{GIT_BRANCH}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newPulseApp(t, func(c *app.Config) { c.EnvPulseFile = path })
	if got, err := a.Chain.Lookup("BASE_URL"); err != nil || got != "http://localhost:8080" {
		t.Fatalf("Lookup(BASE_URL) = %q, %v", got, err)
	}
	if _, err := a.Chain.Lookup("GIT_BRANCH"); !errors.Is(err, secrets.ErrUnresolved) {
		t.Errorf("GIT_BRANCH resolved outside a repository: %v", err)
	}
	if _, err := a.Chain.Lookup("PREVIEW"); !errors.Is(err, secrets.ErrUnresolved) {
		t.Errorf("a GIT_BRANCH-dependent value resolved outside a repository: %v", err)
	}
}

// TestMalformedEnvPulseDegradesButKeepsServing pins fail-open boot with a
// loud report: a typo must not brick an API client, and it must not vanish.
func TestMalformedEnvPulseDegradesButKeepsServing(t *testing.T) {
	repo := gitRepo(t, "feature/snapshots")
	path := filepath.Join(repo, env.FileName)
	if err := os.WriteFile(path, []byte("[production]\nBASE_URL = x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newPulseApp(t, func(c *app.Config) { c.EnvPulseFile = path })
	found := false
	for _, d := range a.Degradations {
		if strings.Contains(d, ".env.pulse") {
			found = true
		}
	}
	if !found {
		t.Errorf("Degradations = %v, want one naming .env.pulse", a.Degradations)
	}
	// The reserved variable is still injected: it depends on git, not on the
	// file that failed to parse.
	if got, err := a.Chain.Lookup("GIT_BRANCH"); err != nil || got != "feature/snapshots" {
		t.Errorf("GIT_BRANCH = %q, %v; want the branch", got, err)
	}
	if _, err := a.Chain.Lookup("BASE_URL"); !errors.Is(err, secrets.ErrUnresolved) {
		t.Errorf("BASE_URL resolved from a malformed file: %v", err)
	}
}

// TestDisabledEnvPulseReadsNoGit proves the test-facing switch: Config{} (no
// EnvPulseFile) never spawns git, so suites stay hermetic.
func TestDisabledEnvPulseReadsNoGit(t *testing.T) {
	a := newPulseApp(t, nil)
	if _, err := a.Chain.Lookup("GIT_BRANCH"); !errors.Is(err, secrets.ErrUnresolved) {
		t.Errorf("GIT_BRANCH resolved with the feature disabled: %v", err)
	}
	if sources := a.Chain.Sources(); contains(sources, "env.pulse") {
		t.Errorf("chain sources = %v, want no env.pulse", sources)
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
