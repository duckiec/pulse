package engine

// Semantic three-way merge for `.http` collections (`pulse merge`).
//
// # Why a merge driver and not a text diff
//
// A `.http` file is a list of requests, and a request is a record: method,
// target, headers, directives, body. A line-based merge sees two adjacent
// changed lines and conflicts; the record merge defined here sees "A added a
// header" and "B added an assertion" and keeps both. The unit of merge is
// therefore the REQUEST, and inside it the field.
//
// # Identity
//
// Requests are matched by `# @name` when they have one, else by
// `method + ' ' + target`. A rename on one side is a field merge, not a delete
// plus an add, because the name is part of the record. Unnamed requests that
// both change their target cannot be matched; they surface as one request from
// each side rather than being silently paired.
//
// # What merges without a conflict
//
//   - assertions: set union of additions, minus a removal
//   - headers and in-block variables: per-key three-way merge
//   - hook/sign directives, method+target, body: scalar three-way merge
//
// Two sides changing the SAME field to different values produces standard Git
// `<<<<<<<`/`=======`/`>>>>>>>` markers around the smallest owning unit (one
// header, the request line, the body, or the whole request when one side
// deleted it and the other edited it). Markers are deliberately not valid
// `.http` syntax: the file is unresolved until a human edits it, exactly as
// Git intends.
//
// # What is preserved
//
// An unchanged request is copied from the source byte-for-byte (via
// Document.RequestText), and so is an unchanged file preamble. Free comments
// and disabled headers have no AST node, so they survive only inside such a
// byte-identical copy; a request that actually merged is re-emitted
// canonically. That is a documented limitation, not a silent reformat.

import (
	"fmt"
	"strings"

	"pulse/internal/ast"
)

// MergeConflictMarker labels. They are constants so tests and the CLI report
// cannot drift from the text a human sees.
const (
	mergeOursStart = "<<<<<<< current"
	mergeSeparator = "======="
	mergeTheirsEnd = ">>>>>>> other"
)

// MergeResult is the outcome of one semantic merge.
type MergeResult struct {
	// Text is the merged document (trailing newline included).
	Text []byte
	// Conflicts counts the conflict regions written into Text. Zero means the
	// merge is clean (the driver's exit 0).
	Conflicts int
	// Requests is the number of requests in the merged document, for the
	// driver's one-line report.
	Requests int
}

// Merge performs a three-way semantic merge of `.http` documents.
//
// base is the merge ancestor (%O), ours the current branch (%A), theirs the
// incoming side (%B). It never writes anywhere: the caller owns persistence,
// which keeps this testable without a git repository.
func Merge(base, ours, theirs []byte) (MergeResult, error) {
	bdoc, err := parseMergeInput("ancestor", base)
	if err != nil {
		return MergeResult{}, err
	}
	defer bdoc.Release()
	adoc, err := parseMergeInput("current", ours)
	if err != nil {
		return MergeResult{}, err
	}
	defer adoc.Release()
	tdoc, err := parseMergeInput("other", theirs)
	if err != nil {
		return MergeResult{}, err
	}
	defer tdoc.Release()

	m := &merger{docs: [3]*ast.Document{bdoc, adoc, tdoc}, text: [3][]byte{base, ours, theirs}}
	return m.run()
}

// parseMergeInput parses one side, naming it in the diagnostic. A merge driver
// that cannot parse its input must fail loudly: guessing a merge for a file
// Pulse does not understand is how a conflict becomes corruption.
func parseMergeInput(side string, src []byte) (*ast.Document, error) {
	arena := ast.AcquireArena()
	arena.Reset(src)
	doc, err := ast.Parse(arena)
	if err != nil {
		arena.Release()
		return nil, fmt.Errorf("merge: %s is not a valid .http document: %w", side, err)
	}
	return doc, nil
}

