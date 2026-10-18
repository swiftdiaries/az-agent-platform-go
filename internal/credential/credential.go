// Package credential carries the UI user's auth headers from the inbound
// request to outbound MCP calls.
//
// Invariant: credentials live only in the memory of the run that owns them.
// They are never written to definitions, history, inbox, events, traces or
// logs. The invariant is enforced by type: Credential is opaque, every
// formatting, serialization and logging path yields a constant redaction
// marker, and the only way to use one is Apply.
//
// This is a leaf package: it imports only the standard library.
package credential

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

const redacted = "[REDACTED]"

// secret holds the forwarded headers. It sits behind a pointer so that fmt's
// reflection fallbacks (the %p bad-verb path, and values in unexported fields
// of other structs) print only an address, never the headers.
type secret struct {
	headers http.Header
}

// Credential is a request-scoped set of auth headers. The zero value is valid
// and empty; applying it is a no-op. Credential values are immutable.
type Credential struct {
	s *secret
}

// FromRequest copies the named headers (all values, deep-copied) from r into a
// new Credential. Headers not named are never copied. It returns the zero
// Credential when r is nil, names is empty, or none of the named headers are
// present.
func FromRequest(r *http.Request, names []string) Credential {
	if r == nil || len(names) == 0 {
		return Credential{}
	}
	var headers http.Header
	for _, name := range names {
		key := http.CanonicalHeaderKey(name)
		values := r.Header.Values(key)
		if len(values) == 0 {
			continue
		}
		if headers == nil {
			headers = make(http.Header, len(names))
		}
		headers[key] = append([]string(nil), values...)
	}
	if headers == nil {
		return Credential{}
	}
	return Credential{s: &secret{headers: headers}}
}

// Apply sets the credential's headers on req, replacing any existing header of
// the same name. It never mutates the credential and is a no-op for the zero
// Credential.
func (c Credential) Apply(req *http.Request) {
	if c.s == nil || req == nil {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header, len(c.s.headers))
	}
	for name, values := range c.s.headers {
		req.Header.Del(name)
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
}

// String returns the redaction marker.
func (c Credential) String() string { return redacted }

// GoString returns the redaction marker.
func (c Credential) GoString() string { return redacted }

// Format writes the redaction marker for every verb.
func (c Credential) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redacted))
}

// MarshalJSON returns the JSON string "[REDACTED]".
func (c Credential) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redacted + `"`), nil
}

// LogValue returns the redaction marker as an slog value.
func (c Credential) LogValue() slog.Value { return slog.StringValue(redacted) }

var errInvalidName = errors.New("invalid credential header name")

// reserved lists transport/protocol headers that must never be sourced from a
// credential, keyed by canonical name.
var reserved = map[string]struct{}{
	"Host":                 {},
	"Content-Length":       {},
	"Content-Type":         {},
	"Transfer-Encoding":    {},
	"Connection":           {},
	"Accept":               {},
	"Mcp-Session-Id":       {},
	"Mcp-Protocol-Version": {},
	"Last-Event-Id":        {},
	"Traceparent":          {},
	"Tracestate":           {},
}

// ParseHeaderNames parses the comma-separated AGENT_PLATFORM_CREDENTIAL_HEADERS
// value into canonical header names, in input order. A blank value yields nil.
// Each entry must be a non-empty RFC 7230 token, unique after canonicalization,
// and not a reserved transport/protocol header. Errors never echo the value.
func ParseHeaderNames(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	names := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.Trim(part, " \t")
		if !isToken(part) {
			return nil, errInvalidName
		}
		name := http.CanonicalHeaderKey(part)
		if _, bad := reserved[name]; bad {
			return nil, errInvalidName
		}
		if _, dup := seen[name]; dup {
			return nil, errInvalidName
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

// isToken reports whether s is a non-empty RFC 7230 token.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isTchar(s[i]) {
			return false
		}
	}
	return true
}

func isTchar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}
