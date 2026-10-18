// Package settings resolves layered, typed per-tenant and per-user settings.
//
// Values layer built-in defaults, then the deploy default, then the tenant,
// then the user. A user layer may only narrow limits. Settings are resolved
// once per run and recorded with a version digest of the applied values.
package settings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

const (
	DefaultMaxActiveJourneys = 2
	MinMaxActiveJourneys     = 1
	MaxMaxActiveJourneys     = 4
)

var (
	// ErrUnavailable wraps resolution failures.
	ErrUnavailable = errors.New("settings unavailable")
	// ErrInvalid wraps compile and validation failures.
	ErrInvalid = errors.New("invalid settings")
)

// Scope identifies whose settings are resolved.
type Scope struct{ Tenant, Principal string }

// Cost holds cost-seam settings.
type Cost struct {
	// Profile is an opaque reference interpreted by the cost adapter; "" means none.
	Profile string `json:"profile"`
}

// Settings are the resolved, applied values for one run. Digest is a version
// digest of the applied values.
type Settings struct {
	MaxActiveJourneys int    `json:"max_active_journeys"`
	Cost              Cost   `json:"cost"`
	Digest            string `json:"digest"`
}

// Record returns the JSON the journal stores on the run: applied values and digest.
func (s Settings) Record() json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil { // unreachable: plain strings and ints
		return json.RawMessage(`{}`)
	}
	return b
}

// Source is the settings port.
type Source interface {
	Resolve(ctx context.Context, scope Scope) (Settings, error)
}

// Layer is one partial layer. nil means not set at this layer.
type Layer struct {
	MaxActiveJourneys *int       `json:"max_active_journeys,omitempty" yaml:"max_active_journeys,omitempty"`
	Cost              *CostLayer `json:"cost,omitempty" yaml:"cost,omitempty"`
}

// CostLayer is the partial cost layer.
type CostLayer struct {
	Profile *string `json:"profile,omitempty" yaml:"profile,omitempty"`
}

// Document is the whole layered configuration: deploy default, tenant, user.
type Document struct {
	Default Layer                       `json:"default" yaml:"default"`
	Tenants map[string]Layer            `json:"tenants,omitempty" yaml:"tenants,omitempty"`
	Users   map[string]map[string]Layer `json:"users,omitempty" yaml:"users,omitempty"` // tenant -> principal -> layer
}

// Compiled is an immutable resolver over a validated Document.
type Compiled struct {
	def     Layer
	tenants map[string]Layer
	users   map[string]map[string]Layer
}

var _ Source = (*Compiled)(nil)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func copyLayer(l Layer) Layer {
	var out Layer
	if l.MaxActiveJourneys != nil {
		v := *l.MaxActiveJourneys
		out.MaxActiveJourneys = &v
	}
	if l.Cost != nil {
		c := CostLayer{}
		if l.Cost.Profile != nil {
			p := *l.Cost.Profile
			c.Profile = &p
		}
		out.Cost = &c
	}
	return out
}

func checkBounds(name string, l Layer) error {
	if n := l.MaxActiveJourneys; n != nil && (*n < MinMaxActiveJourneys || *n > MaxMaxActiveJourneys) {
		return invalidf("%s: max_active_journeys %d outside %d-%d", name, *n, MinMaxActiveJourneys, MaxMaxActiveJourneys)
	}
	return nil
}

func apply(s *Settings, l Layer) {
	if l.MaxActiveJourneys != nil {
		s.MaxActiveJourneys = *l.MaxActiveJourneys
	}
	if l.Cost != nil && l.Cost.Profile != nil {
		s.Cost.Profile = *l.Cost.Profile
	}
}

