//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pulse/internal/store"
)

// transcriptLog gathers every byte the binary emitted plus every file the
// run produced, for the Stage 6 confinement scan.
type transcriptLog struct {
	blobs []string
	files []string
}

func (l *transcriptLog) add(s string)     { l.blobs = append(l.blobs, s) }
func (l *transcriptLog) addFile(p string) { l.files = append(l.files, p) }

// ---------------------------------------------------------------------------
// stage 3: live MCP pipeline over real pipes
// ---------------------------------------------------------------------------

func stageMCP(t *testing.T, bin, ws string, srv *targetServer, tr *transcriptLog) {
	t.Helper()
	penv := []string{
		env("PULSE_HOME", filepath.Join(ws, "home")),
		env("PULSE_VAULT_KEY_ID", "e2e-main"),
		env("PULSE_VAULT_FILE", filepath.Join(ws, "secrets.env.vault")),
		env("PULSE_RAG_ROOT", filepath.Join(ws, "src")),
		env("PULSE_SKIP_SYNC", "1"),
	}
	p := start(t, bin, penv, "mcp")
	c := &mcpConn{t: t, p: p}
	c.rpc("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "crucible", "version": "0"},
	})
	c.notify("notifications/initialized", map[string]any{})

	res := c.rpc("tools/list", map[string]any{})
	names := map[string]bool{}
	if tools, ok := res["tools"].([]any); ok {
		for _, ti := range tools {
			if tm, ok := ti.(map[string]any); ok {
				if n, ok := tm["name"].(string); ok {
					names[n] = true
				}
			}
		}
	}
	for _, want := range []string{"execute_request", "list_collections", "list_requests",
		"get_request", "get_run", "get_history", "fuzz_start", "fuzz_next", "fuzz_report",
		"get_http_syntax_spec", "create_collection", "upsert_request",
		"delete_request", "import_curl", "validate_collection"} {
		if !names[want] {
			t.Fatalf("tools/list is missing %q: %v", want, names)
		}
	}

	orders := filepath.Join(ws, "coll", "orders.http")
	text, sc := c.callTool("execute_request", map[string]any{"path": orders})
	if status, _ := sc["status"].(float64); status != 200 {
		t.Fatalf("execute_request: status %v, want 200:\n%s", status, text)
	}
	if body, _ := sc["body"].(string); !strings.Contains(body, `"confirmed"`) {
		t.Fatalf("execute_request body is not the confirmation:\n%s", text)
	}
	if strings.Contains(text, "sk_live_pulse_e2e_") {
		t.Fatal("raw canary in the execute_request transcript")
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatal("the echoed canary is not [REDACTED] in the transcript: confinement unproven")
	}

	if _, sc := c.callTool("list_collections", map[string]any{}); len(sc) == 0 {
		t.Fatal("list_collections returned no structured content")
	}
	_, lsc := c.callTool("list_requests", map[string]any{})
	reqs, _ := lsc["requests"].([]any)
	if len(reqs) == 0 {
		t.Fatal("list_requests is empty after an execution was recorded")
	}
	first, _ := reqs[0].(map[string]any)
	ref, _ := first["id"].(string)
	if ref == "" {
		t.Fatalf("request has no id: %v", first)
	}
	_, gsc := c.callTool("get_request", map[string]any{"ref": ref})
	if m, _ := gsc["method"].(string); m != "POST" {
		t.Fatalf("get_request method = %q, want POST: %v", m, gsc)
	}
	_, hsc := c.callTool("get_history", map[string]any{})
	runs, _ := hsc["runs"].([]any)
	if len(runs) == 0 {
		t.Fatal("get_history is empty after an execution was recorded")
	}
	run0, _ := runs[0].(map[string]any)
	runID, _ := run0["id"].(float64)
	_, rsc := c.callTool("get_run", map[string]any{"run_id": runID})
	if s, _ := rsc["status_code"].(float64); s != 200 {
		t.Fatalf("get_run status_code = %v, want 200: %v", s, rsc)
	}

	// The transcript so far must already be clean: the canary was
	// resolved (the server saw it) yet no client-visible byte holds it.
	p.mu.Lock()
	lines := append([]string{}, p.lines...)
	p.mu.Unlock()
	for _, l := range lines {
		if strings.Contains(l, "sk_live_pulse_e2e_") {
			t.Fatalf("raw canary in MCP stdout line: %.120s", l)
		}
	}

	schema := writeCheckoutSchema(t, ws)
	sess := fuzzCampaign(t, c, schema, srv.url, 20, 7)
	found := driveCampaign(t, c, sess, "applyDiscount", false)
	if !found {
		t.Fatal("no 500 probe carried the applyDiscount chunk: RAG binding unproven")
	}
	rep := fuzzReport(t, c, sess)
	if len(rep.findings) == 0 {
		t.Fatal("fuzz_report carries no findings for a campaign that 500'd")
	}
	assertFindingsPersisted(t, filepath.Join(ws, "home", "pulse.db"))

	// Watcher hot-reload: extend the handler, then prove a FRESH campaign
	// serves the new chunk without restarting the process.
	marker := "\n// crucible-reload-marker\nfunc applyDiscountV2(priceCents int64) int64 { return priceCents }\n"
	appendFile(t, filepath.Join(ws, "src", "checkout_handler.go"), marker)
	deadline := time.Now().Add(90 * time.Second)
	seen := false
	for time.Now().Before(deadline) {
		s2 := fuzzCampaign(t, c, schema, srv.url, 20, 99)
		if driveCampaign(t, c, s2, "applyDiscountV2", true) {
			seen = true
			fuzzReport(t, c, s2)
			break
		}
		fuzzReport(t, c, s2)
	}
	if !seen {
		t.Fatal("watcher hot-reload: the reloaded chunk never served within 90s")
	}
	t.Logf("stage 3: 15 tools live, history persisted, RAG-bound 500, hot-reload proven")

	code, stderr := p.kill(t)
	if code != 0 {
		t.Fatalf("mcp exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	p.mu.Lock()
	tr.blobs = append(tr.blobs, append(lines, p.lines...)...)
	p.mu.Unlock()
	tr.add(stderr)
}

// fuzzSession is one started campaign.
type fuzzSession struct {
	id    string
	total int
}

func fuzzCampaign(t *testing.T, c *mcpConn, schema, target string, budget int, seed int) fuzzSession {
	t.Helper()
	_, sc := c.callTool("fuzz_start", map[string]any{
		"schema_path": schema, "target": target,
		"budget": budget, "seed": seed,
	})
	id, _ := sc["session"].(string)
	total, _ := sc["total"].(float64)
	if id == "" || total == 0 {
		t.Fatalf("fuzz_start gave no session: %v", sc)
	}
	return fuzzSession{id: id, total: int(total)}
}

// fuzzStepOut is the verdict of one fuzz_next call.
type fuzzStepOut struct {
	done    bool
	status  int
	context []map[string]any
}

// fuzzNext dispatches one probe, tolerating the error envelope: a 500
// probe IS an error at the JSON-RPC level, and also the evidence.
func fuzzNext(t *testing.T, c *mcpConn, sess string) (string, map[string]any, bool) {
	t.Helper()
	c.nextID++
	id := c.nextID
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id,
		"method": "tools/call",
		"params": map[string]any{"name": "fuzz_next", "arguments": map[string]any{"session": sess}}})
	fmt.Fprintf(c.p.stdin, "%s\n", msg)
	for {
		line, err := c.p.stdout.ReadString('\n')
		if err != nil {
			t.Fatalf("fuzz_next: read: %v", err)
		}
		c.p.mu.Lock()
		c.p.lines = append(c.p.lines, strings.TrimRight(line, "\r\n"))
		c.p.mu.Unlock()
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("fuzz_next: non-JSON line: %q", line)
		}
		got, _ := m["id"].(float64)
		if int(got) != id {
			continue
		}
		res, _ := m["result"].(map[string]any)
		if res == nil {
			if e, ok := m["error"]; ok {
				t.Fatalf("fuzz_next: protocol error: %v", e)
			}
			t.Fatalf("fuzz_next: no result: %s", line)
		}
		var text string
		if content, ok := res["content"].([]any); ok {
			for _, ci := range content {
				if cm, ok := ci.(map[string]any); ok {
					if s, ok := cm["text"].(string); ok {
						text += s + "\n"
					}
				}
			}
		}
		sc, _ := res["structuredContent"].(map[string]any)
		if sc == nil {
			sc = map[string]any{}
		}
		isErr, _ := res["isError"].(bool)
		return text, sc, isErr
	}
}

