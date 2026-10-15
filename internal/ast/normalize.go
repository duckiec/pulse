package ast

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizeURL adds a missing scheme so beginners never hit a cryptic URL
// parser failure on `localhost:8080/health`: loopback targets (localhost,
// 127.0.0.1, [::1], with or without port) default to `http://`, everything
// else to `https://`. Inputs that already carry a scheme, are path-only, or
// are `{{template}}` references pass through untouched. The result always
// re-parses via net/url; anything still invalid fails closed.
func NormalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("ast: normalize: empty URL")
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "{{") {
		return s, nil
	}
	if strings.Contains(s, "://") {
		// A scheme is present: trust the author's choice, but still prove
		// the URL parses — a bare `https://` must not slip through.
		if u, err := url.Parse(s); err != nil || u.Hostname() == "" {
			return "", fmt.Errorf("ast: normalize: invalid URL %q", raw)
		}
		return s, nil
	}
	host := s
	if i := strings.IndexByte(s, '/'); i >= 0 {
		host = s[:i]
	}
	scheme := "https://"
	switch {
	case host == "localhost", host == "127.0.0.1", host == "[::1]",
		strings.HasPrefix(host, "localhost:"),
		strings.HasPrefix(host, "127.0.0.1:"),
		strings.HasPrefix(host, "[::1]:"):
		scheme = "http://"
	}
	out := scheme + s
	u, err := url.Parse(out)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("ast: normalize: invalid URL %q", raw)
	}
	return out, nil
}
