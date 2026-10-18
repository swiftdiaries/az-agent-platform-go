package definitions

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// Set is an immutable compiled definition set. It serves its current
// journeys and, after Retain, older compiled sets by set digest.
type Set struct {
	catalog CatalogConfig
	// conversationPrompt is the base prompt of the single conversation agent.
	conversationPrompt string
	digest             string
	order              []string
	journeys           map[string]Journey
	sets               map[string]*Set // retained set digest -> original compiled set
	original           *Set            // compiled set a Retain copy was made from; nil if compiled
	canonical          map[string][]byte
	skillBytes         int
	skillFiles         int
}

// Retain returns a copy of current that can also serve the retained sets by
// set digest. A journey digest present with different canonical content is an
// error, as is exceeding the aggregate skill limits.
func Retain(current *Set, retained ...*Set) (*Set, error) {
	if current == nil {
		return nil, errors.New("current definition set is required")
	}
	out := &Set{
		catalog: current.catalog, digest: current.digest, conversationPrompt: current.conversationPrompt,
		order:      append([]string(nil), current.order...),
		journeys:   make(map[string]Journey, len(current.journeys)),
		sets:       make(map[string]*Set, len(current.sets)),
		original:   current.compiled(),
		canonical:  make(map[string][]byte, len(current.canonical)),
		skillBytes: current.skillBytes, skillFiles: current.skillFiles,
	}
	for id, j := range current.journeys {
		out.journeys[id] = j
	}
	for digest, o := range current.sets {
		out.sets[digest] = o
	}
	for digest, c := range current.canonical {
		out.canonical[digest] = bytes.Clone(c)
	}
	for _, old := range retained {
		if old == nil {
			continue
		}
		contributes := !out.Serves(old.digest)
		for digest := range old.sets {
			contributes = contributes || !out.Serves(digest)
		}
		if contributes && (out.skillBytes > MaxCompiledSkillBytes-old.skillBytes || out.skillFiles > MaxCompiledSkillFiles-old.skillFiles) {
			return nil, fmt.Errorf("retained skill bundles exceed aggregate limits")
		}
		if contributes {
			out.skillBytes += old.skillBytes
			out.skillFiles += old.skillFiles
		}
		for digest, canonical := range old.canonical {
			if existing, ok := out.canonical[digest]; ok {
				if !bytes.Equal(existing, canonical) {
					return nil, fmt.Errorf("definition digest collision %s", digest)
				}
				continue
			}
			out.canonical[digest] = bytes.Clone(canonical)
		}
		for digest, o := range old.sets {
			if _, ok := out.sets[digest]; !ok && digest != out.digest {
				out.sets[digest] = o
			}
		}
		if _, ok := out.sets[old.digest]; !ok && old.digest != out.digest {
			out.sets[old.digest] = old.compiled()
		}
	}
	return out, nil
}

// compiled returns the originally compiled set a Retain copy was made from, or
// s itself when s was compiled directly.
func (s *Set) compiled() *Set {
	if s.original != nil {
		return s.original
	}
	return s
}

// Version returns the compiled set with this set digest: s itself for
// s.Digest(), or a set retained by Retain (the original compiled *Set, also
// through chained Retain calls).
func (s *Set) Version(digest string) (*Set, bool) {
	if s == nil {
		return nil, false
	}
	if digest == s.digest {
		return s, true
	}
	v, ok := s.sets[digest]
	return v, ok
}

// Serves reports whether Version(digest) succeeds.
func (s *Set) Serves(digest string) bool {
	_, ok := s.Version(digest)
	return ok
}

// Digest identifies the catalog configuration and the ordered current journeys.
func (s *Set) Digest() string { return s.digest }

// Catalog returns the integration input of the current journeys.
func (s *Set) Catalog() CatalogConfig { return s.catalog }

// ConversationPrompt returns the base prompt of the conversation agent, or ""
// when the configuration declares none.
func (s *Set) ConversationPrompt() string { return s.conversationPrompt }

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

type canonicalJourney struct {
	ID, Description, Prompt string
	Skills                  []skillIdentity
	ProductVersions         VersionRange
	Tools                   []Selector
	Deny                    []string
	MaxTools                int
	Catalog                 CatalogConfig
}

func canonicalBytes(j Journey) ([]byte, error) {
	return json.Marshal(canonicalJourney{
		ID: j.ID, Description: j.Description, Prompt: j.Prompt,
		Skills:          skillIdentities(j.Skills),
		ProductVersions: j.ProductVersions, Tools: cloneSelectors(j.Tools),
		Deny: append([]string(nil), j.Deny...), MaxTools: j.MaxTools, Catalog: j.Catalog,
	})
}

func hexDigest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func setDigest(catalog CatalogConfig, conversationPrompt string, digests []string) (string, error) {
	data, err := json.Marshal(struct {
		Catalog            CatalogConfig
		ConversationPrompt string
		Journeys           []string
	}{catalog, conversationPrompt, digests})
	if err != nil {
		return "", err
	}
	return hexDigest(data), nil
}
