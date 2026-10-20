package cost

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
)

// HeaderNames maps attribution fields to header names; "" = not sent.
type HeaderNames struct{ Tenant, Principal, Conversation, Run, Journey, Profile string }

// DefaultHeaderNames returns the default header names.
func DefaultHeaderNames() HeaderNames {
	return HeaderNames{
		Tenant:       "X-Agent-Tenant",
		Principal:    "X-Agent-Principal",
		Conversation: "X-Agent-Conversation",
		Run:          "X-Agent-Run",
		Journey:      "X-Agent-Journey",
		Profile:      "X-Agent-Cost-Profile",
	}
}

var errInvalidNames = errors.New("invalid cost header names")

// reserved lists headers that must never carry attribution, keyed by
// canonical name: the internal/credential transport set plus credential
// carriers.
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
	"Authorization":        {},
	"Proxy-Authorization":  {},
	"Cookie":               {},
	"Api-Key":              {},
	"X-Api-Key":            {},
}

// ParseHeaderNames parses AGENT_PLATFORM_COST_HEADERS: comma-separated
// key=Header-Name pairs with keys in {tenant, principal, conversation, run,
// journey, profile}. Blank yields DefaultHeaderNames(). Errors never echo the
// value.
func ParseHeaderNames(value string) (HeaderNames, error) {
	if strings.TrimSpace(value) == "" {
		return DefaultHeaderNames(), nil
	}
	var n HeaderNames
	seen := map[string]struct{}{}
	for _, part := range strings.Split(value, ",") {
		key, name, ok := strings.Cut(strings.Trim(part, " \t"), "=")
		if !ok {
			return HeaderNames{}, errInvalidNames
		}
		key, name = strings.Trim(key, " \t"), strings.Trim(name, " \t")
		if _, dup := seen[key]; dup {
			return HeaderNames{}, errInvalidNames
		}
		seen[key] = struct{}{}
		var dst *string
		switch key {
		case "tenant":
			dst = &n.Tenant
		case "principal":
			dst = &n.Principal
		case "conversation":
			dst = &n.Conversation
		case "run":
			dst = &n.Run
		case "journey":
			dst = &n.Journey
		case "profile":
			dst = &n.Profile
		default:
			return HeaderNames{}, errInvalidNames
		}
		if name == "" {
			return HeaderNames{}, errInvalidNames
		}
		*dst = name
	}
	if err := n.normalize(); err != nil {
		return HeaderNames{}, err
	}
	return n, nil
}

// normalize canonicalizes and validates the non-empty names.
func (n *HeaderNames) normalize() error {
	seen := map[string]struct{}{}
	for _, p := range []*string{&n.Tenant, &n.Principal, &n.Conversation, &n.Run, &n.Journey, &n.Profile} {
		if *p == "" {
			continue
		}
		if !isToken(*p) {
			return errInvalidNames
		}
		c := http.CanonicalHeaderKey(*p)
		if _, bad := reserved[c]; bad {
			return errInvalidNames
		}
		if _, dup := seen[c]; dup {
			return errInvalidNames
		}
		seen[c] = struct{}{}
		*p = c
	}
	return nil
}

// Headers is the header-attribution gate: Admit always allows; Access sets
// each configured header to the attribution value.
type Headers struct {
	names HeaderNames
}

var _ Gate = (*Headers)(nil)

// NewHeaders validates names like ParseHeaderNames.
func NewHeaders(names HeaderNames) (*Headers, error) {
	if err := names.normalize(); err != nil {
		return nil, err
	}
	return &Headers{names: names}, nil
}

func (h *Headers) Admit(context.Context, Attribution, settings.Cost) (Decision, error) {
	return Allow(), nil
}

// Access sets each configured header to its value. Empty values and values
// containing control characters are omitted, and any existing header of that
// name is removed.
func (h *Headers) Access(_ context.Context, a Attribution, s settings.Cost) (Access, error) {
	set := map[string]string{}
	for _, f := range []struct{ name, val string }{
		{h.names.Tenant, a.Tenant},
		{h.names.Principal, a.Principal},
		{h.names.Conversation, a.Conversation},
		{h.names.Run, a.Run},
		{h.names.Journey, a.Journey},
		{h.names.Profile, s.Profile},
	} {
		if f.name == "" {
			continue
		}
		if hasControl(f.val) {
			f.val = ""
		}
		set[f.name] = f.val
	}
	if len(set) == 0 {
		return Access{}, nil
	}
	return Access{d: &decoration{set: set}}, nil
}

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}
