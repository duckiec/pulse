package ast

import (
	"fmt"
	"net/url"
	"strings"
)

// ExtractedSecret is one literal secret lifted out of an imported command.
// The returned request carries only the Placeholder (`{{NAME}}`); Value travels
// exclusively through this list so the caller can offer a vault-seal flow.
// Source names where the raw value came from (`header:Authorization`,
// `flag:--user`, `query:api_key`) and never carries the value itself.
type ExtractedSecret struct {
	Placeholder string
	Value       string
	Source      string
}

// tokenizeCurl splits a shell command into words: single quotes (literal),
// double quotes (`\\`, `\"`, `\$`, backtick escapes), ANSI-C `$'...'` quotes,
// backslash-newline continuations, and backslash escapes outside quotes.
// Unterminated quotes fail closed.
func tokenizeCurl(s string) ([]string, error) {
	var words []string
	var cur []byte
	var inS, inD, inA bool // single, double, ANSI-C quote
	esc := false           // backslash seen inside "" or outside quotes
	ansiEsc := false       // backslash seen inside $''
	started := false       // current word has content (incl. empty quotes)

	flush := func() {
		if started {
			words = append(words, string(cur))
			cur = cur[:0]
			started = false
		}
	}
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case ansiEsc:
			ansiEsc = false
			started = true
			switch c {
			case 'n':
				cur = append(cur, '\n')
			case 't':
				cur = append(cur, '\t')
			case 'r':
				cur = append(cur, '\r')
			case '0':
				cur = append(cur, 0)
			case 'x':
				if i+2 >= len(s) {
					return nil, fmt.Errorf("ast: curl: truncated \\x escape in $'...' quote")
				}
				hi, ok1 := hexVal(s[i+1])
				lo, ok2 := hexVal(s[i+2])
				if !ok1 || !ok2 {
					return nil, fmt.Errorf("ast: curl: bad \\x escape in $'...' quote")
				}
				cur = append(cur, hi<<4|lo)
				i += 2
			default:
				// Unknown ANSI-C escapes keep the backslash: visible, never
				// a silently wrong byte.
				cur = append(cur, '\\', c)
			}
		case inA:
			if c == '\\' {
				ansiEsc = true
			} else if c == '\'' {
				inA = false
			} else {
				cur = append(cur, c)
			}
			started = true
		case esc:
			esc = false
			if c == '\n' {
				break // line continuation: dropped, starts no word
			}
			started = true
			if inD {
				// Inside double quotes a backslash survives unless it
				// escapes $, `, ", \, or newline (shell rule).
				if c != '$' && c != '`' && c != '"' && c != '\\' {
					cur = append(cur, '\\')
				}
			}
			cur = append(cur, c)
		case inS:
			started = true
			if c == '\'' {
				inS = false
			} else {
				cur = append(cur, c)
			}
		case inD:
			started = true
			if c == '"' {
				inD = false
			} else if c == '\\' {
				esc = true
			} else {
				cur = append(cur, c)
			}
		case c == '\'':
			inS = true
			started = true
		case c == '"':
			inD = true
			started = true
		case c == '\\':
			esc = true
		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			inA = true
			started = true
			i++
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		default:
			started = true
			cur = append(cur, c)
		}
		i++
	}
	if esc {
		// Trailing lone backslash: literal, matching shell behavior closely
		// enough for pasted commands without inventing content.
		cur = append(cur, '\\')
		started = true
	}
	if inS || inD || inA {
		return nil, fmt.Errorf("ast: curl: unterminated quote")
	}
	flush()
	return words, nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// curlStringFlags are long flags (and short pairs) taking a value, including
// the `--flag=value` single-token form.
var curlShortFlags = map[string]string{
	"-X": "request",
	"-H": "header",
	"-d": "data",
	"-A": "agent",
	"-b": "cookie",
	"-u": "user",
}

// curlLongFlags maps every supported long flag to its canonical name.
var curlLongFlags = map[string]string{
	"request": "request", "header": "header", "data": "data",
	"data-raw": "data", "data-binary": "data", "data-ascii": "data",
	"data-urlencode": "data-urlencode", "json": "json", "user": "user",
	"user-agent": "agent", "cookie": "cookie", "location": "location",
	"compressed": "compressed", "url": "url",
}

