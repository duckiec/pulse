package fuzz_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"pulse/internal/fuzz"
)

// writeDoc writes a fixture document and returns its path.
func writeDoc(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

const yamlDoc = `openapi: 3.0.3
info:
  title: Users
  version: 1.0.0
paths:
  /users/{id}:
    get:
      operationId: getUser
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: integer
            format: int64
            minimum: 1
            maximum: 1000
        - name: verbose
          in: query
          schema:
            type: boolean
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/User'
  /users:
    post:
      operationId: createUser
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/NewUser'
      responses:
        '201':
          description: created
components:
  schemas:
    User:
      type: object
      required: [id]
      properties:
        id:
          type: integer
        name:
          type: string
          minLength: 1
          maxLength: 64
          pattern: '^[a-z]+$'
    NewUser:
      type: object
      required: [name]
      properties:
        name:
          type: string
          minLength: 1
          maxLength: 32
        age:
          type: integer
          minimum: 0
          maximum: 150
        tags:
          type: array
          minItems: 1
          maxItems: 5
          items:
            type: string
`

// TestLoadOpenAPIFromYAML is the primary schema-ingestion gate.
//
// It is the YAML path specifically because the JSON path is `encoding/json`
// and therefore uninteresting — the risk lives entirely in the hand-rolled
// subset parser.
func TestLoadOpenAPIFromYAML(t *testing.T) {
	ir, err := fuzz.LoadOpenAPI(writeDoc(t, "api.yaml", yamlDoc))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	if ir.Version != "3.0.3" {
		t.Errorf("version = %q, want 3.0.3", ir.Version)
	}
	if len(ir.Ops) != 2 {
		t.Fatalf("loaded %d operations, want 2:\n%+v", len(ir.Ops), ir.Ops)
	}

	// Operations must be sorted, or the corpus order depends on map iteration.
	// "/users" sorts before "/users/{id}" because it is a prefix of it; the
	// method only breaks a tie between equal paths.
	if ir.Ops[0].ID != "POST /users" || ir.Ops[1].ID != "GET /users/{id}" {
		t.Errorf("operations are not sorted by (path, method): %s, %s",
			ir.Ops[0].ID, ir.Ops[1].ID)
	}

	get := ir.Op("GET /users/{id}")
	if get == nil {
		t.Fatal("GET /users/{id} not found")
	}
	if len(get.Params) != 2 {
		t.Fatalf("GET has %d params, want 2: %+v", len(get.Params), get.Params)
	}
	// Params sorted by (in, name): "path" < "query".
	if get.Params[0].Name != "id" || get.Params[0].In != "path" {
		t.Errorf("first param = %+v, want the path param", get.Params[0])
	}
	if !get.Params[0].Required {
		t.Error("the path parameter is not marked required")
	}
	idSchema := get.Params[0].Schema
	if idSchema.Minimum == nil || *idSchema.Minimum != 1 {
		t.Errorf("id minimum = %v, want 1", idSchema.Minimum)
	}
	if idSchema.Maximum == nil || *idSchema.Maximum != 1000 {
		t.Errorf("id maximum = %v, want 1000", idSchema.Maximum)
	}
	// The $ref into components must have resolved.
	if len(get.Responses) != 1 {
		t.Errorf("GET has %d responses, want 1: %+v", len(get.Responses), get.Responses)
	}
	if sc := get.Responses["200"]; sc == nil || sc.Properties["name"] == nil {
		t.Error("the 200 response schema did not resolve through $ref")
	} else if p := sc.Properties["name"]; p.MaxLength == nil || *p.MaxLength != 64 {
		t.Errorf("resolved User.name maxLength = %v, want 64", p.MaxLength)
	}

	post := ir.Op("POST /users")
	if post == nil || post.RequestBody == nil {
		t.Fatal("POST /users has no request body")
	}
	if post.RequestBody.Properties["tags"] == nil {
		t.Error("the request body did not resolve through $ref")
	}
	if !post.RequestBody.Properties["tags"].UniqueItems {
		t.Log("uniqueItems absent, as written in the fixture")
	}
}

// TestLoadOpenAPIFromJSON proves the JSON path and the YAML path agree, and
// that format detection is by CONTENT rather than by file extension.
func TestLoadOpenAPIFromJSON(t *testing.T) {
	jsonDoc := `{
	  "openapi": "3.1.0",
	  "info": {"title": "T", "version": "1"},
	  "paths": {
	    "/ping": {
	      "get": {
	        "responses": {"200": {"description": "ok"}}
	      }
	    }
	  }
	}`
	// Named .yaml on purpose: detection must not trust the extension.
	ir, err := fuzz.LoadOpenAPI(writeDoc(t, "api.yaml", jsonDoc))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	if len(ir.Ops) != 1 || ir.Ops[0].ID != "GET /ping" {
		t.Errorf("JSON document loaded as %+v", ir.Ops)
	}
	if ir.Version != "3.1.0" {
		t.Errorf("version = %q", ir.Version)
	}
}

// TestLoadOpenAPIRejectsRemoteRefs proves the offline invariant is enforced.
//
// A remote $ref would be a network fetch triggered by the user merely pointing
// Pulse at a file. That is Pulse making an outbound request on its own
// initiative, which invariant I-3 forbids outright.
func TestLoadOpenAPIRejectsRemoteRefs(t *testing.T) {
	doc := `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /a:
    get:
      parameters:
        - name: x
          in: query
          schema:
            $ref: 'https://example.com/schema.json#/Thing'
      responses:
        '200': {description: ok}
`
	_, err := fuzz.LoadOpenAPI(writeDoc(t, "remote.yaml", doc))
	if err == nil {
		t.Fatal("a remote $ref was accepted; Pulse would have fetched it")
	}
	var remote *fuzz.ErrRemoteRef
	if !errors.As(err, &remote) {
		t.Errorf("error = %v (%T), want *fuzz.ErrRemoteRef", err, err)
	}
}

// TestLoadOpenAPIRejectsRefCycles proves the resolver terminates with a typed
// error rather than recursing until the stack runs out.
func TestLoadOpenAPIRejectsRefCycles(t *testing.T) {
	doc := `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /a:
    get:
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/A'
components:
  schemas:
    A:
      $ref: '#/components/schemas/B'
    B:
      $ref: '#/components/schemas/A'
`
	_, err := fuzz.LoadOpenAPI(writeDoc(t, "cycle.yaml", doc))
	if err == nil {
		t.Fatal("a $ref cycle was accepted")
	}
	var cyc *fuzz.ErrRefCycle
	if !errors.As(err, &cyc) {
		t.Errorf("error = %v (%T), want *fuzz.ErrRefCycle", err, err)
	}
}

// TestLoadOpenAPIRejectsNonOpenAPIInput proves a bad document fails loudly.
//
// Silently fuzzing an empty corpus looks exactly like a clean run, which is the
// worst possible failure for a tool whose output is a pass/fail verdict.
func TestLoadOpenAPIRejectsNonOpenAPIInput(t *testing.T) {
	cases := []struct{ name, body string }{
		{"swagger 2", `{"swagger": "2.0", "paths": {}}`},
		{"no paths", "openapi: 3.0.0\ninfo: {title: T, version: '1'}\n"},
		{"empty", ""},
		{"not a mapping", "- a\n- b\n"},
		{"broken json", `{"openapi": "3.0.0",`},
		{"anchors", "openapi: 3.0.0\ninfo: &a {title: T}\n"},
		{"block scalar", "openapi: 3.0.0\ninfo:\n  title: |\n    text\n"},
		{"tab indentation", "openapi: 3.0.0\ninfo:\n\ttitle: T\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := fuzz.LoadOpenAPI(writeDoc(t, "doc.yaml", c.body))
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			var unsupported *fuzz.ErrUnsupportedDocument
			if !errors.As(err, &unsupported) {
				t.Errorf("error = %v (%T), want *fuzz.ErrUnsupportedDocument", err, err)
			}
			if !strings.Contains(err.Error(), "fuzz:") {
				t.Errorf("error %q is not namespaced to the fuzz package", err)
			}
		})
	}
}

