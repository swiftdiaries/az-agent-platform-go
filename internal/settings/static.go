package settings

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

// maxStaticSize bounds the settings file read at startup.
const maxStaticSize = 1 << 20 // 1 MiB

// Static is an immutable Source loaded once from a strict YAML file.
type Static struct {
	c *Compiled
}

var _ Source = (*Static)(nil)

// LoadStatic reads and parses the settings file at path. A missing, unreadable
// or oversized (over 1 MiB) file is an error.
func LoadStatic(path string) (*Static, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("settings: open %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStaticSize+1))
	if err != nil {
		return nil, fmt.Errorf("settings: read %s: %w", path, err)
	}
	if len(data) > maxStaticSize {
		return nil, invalidf("%s exceeds %d bytes", path, maxStaticSize)
	}
	s, err := ParseStatic(data)
	if err != nil {
		return nil, fmt.Errorf("settings: %s: %w", path, err)
	}
	return s, nil
}

// ParseStatic strictly parses data as exactly one settings document: unknown
// fields, duplicate keys, an empty input and extra documents are all rejected.
func ParseStatic(data []byte) (*Static, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, invalidf("empty settings file; state at least \"default:\"")
		}
		return nil, invalidf("%v", err)
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return nil, invalidf("%v", err)
	default:
		return nil, invalidf("multiple YAML documents; exactly one allowed")
	}
	c, err := Compile(doc)
	if err != nil {
		return nil, err
	}
	return &Static{c: c}, nil
}

// Resolve implements Source.
func (s *Static) Resolve(ctx context.Context, scope Scope) (Settings, error) {
	return s.c.Resolve(ctx, scope)
}
