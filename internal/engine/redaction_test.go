package engine_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"pulse/internal/ast"
	"pulse/internal/engine"
	"pulse/internal/secrets"
)

// This file is the end-to-end proof that the redaction chokepoint is actually
// WIRED (internal/secrets/redact_test.go proves it works in isolation). It uses
// only the public API, so it also proves the injection point is usable from
// outside the package without importing secrets into the engine itself.

type collectingSink struct {
	mu     sync.Mutex
	body   strings.Builder
	result *engine.RunResult
	err    error
	done   chan struct{}
}

func newCollectingSink() *collectingSink { return &collectingSink{done: make(chan struct{})} }

func (s *collectingSink) Chunk(c *engine.Chunk) {
	if !c.Final {
		s.mu.Lock()
		s.body.Write(c.Bytes())
		s.mu.Unlock()
	}
	c.Recycle()
}

func (s *collectingSink) Done(r *engine.RunResult) {
	s.mu.Lock()
	s.result = r
	s.mu.Unlock()
	close(s.done)
}

func (s *collectingSink) Failed(e error) {
	s.mu.Lock()
	s.err = e
	s.mu.Unlock()
	close(s.done)
}

func (s *collectingSink) payload() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.String()
}

const engineSecret = "sk-live-EXAMPLE-4f7b2c9d-secret"

// TestResponseBodyIsRedactedBeforeSink is the inbound chokepoint gate: a server
// that echoes a secret back in the response BODY must not have it reach the
// Sink, because the Sink is what renders and persists.
func TestResponseBodyIsRedactedBeforeSink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"error":"invalid api key `+engineSecret+`","hint":"see docs"}`)
	}))
	defer srv.Close()

	r := secrets.NewRedactor()
	r.Register(engineSecret)

	doc, release := parseDoc(t, "GET "+srv.URL+"/x HTTP/1.1\n\n###\n")
	defer release()

	sink := newCollectingSink()
	e := engine.New(doc, engine.WithRedactor(r))
	for req := range doc.Requests() {
		e.Run(context.Background(), req, sink).Wait()
		break
	}
	<-sink.done

	if sink.err != nil {
		t.Fatalf("run failed: %v", sink.err)
	}
	got := sink.payload()
	if strings.Contains(got, engineSecret) {
		t.Fatalf("SECRET LEAKED to the sink in the response body: %s", got)
	}
	if !strings.Contains(got, secrets.Placeholder) {
		t.Errorf("body should carry the placeholder: %s", got)
	}
	// The non-secret part of the payload must survive scrubbing.
	if !strings.Contains(got, "see docs") {
		t.Errorf("redaction destroyed unrelated content: %s", got)
	}
	if r.Scrubbed() == 0 {
		t.Error("redactor counted no scrub")
	}
}

// TestResponseHeadersAreRedactedBeforeSink proves the header chokepoint with a
// REAL secrets.Redactor wired through the engine's interface.
func TestResponseHeadersAreRedactedBeforeSink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session="+engineSecret)
		w.Header().Set("X-Trace", "visible")
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	r := secrets.NewRedactor()
	r.Register(engineSecret)

	doc, release := parseDoc(t, "GET "+srv.URL+"/x HTTP/1.1\n\n###\n")
	defer release()

	sink := newCollectingSink()
	e := engine.New(doc, engine.WithRedactor(r))
	for req := range doc.Requests() {
		e.Run(context.Background(), req, sink).Wait()
		break
	}
	<-sink.done

	if sink.result == nil {
		t.Fatalf("run did not complete: %v", sink.err)
	}
	blob := sink.result.Header.Get("Set-Cookie")
	if strings.Contains(blob, engineSecret) {
		t.Fatalf("SECRET LEAKED in response headers: %q", blob)
	}
	if got := sink.result.Header.Get("X-Trace"); got != "visible" {
		t.Errorf("benign header must survive: %q", got)
	}
}

// TestOutboundSecretStillReachesTheServer guards the opposite direction, and
// guards against the most dangerous plausible regression: redacting the OUTBOUND
// request would send "[REDACTED]" as the credential and break every
// authenticated call while looking correct in every other test.
func TestOutboundSecretStillReachesTheServer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	r := secrets.NewRedactor()
	r.Register(engineSecret)
	// A Chain is directly usable as the engine's resolver: keychain first,
	// ephemeral .env second, hard error last.
	chain := secrets.NewChain(
		secrets.WithEnv(map[string]string{"token": engineSecret}),
		secrets.WithRedactor(r),
	)

	doc, release := parseDoc(t,
		"GET "+srv.URL+"/x HTTP/1.1\nAuthorization: Bearer {{token}}\n\n###\n")
	defer release()

	sink := newCollectingSink()
	e := engine.New(doc,
		engine.WithResolver(chain),
		engine.WithRedactor(r),
	)
	for req := range doc.Requests() {
		e.Run(context.Background(), req, sink).Wait()
		break
	}
	<-sink.done

	if sink.err != nil {
		t.Fatalf("run failed: %v", sink.err)
	}
	if gotAuth != "Bearer "+engineSecret {
		t.Fatalf("the server must receive the REAL secret; got %q", gotAuth)
	}
	// ...while nothing that reached the sink carries it.
	if strings.Contains(sink.payload(), engineSecret) {
		t.Error("secret leaked to the sink")
	}
}

// TestEngineRunsWithoutARedactor proves the redactor is optional: a nil
// Redactor must not panic or change success behaviour.
func TestEngineRunsWithoutARedactor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "plain body")
	}))
	defer srv.Close()

	doc, release := parseDoc(t, "GET "+srv.URL+"/x HTTP/1.1\n\n###\n")
	defer release()

	sink := newCollectingSink()
	e := engine.New(doc)
	if e.Redactor() != nil {
		t.Error("Redactor() should be nil when none was installed")
	}
	for req := range doc.Requests() {
		e.Run(context.Background(), req, sink).Wait()
		break
	}
	<-sink.done
	if sink.err != nil || sink.result == nil {
		t.Fatalf("run should succeed with no redactor: %v", sink.err)
	}
	if !strings.Contains(sink.payload(), "plain body") {
		t.Errorf("payload mangled with no redactor: %q", sink.payload())
	}
}

// parseDoc parses src into a document plus its release func.
func parseDoc(t *testing.T, src string) (*ast.Document, func()) {
	t.Helper()
	arena := ast.AcquireArena()
	arena.Reset([]byte(src))
	doc, err := ast.Parse(arena)
	if err != nil {
		arena.Release()
		t.Fatalf("parse: %v", err)
	}
	return doc, doc.Release
}
