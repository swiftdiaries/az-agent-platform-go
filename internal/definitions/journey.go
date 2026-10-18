package definitions

const (
	MaxPromptBytes        = 256 << 10
	MaxConfigBytes        = 1 << 20
	MaxSkillBytes         = 256 << 10
	MaxSkillMetadata      = 8 << 10
	MaxSupportingBytes    = 1 << 20
	MaxCompiledSkillBytes = 8 << 20
	MaxCompiledSkillFiles = 256
)

// Journey is one compiled journey definition. Compile fills every field.
type Journey struct {
	ID           string
	Description  string
	SystemPrompt string
	Prompt       string
	Skills       []SkillPackage
	Digest       string
	Routing      Routing

	Tools           []Selector
	Deny            []string
	MaxTools        int
	ProductVersions VersionRange
	Catalog         CatalogConfig
	Skill           string
}

// Routing is accepted until the planner replaces keyword routing (H4b).
type Routing struct {
	Default  bool
	Keywords []string
	Priority int
}