// Compile validates doc and returns an immutable resolver over a deep copy of it.
func Compile(doc Document) (*Compiled, error) {
	c := &Compiled{
		def:     copyLayer(doc.Default),
		tenants: make(map[string]Layer, len(doc.Tenants)),
		users:   make(map[string]map[string]Layer, len(doc.Users)),
	}
	if err := checkBounds("default", c.def); err != nil {
		return nil, err
	}
	for _, t := range sortedKeys(doc.Tenants) {
		if t == "" {
			return nil, invalidf("tenant: empty tenant key")
		}
		l := copyLayer(doc.Tenants[t])
		if err := checkBounds(fmt.Sprintf("tenant %q", t), l); err != nil {
			return nil, err
		}
		c.tenants[t] = l
	}
	for _, t := range sortedKeys(doc.Users) {
		if t == "" {
			return nil, invalidf("user: empty tenant key")
		}
		byPrincipal := make(map[string]Layer, len(doc.Users[t]))
		for _, p := range sortedKeys(doc.Users[t]) {
			if p == "" {
				return nil, invalidf("user %q: empty principal key", t)
			}
			name := fmt.Sprintf("user %q/%q", t, p)
			l := copyLayer(doc.Users[t][p])
			if err := checkBounds(name, l); err != nil {
				return nil, err
			}
			// TODO(settings): user-level cost settings are deferred; cost.profile is
			// tenant-level (and deploy-default) only for now. A future user cost
			// layer must define its own narrowing rule (what "narrower" means for
			// a cost profile) before it is accepted here and applied in Resolve.
			if l.Cost != nil {
				return nil, invalidf("%s: cost cannot be set at user level", name)
			}
			byPrincipal[p] = l
		}
		c.users[t] = byPrincipal
	}
	return c, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Resolve returns the layered settings for scope, with Digest set.
func (c *Compiled) Resolve(ctx context.Context, scope Scope) (Settings, error) {
	if err := ctx.Err(); err != nil {
		return Settings{}, err
	}
	if scope.Tenant == "" {
		return Settings{}, fmt.Errorf("%w: empty tenant", ErrUnavailable)
	}
	return Merge(c.def, c.tenants[scope.Tenant], c.users[scope.Tenant][scope.Principal]), nil
}

// Merge layers built-in defaults, def, tenant, then user: the user's max_active_journeys is clamped to the
// tenant-effective value (min). User cost is ignored (Compile still rejects it). Digest is set. Pure.
func Merge(def, tenant, user Layer) Settings {
	s := Settings{MaxActiveJourneys: DefaultMaxActiveJourneys}
	apply(&s, def)
	apply(&s, tenant)
	if n := user.MaxActiveJourneys; n != nil && *n < s.MaxActiveJourneys {
		s.MaxActiveJourneys = *n
	}
	s.Digest = digestOf(s.MaxActiveJourneys, s.Cost.Profile)
	return s
}

// digestOf is the lowercase hex SHA-256 of the canonical JSON of the applied values.
func digestOf(maxActive int, profile string) string {
	b, _ := json.Marshal(struct {
		Cost              Cost `json:"cost"`
		MaxActiveJourneys int  `json:"max_active_journeys"`
	}{Cost{profile}, maxActive})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Mutable is a concurrency-safe Source for tests.
type Mutable struct {
	mu   sync.RWMutex
	c    *Compiled
	fail error
}

var _ Source = (*Mutable)(nil)

// NewMutable compiles doc into a Mutable.
func NewMutable(doc Document) (*Mutable, error) {
	c, err := Compile(doc)
	if err != nil {
		return nil, err
	}
	return &Mutable{c: c}, nil
}

// Set validates and replaces the document. On error the previous document is kept.
func (m *Mutable) Set(doc Document) error {
	c, err := Compile(doc)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.c = c
	m.mu.Unlock()
	return nil
}

// Fail makes Resolve fail wrapping ErrUnavailable and err; nil clears the failure.
func (m *Mutable) Fail(err error) {
	m.mu.Lock()
	m.fail = err
	m.mu.Unlock()
}

// Resolve implements Source.
func (m *Mutable) Resolve(ctx context.Context, scope Scope) (Settings, error) {
	m.mu.RLock()
	c, fail := m.c, m.fail
	m.mu.RUnlock()
	if fail != nil {
		return Settings{}, fmt.Errorf("%w: %w", ErrUnavailable, fail)
	}
	return c.Resolve(ctx, scope)
}
