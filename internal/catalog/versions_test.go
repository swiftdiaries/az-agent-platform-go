package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

const (
	journeysPath = "../../configs/journeys.yaml"
	catalogsDir  = "../../testdata/catalogs"
)

// supportedVersions returns the product versions that have a recorded
// tools/list fixture, mapped to the parsed descriptors.
func supportedVersions(t *testing.T) map[string][]Descriptor {
	t.Helper()
	entries, err := os.ReadDir(catalogsDir)
	if err != nil {
		t.Fatalf("read %s: %v", catalogsDir, err)
	}
	out := map[string][]Descriptor{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(catalogsDir, e.Name(), "tools.json"))
		if err != nil {
			t.Fatalf("version %s: %v", e.Name(), err)
		}
		tools, err := ParseDescriptors(raw)
		if err != nil {
			t.Fatalf("version %s: %v", e.Name(), err)
		}
		out[e.Name()] = tools
	}
	if len(out) == 0 {
		t.Fatalf("no catalog fixtures under %s", catalogsDir)
	}
	return out
}

func sortedVersions(m map[string][]Descriptor) []string {
	versions := make([]string, 0, len(m))
	for v := range m {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	return versions
}

func aliasesOf(res Resolution) []string {
	aliases := make([]string, len(res.Tools))
	for i, tool := range res.Tools {
		aliases[i] = tool.Alias
	}
	return aliases
}

func TestJourneysResolveAgainstSupportedVersions(t *testing.T) {
	set, err := compileFile(journeysPath)
	if err != nil {
		t.Fatalf("compile journeys: %v", err)
	}
	metaKey := set.Catalog().OperationMetaKey
	fixtures := supportedVersions(t)
	versions := sortedVersions(fixtures)

	// aliases[journeyID][version] is the alias list resolved for that pair.
	aliases := map[string]map[string][]string{}
	for _, version := range versions {
		for _, journey := range set.Journeys() {
			if !journey.ProductVersions.IsZero() && !journey.ProductVersions.Contains(version) {
				continue
			}
			t.Run(journey.ID+"/"+version, func(t *testing.T) {
				snapshot := Snapshot{ProductVersion: version, Tools: fixtures[version]}
				res := Resolve(journey, snapshot, metaKey)
				if !res.Available() {
					t.Fatalf("journey %q unavailable for product version %q: %v", journey.ID, version, res.Unavailable)
				}
				if len(res.Tools) == 0 {
					t.Fatalf("journey %q resolved no tools for product version %q", journey.ID, version)
				}
				for _, tool := range res.Tools {
					if tool.Alias == "" || tool.RemoteName == "" {
						t.Errorf("journey %q version %q: tool %+v has an empty alias or remote name", journey.ID, version, tool)
					}
				}
				if aliases[journey.ID] == nil {
					aliases[journey.ID] = map[string][]string{}
				}
				aliases[journey.ID][version] = aliasesOf(res)
			})
		}
	}

	for id, byVersion := range aliases {
		var first string
		var want []string
		for _, version := range versions {
			got, ok := byVersion[version]
			if !ok {
				continue
			}
			if first == "" {
				first, want = version, got
				continue
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("journey %q: aliases differ between versions %s %v and %s %v", id, first, want, version, got)
			}
		}
	}
}

// TestJourneysResolveAgainstSupportedVersionsDetectsBreakage proves the guard
// above can fail: breaking a fixture in memory must make Resolve report it.
func TestJourneysResolveAgainstSupportedVersionsDetectsBreakage(t *testing.T) {
	set, err := compileFile(journeysPath)
	if err != nil {
		t.Fatalf("compile journeys: %v", err)
	}
	metaKey := set.Catalog().OperationMetaKey
	fixtures := supportedVersions(t)
	version := sortedVersions(fixtures)[0]

	journey, ok := set.Journey("shift-swap")
	if !ok {
		t.Fatal("journey shift-swap not found")
	}
	const op = "request_shift_swap"
	index := -1
	for i, d := range fixtures[version] {
		if d.Meta[metaKey] == op {
			index = i
			break
		}
	}
	if index < 0 {
		t.Fatalf("fixture %s has no tool for %s", version, op)
	}
	clone := func() []Descriptor { return append([]Descriptor(nil), fixtures[version]...) }

	if res := Resolve(journey, Snapshot{ProductVersion: version, Tools: clone()}, metaKey); !res.Available() {
		t.Fatalf("unbroken fixture is unavailable: %v", res.Unavailable)
	}

	t.Run("missing", func(t *testing.T) {
		tools := clone()
		tools = append(tools[:index], tools[index+1:]...)
		res := Resolve(journey, Snapshot{ProductVersion: version, Tools: tools}, metaKey)
		if res.Available() || res.Unavailable[0] != (Unavailable{ReasonMissing, op}) {
			t.Fatalf("Unavailable = %v, want %s %s", res.Unavailable, ReasonMissing, op)
		}
	})

	t.Run("ambiguous", func(t *testing.T) {
		tools := clone()
		dup := tools[index]
		var meta map[string]any
		raw, _ := json.Marshal(dup.Meta)
		_ = json.Unmarshal(raw, &meta)
		dup.Meta, dup.Name = meta, dup.Name+"_copy"
		tools = append(tools, dup)
		res := Resolve(journey, Snapshot{ProductVersion: version, Tools: tools}, metaKey)
		if res.Available() || res.Unavailable[0] != (Unavailable{ReasonAmbiguous, op}) {
			t.Fatalf("Unavailable = %v, want %s %s", res.Unavailable, ReasonAmbiguous, op)
		}
	})
}
