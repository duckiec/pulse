package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"pulse/internal/ast"
	"pulse/internal/engine"
)

// spaceKey is the canonical Space press: the keyMsg helper only builds
// runes and a few named keys, and " " must arrive as KeySpace for
// tea.Key.String() to report " ".
func spaceKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeySpace} }

func ctrlKey(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

type saveCall struct {
	root, rel, name string
	block           []byte
}

func fakeSavers(saves *[]saveCall, files map[string][]byte, seals *map[string]string) (
	func(string, string, string, []byte) tea.Msg,
	func(string, string, []byte) tea.Msg,
	func(string, string) tea.Msg,
	func(string, string) tea.Msg,
) {
	saveReq := func(root, rel, name string, block []byte) tea.Msg {
		*saves = append(*saves, saveCall{root, rel, name, append([]byte(nil), block...)})
		return saveDoneMsg{rel: rel}
	}
	saveFile := func(root, rel string, content []byte) tea.Msg {
		files[rel] = append([]byte(nil), content...)
		return fileLoadedMsg{rel: rel, content: append([]byte(nil), content...)}
	}
	loadFile := FileLoader(func(root, rel string) ([]byte, error) {
		raw, ok := files[rel]
		if !ok {
			return nil, errors.New("no such file")
		}
		return raw, nil
	})
	seal := func(name, value string) tea.Msg {
		(*seals)[name] = value
		return sealedMsg{name: name}
	}
	return saveReq, saveFile, loadFile, seal
}

func composerModel(t *testing.T, src string) *Model {
	t.Helper()
	doc := parseDoc(t, src)
	m := New(context.Background(), nil, doc, false)
	saves := []saveCall{}
	files := map[string][]byte{}
	seals := map[string]string{}
	m.SetWorkspace(t.TempDir(), "requests.http")
	m.SetSavers(fakeSavers(&saves, files, &seals))
	m.SetRunnerFactory(func(d *ast.Document) Runner { return engine.New(d) })
	m.Bind(func(tea.Msg) {})
	return m
}

// TestScratchpadBootsWelcoming proves the blank-state contract: the
// template parses, the model flags scratch with the requests.http target,
// and both the welcome pane and the footer name the five verbs.
func TestScratchpadBootsWelcoming(t *testing.T) {
	doc, err := ParseScratch()
	if err != nil {
		t.Fatalf("ParseScratch: %v", err)
	}
	t.Cleanup(doc.Release)
	if doc.Len() != 1 {
		t.Fatalf("scratch requests = %d, want the single Untitled", doc.Len())
	}
	m := NewScratchpad(context.Background(), nil, doc, t.TempDir())
	if !m.Scratch() || m.Touched() {
		t.Fatal("scratchpad must start scratch and untouched")
	}
	m.width, m.height = 120, 40
	view := m.View()
	for _, want := range []string{"welcome to pulse", "Untitled", "[e]", "[i]", "[g]", "[n]", "[?]"} {
		if !strings.Contains(view, want) {
			t.Errorf("welcome view omits %q", want)
		}
	}
	if foot := m.viewFooter(); foot != m.keys.ScratchFooter() {
		t.Errorf("scratch footer = %q, want the five verbs", foot)
	}
	// First interaction retires the welcome for the workbench.
	m = updateKeys(t, m, "e")
	if !m.Touched() || !m.editor.Active() {
		t.Error("e must touch the pad and open the composer")
	}
	if out := m.View(); strings.Contains(out, "welcome to pulse") {
		t.Error("welcome survived first touch")
	}
}

// TestComposerSeedsAndCycles proves e seeds every field from the current
// request (name, method, URL, headers, body, preserved asserts) and Space
// cycles the method picker.
func TestComposerSeedsAndCycles(t *testing.T) {
	m := composerModel(t, "# @name demo\n# @assert status == 200\nPOST https://x.local/n HTTP/1.1\nContent-Type: application/json\n\n{\"a\":1}\n\n###\n")
	m = updateKeys(t, m, "e")
	if !m.editor.Active() {
		t.Fatal("e did not open the composer")
	}
	if got := m.editor.name.Value(); got != "demo" {
		t.Errorf("name = %q, want demo", got)
	}
	if got := m.editor.url.Value(); got != "https://x.local/n" {
		t.Errorf("url = %q", got)
	}
	if got := m.editor.body.Value(); !strings.Contains(got, `"a":1`) {
		t.Errorf("body = %q, want the request body", got)
	}
	if !strings.Contains(m.editor.preserve, "# @assert status == 200") {
		t.Errorf("preserve dropped the assert: %q", m.editor.preserve)
	}
	method, _, headers := m.editor.Values()
	if method != "POST" || !strings.Contains(headers, "Content-Type: application/json") {
		t.Errorf("seeded values = %q %q", method, headers)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(*Model)
	// Move focus to method (name=0 -> method=1 is one Cycle backwards;
	// Cycle only goes forward, so cycle around) then Space through it.
	for m.editor.Focus() != 1 {
		m = updateKeys(t, m, "tab")
	}
	m = updateKeys(t, m, " ")
	if got := m.editor.method.Value(); got != "PUT" {
		t.Errorf("space on POST = %q, want PUT", got)
	}
}

// TestComposerHeaderOps proves the list verbs: a appends, e edits, Space
// disables, d deletes — and a colon-less line is refused with rows intact.
func TestComposerHeaderOps(t *testing.T) {
	m := composerModel(t, "GET https://x.local/ HTTP/1.1\nX-A: 1\n\n###\n")
	m = updateKeys(t, m, "e", "tab") // URL focus steps once to the headers list
	if !m.editor.HeaderNav() {
		t.Fatalf("focus = %d, want the headers list", m.editor.Focus())
	}
	m = updateKeys(t, m, "a")
	if !m.editor.LineEditing() {
		t.Fatal("a did not open the line editor")
	}
	for _, r := range "X-B: 2" {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(*Model)
	}
	m = updateKeys(t, m, "enter")
	if _, _, headers := m.editor.Values(); !strings.Contains(headers, "X-B: 2") {
		t.Errorf("headers = %q, want the appended row", headers)
	}
	m = updateKeys(t, m, " ")
	if _, _, headers := m.editor.Values(); !strings.Contains(headers, "# X-B: 2") {
		t.Errorf("headers = %q, want the row disabled", headers)
	}
	m = updateKeys(t, m, "d")
	if _, _, headers := m.editor.Values(); strings.Contains(headers, "X-B") {
		t.Errorf("headers = %q, want the row deleted", headers)
	}
	// Colon-less lines fail closed with the list untouched.
	m = updateKeys(t, m, "a")
	for _, r := range "bogus" {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(*Model)
	}
	before := len(m.editor.headers)
	m = updateKeys(t, m, "enter")
	if len(m.editor.headers) != before {
		t.Error("colon-less header entered the list")
	}
	if !strings.Contains(m.Toast(), "no ':'") {
		t.Errorf("toast = %q, want the refusal named", m.Toast())
	}
	m = updateKeys(t, m, "esc")
	if m.editor.LineEditing() || !m.editor.Active() {
		t.Error("esc must cancel the line edit, not the composer")
	}
	m = updateKeys(t, m, "esc")
	if m.editor.Active() {
		t.Error("second esc must close the composer")
	}
}

// TestComposerFormatsJSON proves f pretty-prints a JSON body and refuses
// a non-JSON one with the body untouched.
func TestComposerFormatsJSON(t *testing.T) {
	m := composerModel(t, "POST https://x.local/n HTTP/1.1\n\n###\n")
	m = updateKeys(t, m, "e")
	m.editor.body.SetValue(`{"a":1,"b":[1,2]}`)
	m = updateKeys(t, m, "f")
	if got := m.editor.body.Value(); !strings.Contains(got, "\n  \"a\": 1") {
		t.Errorf("body not formatted:\n%s", got)
	}
	m.editor.body.SetValue("plain text")
	m = updateKeys(t, m, "f")
	if got := m.editor.body.Value(); got != "plain text" {
		t.Errorf("non-JSON body rewritten to %q", got)
	}
	if !strings.Contains(m.Toast(), "not JSON") {
		t.Errorf("toast = %q, want the refusal", m.Toast())
	}
}

// TestComposerSavesThroughTheSeam proves ctrl+s persists the block under
// the request name via the injected saver — and that an unnamed request
// fails closed before any seam is touched.
func TestComposerSavesThroughTheSeam(t *testing.T) {
	m := composerModel(t, "# @name demo\nGET https://x.local/a HTTP/1.1\n\n###\n")
	saves := []saveCall{}
	files := map[string][]byte{}
	seals := map[string]string{}
	m.SetSavers(fakeSavers(&saves, files, &seals))
	m = updateKeys(t, m, "e")
	updated, cmd := m.Update(ctrlKey(tea.KeyCtrlS))
	m = updated.(*Model)
	if cmd == nil {
		t.Fatalf("ctrl+s produced no command: %q", m.Toast())
	}
	if msg := cmd(); msg == nil {
		t.Fatal("save command produced no message")
	} else {
		updated, _ := m.Update(msg)
		m = updated.(*Model)
	}
	if len(saves) != 1 || saves[0].name != "demo" || saves[0].rel != "requests.http" {
		t.Fatalf("saver calls = %+v, want one upsert for demo", saves)
	}
	if !strings.Contains(string(saves[0].block), "GET https://x.local/a") {
		t.Errorf("saved block:\n%s", saves[0].block)
	}
	if got := m.Toast(); !strings.Contains(got, "saved") {
		t.Errorf("toast = %q, want confirmation", got)
	}
	// Unnamed requests never reach the seam.
	m.editor.name.SetValue("")
	n := len(saves)
	m = updateKeys(t, m, "ctrl+s")
	if len(saves) != n {
		t.Error("unnamed save reached the seam")
	}
	if !strings.Contains(m.Toast(), "name") {
		t.Errorf("toast = %q, want the naming prompt", m.Toast())
	}
}

// TestImportPopulatesComposer proves [i]: a curl bearing a bearer token
// fills the composer with the placeholder (never the secret) and counts
// the extraction in the toast.
func TestImportPopulatesComposer(t *testing.T) {
	m := composerModel(t, "GET https://x.local/ HTTP/1.1\n\n###\n")
	m = updateKeys(t, m, "i")
	if !m.imprt.Active() {
		t.Fatal("i did not open the import modal")
	}
	m.imprt.paste.SetValue("curl -H 'Authorization: Bearer sk-abc123' https://api.example.com/v1/notes")
	m = updateKeys(t, m, "enter")
	if m.imprt.Active() || !m.editor.Active() {
		t.Fatal("enter must close import and reveal the composer")
	}
	_, _, headers := m.editor.Values()
	if !strings.Contains(headers, "{{BEARER_TOKEN}}") {
		t.Errorf("headers = %q, want the placeholder", headers)
	}
	if strings.Contains(headers, "sk-abc123") {
		t.Errorf("raw secret in composer headers: %q", headers)
	}
	if !strings.Contains(m.Toast(), "1 secret") {
		t.Errorf("toast = %q, want the extraction counted", m.Toast())
	}
	// A bogus paste is refused with the modal held open.
	m = updateKeys(t, m, "esc", "i")
	m.imprt.paste.SetValue("wget https://x.local/")
	m = updateKeys(t, m, "enter")
	if !m.imprt.Active() {
		t.Error("rejected import must hold the modal open")
	}
	if !strings.Contains(m.Toast(), "import rejected") {
		t.Errorf("toast = %q, want the rejection", m.Toast())
	}
}

// TestVaultSealsAndClearsTheDoctor proves [v]: the typed pair seals
// through the seam, the toast names only {{NAME}}, and a matching doctor
// clears so the pane hint vanishes.
func TestVaultSealsAndClearsTheDoctor(t *testing.T) {
	m := composerModel(t, "GET https://x.local/ HTTP/1.1\n\n###\n")
	m.applyFailedRun(errors.New(`engine: unresolved variable {{API_TOKEN}} at line 1`))
	if m.Doctor() == "" {
		t.Fatal("unresolved failure produced no doctor")
	}
	m = updateKeys(t, m, "v")
	if !m.vault.Active() {
		t.Fatal("v did not open the vault modal")
	}
	m.vault.name.SetValue("API_TOKEN")
	m.vault.value.SetValue("tok-live-xyz")
	updated, cmd := m.Update(keyMsg("enter"))
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("seal produced no command")
	}
	updated, _ = m.Update(cmd())
	m = updated.(*Model)
	if got := m.Toast(); !strings.Contains(got, "{{API_TOKEN}}") || strings.Contains(got, "tok-live-xyz") {
		t.Errorf("toast = %q, want the name only", got)
	}
	if m.Doctor() != "" {
		t.Error("matching seal must clear the doctor")
	}
}

// TestDoctorInjectsRemediation proves the error doctor: a missing-secret
// failure renders its plain-English fix in the response pane, while other
// failures stay unadorned.
func TestDoctorInjectsRemediation(t *testing.T) {
	m := composerModel(t, "GET https://x.local/ HTTP/1.1\n\n###\n")
	m.width, m.height = 120, 40
	m.applyFailedRun(errors.New(`engine: unresolved variable {{API_TOKEN}} at line 1`))
	if out := m.View(); !strings.Contains(out, "Secret {{API_TOKEN}} is missing. Press [v] to set it.") {
		t.Errorf("response pane lacks the remediation:\n%s", lastLine(out))
	}
	m.applyFailedRun(errors.New("connection refused"))
	if m.Doctor() != "" {
		t.Errorf("non-secret failure gained a doctor: %q", m.Doctor())
	}
}

// TestYankBuildsPlaceholderCurl proves y renders the request as curl with
// templates intact and reports bytes, never content, in the toast.
func TestYankBuildsPlaceholderCurl(t *testing.T) {
	m := composerModel(t, "POST https://x.local/n HTTP/1.1\nAuthorization: Bearer {{API_TOKEN}}\n\n{}\n\n###\n")
	m = updateKeys(t, m, "y")
	if got := m.Toast(); !strings.Contains(got, "yank") {
		t.Fatalf("toast = %q, want yank confirmation", got)
	}
	m = updateKeys(t, m, "e")
	if got := m.editor.CurlCommand(); !strings.Contains(got, "{{API_TOKEN}}") || !strings.Contains(got, "-X POST") {
		t.Errorf("yank = %q", got)
	}
}

// TestStarterGeneratesTwoNamedRequests proves [g]: the starter lands
// through the file saver and swaps the model onto two named requests.
func TestStarterGeneratesTwoNamedRequests(t *testing.T) {
	doc := parseDoc(t, "GET https://x.local/ HTTP/1.1\n\n###\n")
	m := New(context.Background(), nil, doc, false)
	saves := []saveCall{}
	files := map[string][]byte{}
	seals := map[string]string{}
	m.SetWorkspace(t.TempDir(), "requests.http")
	m.SetSavers(fakeSavers(&saves, files, &seals))
	m.SetRunnerFactory(func(d *ast.Document) Runner { return engine.New(d) })
	m.Bind(func(tea.Msg) {})
	// The starter command runs in the background: drive it, then feed
	// the loaded file back through Update like the runtime would.
	updated, cmd := m.Update(keyMsg("g"))
	m = updated.(*Model)
	if cmd == nil {
		t.Fatalf("g produced no command: %q", m.Toast())
	}
	updated, _ = m.Update(cmd())
	m = updated.(*Model)
	count := 0
	for r := range m.doc.Requests() {
		count++
		if m.doc.RequestName(r) == "" {
			t.Error("starter request is unnamed")
		}
	}
	if count != 2 {
		t.Errorf("starter requests = %d, want 2", count)
	}
}

// TestNewFileCreatesAndSwaps proves [n]: naming a file creates it with
// the untitled template and makes it current.
func TestNewFileCreatesAndSwaps(t *testing.T) {
	m := composerModel(t, "GET https://x.local/ HTTP/1.1\n\n###\n")
	m = updateKeys(t, m, "n")
	if !m.newfile.Active() {
		t.Fatal("n did not open the new-file modal")
	}
	m.newfile.name.SetValue("team/api.http")
	updated, cmd := m.Update(keyMsg("enter"))
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("create produced no command")
	}
	updated, _ = m.Update(cmd())
	m = updated.(*Model)
	if m.saveTarget != "team/api.http" {
		t.Errorf("save target = %q, want the new file", m.saveTarget)
	}
	if got := m.Toast(); !strings.Contains(got, "team/api.http") {
		t.Errorf("toast = %q, want the new file named", got)
	}
}

// TestSwapHoldsOnBadBytes proves the fail-closed swap: an unparsable file
// keeps the current document, request, and save target.
func TestSwapHoldsOnBadBytes(t *testing.T) {
	m := composerModel(t, "GET https://x.local/keep HTTP/1.1\n\n###\n")
	before, target := m.doc, m.saveTarget
	updated, _ := m.Update(fileLoadedMsg{rel: "bad.http", content: []byte("GET\n")})
	m = updated.(*Model)
	if m.doc != before || m.saveTarget != target || m.url != "https://x.local/keep" {
		t.Error("bad file swapped the live document")
	}
	if !strings.Contains(m.Toast(), "does not parse") {
		t.Errorf("toast = %q, want the parse refusal", m.Toast())
	}
}