// TestGenerateIsDeterministic is the package's central claim.
//
// Same schema, same seed, byte-identical corpus. Without it a fuzz finding
// cannot be reproduced, and "it stopped happening" is indistinguishable from
// "this run generated different inputs".
func TestGenerateIsDeterministic(t *testing.T) {
	ir, err := fuzz.LoadOpenAPI(writeDoc(t, "api.yaml", yamlDoc))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	op := ir.Op("POST /users")
	if op == nil {
		t.Fatal("POST /users missing")
	}

	first := fuzz.Generate(op, 42, 0)
	if len(first) == 0 {
		t.Fatal("the generator produced no payloads")
	}
	for i := 0; i < 25; i++ {
		got := fuzz.Generate(op, 42, 0)
		if len(got) != len(first) {
			t.Fatalf("run %d produced %d payloads, first run produced %d", i, len(got), len(first))
		}
		for j := range got {
			if payloadSignature(got[j]) != payloadSignature(first[j]) {
				t.Fatalf("run %d payload %d differs:\n %s\n %s",
					i, j, payloadSignature(got[j]), payloadSignature(first[j]))
			}
		}
	}
	t.Logf("%d payloads, byte-identical across 26 generations", len(first))

	// Indices must be dense and ordered: a TAP plan derives from them.
	for i, p := range first {
		if p.Index != i {
			t.Errorf("payload %d has Index %d; indices must be dense and ordered", i, p.Index)
		}
	}
}