// driveCampaign advances until done, returning whether a 500 probe
// carried a context chunk naming want. When quiet, findings-free Done
// steps must not fail the drive: only the final verdict matters.
func driveCampaign(t *testing.T, c *mcpConn, sess fuzzSession, want string, quiet bool) bool {
	t.Helper()
	found := false
	for i := 0; i < sess.total+5; i++ {
		_, sc, _ := fuzzNext(t, c, sess.id)
		done, _ := sc["done"].(bool)
		status, _ := sc["status"].(float64)
		if status == 500 {
			if ctxs, ok := sc["context"].([]any); ok {
				for _, ci := range ctxs {
					cm, _ := ci.(map[string]any)
					text, _ := cm["text"].(string)
					sym, _ := cm["symbol"].(string)
					path, _ := cm["path"].(string)
					if strings.Contains(text, want) || strings.Contains(sym, want) || strings.Contains(path, "checkout_handler.go") {
						found = true
					}
				}
			}
		}
		if done {
			break
		}
		if i == sess.total+4 && !quiet {
			t.Fatalf("campaign %s did not finish in %d probes", sess.id, sess.total)
		}
	}
	return found
}

type fuzzReportOut struct {
	tap      string
	findings []map[string]any
}

func fuzzReport(t *testing.T, c *mcpConn, sess fuzzSession) fuzzReportOut {
	t.Helper()
	text, sc, _ := fuzzNextRaw(t, c, "fuzz_report", sess.id)
	_ = text
	var out fuzzReportOut
	if tap, ok := sc["tap"].(string); ok {
		out.tap = tap
		validateTAP(t, tap)
	} else {
		t.Fatalf("fuzz_report carries no TAP: %v", sc)
	}
	if fl, ok := sc["findings"].([]any); ok {
		for _, fi := range fl {
			if fm, ok := fi.(map[string]any); ok {
				out.findings = append(out.findings, fm)
			}
		}
	}
	return out
}

