package tenantdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"go.yaml.in/yaml/v3"
)

// maxStaticFileSize is the largest tenant directory file LoadStatic accepts.
const maxStaticFileSize = 1 << 20 // 1 MiB

// Static is an immutable Directory loaded once from a strict YAML file
// (AGENT_PLATFORM_TENANT_DIRECTORY). It holds endpoint IDs only; URLs come
// exclusively from the Allowlist.
type Static struct {
	entries map[string]Entry
}

var _ Directory = (*Static)(nil)

type staticFile struct {
	Tenants map[string]staticEntry `yaml:"tenants"`
}

type staticEntry struct {
	ProductVersion string `yaml:"product_version"`
	Endpoint       string `yaml:"endpoint"`
}

// LoadStatic reads and parses the tenant directory file at path. Files larger
// than 1 MiB are rejected.
func LoadStatic(path string) (*Static, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("tenantdir: open tenant directory: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStaticFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("tenantdir: read tenant directory: %w", err)
	}
	if len(data) > maxStaticFileSize {
		return nil, fmt.Errorf("tenantdir: tenant directory exceeds %d bytes", maxStaticFileSize)
	}
	return ParseStatic(data)
}

// ParseStatic strictly parses a tenant directory document: exactly one YAML
// document, no unknown fields, no duplicate tenants, valid endpoint IDs.
func ParseStatic(data []byte) (*Static, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var doc staticFile
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("tenantdir: tenant directory is empty")
		}
		return nil, fmt.Errorf("tenantdir: invalid tenant directory: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("tenantdir: tenant directory must contain exactly one YAML document")
	}
	if len(doc.Tenants) == 0 {
		return nil, errors.New("tenantdir: tenant directory defines no tenants")
	}

	// Validate in sorted order so errors are deterministic.
	names := make([]string, 0, len(doc.Tenants))
	for name := range doc.Tenants {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make(map[string]Entry, len(names))
	for _, name := range names {
		e := doc.Tenants[name]
		switch {
		case name == "":
			return nil, errors.New("tenantdir: tenant name must not be empty")
		case e.ProductVersion == "":
			return nil, fmt.Errorf("tenantdir: tenant %q: product_version is required", name)
		case !ValidEndpointID(e.Endpoint):
			return nil, fmt.Errorf("tenantdir: tenant %q: endpoint must be a valid endpoint ID", name)
		}
		entries[name] = Entry{ProductVersion: e.ProductVersion, EndpointID: e.Endpoint}
	}
	return &Static{entries: entries}, nil
}

// Resolve implements Directory.
func (s *Static) Resolve(ctx context.Context, tenant string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	if tenant == "" {
		return Entry{}, fmt.Errorf("%w: empty tenant", ErrUnavailable)
	}
	e, ok := s.entries[tenant]
	if !ok {
		return Entry{}, fmt.Errorf("%w: unknown tenant", ErrUnavailable)
	}
	return e, nil
}