// payloadSignature is a comparable rendering of a payload.
func payloadSignature(p fuzz.Payload) string {
	return strings.Join([]string{
		p.OpID, p.Field, p.Location, p.Strategy.String(), p.Note, renderAny(p.Value),
	}, "\x00")
}

func renderAny(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		if len(t) > 24 {
			return "str[" + itoa(len(t)) + "]"
		}
		return t
	default:
		return fmt.Sprintf("%v", v)
	}
}

// TestGenerateCoversDeclaredConstraints is the coverage gate: every constraint
// the schema DECLARES must produce at least one boundary payload.
//
// The point is not that the fuzzer is thorough — it is that it is not silently
// ignoring a constraint it read. A generator that skips `maxLength` because of a
// typo reports a clean run and has tested nothing.
func TestGenerateCoversDeclaredConstraints(t *testing.T) {
	ir, err := fuzz.LoadOpenAPI(writeDoc(t, "api.yaml", yamlDoc))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}

	tests := []struct {
		op    string
		field string
		notes []string
	}{
		{"GET /users/{id}", "id", []string{"min", "min-1", "max", "max+1"}},
		{"POST /users", "name", []string{"minLength-1", "minLength", "maxLength", "maxLength+1"}},
		{"POST /users", "age", []string{"min", "min-1", "max", "max+1"}},
		{"POST /users", "tags", []string{"empty-array", "minItems-1", "minItems", "maxItems", "maxItems+1"}},
	}
	for _, tc := range tests {
		op := ir.Op(tc.op)
		if op == nil {
			t.Errorf("%s not found", tc.op)
			continue
		}
		got := fuzz.Generate(op, 1, 0)
		byField := map[string]map[string]bool{}
		for _, p := range got {
			if byField[p.Field] == nil {
				byField[p.Field] = map[string]bool{}
			}
			byField[p.Field][p.Note] = true
		}
		for _, want := range tc.notes {
			if !byField[tc.field][want] {
				t.Errorf("%s %s: no payload for declared constraint %q; "+
					"present notes: %v", tc.op, tc.field, want, keysOf(byField[tc.field]))
			}
		}
	}
}

// TestGenerateProbesDeclaredPatterns proves a `pattern` produces a
// non-matching probe, and that the probe genuinely does not match.
//
// Without this, a pattern constraint is read off the schema and then never
// used: the field is fuzzed for length and type but not for shape, and the run
// reports coverage it does not have.
func TestGenerateProbesDeclaredPatterns(t *testing.T) {
	doc := `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /slug:
    get:
      parameters:
        - name: slug
          in: query
          schema:
            type: string
            pattern: '^[a-z0-9-]+$'
      responses:
        '200': {description: ok}
`
	ir, err := fuzz.LoadOpenAPI(writeDoc(t, "pattern.yaml", doc))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	re := regexp.MustCompile(`^[a-z0-9-]+$`)

	var found int
	for _, p := range fuzz.Generate(ir.Op("GET /slug"), 1, 0) {
		if p.Note != "pattern-nonmatch" {
			continue
		}
		found++
		s, ok := p.Value.(string)
		if !ok {
			t.Errorf("pattern-nonmatch payload is a %T, want a string", p.Value)
			continue
		}
		if re.MatchString(s) {
			t.Errorf("the non-matching probe %q MATCHES %s", s, re)
		}
	}
	if found == 0 {
		t.Error("a schema with a declared `pattern` produced no pattern-nonmatch probe")
	}
}

