package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Validation is schema-first, then semantic (ROADMAP.md §3.1.3).
//
// Agents are loosely-typed senders: numbers arrive as strings, booleans as
// "true", and extra keys appear without warning. Two rules follow, and they are
// deliberately different:
//
//   - COERCION IS OPT-IN AND PER-FIELD. A declared number accepts the string
//     "30000" because the agent's JSON encoder had no other way to say it. An
//     UNDECLARED key is still rejected: silently dropping input the caller
//     believed was honoured is how an agent convinces itself a request was
//     scoped when it was not.
//   - THE SCHEMA IS NOT THE WHOLE CONTRACT. "exactly one of payload/path" cannot
//     be expressed in JSON Schema, so it is checked after, by hand.
//
// Nothing is coerced silently: every coercion is recorded in the outcome's
// warnings so the agent can see what Pulse did on its behalf.

var (
	schemaOnce sync.Once
	execSchema *jsonschema.Schema
	schemaErr  error
)

// toolSchema is the JSON Schema for execute_request arguments.
// Loose typing is declared in the SCHEMA, not smuggled past it.
//
// index and timeout_ms accept an integer OR a string, and include_body accepts
// a boolean OR a string, because MCP clients routinely serialize those as
// strings and rejecting them at the schema would make the recorded coercion in
// asInt/asBool dead code. Everything else stays strict: an unknown key, or an
// integer where a string is wanted, is still rejected outright.
const toolSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "payload":  {"type": "string",  "maxLength": 4194304},
    "path":     {"type": "string",  "maxLength": 4096},
    "index":      {"type": ["integer", "string"], "minimum": 0, "maximum": 4095},
    "timeout_ms": {"type": ["integer", "string"], "minimum": 1, "maximum": 600000},
    "body_mode": {"enum": ["text", "json"]},
    "include_body": {"type": ["boolean", "string"]}
  },
  "additionalProperties": false
}`

func compiledSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		var doc any
		if err := json.Unmarshal([]byte(toolSchema), &doc); err != nil {
			schemaErr = fmt.Errorf("pulse: the built-in tool schema is not valid JSON: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		const uri = "mem://pulse/execute_request.json"
		if err := c.AddResource(uri, doc); err != nil {
			schemaErr = fmt.Errorf("pulse: cannot register the tool schema: %w", err)
			return
		}
		execSchema, schemaErr = c.Compile(uri)
	})
	return execSchema, schemaErr
}

// coercion records that a value was converted on the caller's behalf.
type coercion struct {
	Field  string
	From   string
	To     string
	Reason string
}

// ValidateExecuteRequest validates an agent's arguments and returns a typed
// input. Any failure is a *ProtocolError with CodeInvalidParams: the agent sent
// something unusable and deserves to be told exactly what.
func ValidateExecuteRequest(args map[string]any) (*ToolInput, error) {
	schema, err := compiledSchema()
	if err != nil {
		return nil, WrapProtocolError(CodeInternalError, "the tool schema failed to compile", err)
	}
	if args == nil {
		args = map[string]any{}
	}
	if err := schema.Validate(args); err != nil {
		return nil, WrapProtocolError(CodeInvalidParams,
			"the arguments do not satisfy the tool schema", describeValidation(err))
	}
	in := &ToolInput{}
	var coerced []coercion

	if v, ok := args["payload"]; ok {
		in.Payload = v.(string)
	}
	if v, ok := args["path"]; ok {
		in.Path = v.(string)
	}
	if v, ok := args["index"]; ok {
		n := asInt(v, "index", &coerced)
		in.Index = n
	}
	if v, ok := args["timeout_ms"]; ok {
		n := asInt(v, "timeout_ms", &coerced)
		in.TimeoutMS = n
	}
	if v, ok := args["body_mode"]; ok {
		in.BodyMode = v.(string)
	}
	if v, ok := args["include_body"]; ok {
		b := asBool(v, "include_body", &coerced)
		in.IncludeBody = &b
	}

	if in.TimeoutMS == 0 {
		in.TimeoutMS = 30_000
	}
	if in.BodyMode == "" {
		in.BodyMode = "text"
	}
	if err := semanticCheck(in); err != nil {
		return nil, err
	}
	if len(coerced) > 0 {
		in.Warnings = describeCoercions(coerced)
	}
	return in, nil
}

// semanticCheck enforces the rules JSON Schema cannot express.
func semanticCheck(in *ToolInput) error {
	hasPayload := strings.TrimSpace(in.Payload) != ""
	hasPath := strings.TrimSpace(in.Path) != ""

	switch {
	case !hasPayload && !hasPath:
		return NewProtocolError(CodeInvalidParams,
			`supply exactly one of "payload" (inline .http source) or "path" (a .http file to read); both are absent`)
	case hasPayload && hasPath:
		return NewProtocolError(CodeInvalidParams,
			`"payload" and "path" are mutually exclusive; an agent that sends both has not decided which source is authoritative`)
	}
	if in.BodyMode != "text" && in.BodyMode != "json" {
		return NewProtocolError(CodeInvalidParams,
			fmt.Sprintf(`"body_mode" must be "text" or "json", got %q`, in.BodyMode))
	}
	// A path that does not exist is an argument error, not a transport failure:
	// reporting it as -32602 lets the agent correct the call, where a -32005
	// would send it hunting for a network problem.
	if in.Path != "" && !pathExists(in.Path) {
		return NewProtocolError(CodeInvalidParams,
			fmt.Sprintf(`"path" does not name a readable file: %s`, in.Path))
	}
	return nil
}

// asInt converts a JSON number, or the string form of one, to an int.
//
// The string case is accepted because MCP clients commonly serialize integers as
// strings; it is recorded rather than silent so the agent can see it.
func asInt(v any, field string, out *[]coercion) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			*out = append(*out, coercion{field, "number", "0", "not an integer; defaulted to 0"})
			return 0
		}
		*out = append(*out, coercion{field, "number", "integer", "the client sent a JSON number"})
		return int(n)
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err != nil {
			*out = append(*out, coercion{field, t, "0", "not a parseable integer; defaulted to 0"})
			return 0
		}
		*out = append(*out, coercion{
			Field: field, From: fmt.Sprintf("string %q", t), To: fmt.Sprintf("integer %d", n),
			Reason: "the client serialized an integer as a string",
		})
		return n
	default:
		*out = append(*out, coercion{field, fmt.Sprintf("%T", v), "0", "unrecognized type; defaulted to 0"})
		return 0
	}
}

// asBool converts the several shapes agents use for booleans.
func asBool(v any, field string, out *[]coercion) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b := strings.EqualFold(strings.TrimSpace(t), "true")
		*out = append(*out, coercion{
			Field: field, From: fmt.Sprintf("string %q", t), To: fmt.Sprintf("%t", b),
			Reason: "the client serialized a boolean as a string",
		})
		return b
	case float64:
		b := t != 0
		*out = append(*out, coercion{field, "number", fmt.Sprintf("%t", b), "the client sent a number for a boolean"})
		return b
	default:
		*out = append(*out, coercion{field, fmt.Sprintf("%T", v), "false", "unrecognized type; defaulted to false"})
		return false
	}
}

func describeCoercions(cs []coercion) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%s: %s -> %s (%s)", c.Field, c.From, c.To, c.Reason))
	}
	return out
}

// describeValidation renders a schema failure for an agent: the instance path,
// what was expected, and what arrived.
func describeValidation(err error) error {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	// The message comes from e.Error(), NOT e.ErrorKind.LocalizedString(nil).
	// The latter dereferences the printer inside the kind's message template and
	// panics on a nil message.Printer, which would turn every schema failure into
	// an internal error instead of the actionable -32602 the agent needs. A
	// schema violation is the one failure mode here that must never be masked.
	var parts []string
	var walk func(e *jsonschema.ValidationError, path string)
	walk = func(e *jsonschema.ValidationError, path string) {
		loc := path
		if len(e.InstanceLocation) > 0 {
			if loc != "" {
				loc += "."
			}
			loc += strings.Join(e.InstanceLocation, ".")
		}
		if loc == "" {
			loc = "(arguments)"
		}
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c, loc)
			}
			return
		}
		parts = append(parts, fmt.Sprintf("%s: %s", loc, e.Error()))
	}
	walk(ve, "")
	if len(parts) == 0 {
		return ve
	}
	sortStrings(parts)
	return errors.New(strings.Join(parts, "; "))
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// pathExists is a seam for tests; production reads the real filesystem.
var pathExists = func(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
