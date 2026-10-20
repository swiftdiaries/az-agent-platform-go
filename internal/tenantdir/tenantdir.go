// Package tenantdir maps a tenant to a product version and an MCP endpoint ID,
// and turns endpoint IDs into URLs only through a deploy-time allowlist.
//
// The Directory port answers "which endpoint ID does this tenant use"; it never
// yields a URL. Allowlist (AGENT_PLATFORM_MCP_ENDPOINTS) is the sole source of
// URLs, and Endpoint's fields are unexported so a usable Endpoint can only be
// produced by Allowlist.Endpoint. Every failure wraps ErrUnavailable so callers
// can fail closed before any request is sent. Errors never contain URLs.
package tenantdir

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// ErrUnavailable marks any directory or allowlist failure: unknown or empty
// tenant, injected failure, or a non-allowlisted endpoint.
var ErrUnavailable = errors.New("tenant directory unavailable")

// maxEndpointIDLen is the longest accepted endpoint ID in bytes.
const maxEndpointIDLen = 128

// Entry is a directory record for one tenant.
type Entry struct {
	ProductVersion string
	EndpointID     string
}

// Directory resolves a tenant to its product version and endpoint ID.
type Directory interface {
	Resolve(ctx context.Context, tenant string) (Entry, error)
}

// ValidEndpointID reports whether id is non-empty, at most 128 bytes, and made
// only of [A-Za-z0-9._-].
func ValidEndpointID(id string) bool {
	if id == "" || len(id) > maxEndpointIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// Mutable is a concurrency-safe in-memory Directory for tests.
type Mutable struct {
	mu      sync.RWMutex
	entries map[string]Entry
	failure error
}

// NewMutable returns a Mutable seeded with a copy of entries.
func NewMutable(entries map[string]Entry) *Mutable {
	m := &Mutable{entries: make(map[string]Entry, len(entries))}
	for k, v := range entries {
		m.entries[k] = v
	}
	return m
}

// Set adds or replaces the entry for tenant.
func (m *Mutable) Set(tenant string, e Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[tenant] = e
}

// Delete removes the entry for tenant.
func (m *Mutable) Delete(tenant string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, tenant)
}

// Fail makes every Resolve fail wrapping ErrUnavailable and err. A nil err
// clears the failure.
func (m *Mutable) Fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failure = err
}

// Resolve implements Directory.
func (m *Mutable) Resolve(ctx context.Context, tenant string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	m.mu.RLock()
	failure := m.failure
	e, ok := m.entries[tenant]
	m.mu.RUnlock()

	if failure != nil {
		return Entry{}, fmt.Errorf("%w: %w", ErrUnavailable, failure)
	}
	if tenant == "" {
		return Entry{}, fmt.Errorf("%w: empty tenant", ErrUnavailable)
	}
	if !ok {
		return Entry{}, fmt.Errorf("%w: unknown tenant", ErrUnavailable)
	}
	if e.ProductVersion == "" || !ValidEndpointID(e.EndpointID) {
		return Entry{}, fmt.Errorf("%w: invalid directory entry", ErrUnavailable)
	}
	return e, nil
}

// Endpoint is an allowlist-resolved MCP endpoint. Its fields are unexported, so
// only Allowlist.Endpoint can construct a usable one; the zero value is invalid.
type Endpoint struct {
	id  string
	url string
}

// ID returns the endpoint ID.
func (e Endpoint) ID() string { return e.id }

// URL returns the allowlisted endpoint URL.
func (e Endpoint) URL() string { return e.url }

// Valid reports whether e was produced by an Allowlist (false for the zero value).
func (e Endpoint) Valid() bool { return e.id != "" && e.url != "" }

// Allowlist maps endpoint ID to URL. It is immutable after construction.
type Allowlist struct {
	urls map[string]string
}

// NewAllowlist validates endpoints (ID to URL) and returns an Allowlist.
func NewAllowlist(endpoints map[string]string) (Allowlist, error) {
	urls := make(map[string]string, len(endpoints))
	for id, raw := range endpoints {
		norm, err := normalizeURL(id, raw)
		if err != nil {
			return Allowlist{}, err
		}
		urls[id] = norm
	}
	return Allowlist{urls: urls}, nil
}

// ParseAllowlist parses "id=url,id2=url2". Items are trimmed of surrounding
// whitespace; an empty spec yields an empty (valid) allowlist.
func ParseAllowlist(spec string) (Allowlist, error) {
	if strings.TrimSpace(spec) == "" {
		return Allowlist{urls: map[string]string{}}, nil
	}
	endpoints := map[string]string{}
	for i, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		id, raw, ok := strings.Cut(item, "=")
		if !ok {
			return Allowlist{}, fmt.Errorf("tenantdir: allowlist item %d: missing '='", i+1)
		}
		id, raw = strings.TrimSpace(id), strings.TrimSpace(raw)
		if !ValidEndpointID(id) {
			return Allowlist{}, fmt.Errorf("tenantdir: allowlist item %d: invalid endpoint id", i+1)
		}
		if _, dup := endpoints[id]; dup {
			return Allowlist{}, fmt.Errorf("tenantdir: duplicate endpoint id %q", id)
		}
		endpoints[id] = raw
	}
	return NewAllowlist(endpoints)
}

// normalizeURL validates raw and returns its normalized form. Errors name the
// endpoint ID but never the URL.
func normalizeURL(id, raw string) (string, error) {
	if !ValidEndpointID(id) {
		return "", errors.New("tenantdir: invalid endpoint id")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", fmt.Errorf("tenantdir: endpoint %q: url does not parse", id)
	case !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https"):
		return "", fmt.Errorf("tenantdir: endpoint %q: url must be absolute http or https", id)
	case u.Hostname() == "":
		return "", fmt.Errorf("tenantdir: endpoint %q: url has no host", id)
	case u.User != nil:
		return "", fmt.Errorf("tenantdir: endpoint %q: url must not contain userinfo", id)
	case u.Fragment != "" || u.RawFragment != "" || strings.Contains(raw, "#"):
		return "", fmt.Errorf("tenantdir: endpoint %q: url must not contain a fragment", id)
	}
	return u.String(), nil
}

// Endpoint returns the endpoint for id, or an error wrapping ErrUnavailable
// when id is not allowlisted.
func (a Allowlist) Endpoint(id string) (Endpoint, error) {
	u, ok := a.urls[id]
	if !ok {
		return Endpoint{}, fmt.Errorf("%w: endpoint not allowlisted", ErrUnavailable)
	}
	return Endpoint{id: id, url: u}, nil
}

// IDs returns the allowlisted endpoint IDs, sorted.
func (a Allowlist) IDs() []string {
	ids := make([]string, 0, len(a.urls))
	for id := range a.urls {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Lookup is the only path from tenant to URL: directory, then endpoint ID, then
// allowlist. Every failure wraps ErrUnavailable and returns a zero Endpoint.
func Lookup(ctx context.Context, d Directory, a Allowlist, tenant string) (Entry, Endpoint, error) {
	if d == nil {
		return Entry{}, Endpoint{}, fmt.Errorf("%w: no directory", ErrUnavailable)
	}
	e, err := d.Resolve(ctx, tenant)
	if err != nil {
		if !errors.Is(err, ErrUnavailable) && ctx.Err() == nil {
			err = fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		return Entry{}, Endpoint{}, err
	}
	ep, err := a.Endpoint(e.EndpointID)
	if err != nil {
		return Entry{}, Endpoint{}, err
	}
	return e, ep, nil
}
