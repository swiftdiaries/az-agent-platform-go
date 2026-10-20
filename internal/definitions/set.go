package definitions

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// Set is an immutable compiled definition set.
type Set struct {
	catalog CatalogConfig
	// conversationPrompt is the base prompt of the single conversation agent.
	conversationPrompt string
	digest             string
	order              []string
	journeys           map[string]Journey
	canonical          map[string][]byte
	skillBytes         int
	skillFiles         int
	bundle             Bundle // files the compile read; immutable
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
