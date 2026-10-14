package secrets

// Hermetic OS-boundary tests (Issue #9, Part 3).
//
// These tests prove the command-construction and wire-framing logic for all
// three keychain adapters WITHOUT a Mac, a D-Bus daemon, or Credential
// Manager: every assertion runs against the platform-independent constructors
// in cmdbuilder.go, dbusframe.go, and credencode.go, which the tagged
// wrappers call verbatim. They run headless on any host — including this
// Linux CI — and the coverage gate pins the seam files at 100% statements.
//
// What is NOT covered here, by design: the last-mile syscalls themselves
// (cmd.Run, bus dial, CredWriteW), which need the native service. Those lines
// are BLOCKED-by-environment, reported as such, and number under ten per
// platform; every DECISION around them is tested below.

import (
	"bytes"
	"strings"
	"testing"
)

// --- Darwin: the secret must never reach argv --------------------------------

// TestDarwinWriteArgvCarriesNoSecret pins the stdin mitigation: the write
// argv holds service, account, and flags — and no element contains, equals,
// or trails the secret. A regression that re-adds `-w <value>` fails here,
// not on a shared workstation via `ps`.
func TestDarwinWriteArgvCarriesNoSecret(t *testing.T) {
	const secret = "s3cr3t-vault-key-9f2c"
	args := darwinAddArgs("pulse", "vault:default")
	joined := "\x00" + strings.Join(args, "\x00") + "\x00"
	if strings.Contains(joined, "\x00"+secret+"\x00") || strings.Contains(strings.Join(args, " "), secret) {
		t.Errorf("write argv contains the secret: %q", args)
	}
	// `-w` MUST be trailing with no password argument: a flag in the value
	// position would be swallowed as the password and hang on a prompt.
	if last := args[len(args)-1]; last != "-w" {
		t.Errorf("write argv does not end in bare -w (stdin sentinel): ends %q in %q", last, args)
	}
	// The update-in-place flag survives the mitigation: `vault init` stays
	// idempotent.
	if !containsArg(args, "-U") {
		t.Errorf("write argv lost -U (update-in-place): %q", args)
	}
	var stderr bytes.Buffer
	cmd := newDarwinAddCmd("pulse", "vault:default", strings.NewReader(secret), &stderr)
	if cmd.Path != darwinSecurityPath {
		t.Errorf("write path = %s, want the pinned system binary %s", cmd.Path, darwinSecurityPath)
	}
	if cmd.Stdin == nil {
		t.Error("write command has no stdin: the secret has nowhere to travel but argv")
	}
	if darwinArgsCarrySecret(cmd, secret) {
		t.Errorf("constructed write command carries the secret in argv: %q", cmd.Args)
	}
	// The negative control: the detector actually fires when a secret IS in
	// argv, so the assertion above is not vacuous.
	leaky := newDarwinAddCmd("pulse", "vault:default", nil, &stderr)
	leaky.Args = append(leaky.Args, secret)
	if !darwinArgsCarrySecret(leaky, secret) {
		t.Error("detector failed on a deliberately leaky argv: the gate proves nothing")
	}
	if darwinArgsCarrySecret(cmd, "") {
		t.Error("detector fires on an empty secret: it would false-positive every run")
	}
}

// TestDarwinReadArgvShape pins the read invocation: password on stdout via
// `-w`, account as a lookup key (not a secret), absolute binary.
func TestDarwinReadArgvShape(t *testing.T) {
	args := darwinFindArgs("pulse", "vault:default")
	want := []string{"find-generic-password", "-s", "pulse", "-a", "vault:default", "-w"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("read argv = %q, want %q", args, want)
	}
	var stdout, stderr bytes.Buffer
	cmd := newDarwinFindCmd("pulse", "vault:default", &stdout, &stderr)
	if cmd.Path != darwinSecurityPath {
		t.Errorf("read path = %s, want %s", cmd.Path, darwinSecurityPath)
	}
	if cmd.Stdin != nil {
		t.Error("read command sets stdin: find-generic-password takes no input")
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// --- Linux D-Bus tests live in dbusframe_test.go (linux-tagged: godbus's
// own darwin/windows files do not compile under Go 1.27, so anything
// importing it stays out of those builds). ---

// --- Windows: credential encoding --------------------------------------------

// TestCredTargetNamespace pins the "pulse/" namespacing: a read looks up
// exactly what the write stored.
func TestCredTargetNamespace(t *testing.T) {
	if got := credTarget("vault:default"); got != "pulse/vault:default" {
		t.Errorf("credTarget = %q, want pulse/vault:default", got)
	}
}

// TestCredBlobRoundTrip proves the encode side and the decode side agree
// without Credential Manager: every value — empty, ASCII, multi-byte
// Unicode — survives encode→bytes→decode verbatim.
func TestCredBlobRoundTrip(t *testing.T) {
	for _, value := range []string{"s3cr3t-vault-key", "", "pässwörd-🔑-42", "a"} {
		units, size, err := encodeCredBlob(value)
		if err != nil {
			t.Fatalf("encode %q: %v", value, err)
		}
		// Size counts BYTES, not units: the exact arithmetic CredWriteW needs.
		if size != uint32(len(units)*2) {
			t.Errorf("encode %q: size %d != 2*units %d", value, size, len(units))
		}
		// NUL termination, mirroring windows.UTF16FromString.
		if len(units) == 0 || units[len(units)-1] != 0 {
			t.Errorf("encode %q: blob is not NUL-terminated", value)
		}
		blob := make([]byte, 0, size)
		for _, u := range units {
			blob = append(blob, byte(u), byte(u>>8))
		}
		if value == "" {
			// An empty credential encodes to a lone terminator, which the
			// read side decodes as empty and rejects — matching advapi32's
			// own empty-blob path. Assert the encoding, not the rejection.
			if len(blob) != 2 {
				t.Errorf("encode empty: %d bytes, want 2 (bare NUL)", len(blob))
			}
			continue
		}
		back, err := decodeCredBlob(blob)
		if err != nil {
			t.Fatalf("decode %q: %v", value, err)
		}
		if back != value {
			t.Errorf("round trip %q -> %q", value, back)
		}
	}
}

// TestCredBlobRejectsMalformed pins the fail-closed decodes: odd-length
// blobs, empty blobs, and NUL-only blobs are errors (never guesses), and a
// NUL inside a value is rejected at encode time — matching
// windows.UTF16FromString, which the write side previously used directly.
func TestCredBlobRejectsMalformed(t *testing.T) {
	for _, blob := range [][]byte{nil, {}, {0x41}, {0x41, 0x00, 0x42}} {
		if s, err := decodeCredBlob(blob); err == nil {
			t.Errorf("decode %v = %q, want a malformed-length error", blob, s)
		}
	}
	if s, err := decodeCredBlob([]byte{0x00, 0x00}); err == nil {
		t.Errorf("decode bare NUL = %q, want an empty error", s)
	}
	if _, _, err := encodeCredBlob("ab\x00cd"); err == nil {
		t.Error("encode NUL-containing value succeeded, want rejection")
	}
}
