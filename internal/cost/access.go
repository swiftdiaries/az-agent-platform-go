package cost

import (
	"fmt"
	"log/slog"
	"net/http"
)

const redacted = "[REDACTED]"

// decoration holds the headers to set (non-empty value) or remove (empty
// value). It sits behind a pointer so fmt's reflection fallbacks print only
// an address, never the values.
type decoration struct {
	set map[string]string
}

// Access is an opaque, redacting request decorator. The zero value is valid
// and applies nothing. Every formatting, serialization and logging path
// yields "[REDACTED]".
type Access struct {
	d *decoration
}

// Apply decorates req in place, replacing same-named headers. It is a no-op
// for the zero Access or a nil req.
func (a Access) Apply(req *http.Request) {
	if a.d == nil || req == nil {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header, len(a.d.set))
	}
	for name, v := range a.d.set {
		if v == "" {
			req.Header.Del(name)
			continue
		}
		req.Header.Set(name, v)
	}
}

func (a Access) String() string   { return redacted }
func (a Access) GoString() string { return redacted }

// Format writes the redaction marker for every verb.
func (a Access) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// MarshalJSON returns the JSON string "[REDACTED]".
func (a Access) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// LogValue returns the redaction marker as an slog value.
func (a Access) LogValue() slog.Value { return slog.StringValue(redacted) }
