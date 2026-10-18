package tenantdir

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestMutableResolve(t *testing.T) {
	ctx := context.Background()
	m := NewMutable(map[string]Entry{"t1": {ProductVersion: "v1", EndpointID: "ep-1"}})

	e, err := m.Resolve(ctx, "t1")
	if err != nil || e != (Entry{"v1", "ep-1"}) {
		t.Fatalf("Resolve t1 = %v, %v", e, err)
	}
	m.Set("t2", Entry{ProductVersion: "v2", EndpointID: "ep-2"})
	if e, err = m.Resolve(ctx, "t2"); err != nil || e.EndpointID != "ep-2" {
		t.Fatalf("Resolve t2 = %v, %v", e, err)
	}

	if _, err = m.Resolve(ctx, "nope"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown tenant err = %v", err)
	}
	if _, err = m.Resolve(ctx, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("empty tenant err = %v", err)
	}

	m.Delete("t2")
	if _, err = m.Resolve(ctx, "t2"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("deleted tenant err = %v", err)
	}

	for name, bad := range map[string]Entry{
		"no-version":  {EndpointID: "ep"},
		"no-endpoint": {ProductVersion: "v1"},
		"bad-id":      {ProductVersion: "v1", EndpointID: "ep/../x"},
	} {
		m.Set("bad", bad)
		if _, err = m.Resolve(ctx, "bad"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}

	cause := errors.New("boom")
	m.Fail(cause)
	_, err = m.Resolve(ctx, "t1")
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("Fail err = %v", err)
	}
	m.Fail(nil)
	if _, err = m.Resolve(ctx, "t1"); err != nil {
		t.Fatalf("after clear: %v", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = m.Resolve(cctx, "t1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled err = %v", err)
	}
}

func TestMutableConcurrent(t *testing.T) {
	m := NewMutable(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.Set(fmt.Sprintf("t%d", i), Entry{ProductVersion: "v", EndpointID: "ep"})
				if j%50 == 0 {
					m.Delete(fmt.Sprintf("t%d", i))
				}
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = m.Resolve(context.Background(), fmt.Sprintf("t%d", i))
				if j%100 == 0 {
					m.Fail(errors.New("x"))
					m.Fail(nil)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestAllowlistParse(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    map[string]string
		wantErr bool
	}{
		{"valid multi", "a=https://a.example/mcp,b.1_x=http://b.example:8080/m?k=v", map[string]string{"a": "https://a.example/mcp", "b.1_x": "http://b.example:8080/m?k=v"}, false},
		{"whitespace", "  a = https://a.example/mcp ,\n b=http://b.example ", map[string]string{"a": "https://a.example/mcp", "b": "http://b.example"}, false},
		{"empty spec", "", map[string]string{}, false},
		{"blank spec", "   ", map[string]string{}, false},
		{"missing equals", "a", nil, true},
		{"duplicate id", "a=http://x.example,a=http://y.example", nil, true},
		{"bad id chars", "a b=http://x.example", nil, true},
		{"empty id", "=http://x.example", nil, true},
		{"relative url", "a=/mcp", nil, true},
		{"no host", "a=http://", nil, true},
		{"ftp scheme", "a=ftp://x.example/", nil, true},
		{"userinfo", "a=https://user:secret@host/x", nil, true},
		{"fragment", "a=https://x.example/m#frag", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAllowlist(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(a.IDs()) != len(tc.want) {
				t.Fatalf("IDs = %v", a.IDs())
			}
			for id, u := range tc.want {
				ep, err := a.Endpoint(id)
				if err != nil || ep.URL() != u {
					t.Fatalf("Endpoint(%q) = %q, %v; want %q", id, ep.URL(), err, u)
				}
			}
		})
	}

	_, err := ParseAllowlist("a=https://user:secret@host/x")
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "host/x") {
		t.Fatalf("error leaks URL: %v", err)
	}
	_, err = NewAllowlist(map[string]string{"a": "https://user:secret@host/x"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("NewAllowlist error leaks URL: %v", err)
	}
	_, err = NewAllowlist(map[string]string{"bad id": "https://x.example"})
	if err == nil {
		t.Fatal("NewAllowlist accepted bad id")
	}
	_, err = ParseAllowlist("a=http://%zz/secretpath")
	if err == nil || strings.Contains(err.Error(), "secretpath") {
		t.Fatalf("parse error leaks URL: %v", err)
	}

	ids := mustAllow(t, "z=http://z.example,a=http://a.example").IDs()
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "z" {
		t.Fatalf("IDs not sorted: %v", ids)
	}
}

func mustAllow(t *testing.T, spec string) Allowlist {
	t.Helper()
	a, err := ParseAllowlist(spec)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAllowlistEndpoint(t *testing.T) {
	a := mustAllow(t, "ep-1=https://mcp.example/v1")
	ep, err := a.Endpoint("ep-1")
	if err != nil || !ep.Valid() || ep.ID() != "ep-1" || ep.URL() != "https://mcp.example/v1" {
		t.Fatalf("Endpoint = %+v, %v", ep, err)
	}
	ep, err = a.Endpoint("other")
	if !errors.Is(err, ErrUnavailable) || ep.Valid() {
		t.Fatalf("unlisted = %+v, %v", ep, err)
	}
	var zero Endpoint
	if zero.Valid() || zero.URL() != "" || zero.ID() != "" {
		t.Fatal("zero Endpoint must be invalid")
	}
	var za Allowlist
	if _, err := za.Endpoint("ep-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("zero Allowlist err = %v", err)
	}
}

type errDir struct{ err error }

func (d errDir) Resolve(context.Context, string) (Entry, error) { return Entry{}, d.err }

func TestLookup(t *testing.T) {
	ctx := context.Background()
	a := mustAllow(t, "ep-1=https://mcp.example/v1")
	m := NewMutable(map[string]Entry{
		"t1": {ProductVersion: "v1", EndpointID: "ep-1"},
		"t2": {ProductVersion: "v2", EndpointID: "rogue"},
	})

	e, ep, err := Lookup(ctx, m, a, "t1")
	if err != nil || e.ProductVersion != "v1" || !ep.Valid() || ep.URL() != "https://mcp.example/v1" || ep.ID() != "ep-1" {
		t.Fatalf("happy = %v %+v %v", e, ep, err)
	}

	if _, ep, err = Lookup(ctx, m, a, "unknown"); !errors.Is(err, ErrUnavailable) || ep.Valid() {
		t.Fatalf("unknown = %+v %v", ep, err)
	}

	_, ep, err = Lookup(ctx, m, a, "t2")
	if !errors.Is(err, ErrUnavailable) || ep.Valid() {
		t.Fatalf("non-allowlisted = %+v %v", ep, err)
	}

	plain := errors.New("connection refused")
	_, ep, err = Lookup(ctx, errDir{plain}, a, "t1")
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, plain) || ep.Valid() {
		t.Fatalf("plain adapter error = %+v %v", ep, err)
	}

	if _, _, err = Lookup(ctx, nil, a, "t1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil directory err = %v", err)
	}
}