// merger carries the three parsed sides for one merge.
type merger struct {
	docs [3]*ast.Document // 0 base, 1 current (ours), 2 other (theirs)
	text [3][]byte
}

// reqModel is a request reduced to mergeable fields. Map values are ordered
// slices so a repeated header (Set-Cookie) is compared as a whole: dropping one
// occurrence while keeping another is a change, not a no-op.
type reqModel struct {
	name, method, url, hook, sign, body string
	vars                                kvModel
	headers                             kvModel
	asserts                             []string
}

// kvModel is an ordered key -> values map plus the original spelling of each
// key, so a merged header keeps the user's capitalisation.
type kvModel struct {
	order  []string
	vals   map[string][]string
	spells map[string]string
}

func newKVModel() kvModel {
	return kvModel{vals: map[string][]string{}, spells: map[string]string{}}
}

func (k *kvModel) add(key, spell, value string) {
	if _, ok := k.vals[key]; !ok {
		k.order = append(k.order, key)
		k.spells[key] = spell
	}
	k.vals[key] = append(k.vals[key], value)
}

// modelOf projects one parsed request onto its mergeable fields.
func modelOf(doc *ast.Document, r *ast.Request) reqModel {
	m := reqModel{
		name:    doc.RequestName(r),
		method:  r.Method.String(),
		url:     doc.Str(r.URLStart, r.URLEnd),
		hook:    doc.WasmHook(r),
		sign:    doc.WasmSign(r),
		body:    string(doc.BodyText(r.Body)),
		vars:    newKVModel(),
		headers: newKVModel(),
	}
	for _, v := range doc.VarsOf(r) {
		m.vars.add(doc.Str(v.NameStart, v.NameEnd), doc.Str(v.NameStart, v.NameEnd),
			doc.Str(v.ValStart, v.ValEnd))
	}
	for _, h := range r.Headers {
		name := doc.Str(h.NameStart, h.NameEnd)
		val := ""
		if h.ValStart >= 0 && h.ValEnd > h.ValStart {
			val = doc.Str(h.ValStart, h.ValEnd)
		}
		m.headers.add(strings.ToLower(name), name, val)
	}
	for _, a := range doc.AssertsOf(r) {
		m.asserts = append(m.asserts, strings.TrimSpace(doc.Str(a.Start, a.End)))
	}
	return m
}

// key is the merge identity.
func (m reqModel) key() string {
	if m.name != "" {
		return "name:" + m.name
	}
	return "line:" + m.method + " " + m.url
}

// equal compares every mergeable field; used to decide whether a request may be
// copied verbatim and whether a request was edited at all.
func (m reqModel) equal(o reqModel) bool {
	return m.name == o.name && m.method == o.method && m.url == o.url &&
		m.hook == o.hook && m.sign == o.sign && m.body == o.body &&
		kvEqual(m.vars, o.vars) && kvEqual(m.headers, o.headers) &&
		strings.Join(m.asserts, "\x00") == strings.Join(o.asserts, "\x00")
}

func kvEqual(a, b kvModel) bool {
	if len(a.vals) != len(b.vals) {
		return false
	}
	for k, av := range a.vals {
		bv, ok := b.vals[k]
		if !ok || !sliceEqual(av, bv) {
			return false
		}
	}
	return true
}

// valueConflict is one field both sides changed differently.
type valueConflict struct {
	a, b string // rendered text for each side (empty means deleted)
}

// kvLine is one rendered variable or header line; conflict replaces it.
type kvLine struct {
	key      string
	name     string
	values   []string
	conflict *valueConflict
}

// mergedRequest is the merge output for one request.
type mergedRequest struct {
	name, method, url, hook, sign, body string
	vars                                []kvLine
	headers                             []kvLine
	asserts                             []string
	conflict                            *valueConflict // whole-request conflict
}