// TestGenerateEmitsEveryStrategy proves the strategy families are not empty.
func TestGenerateEmitsEveryStrategy(t *testing.T) {
	ir, err := fuzz.LoadOpenAPI(writeDoc(t, "api.yaml", yamlDoc))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	seen := map[fuzz.Strategy]int{}
	for i := range ir.Ops {
		for _, p := range fuzz.Generate(&ir.Ops[i], 7, 0) {
			seen[p.Strategy]++
		}
	}
	for s := fuzz.Strategy(0); s < fuzz.StrategyCount; s++ {
		if seen[s] == 0 {
			t.Errorf("strategy %s emitted no payloads", s)
		}
	}
	t.Logf("strategy coverage: %v", seen)
}

// TestGenerateHonoursTheBudget proves a budget truncates WITHOUT silently
// dropping whole fields.
//
// A prefix truncation would only ever exercise the first parameter
// alphabetically, so a budget of 10 on a 50-payload corpus would never touch
// `tags` — and the run would report itself as having fuzzed the operation.
func TestGenerateHonoursTheBudget(t *testing.T) {
	ir, err := fuzz.LoadOpenAPI(writeDoc(t, "api.yaml", yamlDoc))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	op := ir.Op("POST /users")
	if op == nil {
		t.Fatal("POST /users missing")
	}
	full := fuzz.Generate(op, 1, 0)

	for _, budget := range []int{1, 5, 17, len(full) - 1} {
		got := fuzz.Generate(op, 1, budget)
		if len(got) != budget {
			t.Errorf("budget %d produced %d payloads", budget, len(got))
		}
		fields := map[string]bool{}
		for i, p := range got {
			if p.Index != i {
				t.Errorf("budget %d: payload %d has Index %d", budget, i, p.Index)
			}
			fields[p.Field] = true
		}
		if budget >= 5 && len(fields) < 2 {
			t.Errorf("budget %d covered only the field(s) %v; truncation dropped "+
				"whole parameters instead of sampling across them", budget, keysOfBool(fields))
		}
	}
}

// TestGenerateCoversEverySchemaType proves no declared type is silently skipped.
func TestGenerateCoversEverySchemaType(t *testing.T) {
	doc := `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /t:
    post:
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                s: {type: string}
                i: {type: integer}
                n: {type: number}
                b: {type: boolean}
                a: {type: array, items: {type: string}}
      responses:
        '200': {description: ok}
`
	ir, err := fuzz.LoadOpenAPI(writeDoc(t, "types.yaml", doc))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	got := fuzz.Generate(ir.Op("POST /t"), 3, 0)
	fields := map[string]int{}
	for _, p := range got {
		fields[p.Field]++
	}
	for _, want := range []string{"s", "i", "n", "b", "a"} {
		if fields[want] == 0 {
			t.Errorf("field %q produced no payloads; fields covered: %v", want, fields)
		}
	}
}

// TestGenerateIsSeedStable proves a different seed changes only the CHOICE of
// equally-valid extras, never the coverage of declared constraints.
//
// If the seed could remove a `maxLength+1` case, a "fixed" finding would
// disappear on re-run and the tool would be reporting on a corpus it invented.
func TestGenerateIsSeedStable(t *testing.T) {
	ir, err := fuzz.LoadOpenAPI(writeDoc(t, "api.yaml", yamlDoc))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	op := ir.Op("POST /users")
	if op == nil {
		t.Fatal("POST /users missing")
	}

	required := []string{"minLength-1", "minLength", "maxLength", "maxLength+1"}
	for _, seed := range []uint64{0, 1, 999, 1 << 40, ^uint64(0)} {
		notes := map[string]bool{}
		for _, p := range fuzz.Generate(op, seed, 0) {
			notes[p.Note] = true
		}
		for _, want := range required {
			if !notes[want] {
				t.Errorf("seed %d dropped the declared-constraint case %q", seed, want)
			}
		}
	}
}

// keysOf lists the keys of a note set, for a readable failure message.
func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOfBool(m map[string]bool) []string { return keysOf(m) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