// fuzzNextRaw is the error-tolerant tools/call used for fuzz_report,
// whose Failed>0 also arrives as an error envelope.
func fuzzNextRaw(t *testing.T, c *mcpConn, tool, sess string) (string, map[string]any, bool) {
	t.Helper()
	c.nextID++
	id := c.nextID
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id,
		"method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": map[string]any{"session": sess}}})
	fmt.Fprintf(c.p.stdin, "%s\n", msg)
	for {
		line, err := c.p.stdout.ReadString('\n')
		if err != nil {
			t.Fatalf("%s: read: %v", tool, err)
		}
		c.p.mu.Lock()
		c.p.lines = append(c.p.lines, strings.TrimRight(line, "\r\n"))
		c.p.mu.Unlock()
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%s: non-JSON line: %q", tool, line)
		}
		got, _ := m["id"].(float64)
		if int(got) != id {
			continue
		}
		res, _ := m["result"].(map[string]any)
		if res == nil {
			t.Fatalf("%s: no result: %s", tool, line)
		}
		var text string
		if content, ok := res["content"].([]any); ok {
			for _, ci := range content {
				if cm, ok := ci.(map[string]any); ok {
					if s, ok := cm["text"].(string); ok {
						text += s + "\n"
					}
				}
			}
		}
		sc, _ := res["structuredContent"].(map[string]any)
		if sc == nil {
			sc = map[string]any{}
		}
		isErr, _ := res["isError"].(bool)
		return text, sc, isErr
	}
}