// ImportCurl parses a copied `curl` command into a PlainRequest plus the
// secrets lifted out of it. The request carries only `{{PLACEHOLDER}}`
// references; raw values travel exclusively in the returned secrets list.
// Unknown flags, multiple URLs, and `@file` bodies fail closed with the
// offending input named.
func ImportCurl(command string) (PlainRequest, []ExtractedSecret, error) {
	words, err := tokenizeCurl(command)
	if err != nil {
		return PlainRequest{}, nil, err
	}
	if len(words) == 0 || words[0] != "curl" {
		return PlainRequest{}, nil, fmt.Errorf("ast: curl: command must start with curl")
	}
	var (
		method    string
		methodSet bool
		rawURL    string
		urlSet    bool
		datas     []string
		jsonData  bool
		basicUser string
		basicSet  bool
		req       PlainRequest
	)
	// flagVal consumes `--flag value` or `--flag=value` uniformly.
	flagVal := func(w string, i *int, name string) (string, error) {
		if eq := strings.IndexByte(w, '='); eq >= 0 {
			return w[eq+1:], nil
		}
		*i++
		if *i >= len(words) {
			return "", fmt.Errorf("ast: curl: flag %s needs a value", name)
		}
		return words[*i], nil
	}
	addHeader := func(name, value string) {
		for i := range req.Headers {
			if strings.EqualFold(req.Headers[i].Name, name) && name != "Cookie" {
				req.Headers[i].Value = value
				return
			}
		}
		req.Headers = append(req.Headers, PlainHeader{Name: name, Value: value})
	}
	i := 1
	positionals := 0
	for ; i < len(words); i++ {
		w := words[i]
		if w == "--" {
			i++
			break
		}
		name := ""
		isFlag := false
		if v, ok := curlShortFlags[w]; ok {
			name, isFlag = v, true
		} else if strings.HasPrefix(w, "--") {
			base := w[2:]
			if eq := strings.IndexByte(base, '='); eq >= 0 {
				base = base[:eq]
			}
			if v, ok := curlLongFlags[base]; ok {
				name, isFlag = v, true
			} else {
				return PlainRequest{}, nil, fmt.Errorf("ast: curl: unsupported flag %q", w)
			}
		} else if strings.HasPrefix(w, "-") && len(w) > 1 {
			return PlainRequest{}, nil, fmt.Errorf("ast: curl: unsupported flag %q", w)
		}
		if !isFlag {
			positionals++
			if positionals > 1 {
				return PlainRequest{}, nil, fmt.Errorf("ast: curl: multiple URLs %q and %q", rawURL, w)
			}
			rawURL, urlSet = w, true
			continue
		}
		val, err := flagVal(w, &i, name)
		if err != nil {
			return PlainRequest{}, nil, err
		}
		switch name {
		case "request":
			method = strings.ToUpper(val)
			methodSet = true
			switch method {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
			default:
				return PlainRequest{}, nil, fmt.Errorf("ast: curl: unsupported method %q", val)
			}
		case "header":
			colon := strings.IndexByte(val, ':')
			if colon < 0 {
				return PlainRequest{}, nil, fmt.Errorf("ast: curl: header %q has no ':'", val)
			}
			addHeader(strings.TrimSpace(val[:colon]), strings.TrimSpace(val[colon+1:]))
		case "data":
			if strings.HasPrefix(val, "@") {
				return PlainRequest{}, nil, fmt.Errorf("ast: curl: @file bodies are not imported; paste the body text")
			}
			datas = append(datas, val)
		case "data-urlencode":
			v := val
			if eq := strings.IndexByte(val, '='); eq >= 0 {
				v = val[:eq+1] + url.QueryEscape(val[eq+1:])
			} else {
				v = url.QueryEscape(val)
			}
			datas = append(datas, v)
		case "json":
			datas = append(datas, val)
			jsonData = true
		case "user":
			basicUser, basicSet = val, true
		case "agent":
			addHeader("User-Agent", val)
		case "cookie":
			merged := val
			for j := range req.Headers {
				if strings.EqualFold(req.Headers[j].Name, "Cookie") {
					merged = req.Headers[j].Value + "; " + val
					req.Headers[j].Value = merged
					merged = ""
					break
				}
			}
			if merged != "" {
				req.Headers = append(req.Headers, PlainHeader{Name: "Cookie", Value: val})
			}
		case "url":
			rawURL, urlSet = val, true
			positionals++
		case "location", "compressed":
			// Transport hints the engine already owns: follow-redirects and
			// accept-encoding are engine behavior, not request content.
		}
	}
	for ; i < len(words); i++ {
		positionals++
		if positionals > 1 {
			return PlainRequest{}, nil, fmt.Errorf("ast: curl: multiple URLs %q and %q", rawURL, words[i])
		}
		rawURL, urlSet = words[i], true
	}
	if !urlSet {
		return PlainRequest{}, nil, fmt.Errorf("ast: curl: no URL in command")
	}
	norm, err := NormalizeURL(rawURL)
	if err != nil {
		return PlainRequest{}, nil, err
	}
	req.URL = norm
	switch {
	case methodSet:
		req.Method = method
	case len(datas) > 0:
		req.Method = "POST"
	default:
		req.Method = "GET"
	}
	if len(datas) > 0 {
		req.Body = strings.Join(datas, "&")
	}
	if jsonData {
		hasCT := false
		for _, h := range req.Headers {
			if strings.EqualFold(h.Name, "Content-Type") {
				hasCT = true
				break
			}
		}
		if !hasCT {
			req.Headers = append(req.Headers, PlainHeader{Name: "Content-Type", Value: "application/json"})
		}
	}
	secrets := extractSecrets(&req, basicUser, basicSet)
	return req, secrets, nil
}

