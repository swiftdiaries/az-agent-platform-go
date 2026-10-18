package settings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const staticSample = `default:
  max_active_journeys: 2
  cost:
    profile: standard
tenants:
  tenant-a:
    max_active_journeys: 3
    cost: {profile: gold}
users:
  tenant-a:
    "tenant-a/alice":
      max_active_journeys: 1
`

func writeStatic(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStaticLoad(t *testing.T) {
	ctx := context.Background()
	s, err := LoadStatic(writeStatic(t, staticSample))
	if err != nil {
		t.Fatal(err)
	}
	var _ Source = s

	resolve := func(tenant, principal string) Settings {
		t.Helper()
		got, err := s.Resolve(ctx, Scope{Tenant: tenant, Principal: principal})
		if err != nil {
			t.Fatalf("Resolve(%s,%s): %v", tenant, principal, err)
		}
		return got
	}

	alice := resolve("tenant-a", "tenant-a/alice")
	if alice.MaxActiveJourneys != 1 || alice.Cost.Profile != "gold" {
		t.Fatalf("alice = %+v", alice)
	}
	bob := resolve("tenant-a", "tenant-a/bob")
	if bob.MaxActiveJourneys != 3 || bob.Cost.Profile != "gold" {
		t.Fatalf("bob = %+v", bob)
	}
	z := resolve("tenant-z", "tenant-z/zed")
	if z.MaxActiveJourneys != 2 || z.Cost.Profile != "standard" {
		t.Fatalf("tenant-z = %+v", z)
	}
	if alice.Digest == "" || alice.Digest == bob.Digest || bob.Digest == z.Digest || alice.Digest == z.Digest {
		t.Fatalf("digests should differ: %s %s %s", alice.Digest, bob.Digest, z.Digest)
	}
	if again := resolve("tenant-a", "tenant-a/alice"); again.Digest != alice.Digest {
		t.Fatalf("digest not stable: %s vs %s", again.Digest, alice.Digest)
	}
	// Same applied values give the same digest regardless of principal.
	if other := resolve("tenant-z", "tenant-z/other"); other.Digest != z.Digest {
		t.Fatalf("digest should depend on values only: %s vs %s", other.Digest, z.Digest)
	}

	empty, err := LoadStatic(writeStatic(t, "default: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := empty.Resolve(ctx, Scope{Tenant: "t", Principal: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxActiveJourneys != DefaultMaxActiveJourneys || got.Cost.Profile != "" || got.Digest != digestOf(DefaultMaxActiveJourneys, "") {
		t.Fatalf("built-ins = %+v", got)
	}

	if _, err := s.Resolve(ctx, Scope{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("empty tenant err = %v, want ErrUnavailable", err)
	}
}

func TestStaticStrict(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"unknown top-level key", "default: {}\nextra: 1\n"},
		{"unknown layer key", "default:\n  max_active_journey: 2\n"},
		{"unknown cost key", "default:\n  cost:\n    profil: gold\n"},
		{"unknown tenant layer key", "default: {}\ntenants:\n  a:\n    bogus: 1\n"},
		{"empty file", ""},
		{"comment-only file", "# nothing here\n"},
		{"two documents", "default: {}\n---\ndefault: {}\n"},
		{"duplicate key", "default:\n  max_active_journeys: 2\n  max_active_journeys: 3\n"},
		{"duplicate tenant", "default: {}\ntenants:\n  a: {}\n  a: {}\n"},
		{"out-of-bounds value", "default:\n  max_active_journeys: 5\n"},
		{"user setting cost", "default: {}\nusers:\n  t:\n    p:\n      cost: {profile: gold}\n"},
		{"string where int expected", "default:\n  max_active_journeys: two\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseStatic([]byte(tc.yaml)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseStatic err = %v, want ErrInvalid", err)
			}
			if _, err := LoadStatic(writeStatic(t, tc.yaml)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("LoadStatic err = %v, want ErrInvalid", err)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if s, err := LoadStatic(filepath.Join(t.TempDir(), "absent.yaml")); err == nil || s != nil {
			t.Fatalf("LoadStatic = %v, %v; want error", s, err)
		}
	})

	t.Run("oversized file", func(t *testing.T) {
		big := "default: {}\n# " + strings.Repeat("x", maxStaticSize) + "\n"
		s, err := LoadStatic(writeStatic(t, big))
		if err == nil || s != nil {
			t.Fatalf("LoadStatic = %v, %v; want error", s, err)
		}
	})

	t.Run("file at the limit loads", func(t *testing.T) {
		head := "default: {}\n# "
		body := head + strings.Repeat("x", maxStaticSize-len(head))
		if len(body) != maxStaticSize {
			t.Fatalf("setup: %d", len(body))
		}
		if _, err := LoadStatic(writeStatic(t, body)); err != nil {
			t.Fatal(err)
		}
	})
}
