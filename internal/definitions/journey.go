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
	ID          string
	Description string
	// Prompt is the body of the journey's SKILL.md (everything after the
	// closing frontmatter line, trimmed); there is no separate system prompt.
	Prompt string
	Skills []SkillPackage
	Digest string

	Tools           []Selector
	Deny            []string
	MaxTools        int
	ProductVersions VersionRange
	Catalog         CatalogConfig
	Skill           string
}
