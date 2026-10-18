package definitions

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Set is an immutable compiled definition set. It serves its current
// journeys and, after Retain, older journey versions by digest.
type Set struct {
	catalog    CatalogConfig
	digest     string
	order      []string
	journeys   map[string]Journey
	versions   map[string]Journey
	origin     map[string]*Set // retained digest -> set it was compiled in
	canonical  map[string][]byte
	skillBytes int
	skillFiles int
}

// Retain returns a copy of current that can also serve the journey versions of
// the retained sets. A digest present with different canonical content is an
// error, as is exceeding the aggregate skill limits.
func Retain(current *Set, retained ...*Set) (*Set, error) {
	if current == nil {
		return nil, errors.New("current definition set is required")
	}
	out := &Set{
		catalog: current.catalog, digest: current.digest,
		order:      append([]string(nil), current.order...),
		journeys:   make(map[string]Journey, len(current.journeys)),
		versions:   make(map[string]Journey, len(current.versions)),
		origin:     make(map[string]*Set, len(current.origin)),
		canonical:  make(map[string][]byte, len(current.canonical)),
		skillBytes: current.skillBytes, skillFiles: current.skillFiles,
	}
	for id, j := range current.journeys {
		out.journeys[id] = j
	}
	for digest, j := range current.versions {
		out.versions[digest] = j
	}
	for digest, o := range current.origin {
		out.origin[digest] = o
	}
	for digest, c := range current.canonical {
		out.canonical[digest] = bytes.Clone(c)
	}
	for _, old := range retained {
		if old == nil {
			continue
		}
		retain := false
		for digest := range old.versions {
			if _, exists := out.versions[digest]; !exists {
				retain = true
			}
		}
		if retain && (out.skillBytes > MaxCompiledSkillBytes-old.skillBytes || out.skillFiles > MaxCompiledSkillFiles-old.skillFiles) {
			return nil, fmt.Errorf("retained skill bundles exceed aggregate limits")
		}
		if retain {
			out.skillBytes += old.skillBytes
			out.skillFiles += old.skillFiles
		}
		for digest, journey := range old.versions {
			if existing, ok := out.canonical[digest]; ok && !bytes.Equal(existing, old.canonical[digest]) {
				return nil, fmt.Errorf("definition digest collision %s", digest)
			}
			if _, ok := out.versions[digest]; !ok {
				out.versions[digest] = journey
				out.canonical[digest] = bytes.Clone(old.canonical[digest])
				if o, ok := old.origin[digest]; ok {
					out.origin[digest] = o
				} else {
					out.origin[digest] = old
				}
			}
		}
	}
	return out, nil
}

// Digest identifies the catalog configuration and the ordered current journeys.
func (s *Set) Digest() string { return s.digest }

// Catalog returns the integration input of the current journeys.
func (s *Set) Catalog() CatalogConfig { return s.catalog }

// Journeys returns the current journeys in configuration order.
func (s *Set) Journeys() []Journey {
	out := make([]Journey, len(s.order))
	for i, id := range s.order {
		out[i] = cloneJourney(s.journeys[id])
	}
	return out
}

// Journey returns a current journey by ID.
func (s *Set) Journey(id string) (Journey, bool) {
	j, ok := s.journeys[id]
	return cloneJourney(j), ok
}

// DefaultJourney returns the first journey of the configuration.
func (s *Set) DefaultJourney() (Journey, bool) {
	if s == nil || len(s.order) == 0 {
		return Journey{}, false
	}
	return s.Journey(s.order[0])
}

// Infer routes text with the same semantics as Registry.Infer.
func (s *Set) Infer(text string) (Journey, bool) {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return Journey{}, false
	}
	bestPriority, bestIndex := 0, -1
	for index, id := range s.order {
		journey := s.journeys[id]
		matched := false
		for _, keyword := range journey.Routing.Keywords {
			matched = matched || strings.Contains(text, keyword)
		}
		if matched && (bestIndex < 0 || journey.Routing.Priority > bestPriority) {
			bestPriority, bestIndex = journey.Routing.Priority, index
		}
	}
	if bestIndex >= 0 {
		return cloneJourney(s.journeys[s.order[bestIndex]]), true
	}
	for _, id := range s.order {
		if s.journeys[id].Routing.Default {
			return cloneJourney(s.journeys[id]), true
		}
	}
	return s.DefaultJourney()
}

// Pinned returns any current or retained journey version by digest together
// with the compiled set it belongs to.
func (s *Set) Pinned(digest string) (Journey, *Set, bool) {
	if s == nil {
		return Journey{}, nil, false
	}
	j, ok := s.versions[digest]
	if !ok {
		return Journey{}, nil, false
	}
	if o, ok := s.origin[digest]; ok {
		return cloneJourney(j), o, true
	}
	return cloneJourney(j), s, true
}

// JourneyIDForDigest returns the journey ID of a current or retained version.
func (s *Set) JourneyIDForDigest(digest string) (string, bool) {
	j, ok := s.versions[digest]
	return j.ID, ok
}

// HasDigest reports whether the set can serve the journey version.
func (s *Set) HasDigest(digest string) bool {
	_, ok := s.versions[digest]
	return ok
}

type canonicalJourney struct {
	ID, Description, Prompt string
	Skills                  []skillIdentity
	Routing                 Routing
	ProductVersions         VersionRange
	Tools                   []Selector
	Deny                    []string
	MaxTools                int
	Catalog                 CatalogConfig
}

func canonicalBytes(j Journey) ([]byte, error) {
	return json.Marshal(canonicalJourney{
		ID: j.ID, Description: j.Description, Prompt: j.Prompt,
		Skills: skillIdentities(j.Skills), Routing: cloneRouting(j.Routing),
		ProductVersions: j.ProductVersions, Tools: cloneSelectors(j.Tools),
		Deny: append([]string(nil), j.Deny...), MaxTools: j.MaxTools, Catalog: j.Catalog,
	})
}

func hexDigest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func setDigest(catalog CatalogConfig, digests []string) (string, error) {
	data, err := json.Marshal(struct {
		Catalog  CatalogConfig
		Journeys []string
	}{catalog, digests})
	if err != nil {
		return "", err
	}
	return hexDigest(data), nil
}
