package definitions

import (
	"strings"
)

// Class is the reviewed overlay class of an operation. An unset class means
// "unclassified" and resolves to effectful at the consumer.
type Class string

const (
	ClassReadOnly   Class = "read_only"
	ClassIdempotent Class = "idempotent"
	ClassEffectful  Class = "effectful"
)

// CatalogConfig is the integration input shared by every journey of a set.
type CatalogConfig struct {
	OperationMetaKey string
	CallIDHeader     string
}

// Selector picks operations from a catalog snapshot. Exactly one of Ops
// (alternatives in preference order) and Tags is set.
type Selector struct {
	Ops  []string
	Tags []string
	// Alias is the model-facing tool name: for op selectors the explicit alias
	// or AliasFor(Ops[0]); empty for tag selectors.
	Alias            string
	ReadOnlyHint     bool
	Class            Class
	ApprovalRequired bool
}

// VersionRange is a half-open product version range; either bound may be empty.
type VersionRange struct {
	Min, MaxExclusive string
}

// IsZero reports whether the range has no bounds.
func (r VersionRange) IsZero() bool { return r.Min == "" && r.MaxExclusive == "" }

// Contains reports whether v >= Min (when set) and v < MaxExclusive (when set).
func (r VersionRange) Contains(v string) bool {
	if r.Min != "" && CompareVersions(v, r.Min) < 0 {
		return false
	}
	if r.MaxExclusive != "" && CompareVersions(v, r.MaxExclusive) >= 0 {
		return false
	}
	return true
}

// CompareVersions compares dot-separated versions. Numeric segments compare
// numerically, other segments lexicographically, numeric before non-numeric,
// and a missing segment before a present one.
func CompareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		switch {
		case i >= len(as):
			return -1
		case i >= len(bs):
			return 1
		}
		if c := compareSegment(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return 0
}

func compareSegment(a, b string) int {
	an, bn := isNumeric(a), isNumeric(b)
	switch {
	case an && bn:
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// AliasFor derives the model-facing tool alias from an operation key: every
// byte outside [A-Za-z0-9_] becomes '_'. The alias is valid iff it matches
// ^[A-Za-z_][A-Za-z0-9_]{0,63}$.
func AliasFor(op string) (string, bool) {
	alias := make([]byte, len(op))
	for i := 0; i < len(op); i++ {
		c := op[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			alias[i] = c
		default:
			alias[i] = '_'
		}
	}
	return string(alias), ValidAlias(string(alias))
}

// ValidAlias reports whether s matches ^[A-Za-z_][A-Za-z0-9_]{0,63}$.
func ValidAlias(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// IsReservedToolName reports whether name belongs to the product runtime.
func IsReservedToolName(name string) bool {
	switch name {
	case "request_user_input", "read_skill_resource", "activate_journey":
		return true
	}
	return false
}

func cloneSelectors(in []Selector) []Selector {
	if in == nil {
		return nil
	}
	out := make([]Selector, len(in))
	for i, s := range in {
		s.Ops = append([]string(nil), s.Ops...)
		s.Tags = append([]string(nil), s.Tags...)
		out[i] = s
	}
	return out
}