// run merges every request and assembles the document.
func (m *merger) run() (MergeResult, error) {
	baseIdx := indexRequests(m.docs[0])
	oursIdx := indexRequests(m.docs[1])
	theirsIdx := indexRequests(m.docs[2])

	res := MergeResult{}
	var blocks []string
	emitted := map[string]bool{}

	// Current-branch order first: a merge should not reshuffle the file the
	// developer is looking at.
	for _, or := range listRequests(m.docs[1]) {
		key := or.key
		bm, hasBase := baseIdx[key]
		theirsEntry, hasTheirs := theirsIdx[key]
		switch {
		case hasTheirs:
			mr := mergeRequest(bm, hasBase, or.model, theirsEntry.model)
			blocks = append(blocks, m.renderRequest(m.docs[1], or.request, mr, or.model))
			res.Conflicts += conflictCount(mr)
			emitted[key] = true
		case hasBase:
			// Theirs deleted it. Ours untouched => delete; ours edited =>
			// modify/delete conflict, which a merge driver must not resolve
			// silently.
			if or.model.equal(bm.model) {
				emitted[key] = true
				continue
			}
			blocks = append(blocks, conflictBlock(string(m.docs[1].RequestText(or.request)), ""))
			res.Conflicts++
			emitted[key] = true
		default:
			// Added by ours only (or an unmatched rename): keep verbatim.
			blocks = append(blocks, string(m.docs[1].RequestText(or.request)))
			emitted[key] = true
		}
	}

	// Incoming-only requests, in their own order. This covers additions and the
	// modify/delete case where OUR side deleted a request theirs edited.
	for _, tr := range listRequests(m.docs[2]) {
		key := tr.key
		if emitted[key] {
			continue
		}
		if _, hasOurs := oursIdx[key]; hasOurs {
			continue // already merged above
		}
		emitted[key] = true
		bm, hasBase := baseIdx[key]
		if hasBase {
			if tr.model.equal(bm.model) {
				// Both deleted, or ours deleted an untouched request.
				continue
			}
			blocks = append(blocks, conflictBlock("", string(m.docs[2].RequestText(tr.request))))
			res.Conflicts++
			continue
		}
		blocks = append(blocks, string(m.docs[2].RequestText(tr.request)))
	}

	preamble, preConflicts := m.preamble(oursIdx)
	res.Conflicts += preConflicts
	var b strings.Builder
	b.WriteString(preamble)
	if len(blocks) > 0 {
		b.WriteString(strings.Join(blocks, "\n\n###\n"))
		b.WriteString("\n")
	}
	res.Text = []byte(b.String())
	res.Requests = len(blocks)
	return res, nil
}