// sensitiveHeaders always carry credential material: their values are replaced
// wholesale, never inspected for "looks like a secret" cleverness.
var sensitiveHeaders = map[string]string{
	"authorization":       "AUTH_TOKEN",
	"proxy-authorization": "PROXY_TOKEN",
	"x-api-key":           "API_KEY",
	"api-key":             "API_KEY",
	"apikey":              "API_KEY",
	"x-auth-token":        "API_KEY",
	"x-token":             "API_KEY",
	"token":               "API_KEY",
	"cookie":              "COOKIE",
}

// suspiciousQueryKeys marks query parameters whose values are secrets by
// position, whatever they look like.
func suspiciousQueryKeys(key string) bool {
	k := strings.ToLower(key)
	for _, sub := range []string{"token", "api_key", "apikey", "api-key", "secret", "password", "passwd", "auth", "access_key", "client_secret"} {
		if strings.Contains(k, sub) {
			return true
		}
	}
	return false
}

// highEntropyValue reports long, dense, token-shaped values: at least 20
// characters with 80%+ from the base64url-ish alphabet.
func highEntropyValue(v string) bool {
	if len(v) < 20 {
		return false
	}
	dense := 0
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '+' || c == '/' || c == '=' || c == '_' || c == '-' || c == '.' || c == '%' {
			dense++
		}
	}
	return dense*5 >= len(v)*4
}

// extractSecrets rewrites credential-bearing fields of req in place to
// `{{PLACEHOLDER}}` references and returns the raw values. The mutation gate
// for this function is deletion: removing the rewrite must turn the
// credential tests red (the raw secret would appear in the request).
func extractSecrets(req *PlainRequest, basicUser string, basicSet bool) []ExtractedSecret {
	var out []ExtractedSecret
	for i := range req.Headers {
		h := &req.Headers[i]
		lower := strings.ToLower(h.Name)
		ph, sensitive := sensitiveHeaders[lower]
		if !sensitive {
			continue
		}
		src := "header:" + h.Name
		switch {
		case lower == "authorization" && strings.HasPrefix(h.Value, "Bearer "):
			raw := strings.TrimPrefix(h.Value, "Bearer ")
			h.Value = "Bearer {{BEARER_TOKEN}}"
			out = append(out, ExtractedSecret{Placeholder: "BEARER_TOKEN", Value: raw, Source: src})
		case lower == "authorization" && strings.HasPrefix(h.Value, "Basic "):
			raw := strings.TrimPrefix(h.Value, "Basic ")
			h.Value = "Basic {{BASIC_TOKEN}}"
			out = append(out, ExtractedSecret{Placeholder: "BASIC_TOKEN", Value: raw, Source: src})
		case lower == "cookie":
			if h.Value != "" {
				out = append(out, ExtractedSecret{Placeholder: "COOKIE", Value: h.Value, Source: src})
				h.Value = "{{COOKIE}}"
			}
		default:
			if h.Value != "" {
				out = append(out, ExtractedSecret{Placeholder: ph, Value: h.Value, Source: src})
				h.Value = "{{" + ph + "}}"
			}
		}
	}
	if basicSet {
		hasAuth := false
		for _, h := range req.Headers {
			if strings.EqualFold(h.Name, "Authorization") {
				hasAuth = true
				break
			}
		}
		// An explicit Authorization header wins (curl semantics); the -u
		// value is still extracted so the caller can seal rather than drop it.
		out = append(out, ExtractedSecret{Placeholder: "BASIC_AUTH", Value: basicUser, Source: "flag:--user"})
		if !hasAuth {
			req.Headers = append(req.Headers, PlainHeader{Name: "Authorization", Value: "Basic {{BASIC_AUTH}}"})
		}
	}
	// Query parameters: suspicious keys or high-entropy values become
	// {{QUERY_TOKEN}}. The query rebuilds via Encode, which may reorder
	// pairs — documented, and irrelevant to execution semantics.
	if u, err := url.Parse(req.URL); err == nil && u.RawQuery != "" {
		q, err := url.ParseQuery(u.RawQuery)
		if err == nil {
			changed := false
			for key, vals := range q {
				for vi, v := range vals {
					if v != "" && (suspiciousQueryKeys(key) || highEntropyValue(v)) {
						out = append(out, ExtractedSecret{Placeholder: "QUERY_TOKEN", Value: v, Source: "query:" + key})
						vals[vi] = "{{QUERY_TOKEN}}"
						changed = true
					}
				}
				q[key] = vals
			}
			if changed {
				u.RawQuery = q.Encode()
				req.URL = u.String()
			}
		}
	}
	return out
}
