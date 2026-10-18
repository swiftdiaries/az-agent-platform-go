package settings

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

func ip(n int) *int       { return &n }
func sp(s string) *string { return &s }

func mustCompile(t *testing.T, doc Document) *Compiled {
	t.Helper()
	c, err := Compile(doc)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return c
}

func resolve(t *testing.T, c *Compiled, tenant, principal string) Settings {
	t.Helper()
	s, err := c.Resolve(context.Background(), Scope{Tenant: tenant, Principal: principal})
	if err != nil {
		t.Fatalf("Resolve(%q,%q): %v", tenant, principal, err)
	}
	return s
}

func TestLayering(t *testing.T) {
	t.Run("built-ins only", func(t *testing.T) {
		c := mustCompile(t, Document{})
		s := resolve(t, c, "a", "p")
		if s.MaxActiveJourneys != DefaultMaxActiveJourneys || s.Cost.Profile != "" {
			t.Fatalf("got %+v", s)
		}
		if s.Digest == "" {
			t.Fatal("digest empty")
		}
	})

	doc := Document{
		Default: Layer{MaxActiveJourneys: ip(3), Cost: &CostLayer{Profile: sp("std")}},
		Tenants: map[string]Layer{
			"a": {MaxActiveJourneys: ip(4), Cost: &CostLayer{Profile: sp("gold")}},
			"b": {MaxActiveJourneys: ip(2)},
		},
		Users: map[string]map[string]Layer{
			"a": {"p": {MaxActiveJourneys: ip(1)}},
			"b": {"q": {MaxActiveJourneys: ip(1)}},
		},
	}
	c := mustCompile(t, doc)

	t.Run("default overrides built-in", func(t *testing.T) {
		s := resolve(t, c, "unknown", "p")
		if s.MaxActiveJourneys != 3 || s.Cost.Profile != "std" {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("tenant overrides default", func(t *testing.T) {
		s := resolve(t, c, "a", "other")
		if s.MaxActiveJourneys != 4 || s.Cost.Profile != "gold" {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("tenant without cost inherits default profile", func(t *testing.T) {
		s := resolve(t, c, "b", "other")
		if s.MaxActiveJourneys != 2 || s.Cost.Profile != "std" {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("user narrows tenant", func(t *testing.T) {
		s := resolve(t, c, "a", "p")
		if s.MaxActiveJourneys != 1 || s.Cost.Profile != "gold" {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("other tenant's user layer not applied", func(t *testing.T) {
		// principal "q" has a user layer under tenant b only.
		s := resolve(t, c, "a", "q")
		if s.MaxActiveJourneys != 4 {
			t.Fatalf("got %+v", s)
		}
		s = resolve(t, c, "b", "p")
		if s.MaxActiveJourneys != 2 {
			t.Fatalf("got %+v", s)
		}
		s = resolve(t, c, "b", "q")
		if s.MaxActiveJourneys != 1 {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("empty explicit profile clears default", func(t *testing.T) {
		c := mustCompile(t, Document{
			Default: Layer{Cost: &CostLayer{Profile: sp("std")}},
			Tenants: map[string]Layer{"a": {Cost: &CostLayer{Profile: sp("")}}},
		})
		if s := resolve(t, c, "a", "p"); s.Cost.Profile != "" {
			t.Fatalf("got %+v", s)
		}
		if s := resolve(t, c, "z", "p"); s.Cost.Profile != "std" {
			t.Fatalf("got %+v", s)
		}
	})
	t.Run("input mutation after compile has no effect", func(t *testing.T) {
		d := Document{
			Default: Layer{MaxActiveJourneys: ip(3)},
			Tenants: map[string]Layer{"a": {MaxActiveJourneys: ip(2)}},
		}
		c := mustCompile(t, d)
		*d.Default.MaxActiveJourneys = 1
		d.Tenants["a"] = Layer{MaxActiveJourneys: ip(4)}
		delete(d.Tenants, "a")
		if s := resolve(t, c, "a", "p"); s.MaxActiveJourneys != 2 {
			t.Fatalf("got %+v", s)
		}
		if s := resolve(t, c, "x", "p"); s.MaxActiveJourneys != 3 {
			t.Fatalf("got %+v", s)
		}
	})
}

func TestNarrowOnly(t *testing.T) {
	user := func(tenantLayer *Layer, def *int, u Layer) Document {
		d := Document{Default: Layer{MaxActiveJourneys: def}}
		if tenantLayer != nil {
			d.Tenants = map[string]Layer{"a": *tenantLayer}
		}
		d.Users = map[string]map[string]Layer{"a": {"p": u}}
		return d
	}
	clamp := []struct {
		name string
		doc  Document
		want int
	}{
		{"user above tenant clamped", user(&Layer{MaxActiveJourneys: ip(2)}, nil, Layer{MaxActiveJourneys: ip(3)}), 2},
		{"user equal tenant", user(&Layer{MaxActiveJourneys: ip(2)}, nil, Layer{MaxActiveJourneys: ip(2)}), 2},
		{"user below tenant narrows", user(&Layer{MaxActiveJourneys: ip(3)}, nil, Layer{MaxActiveJourneys: ip(1)}), 1},
		{"no tenant layer, above default clamped", user(nil, ip(2), Layer{MaxActiveJourneys: ip(3)}), 2},
		{"no tenant layer, above built-in default clamped", user(nil, nil, Layer{MaxActiveJourneys: ip(3)}), DefaultMaxActiveJourneys},
		{"tenant layer without max uses default", user(&Layer{}, ip(2), Layer{MaxActiveJourneys: ip(3)}), 2},
		{"lowered tenant limit does not reject", user(&Layer{MaxActiveJourneys: ip(1)}, ip(4), Layer{MaxActiveJourneys: ip(4)}), 1},
	}
	for _, tc := range clamp {
		t.Run(tc.name, func(t *testing.T) {
			s := resolve(t, mustCompile(t, tc.doc), "a", "p")
			if s.MaxActiveJourneys != tc.want {
				t.Fatalf("MaxActiveJourneys = %d, want %d", s.MaxActiveJourneys, tc.want)
			}
			if s.Digest != digestOf(tc.want, s.Cost.Profile) {
				t.Fatalf("digest does not carry clamped value")
			}
			var rec Settings
			if err := json.Unmarshal(s.Record(), &rec); err != nil || rec.MaxActiveJourneys != tc.want || rec.Digest != s.Digest {
				t.Fatalf("record = %s (%v), want clamped %d", s.Record(), err, tc.want)
			}
		})
	}
	invalid := []struct {
		name string
		doc  Document
	}{
		{"user sets cost", user(nil, nil, Layer{Cost: &CostLayer{Profile: sp("x")}})},
		{"user sets empty cost", user(nil, nil, Layer{Cost: &CostLayer{}})},
		{"user out of bounds", user(nil, nil, Layer{MaxActiveJourneys: ip(5)})},
		{"empty tenant key", Document{Users: map[string]map[string]Layer{"": {"p": {}}}}},
		{"empty principal key", Document{Users: map[string]map[string]Layer{"a": {"": {}}}}},
		{"empty tenant layer key", Document{Tenants: map[string]Layer{"": {}}}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compile(tc.doc); !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
	t.Run("empty explicit profile on tenant", func(t *testing.T) {
		mustCompile(t, Document{Tenants: map[string]Layer{"a": {Cost: &CostLayer{Profile: sp("")}}}})
	})
}

func TestMergeDefaultsOnly(t *testing.T) {
	s := Merge(Layer{}, Layer{}, Layer{})
	if s.MaxActiveJourneys != DefaultMaxActiveJourneys || s.Cost.Profile != "" {
		t.Fatalf("got %+v", s)
	}
	if s.Digest != digestOf(DefaultMaxActiveJourneys, "") {
		t.Fatalf("digest not set")
	}
}

func TestMergeDefOnly(t *testing.T) {
	s := Merge(Layer{MaxActiveJourneys: ip(4), Cost: &CostLayer{Profile: sp("d")}}, Layer{}, Layer{})
	if s.MaxActiveJourneys != 4 || s.Cost.Profile != "d" {
		t.Fatalf("got %+v", s)
	}
}

func TestMergeTenantOverridesDef(t *testing.T) {
	s := Merge(
		Layer{MaxActiveJourneys: ip(4), Cost: &CostLayer{Profile: sp("d")}},
		Layer{MaxActiveJourneys: ip(3), Cost: &CostLayer{Profile: sp("t")}},
		Layer{})
	if s.MaxActiveJourneys != 3 || s.Cost.Profile != "t" {
		t.Fatalf("got %+v", s)
	}
}

func TestMergeUserNarrows(t *testing.T) {
	s := Merge(Layer{}, Layer{MaxActiveJourneys: ip(3)}, Layer{MaxActiveJourneys: ip(1)})
	if s.MaxActiveJourneys != 1 {
		t.Fatalf("got %d", s.MaxActiveJourneys)
	}
}

func TestMergeUserAboveTenantClamped(t *testing.T) {
	s := Merge(Layer{MaxActiveJourneys: ip(4)}, Layer{MaxActiveJourneys: ip(2)}, Layer{MaxActiveJourneys: ip(4)})
	if s.MaxActiveJourneys != 2 {
		t.Fatalf("got %d", s.MaxActiveJourneys)
	}
}

func TestMergeUserAboveDeployDefaultClamped(t *testing.T) {
	s := Merge(Layer{MaxActiveJourneys: ip(3)}, Layer{}, Layer{MaxActiveJourneys: ip(4)})
	if s.MaxActiveJourneys != 3 {
		t.Fatalf("got %d", s.MaxActiveJourneys)
	}
	s = Merge(Layer{}, Layer{}, Layer{MaxActiveJourneys: ip(4)})
	if s.MaxActiveJourneys != DefaultMaxActiveJourneys {
		t.Fatalf("built-in: got %d", s.MaxActiveJourneys)
	}
}

func TestMergeUserCostIgnored(t *testing.T) {
	s := Merge(Layer{}, Layer{Cost: &CostLayer{Profile: sp("t")}}, Layer{Cost: &CostLayer{Profile: sp("u")}})
	if s.Cost.Profile != "t" {
		t.Fatalf("got %q", s.Cost.Profile)
	}
}

func TestMergeDigestMatchesResolve(t *testing.T) {
	def := Layer{MaxActiveJourneys: ip(4), Cost: &CostLayer{Profile: sp("d")}}
	ten := Layer{MaxActiveJourneys: ip(3)}
	usr := Layer{MaxActiveJourneys: ip(2)}
	c := mustCompile(t, Document{
		Default: def,
		Tenants: map[string]Layer{"a": ten},
		Users:   map[string]map[string]Layer{"a": {"p": usr}},
	})
	got := resolve(t, c, "a", "p")
	if want := Merge(def, ten, usr); got != want {
		t.Fatalf("Resolve %+v != Merge %+v", got, want)
	}
}

func TestBounds(t *testing.T) {
	place := map[string]func(n *int) Document{
		"default": func(n *int) Document { return Document{Default: Layer{MaxActiveJourneys: n}} },
		"tenant": func(n *int) Document {
			return Document{Tenants: map[string]Layer{"a": {MaxActiveJourneys: n}}}
		},
		"user": func(n *int) Document {
			return Document{
				Default: Layer{MaxActiveJourneys: ip(4)},
				Users:   map[string]map[string]Layer{"a": {"p": {MaxActiveJourneys: n}}},
			}
		},
	}
	names := make([]string, 0, len(place))
	for k := range place {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, where := range names {
		mk := place[where]
		for _, bad := range []int{0, 5, -1} {
			if _, err := Compile(mk(ip(bad))); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s %d: want ErrInvalid, got %v", where, bad, err)
			}
		}
		for _, good := range []int{1, 4} {
			if _, err := Compile(mk(ip(good))); err != nil {
				t.Errorf("%s %d: unexpected error %v", where, good, err)
			}
		}
	}

	t.Run("message names layer", func(t *testing.T) {
		_, err := Compile(place["tenant"](ip(9)))
		if err == nil || !contains(err.Error(), `tenant "a"`) {
			t.Fatalf("got %v", err)
		}
		_, err = Compile(place["user"](ip(9)))
		if err == nil || !contains(err.Error(), `user "a"/"p"`) {
			t.Fatalf("got %v", err)
		}
	})
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestDigest(t *testing.T) {
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)

	viaDefault := mustCompile(t, Document{Default: Layer{MaxActiveJourneys: ip(3), Cost: &CostLayer{Profile: sp("p1")}}})
	viaTenant := mustCompile(t, Document{Tenants: map[string]Layer{"a": {MaxActiveJourneys: ip(3), Cost: &CostLayer{Profile: sp("p1")}}}})
	viaUser := mustCompile(t, Document{
		Default: Layer{MaxActiveJourneys: ip(4), Cost: &CostLayer{Profile: sp("p1")}},
		Users:   map[string]map[string]Layer{"a": {"p": {MaxActiveJourneys: ip(3)}}},
	})

	d1 := resolve(t, viaDefault, "a", "p").Digest
	d2 := resolve(t, viaTenant, "a", "p").Digest
	d3 := resolve(t, viaUser, "a", "p").Digest
	if !hex64.MatchString(d1) {
		t.Fatalf("digest %q not 64 lowercase hex", d1)
	}
	if d1 != d2 || d1 != d3 {
		t.Fatalf("digests differ: %s %s %s", d1, d2, d3)
	}

	diffMax := mustCompile(t, Document{Default: Layer{MaxActiveJourneys: ip(2), Cost: &CostLayer{Profile: sp("p1")}}})
	diffProfile := mustCompile(t, Document{Default: Layer{MaxActiveJourneys: ip(3), Cost: &CostLayer{Profile: sp("p2")}}})
	if resolve(t, diffMax, "a", "p").Digest == d1 {
		t.Error("different max produced the same digest")
	}
	if resolve(t, diffProfile, "a", "p").Digest == d1 {
		t.Error("different profile produced the same digest")
	}

	t.Run("canonical form", func(t *testing.T) {
		c := mustCompile(t, Document{})
		s := resolve(t, c, "a", "p")
		want := digestOf(2, "")
		if s.Digest != want {
			t.Fatalf("digest %s want %s", s.Digest, want)
		}
	})

	t.Run("record", func(t *testing.T) {
		s := resolve(t, viaDefault, "a", "p")
		raw := s.Record()
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		want := []string{"cost", "digest", "max_active_journeys"}
		if len(keys) != len(want) {
			t.Fatalf("keys %v", keys)
		}
		for i := range want {
			if keys[i] != want[i] {
				t.Fatalf("keys %v", keys)
			}
		}
		var back Settings
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if back != s {
			t.Fatalf("round trip %+v != %+v", back, s)
		}
		var cost map[string]string
		if err := json.Unmarshal(m["cost"], &cost); err != nil || len(cost) != 1 || cost["profile"] != "p1" {
			t.Fatalf("cost %s err %v", m["cost"], err)
		}
	})
}

func TestMutable(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Tenant: "a", Principal: "p"}
	good := Document{Default: Layer{MaxActiveJourneys: ip(3)}}

	m, err := NewMutable(good)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := m.Resolve(ctx, scope); err != nil || s.MaxActiveJourneys != 3 {
		t.Fatalf("got %+v %v", s, err)
	}
	var _ Source = m
	var _ Source = (*Compiled)(nil)

	t.Run("NewMutable invalid", func(t *testing.T) {
		if _, err := NewMutable(Document{Default: Layer{MaxActiveJourneys: ip(0)}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("Set valid replaces", func(t *testing.T) {
		if err := m.Set(Document{Default: Layer{MaxActiveJourneys: ip(1)}}); err != nil {
			t.Fatal(err)
		}
		if s, _ := m.Resolve(ctx, scope); s.MaxActiveJourneys != 1 {
			t.Fatalf("got %+v", s)
		}
	})

	t.Run("Set invalid keeps previous", func(t *testing.T) {
		err := m.Set(Document{Default: Layer{MaxActiveJourneys: ip(9)}})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("got %v", err)
		}
		if s, err := m.Resolve(ctx, scope); err != nil || s.MaxActiveJourneys != 1 {
			t.Fatalf("got %+v %v", s, err)
		}
	})

	t.Run("Fail and clear", func(t *testing.T) {
		boom := errors.New("boom")
		m.Fail(boom)
		_, err := m.Resolve(ctx, scope)
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, boom) {
			t.Fatalf("got %v", err)
		}
		m.Fail(nil)
		if s, err := m.Resolve(ctx, scope); err != nil || s.MaxActiveJourneys != 1 {
			t.Fatalf("got %+v %v", s, err)
		}
	})

	t.Run("empty tenant", func(t *testing.T) {
		if _, err := m.Resolve(ctx, Scope{Principal: "p"}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		c := mustCompile(t, Document{})
		if _, err := c.Resolve(ctx, Scope{}); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := m.Resolve(cctx, scope); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				for j := 0; j < 200; j++ {
					_ = m.Set(Document{Default: Layer{MaxActiveJourneys: ip(1 + j%4)}})
					m.Fail(nil)
				}
			}()
			go func() {
				defer wg.Done()
				for j := 0; j < 200; j++ {
					s, err := m.Resolve(ctx, scope)
					if err != nil {
						continue
					}
					if s.MaxActiveJourneys < 1 || s.MaxActiveJourneys > 4 {
						t.Errorf("out of range: %+v", s)
						return
					}
				}
			}()
		}
		wg.Wait()
	})
}
