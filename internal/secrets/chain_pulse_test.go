package secrets_test

import (
	"errors"
	"testing"

	"pulse/internal/secrets"
)

// TestEnvPulseTierPrecedence pins the intra-chain order: keychain, then the
// Git-aware context, then the plain .env, then the vault, then a hard error.
func TestEnvPulseTierPrecedence(t *testing.T) {
	backend := fakeBackend{items: map[string]string{"KC": "keychain"}}
	chain := secrets.NewChain(
		secrets.WithBackend(backend),
		secrets.WithEnvPulse(map[string]string{"KC": "pulse", "A": "pulse", "B": "pulse"}),
		secrets.WithEnv(map[string]string{"B": "env", "C": "env"}),
		secrets.WithVaultEnv(map[string]string{"C": "vault", "D": "vault"}),
	)
	cases := map[string]string{
		"KC": "keychain",
		"A":  "pulse",
		"B":  "pulse",
		"C":  "env",
		"D":  "vault",
	}
	for key, want := range cases {
		got, err := chain.Lookup(key)
		if err != nil {
			t.Errorf("Lookup(%q): %v", key, err)
			continue
		}
		if got != want {
			t.Errorf("Lookup(%q) = %q, want %q", key, got, want)
		}
	}
	if _, err := chain.Lookup("MISSING"); !errors.Is(err, secrets.ErrUnresolved) {
		t.Errorf("Lookup(MISSING) = %v, want ErrUnresolved", err)
	}
	sources := chain.Sources()
	if len(sources) < 2 || sources[1] != "env.pulse" {
		t.Errorf("Sources = %v, want env.pulse directly after the keychain", sources)
	}
}

// TestEnvPulseAloneMakesTheChainAvailable pins the honest availability probe:
// a workspace configured only through .env.pulse is usable.
func TestEnvPulseAloneMakesTheChainAvailable(t *testing.T) {
	chain := secrets.NewChain(secrets.WithEnvPulse(map[string]string{"A": "1"}))
	if err := chain.Available(); err != nil {
		t.Errorf("Available with only .env.pulse = %v", err)
	}
}

// TestClearScrubsThePulseTier proves teardown zeroes the branch context just
// like the other in-memory tiers (AGENTS.md §4.3).
func TestClearScrubsThePulseTier(t *testing.T) {
	chain := secrets.NewChain(secrets.WithEnvPulse(map[string]string{"A": "1"}))
	if _, err := chain.Lookup("A"); err != nil {
		t.Fatalf("Lookup before Clear: %v", err)
	}
	chain.Clear()
	if _, err := chain.Lookup("A"); !errors.Is(err, secrets.ErrUnresolved) {
		t.Errorf("Lookup after Clear = %v, want ErrUnresolved", err)
	}
}