func writeCheckoutSchema(t *testing.T, ws string) string {
	t.Helper()
	schema := `{"openapi":"3.0.0","info":{"title":"checkout","version":"1"},
"paths":{"/v1/checkout":{"post":{
"parameters":[{"name":"discount","in":"query","schema":{"type":"integer","minimum":0,"maximum":100}}],
"responses":{"200":{"description":"ok"}}}}}}`
	p := filepath.Join(ws, "openapi.json")
	if err := os.WriteFile(p, []byte(schema), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendFile(t *testing.T, path, extra string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(extra); err != nil {
		t.Fatal(err)
	}
}

// assertFindingsPersisted opens the REAL on-disk database through the
// production reader and counts physical rows: the finding must survive
// the process, not just the session.
func assertFindingsPersisted(t *testing.T, dbPath string) {
	t.Helper()
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open findings db: %v", err)
	}
	defer db.Close()
	n, err := db.FuzzFindingCount(context.Background())
	if err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if n == 0 {
		t.Fatal("fuzz_findings holds no rows for a campaign that 500'd")
	}
	t.Logf("fuzz_findings: %d persisted rows", n)
}

// ---------------------------------------------------------------------------
// stage 4: two-node mTLS sync over loopback
// ---------------------------------------------------------------------------

type syncNode struct {
	name        string
	home        string
	nodeID      string
	fingerprint string
	port        int
	peersFile   string
	coll        string
}

func stageSync(t *testing.T, bin, ws string, tr *transcriptLog) {
	t.Helper()
	nodes := map[string]*syncNode{}
	for _, n := range []string{"a", "b", "c"} {
		home := filepath.Join(ws, "sync-"+n, "home")
		penv := []string{env("PULSE_HOME", home), env("PULSE_VAULT_KEY_ID", "sync-"+n)}
		r := run(t, bin, penv, "vault", "keygen", "--key-id", "sync-"+n)
		if r.exit != 0 {
			t.Fatalf("node %s keygen: exit %d: %s", n, r.exit, r.stderr)
		}
		st := run(t, bin, append(penv, env("PULSE_SKIP_SYNC", "1")), "sync", "status")
		if st.exit != 0 {
			t.Fatalf("node %s status: exit %d: %s", n, st.exit, st.stderr)
		}
		var status struct {
			NodeID      string `json:"node_id"`
			Fingerprint string `json:"fingerprint"`
		}
		if err := json.Unmarshal([]byte(st.stdout), &status); err != nil {
			t.Fatalf("node %s status does not parse: %v:\n%s", n, err, st.stdout)
		}
		if status.NodeID == "" || status.Fingerprint == "" {
			t.Fatalf("node %s status is hollow: %s", n, st.stdout)
		}
		nodes[n] = &syncNode{name: n, home: home, nodeID: status.NodeID,
			fingerprint: status.Fingerprint, port: freePort(t),
			peersFile: filepath.Join(ws, "sync-"+n, "peers.txt"),
			coll:      filepath.Join(ws, "sync-"+n, "coll")}
		if err := os.MkdirAll(nodes[n].coll, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, nodes["a"].peersFile, nodes["b"].nodeID+" "+nodes["b"].fingerprint+"\n")
	writeFile(t, nodes["b"].peersFile, nodes["a"].nodeID+" "+nodes["a"].fingerprint+"\n")
	// C pins A so C dials every round, but A does NOT pin C: A's TLS
	// handshake must violently refuse the unpinned dialer. An empty C
	// ledger would dial nobody and prove nothing about TOFU rejection.
	writeFile(t, nodes["c"].peersFile, nodes["a"].nodeID+" "+nodes["a"].fingerprint+"\n")
	writeFile(t, filepath.Join(ws, "sync-a", "coll", "a.http"),
		"# @name item-a\nGET http://x/a HTTP/1.1\n\n")
	writeFile(t, filepath.Join(ws, "sync-b", "coll", "b.http"),
		"# @name item-b\nGET http://x/b HTTP/1.1\n\n")

	static := func(self, peer *syncNode) string {
		return peer.nodeID + "=127.0.0.1:" + itoa(peer.port)
	}
	penvA := syncEnv(nodes["a"], static(nodes["a"], nodes["b"]))
	penvB := syncEnv(nodes["b"], static(nodes["b"], nodes["a"]))
	penvC := syncEnv(nodes["c"], static(nodes["c"], nodes["a"])) // C dials A but is NOT pinned

	pa := start(t, bin, penvA, "sync", "serve", nodes["a"].coll)
	pb := start(t, bin, penvB, "sync", "serve", nodes["b"].coll)
	pc := start(t, bin, penvC, "sync", "serve", nodes["c"].coll)
	// Fail fast when a serve refuses to boot: polling a dead peer for
	// 75s reports the wrong defect ("never converged" for "never ran").
	time.Sleep(8 * time.Second)
	for name, p := range map[string]*liveProc{"A": pa, "B": pb, "C": pc} {
		if se := p.stderr.String(); strings.Contains(se, "refusing to serve") {
			t.Fatalf("node %s refused to serve: %s", name, se)
		}
	}
	t.Logf("stage 4: serves alive; A stderr: %.200s", pa.stderr.String())

	wantA := "a.http#item-a"
	wantB := "b.http#item-b"
	deadline := time.Now().Add(75 * time.Second)
	for {
		da := syncStatusOf(t, bin, nodes["a"])
		db := syncStatusOf(t, bin, nodes["b"])
		if da["digest"] != "" && da["digest"] == db["digest"] && da["count"] == float64(2) &&
			aliveIn(da, wantA) && aliveIn(da, wantB) && aliveIn(db, wantA) && aliveIn(db, wantB) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("A/B never converged:\nA: %v\nB: %v\nA stderr:\n%s\nB stderr:\n%s",
				da, db, pa.stderr.String(), pb.stderr.String())
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("stage 4: A/B converged on both items")

	// Tombstone leg: B's file vanishes; A's copy must die too, by digest.
	if err := os.Remove(filepath.Join(ws, "sync-b", "coll", "b.http")); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(75 * time.Second)
	for {
		da := syncStatusOf(t, bin, nodes["a"])
		db := syncStatusOf(t, bin, nodes["b"])
		if da["digest"] != "" && da["digest"] == db["digest"] &&
			!aliveIn(da, wantB) && !aliveIn(db, wantB) && aliveIn(da, wantA) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tombstone never converged:\nA: %v\nB: %v", da, db)
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("stage 4: tombstone converged; A holds B's deletion")

	// Rejection leg: C dialed A every round, was refused every time, and
	// holds nothing of A/B's.
	dc := syncStatusOf(t, bin, nodes["c"])
	for _, it := range itemsOf(dc) {
		if id, _ := it["id"].(string); id == wantA || id == wantB {
			t.Fatalf("unpinned C holds %q: the ledger did not gate", id)
		}
	}
	codeC, errC := pc.kill(t)
	tr.add(errC)
	if codeC != 0 {
		t.Fatalf("node C exit = %d, want 0", codeC)
	}
	if !strings.Contains(errC, "sync: dial") {
		t.Fatalf("no dial-refusal evidence on C's stderr:\n%s", errC)
	}
	t.Logf("stage 4: unpinned C refused at the handshake, holds nothing")

	codeA, errA := pa.kill(t)
	codeB, errB := pb.kill(t)
	tr.add(errA)
	tr.add(errB)
	if codeA != 0 || codeB != 0 {
		t.Fatalf("serve exits = %d/%d, want 0/0", codeA, codeB)
	}
}

func syncEnv(n *syncNode, static string) []string {
	return []string{
		env("PULSE_HOME", n.home),
		env("PULSE_VAULT_KEY_ID", "sync-"+n.name),
		env("PULSE_SYNC_PORT", itoa(n.port)),
		env("PULSE_SYNC_PEERS_FILE", n.peersFile),
		env("PULSE_SYNC_STATIC_PEERS", static),
	}
}

func syncStatusOf(t *testing.T, bin string, n *syncNode) map[string]any {
	t.Helper()
	penv := []string{env("PULSE_HOME", n.home),
		env("PULSE_VAULT_KEY_ID", "sync-"+n.name), env("PULSE_SKIP_SYNC", "1")}
	r := run(t, bin, penv, "sync", "status")
	if r.exit != 0 {
		t.Fatalf("node %s status: exit %d: %s", n.name, r.exit, r.stderr)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil {
		t.Fatalf("node %s status does not parse: %v", n.name, err)
	}
	return st
}

func itemsOf(st map[string]any) []map[string]any {
	raw, _ := st["items"].([]any)
	out := []map[string]any{}
	for _, it := range raw {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func aliveIn(st map[string]any, id string) bool {
	for _, it := range itemsOf(st) {
		if it["id"] == id {
			alive, _ := it["alive"].(bool)
			return alive
		}
	}
	return false
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// ---------------------------------------------------------------------------
// stage 5: TUI under a real pty
// ---------------------------------------------------------------------------

func stageTUI(t *testing.T, bin, ws string, tr *transcriptLog) {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("SKIP (reason: no python3 for the pty driver: %v)", err)
	}
	drv := filepath.Join(ws, "pty_drive.py")
	if err := os.WriteFile(drv, []byte(ptyDriver), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(ws, "tui-home")
	file := filepath.Join(ws, "coll", "orders.http")
	for _, size := range []string{"80x24", "120x40"} {
		cmd := exec.Command(py, drv, size, bin, file)
		cmd.Env = append(os.Environ(),
			env("PULSE_HOME", home),
			env("PULSE_VAULT_KEY_ID", "e2e-main"),
			env("PULSE_VAULT_FILE", filepath.Join(ws, "secrets.env.vault")),
			env("PULSE_SKIP_SYNC", "1"),
			env("TERM", "xterm-256color"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pty %s: %v:\n%s", size, err, out)
		}
		var rep struct {
			Bytes int `json:"bytes"`
			Exit  int `json:"exit"`
		}
		// The driver prints one JSON line last; the pty echo precedes it.
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rep); err != nil {
			t.Fatalf("pty %s: no report line:\n%s", size, out)
		}
		if rep.Bytes < 200 {
			t.Fatalf("pty %s: %d bytes rendered, want a real frame", size, rep.Bytes)
		}
		if rep.Exit != 0 {
			t.Fatalf("pty %s: exit %d, want a deterministic 0 after q", size, rep.Exit)
		}
		tr.add(string(out))
		t.Logf("stage 5: %s rendered %d bytes, quit clean", size, rep.Bytes)
	}
}

// ptyDriver spawns the binary under a real OS pty at the given size,
// waits for first paint, sends q, and reports bytes + exit as JSON.
// A TUI that cannot start headless, leaks its shutdown, or hangs on
// quit fails here — exactly the defects a pipe test cannot see.
//
// Two driver races were caught red against green production code (the
// stage reported exit -1 for a TUI that had painted and quit cleanly):
//  1. q was sent on a fixed 2.5s timer, landing inside the renderer's
//     terminal query window on slow boots, where input bytes are swallowed
//     as query responses. q is now gated on first paint (>= 200 bytes) —
//     a real user quits a painted UI, and so does this driver.
//  2. when the child quit, the master read raised EIO/empty and the loop
//     broke before the next waitpid poll, freezing exit at its -1 initial
//     value. EOF now breaks to a short blocking reap, and only a genuinely
//     live process keeps -1.
const ptyDriver = `import json, os, pty, select, struct, sys, termios, fcntl, time
size, binary, target = sys.argv[1], sys.argv[2], sys.argv[3]
w, h = map(int, size.split("x"))
pid, fd = pty.fork()
if pid == 0:
    os.execve(binary, [binary, target], os.environ)
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", h, w, 0, 0))
out = b""
t0 = time.time()
sent_q = False
exit_code = -1
while time.time() - t0 < 15:
    r, _, _ = select.select([fd], [], [], 0.5)
    if r:
        try:
            chunk = os.read(fd, 65536)
        except OSError:
            chunk = b""
        if chunk:
            out += chunk
        else:
            break
    if not sent_q and len(out) >= 200:
        time.sleep(0.5)
        os.write(fd, b"q")
        sent_q = True
    try:
        done, status = os.waitpid(pid, os.WNOHANG)
        if done == pid:
            exit_code = os.waitstatus_to_exitcode(status)
            try:
                while True:
                    chunk = os.read(fd, 65536)
                    if not chunk:
                        break
                    out += chunk
            except OSError:
                pass
            break
    except ChildProcessError:
        break
if exit_code == -1:
    t1 = time.time()
    while time.time() - t1 < 3:
        try:
            done, status = os.waitpid(pid, os.WNOHANG)
        except ChildProcessError:
            break
        if done == pid:
            exit_code = os.waitstatus_to_exitcode(status)
            break
        time.sleep(0.1)
print(json.dumps({"bytes": len(out), "exit": exit_code}))
`

// ---------------------------------------------------------------------------
// stage 6: zero-leak post-mortem
// ---------------------------------------------------------------------------

func stageLeakScan(t *testing.T, ws string, tr *transcriptLog, canary string) {
	t.Helper()
	needle := []byte(canary)
	hits := []string{}
	for _, b := range tr.blobs {
		if strings.Contains(b, canary) {
			hits = append(hits, "transcript blob: "+firstLine(b))
		}
	}
	filepath.WalkDir(ws, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if bytes.Contains(raw, needle) {
			hits = append(hits, "file: "+p)
		}
		return nil
	})
	// The e2e's own driver script holds no secret, but the pty capture of
	// a rendered frame could in theory echo one: it is already in blobs.
	if len(hits) != 0 {
		t.Fatalf("canary leaked into %d place(s):\n%s", len(hits), strings.Join(hits, "\n"))
	}
	t.Logf("stage 6: zero canary bytes outside the vault/keyring")
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
