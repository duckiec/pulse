package main

import (
	"context"
	"strings"
	"testing"
)

// TestHelpEverywhere pins the help contract: every entry point answers an
// explicit help request on stdout with exit 0, and never boots a store,
// keychain, or network path to do it.
func TestHelpEverywhere(t *testing.T) {
	cases := []struct {
		name string
		args []string
		// want is a substring that must appear on stdout.
		want string
	}{
		{"top help", []string{"help"}, "usage:"},
		{"top help long flag", []string{"--help"}, "usage:"},
		{"top help short flag", []string{"-h"}, "usage:"},
		{"help mcp", []string{"help", "mcp"}, "pulse mcp"},
		{"mcp help", []string{"mcp", "help"}, "pulse mcp"},
		{"mcp long flag", []string{"mcp", "--help"}, "pulse mcp"},
		{"help mock", []string{"help", "mock"}, "pulse mock <file-or-dir>"},
		{"mock help", []string{"mock", "help"}, "pulse mock <file-or-dir>"},
		{"mock long flag", []string{"mock", "--help"}, "pulse mock <file-or-dir>"},
		{"help record", []string{"help", "record"}, "pulse record <listen>"},
		{"record help", []string{"record", "help"}, "pulse record <listen>"},
		{"record long flag", []string{"record", "--help"}, "pulse record <listen>"},
		{"help test", []string{"help", "test"}, "pulse test <dir>"},
		{"test help", []string{"test", "help"}, "pulse test <dir>"},
		{"test long flag", []string{"test", "--help"}, "pulse test <dir>"},
		{"help vault", []string{"help", "vault"}, "pulse vault <command>"},
		{"vault help", []string{"vault", "help"}, "pulse vault <command>"},
		{"vault long flag", []string{"vault", "--help"}, "pulse vault <command>"},
		{"help vault set", []string{"help", "vault", "set"}, "pulse vault set <KEY>"},
		{"vault set help", []string{"vault", "set", "help"}, "pulse vault set <KEY>"},
		{"vault set long flag", []string{"vault", "set", "--help"}, "pulse vault set <KEY>"},
		{"vault list help", []string{"vault", "list", "help"}, "pulse vault list"},
		{"vault unset help", []string{"vault", "unset", "help"}, "pulse vault unset <KEY>"},
		{"vault init help", []string{"vault", "init", "help"}, "pulse vault init"},
		{"help init", []string{"help", "init"}, "pulse init [file]"},
		{"init help", []string{"init", "help"}, "pulse init [file]"},
		{"init long flag", []string{"init", "--help"}, "pulse init [file]"},
		{"help doctor", []string{"help", "doctor"}, "pulse doctor"},
		{"doctor help", []string{"doctor", "help"}, "pulse doctor"},
		{"doctor long flag", []string{"doctor", "--help"}, "pulse doctor"},
		{"help sync", []string{"help", "sync"}, "pulse sync serve"},
		{"sync help", []string{"sync", "help"}, "pulse sync serve"},
		{"sync long flag", []string{"sync", "--help"}, "pulse sync serve"},
		{"sync serve help", []string{"sync", "serve", "help"}, "pulse sync serve <dir>"},
		{"sync status help", []string{"sync", "status", "help"}, "pulse sync status"},
		{"help sync serve", []string{"help", "sync", "serve"}, "pulse sync serve <dir>"},
		// Edge-case audit: stacked help tokens must still be help,
		// never an unknown-topic/subcommand refusal.
		{"help --help", []string{"help", "--help"}, "usage:"},
		{"help -h", []string{"help", "-h"}, "usage:"},
		{"vault help --help", []string{"vault", "help", "--help"}, "pulse vault <command>"},
		{"help vault --help", []string{"help", "vault", "--help"}, "pulse vault <command>"},
		{"help sync --help", []string{"help", "sync", "--help"}, "pulse sync serve"},
		{"sync --help with trailing junk", []string{"sync", "--help", "extra"}, "pulse sync serve"},
		{"sync help with trailing junk", []string{"sync", "help", "extra"}, "pulse sync serve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			got := run(context.Background(), tc.args, strings.NewReader(""), &stdout, &stderr, false)
			if got != exitOK {
				t.Fatalf("run(%q) = %d, want %d (stderr: %s)", tc.args, got, exitOK, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Fatalf("run(%q) stdout missing %q:\n%s", tc.args, tc.want, stdout.String())
			}
		})
	}
}

// TestHelpUnknownTopicFailsClosed pins the negative: an unknown help topic
// is a usage error on stderr, never a silent top-help print with exit 0.
func TestHelpUnknownTopicFailsClosed(t *testing.T) {
	var stdout, stderr strings.Builder
	got := run(context.Background(), []string{"help", "bogus"}, strings.NewReader(""), &stdout, &stderr, false)
	if got != exitUsage {
		t.Fatalf("run(help bogus) = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(stderr.String(), "unknown topic") {
		t.Fatalf("stderr missing unknown-topic refusal: %q", stderr.String())
	}
}

// TestSyncServeHelpFlagNeverServes pins the audit's sharpest edge: a help
// token anywhere after `serve` is a help request. Without the scan,
// `pulse sync serve <dir> --help` ignored the flag and SERVED the
// directory — a help request must never start a network server. (If this
// regresses, the test hangs in serve instead of returning, so a pass is
// itself the proof.)
func TestSyncServeHelpFlagNeverServes(t *testing.T) {
	var stdout, stderr strings.Builder
	got := run(context.Background(),
		[]string{"sync", "serve", t.TempDir(), "--help"},
		strings.NewReader(""), &stdout, &stderr, false)
	if got != exitOK {
		t.Fatalf("run(sync serve <dir> --help) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pulse sync serve <dir>") {
		t.Fatalf("stdout missing serve help:\n%s", stdout.String())
	}
}

// TestVaultSetHelpKeyEscapeHatch pins the documented ambiguity rule: a key
// literally named `help` is still settable inline with a value, while the
// bare form shows help. This runs against a temp HOME so no keychain or
// real vault is touched... except VaultSet still opens the store, so we
// only assert the help half here and the routing half (two args starting
// with `help` does NOT print help).
func TestVaultSetHelpRoutesToHelpWhenBare(t *testing.T) {
	var stdout, stderr strings.Builder
	got := run(context.Background(), []string{"vault", "set", "help"}, strings.NewReader(""), &stdout, &stderr, false)
	if got != exitOK {
		t.Fatalf("run(vault set help) = %d, want %d (stderr: %s)", got, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pulse vault set <KEY>") {
		t.Fatalf("stdout missing vault set help:\n%s", stdout.String())
	}
}
