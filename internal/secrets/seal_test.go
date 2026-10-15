package secrets

import (
	"errors"
	"testing"
)

// TestSealInstallsAResolvableRedactedValue pins the in-TUI vault helper's
// contract: Seal lands in the ephemeral tier (below the keychain, above the
// error), resolves immediately, and registers with the redactor before it
// returns — so the response pane can never echo it back raw.
func TestSealInstallsAResolvableRedactedValue(t *testing.T) {
	red := NewRedactor()
	c := NewChain(WithRedactor(red), WithBackend(NewMemoryStore()))
	if err := c.Seal("API_TOKEN", "tok-live-123"); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	v, err := c.Resolve("API_TOKEN")
	if err != nil || v != "tok-live-123" {
		t.Fatalf("Resolve after Seal = %q, %v; want the sealed value", v, err)
	}
	if got := red.String("echo tok-live-123 here"); got != "echo [REDACTED] here" {
		t.Errorf("sealed value not scrubbed: %q", got)
	}
}

// TestSealFailsClosed pins the refusals: bad keys and empty values never
// enter the tier (an empty value would read back as a dummy secret).
func TestSealFailsClosed(t *testing.T) {
	c := NewChain(WithBackend(NewMemoryStore()))
	for _, tc := range []struct{ name, val string }{
		{"", "v"},
		{"has space", "v"},
		{"OK_NAME", ""},
	} {
		if err := c.Seal(tc.name, tc.val); err == nil {
			t.Errorf("Seal(%q, %q) accepted, want an error", tc.name, tc.val)
		}
	}
	if _, err := c.Resolve("OK_NAME"); !errors.Is(err, ErrUnresolved) {
		t.Errorf("rejected seal leaked: Resolve = %v", err)
	}
}

// TestSealRanksBelowTheKeychain pins the order: an explicit seal never
// shadows a keychain-held value — the chain's custody order is not
// renegotiated by convenience.
func TestSealRanksBelowTheKeychain(t *testing.T) {
	mem := NewMemoryStore()
	if err := mem.Store("K", "from-keychain"); err != nil {
		t.Fatal(err)
	}
	c := NewChain(WithBackend(mem))
	if err := c.Seal("K", "from-seal"); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.Resolve("K"); v != "from-keychain" {
		t.Errorf("Resolve = %q, want the keychain value to win", v)
	}
}
