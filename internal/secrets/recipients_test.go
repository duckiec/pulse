package secrets_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pulse/internal/secrets"
)

func testPub(t *testing.T) string {
	t.Helper()
	id, err := secrets.GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}
	p, err := id.X25519Pub()
	if err != nil {
		t.Fatalf("X25519Pub: %v", err)
	}
	return secrets.EncodePub(secrets.IdentityPubPrefix, p)
}

// TestParseRecipientsReadsKeysIgnoresCommentsAndOrdersSlots pins the file
// contract: comments and blanks vanish, keys parse in order (slot order is
// sealed bytes — a parser that reordered would fork every seal).
func TestParseRecipientsReadsKeysIgnoresCommentsAndOrdersSlots(t *testing.T) {
	k1, k2 := testPub(t), testPub(t)
	p := filepath.Join(t.TempDir(), "pulse.recipients")
	body := "# team vault recipients\n\n" + k1 + "\n   \n# ops oncall\n" + k2 + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := secrets.ParseRecipients(p)
	if err != nil {
		t.Fatalf("ParseRecipients: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d recipients, want 2", len(got))
	}
	for i, k := range []string{k1, k2} {
		if secrets.EncodePub(secrets.IdentityPubPrefix, got[i]) != k {
			t.Errorf("slot %d is not the file's line %d; order must be stable", i, i)
		}
	}
}

// TestParseRecipientsFailsClosed covers every malformed shape: missing file,
// empty file, comment-only file, a signing key in a decrypt slot, garbage,
// and a duplicate (merge artifact or editing mistake — refused either way).
func TestParseRecipientsFailsClosed(t *testing.T) {
	if _, err := secrets.ParseRecipients(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing recipients file parsed without error")
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"comments only", "# nobody yet\n# really\n"},
		{"signing key in a decrypt slot", strings.Replace(testPub(t), "pulse-pub-", "pulse-sig-", 1) + "\n"},
		{"garbage", "not-a-key\n"},
		{"duplicate", testPub(t) + "\n" + testPub(t) + "\n"},
	} {
		// The duplicate case needs the SAME key twice; testPub makes two.
		body := tc.body
		if tc.name == "duplicate" {
			k := testPub(t)
			body = k + "\n" + k + "\n"
		}
		p := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_"))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := secrets.ParseRecipients(p); err == nil {
			t.Errorf("%s: parsed without error", tc.name)
		}
	}
}

// TestAddRecipientCreatesAppendsAndIgnoresRepeats proves the CLI primitive:
// create-with-header, append, and idempotent re-add (file untouched —
// byte-identical — so setup scripts converge instead of growing).
func TestAddRecipientCreatesAppendsAndIgnoresRepeats(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pulse.recipients")
	k1, k2 := testPub(t), testPub(t)

	if err := secrets.AddRecipient(p, k1); err != nil {
		t.Fatalf("create: %v", err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(raw), "#") || !strings.Contains(string(raw), k1) {
		t.Errorf("created file carries no header or key:\n%s", raw)
	}
	before := string(raw)

	if err := secrets.AddRecipient(p, k1); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if after, _ := os.ReadFile(p); string(after) != before {
		t.Error("re-adding a listed key rewrote the file; the op is not idempotent")
	}
	if err := secrets.AddRecipient(p, k2); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := secrets.ParseRecipients(p)
	if err != nil || len(got) != 2 {
		t.Fatalf("after append: %d recipients, %v", len(got), err)
	}
	if err := secrets.AddRecipient(p, "garbage"); err == nil {
		t.Error("a malformed key was appended")
	}
	if err := secrets.AddRecipient(p, strings.Replace(k1, "pulse-pub-", "pulse-sig-", 1)); err == nil {
		t.Error("a signing key was appended to a decrypt list")
	}
}