// preamble merges the file-scoped variables and returns the text ahead of the
// first request.
//
// When the merged variables equal the current side's, the current preamble is
// copied byte-for-byte, so file-level comments survive. When they differ, the
// variables are re-emitted canonically (comments in the preamble are lost —
// documented in the package comment).
func (m *merger) preamble(oursIdx map[string]*reqEntry) (string, int) {
	lines, conflicts := mergeKV(fileVars(m.docs[0]), fileVars(m.docs[1]), fileVars(m.docs[2]))
	src := m.text[1]
	if len(conflicts) == 0 && appliedKV(fileVars(m.docs[1]), lines) {
		if start := firstRequestStart(m.docs[1], src); start >= 0 {
			return string(src[:start]), 0
		}
		if strings.TrimSpace(string(src)) == "" {
			return "", 0
		}
		return strings.TrimRight(string(src), "\n") + "\n", 0
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(renderKVLine(l, "var"))
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	return b.String(), len(conflicts)
}

// firstRequestStart returns the byte offset where the first request's owned
// text begins, or -1 when the document has no requests.
func firstRequestStart(doc *ast.Document, src []byte) int {
	for r := range doc.Requests() {
		text := doc.RequestText(r)
		if len(text) == 0 {
			return -1
		}
		return indexBytes(src, text)
	}
	return -1
}

// indexBytes is a forward search for needle. The merge driver emits a preamble
// once per run, so a small local helper beats another import for one call.
func indexBytes(haystack, needle []byte) int {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i] != needle[0] {
			continue
		}
		match := true
		for j := 1; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// fileVars projects Document.Vars onto a kvModel.
func fileVars(doc *ast.Document) kvModel {
	m := newKVModel()
	for _, v := range doc.Vars {
		name := doc.Str(v.NameStart, v.NameEnd)
		m.add(name, name, doc.Str(v.ValStart, v.ValEnd))
	}
	return m
}

// mergeRequest runs the field-level three-way merge.
func mergeRequest(base *reqEntry, hasBase bool, ours, theirs reqModel) mergedRequest {
	var b reqModel
	if hasBase && base != nil {
		b = base.model
	}
	out := mergedRequest{
		method: ours.method,
		url:    ours.url,
		body:   ours.body,
		hook:   ours.hook,
		sign:   ours.sign,
		name:   ours.name,
	}

	// Scalar fields. A conflict in ANY of them promotes the whole request to a
	// conflict block: rendering only the first conflicting field would hide the
	// others and leave a "clean" merge that is missing data.
	scalarConflict := false
	if v, c := scalar3(b.name, ours.name, theirs.name); c {
		scalarConflict = true
	} else {
		out.name = v
	}
	if v, c := scalar3(b.method+" "+b.url, ours.method+" "+ours.url, theirs.method+" "+theirs.url); c {
		scalarConflict = true
	} else if v != "" {
		out.method, out.url = splitRequestLine(v)
	}
	if v, c := scalar3(b.hook, ours.hook, theirs.hook); c {
		scalarConflict = true
	} else {
		out.hook = v
	}
	if v, c := scalar3(b.sign, ours.sign, theirs.sign); c {
		scalarConflict = true
	} else {
		out.sign = v
	}
	if v, c := scalar3(b.body, ours.body, theirs.body); c {
		scalarConflict = true
	} else {
		out.body = v
	}
	if scalarConflict {
		out.conflict = &valueConflict{a: renderSide(ours), b: renderSide(theirs)}
		return out
	}

	out.vars, _ = mergeKV(b.vars, ours.vars, theirs.vars)
	out.headers, _ = mergeKV(b.headers, ours.headers, theirs.headers)
	out.asserts = mergeAsserts(b.asserts, ours.asserts, theirs.asserts)
	return out
}

// renderSide renders one side's request canonically, for a whole-request
// conflict block.
func renderSide(m reqModel) string {
	mr := mergedRequest{
		name:    m.name,
		method:  m.method,
		url:     m.url,
		hook:    m.hook,
		sign:    m.sign,
		body:    m.body,
		asserts: m.asserts,
	}
	for _, k := range m.vars.order {
		mr.vars = append(mr.vars, kvLine{key: k, name: m.vars.spells[k], values: m.vars.vals[k]})
	}
	for _, k := range m.headers.order {
		mr.headers = append(mr.headers, kvLine{key: k, name: m.headers.spells[k], values: m.headers.vals[k]})
	}
	return renderBody(mr)
}

// splitRequestLine splits "METHOD target" at the first space.
func splitRequestLine(s string) (string, string) {
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// scalar3 is the classic three-way scalar rule: unchanged sides defer to the
// changed one, identical changes collapse, and two different changes conflict.
func scalar3(base, ours, theirs string) (string, bool) {
	switch {
	case ours == theirs:
		return ours, false
	case ours == base:
		return theirs, false
	case theirs == base:
		return ours, false
	default:
		return "", true
	}
}

// mergeKV merges an ordered keyed map, key by key. Additions from either side
// survive; a key both sides changed differently is a conflict carrying each
// side's rendered lines.
func mergeKV(base, ours, theirs kvModel) ([]kvLine, map[string]valueConflict) {
	out := make([]kvLine, 0, len(ours.order)+len(theirs.order))
	conflicts := map[string]valueConflict{}
	seen := map[string]bool{}
	handle := func(key string) {
		if seen[key] {
			return
		}
		seen[key] = true
		bv, okb := base.vals[key]
		ov, oko := ours.vals[key]
		tv, okt := theirs.vals[key]
		spell := ours.spells[key]
		if !oko {
			spell = theirs.spells[key]
		}
		switch {
		case oko == okt && sliceEqual(ov, tv):
			if oko {
				out = append(out, kvLine{key: key, name: spell, values: ov})
			}
		case oko == okb && sliceEqual(ov, bv):
			if okt {
				out = append(out, kvLine{key: key, name: spell, values: tv})
			}
		case okt == okb && sliceEqual(tv, bv):
			if oko {
				out = append(out, kvLine{key: key, name: spell, values: ov})
			}
		default:
			c := valueConflict{
				a: renderKVValues(spell, ov, oko),
				b: renderKVValues(spell, tv, okt),
			}
			conflicts[key] = c
			out = append(out, kvLine{key: key, name: spell, conflict: &c})
		}
	}
	for _, k := range ours.order {
		handle(k)
	}
	for _, k := range theirs.order {
		handle(k)
	}
	return out, conflicts
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func renderKVValues(spell string, v []string, present bool) string {
	if !present {
		return ""
	}
	var b strings.Builder
	for _, one := range v {
		b.WriteString(spell)
		b.WriteString(": ")
		b.WriteString(one)
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// renderKVLine renders a variable or header line, expanding the joined value
// form into one line per occurrence.
func renderKVLine(l kvLine, kind string) string {
	if l.conflict != nil {
		return conflictBlock(l.conflict.a, l.conflict.b)
	}
	var b strings.Builder
	for _, v := range l.values {
		if kind == "var" {
			b.WriteString("@")
			b.WriteString(l.name)
			b.WriteString(" = ")
			b.WriteString(v)
			b.WriteString("\n")
			continue
		}
		b.WriteString(l.name)
		b.WriteString(":")
		if v != "" {
			b.WriteString(" ")
			b.WriteString(v)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// mergeAsserts is the assertion union: an assertion added by either side lives,
// a removal by either side removes it, and two different additions never
// conflict because assertions are independent.
func mergeAsserts(base, ours, theirs []string) []string {
	inBase := setOf(base)
	inOurs := setOf(ours)
	inTheirs := setOf(theirs)
	var out []string
	seen := map[string]bool{}
	take := func(e string) {
		if seen[e] {
			return
		}
		seen[e] = true
		out = append(out, e)
	}
	for _, e := range ours {
		if inTheirs[e] || !inBase[e] {
			take(e)
		}
	}
	for _, e := range theirs {
		if !inBase[e] && !inOurs[e] {
			take(e)
		}
	}
	return out
}

func setOf(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// conflictCount counts the conflict regions a merged request contributes.
func conflictCount(mr mergedRequest) int {
	if mr.conflict != nil {
		return 1
	}
	n := 0
	for _, l := range mr.vars {
		if l.conflict != nil {
			n++
		}
	}
	for _, l := range mr.headers {
		if l.conflict != nil {
			n++
		}
	}
	return n
}

// renderRequest emits a request, copying the current side verbatim when the
// merge changed nothing.
func (m *merger) renderRequest(doc *ast.Document, r *ast.Request, mr mergedRequest, ours reqModel) string {
	if mr.conflict == nil && conflictCount(mr) == 0 && mergedModelEqual(mr, ours) {
		return string(doc.RequestText(r))
	}
	if mr.conflict != nil {
		return conflictBlock(mr.conflict.a, mr.conflict.b)
	}
	return renderBody(mr)
}

// renderBody is the canonical emitter shared by merged and side rendering.
func renderBody(mr mergedRequest) string {
	var b strings.Builder
	for _, l := range mr.vars {
		b.WriteString(renderKVLine(l, "var"))
	}
	if mr.name != "" {
		b.WriteString("# @name ")
		b.WriteString(mr.name)
		b.WriteString("\n")
	}
	for _, a := range mr.asserts {
		b.WriteString("# @assert ")
		b.WriteString(a)
		b.WriteString("\n")
	}
	if mr.hook != "" {
		b.WriteString("# @wasm ")
		b.WriteString(mr.hook)
		b.WriteString("\n")
	}
	if mr.sign != "" {
		b.WriteString("@wasm-sign ")
		b.WriteString(mr.sign)
		b.WriteString("\n")
	}
	b.WriteString(mr.method)
	b.WriteString(" ")
	b.WriteString(mr.url)
	b.WriteString("\n")
	for _, l := range mr.headers {
		b.WriteString(renderKVLine(l, "header"))
	}
	b.WriteString("\n")
	b.WriteString(mr.body)
	return strings.TrimSuffix(b.String(), "\n")
}

// mergedModelEqual reports whether the merged scalar fields and assertions
// still match the current side, so an untouched request can be copied verbatim.
func mergedModelEqual(mr mergedRequest, ours reqModel) bool {
	return mr.name == ours.name && mr.method == ours.method && mr.url == ours.url &&
		mr.hook == ours.hook && mr.sign == ours.sign && mr.body == ours.body &&
		strings.Join(mr.asserts, "\x00") == strings.Join(ours.asserts, "\x00") &&
		kvLinesMatch(mr.vars, ours.vars) && kvLinesMatch(mr.headers, ours.headers)
}

// kvLinesMatch compares rendered fields against the current side's model. It is
// what lets renderRequest copy an untouched request verbatim: the scalar fields
// alone are not enough, because an incoming header or variable can merge into a
// request whose method, URL and body did not move.
func kvLinesMatch(lines []kvLine, model kvModel) bool {
	if len(lines) != len(model.order) {
		return false
	}
	for i, l := range lines {
		key := model.order[i]
		if l.conflict != nil || l.key != key || !sliceEqual(l.values, model.vals[key]) {
			return false
		}
	}
	return true
}

// conflictBlock wraps two sides in the standard Git markers.
func conflictBlock(a, b string) string {
	return mergeOursStart + "\n" + a + "\n" + mergeSeparator + "\n" + b + "\n" + mergeTheirsEnd
}

// reqEntry is one indexed request plus its model.
type reqEntry struct {
	request *ast.Request
	model   reqModel
	key     string
}

// indexRequests builds key -> entry, numbering duplicate identities so a file
// that repeats a name still merges positionally.
func indexRequests(doc *ast.Document) map[string]*reqEntry {
	out := map[string]*reqEntry{}
	for _, e := range listRequests(doc) {
		out[e.key] = e
	}
	return out
}

// listRequests returns the document's requests in source order with their keys.
func listRequests(doc *ast.Document) []*reqEntry {
	var out []*reqEntry
	count := map[string]int{}
	for r := range doc.Requests() {
		mdl := modelOf(doc, r)
		base := mdl.key()
		count[base]++
		key := base
		if n := count[base]; n > 1 {
			key = fmt.Sprintf("%s#%d", base, n)
		}
		out = append(out, &reqEntry{request: r, model: mdl, key: key})
	}
	return out
}

// appliedKV reports whether rendered lines reproduce exactly the current side's
// variables, i.e. the merge made no change to them.
func appliedKV(current kvModel, lines []kvLine) bool {
	var got strings.Builder
	for _, l := range lines {
		got.WriteString(renderKVLine(l, "var"))
	}
	var want strings.Builder
	for _, key := range current.order {
		for _, v := range current.vals[key] {
			want.WriteString("@")
			want.WriteString(current.spells[key])
			want.WriteString(" = ")
			want.WriteString(v)
			want.WriteString("\n")
		}
	}
	return got.String() == want.String()
}
