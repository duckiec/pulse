package main

import (
	"context"
	"strings"
	"testing"
)

// TestMockRequiresFromCacheAndAPath pins the fail-closed surface of the new
// commands: a missing mode flag or target must be a usage error BEFORE any
// store, listener or proxy is constructed.
func TestMockRequiresFromCacheAndAPath(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no path", []string{"mock", "--from-cache"}, "exactly one .http file or directory"},
		{"two paths", []string{"mock", "a.http", "b.http", "--from-cache"}, "exactly one .http file or directory"},
		{"missing mode", []string{"mock", "."}, "--from-cache is required"},
		{"bad port", []string{"mock", ".", "--from-cache", "--port", "70000"}, "not a valid TCP port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			got := run(context.Background(), tc.args, strings.NewReader(""), &stdout, &stderr, false)
			if got != exitUsage {
				t.Fatalf("run(%q) = %d, want %d\nstdout: %s\nstderr: %s",
					tc.args, got, exitUsage, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr missing %q: %s", tc.want, stderr.String())
			}
		})
	}
}

func TestRecordRequiresTargetAndListen(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no listen", []string{"record", "--target", "http://localhost:8080"}, "exactly one listen address"},
		{"no target", []string{"record", ":9000"}, "--target <url> is required"},
		{"bad target", []string{"record", ":9000", "--target", "localhost:8080"}, "must be an http:// or https:// URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			got := run(context.Background(), tc.args, strings.NewReader(""), &stdout, &stderr, false)
			if got != exitUsage {
				t.Fatalf("run(%q) = %d, want %d\nstdout: %s\nstderr: %s",
					tc.args, got, exitUsage, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr missing %q: %s", tc.want, stderr.String())
			}
		})
	}
}
