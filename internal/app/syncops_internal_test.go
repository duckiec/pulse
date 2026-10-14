package app

// White-box gates for the publish intake (syncops.go): publishDirToStore
// is unexported composition logic, and its load-bearing property —
// steady state costs zero stamps — is only observable before export.
// A publish that re-stamps unchanged requests churns the digest forever
// and anti-entropy never quiesces, so the digest-equality assertion IS
// the gate, not a proxy for it.

import (
	"os"
	"path/filepath"
	"testing"

	"pulse/internal/sync"
)

func writeHTTP(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func publishStore() *sync.Store {
	var node [8]byte
	node[0] = 9
	return sync.NewStore(node, nil, func(string) bool { return false })
}

func TestPublishIsIdempotentAtSteadyState(t *testing.T) {
	dir := t.TempDir()
	writeHTTP(t, dir, "a.http", "# @name one\nGET http://x/one HTTP/1.1\n\n###\n# @name two\nPOST http://x/two HTTP/1.1\n\n")
	s := publishStore()
	ids, skipped, err := publishDirToStore(dir, s)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(skipped) != 0 || len(ids) != 2 {
		t.Fatalf("ids=%v skipped=%v, want 2 ids and no skips", ids, skipped)
	}
	d1 := s.DigestOf()
	if _, _, err := publishDirToStore(dir, s); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if d2 := s.DigestOf(); !d2.Equal(d1) {
		t.Fatal("republish re-stamped unchanged requests: steady state churns, anti-entropy never quiesces")
	}
}

func TestPublishTracksEditsAndVanishedFiles(t *testing.T) {
	dir := t.TempDir()
	writeHTTP(t, dir, "a.http", "# @name one\nGET http://x/one HTTP/1.1\n\n")
	s := publishStore()
	if _, _, err := publishDirToStore(dir, s); err != nil {
		t.Fatalf("publish: %v", err)
	}
	d1 := s.DigestOf()

	// An edit re-stamps exactly the changed request.
	writeHTTP(t, dir, "a.http", "# @name one\nGET http://x/CHANGED HTTP/1.1\n\n")
	if _, _, err := publishDirToStore(dir, s); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if d2 := s.DigestOf(); d2.Equal(d1) {
		t.Fatal("edited URL did not move the digest")
	}
	it := s.Get("a.http#one")
	if it == nil || it.Fields[sync.FieldURL].Value != "http://x/CHANGED" {
		t.Fatalf("edit did not land: %+v", it)
	}

	// A vanished file tombstones its ids instead of forgetting them:
	// forgetting would resurrect the item on the next gossip round.
	if err := os.Remove(filepath.Join(dir, "a.http")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := publishDirToStore(dir, s); err != nil {
		t.Fatalf("publish after delete: %v", err)
	}
	it = s.Get("a.http#one")
	if it == nil {
		t.Fatal("vanished file's item forgotten instead of tombstoned")
	}
	if it.Alive() {
		t.Fatal("vanished file's item still alive after convergence")
	}
}

func TestSplitHostPortRefusesGarbage(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
		wantOK   bool
	}{
		{"127.0.0.1:19431", "127.0.0.1", 19431, true},
		{"peer.lan:1", "peer.lan", 1, true},
		{"", "", 0, false},
		{"noport", "", 0, false},
		{":19431", "", 0, false},
		{"host:0", "", 0, false},
		{"host:99999", "", 0, false},
		{"host:notaport", "", 0, false},
	}
	for _, c := range cases {
		h, p, ok := splitHostPort(c.in)
		if h != c.wantHost || p != c.wantPort || ok != c.wantOK {
			t.Errorf("splitHostPort(%q) = %q,%d,%v", c.in, h, p, ok)
		}
	}
}

func TestPublishSkipsSecretBlockedFilesWhole(t *testing.T) {
	dir := t.TempDir()
	writeHTTP(t, dir, "a.http", "# @name one\nGET http://x/one HTTP/1.1\n\n")
	var node [8]byte
	node[0] = 7
	denyAll := sync.NewStore(node, nil, func(string) bool { return true })
	ids, skipped, err := publishDirToStore(dir, denyAll)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(ids) != 0 || len(skipped) != 1 {
		t.Fatalf("ids=%v skipped=%v, want zero ids and one named skip", ids, skipped)
	}
	if len(denyAll.IDs()) != 0 {
		t.Fatalf("blocked file left residue: %v", denyAll.IDs())
	}
}
