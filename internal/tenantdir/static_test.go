package tenantdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validStaticYAML = `tenants:
  tenant-a:
    product_version: "2026.2"
    endpoint: java-2026-2
  tenant-b:
    product_version: "2026.3"
    endpoint: java-2026-3
`

func TestStaticLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenants.yaml")
	if err := os.WriteFile(path, []byte(validStaticYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadStatic(path)
	if err != nil {
		t.Fatalf("LoadStatic: %v", err)
	}
	var d Directory = s
	ctx := context.Background()

	e, err := d.Resolve(ctx, "tenant-a")
	if err != nil || e != (Entry{ProductVersion: "2026.2", EndpointID: "java-2026-2"}) {
		t.Fatalf("Resolve tenant-a = %v, %v", e, err)
	}
	e, err = d.Resolve(ctx, "tenant-b")
	if err != nil || e != (Entry{ProductVersion: "2026.3", EndpointID: "java-2026-3"}) {
		t.Fatalf("Resolve tenant-b = %v, %v", e, err)
	}
	if _, err = d.Resolve(ctx, "nope"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown tenant err = %v", err)
	}
	if _, err = d.Resolve(ctx, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("empty tenant err = %v", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = d.Resolve(cctx, "tenant-a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx err = %v", err)
	}

	if _, err = LoadStatic(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("LoadStatic of a missing file succeeded")
	}
}

func TestStaticStrict(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"unknown top-level key", "tenants:\n  a:\n    product_version: v\n    endpoint: ep\nextra: 1\n"},
		{"unknown entry key", "tenants:\n  a:\n    product_version: v\n    endpoint: ep\n    url: https://x\n"},
		{"empty document", ""},
		{"comment only", "# nothing\n"},
		{"missing tenants", "other: {}\n"},
		{"empty tenants", "tenants: {}\n"},
		{"two documents", "tenants:\n  a:\n    product_version: v\n    endpoint: ep\n---\ntenants:\n  b:\n    product_version: v\n    endpoint: ep\n"},
		{"duplicate tenant key", "tenants:\n  a:\n    product_version: v\n    endpoint: ep\n  a:\n    product_version: w\n    endpoint: ep2\n"},
		{"empty tenant key", "tenants:\n  \"\":\n    product_version: v\n    endpoint: ep\n"},
		{"empty product_version", "tenants:\n  a:\n    product_version: \"\"\n    endpoint: ep\n"},
		{"missing product_version", "tenants:\n  a:\n    endpoint: ep\n"},
		{"invalid endpoint id", "tenants:\n  a:\n    product_version: v\n    endpoint: \"bad id!\"\n"},
		{"missing endpoint", "tenants:\n  a:\n    product_version: v\n"},
		{"endpoint is a url", "tenants:\n  a:\n    product_version: v\n    endpoint: https://x\n"},
		{"non-mapping yaml", "- a\n- b\n"},
		{"scalar yaml", "just a string\n"},
		{"null tenant entry", "tenants:\n  a:\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ParseStatic([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("ParseStatic accepted %q (static=%v)", tc.yaml, s)
			}
			if strings.Contains(err.Error(), "https://x") {
				t.Fatalf("error echoes a configured value: %v", err)
			}
		})
	}

	t.Run("oversized file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "big.yaml")
		big := validStaticYAML + "# " + strings.Repeat("x", maxStaticFileSize) + "\n"
		if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadStatic(path); err == nil {
			t.Fatal("LoadStatic accepted an oversized file")
		}
	})

	t.Run("file at limit is read", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ok.yaml")
		pad := maxStaticFileSize - len(validStaticYAML) - len("#\n")
		body := validStaticYAML + "#" + strings.Repeat("x", pad) + "\n"
		if len(body) != maxStaticFileSize {
			t.Fatalf("test setup: body is %d bytes", len(body))
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadStatic(path); err != nil {
			t.Fatalf("LoadStatic at limit: %v", err)
		}
	})
}
