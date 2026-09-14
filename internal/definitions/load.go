package definitions

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
)

const (
	MaxPromptBytes     = 256 << 10
	MaxConfigBytes     = 1 << 20
	MaxSkillBytes      = 256 << 10
	MaxSkillMetadata   = 8 << 10
	MaxSupportingBytes = 1 << 20
)

type MCPServer struct {
	ID             string                `json:"id"`
	Endpoint       string                `json:"endpoint"`
	ForwardHeaders []string              `json:"forward_headers"`
	CallIDHeader   string                `json:"call_id_header"`
	Tools          []string              `json:"tools"`
	Policies       map[string]ToolPolicy `json:"policies,omitempty"`
}

// ToolPolicy is reviewed integration configuration; MCP annotations grant no permission.
type ToolPolicy struct {
	Class                 string `json:"class"`
	DeduplicationEvidence string `json:"deduplication_evidence,omitempty"`
}

type MCPBinding struct {
	Server string   `json:"server"`
	Tools  []string `json:"tools"`
}

type Journey struct {
	ID           string                `json:"id"`
	Description  string                `json:"description"`
	SystemPrompt string                `json:"system_prompt"`
	MCP          MCPBinding            `json:"mcp"`
	SkillNames   []string              `json:"skills,omitempty"`
	Prompt       string                `json:"-"`
	Skills       []SkillPackage        `json:"-"`
	Policies     map[string]ToolPolicy `json:"-"`
	Digest       string                `json:"-"`
	Routing      Routing               `json:"routing,omitempty"`
}

type Routing struct {
	Default  bool     `json:"default,omitempty"`
	Keywords []string `json:"keywords,omitempty"`
	Priority int      `json:"priority,omitempty"`
}

type SkillDeclaration struct {
	Name                 string   `json:"name"`
	Root                 string   `json:"root"`
	SupportingFiles      []string `json:"supporting_files,omitempty"`
	Disabled             bool     `json:"disabled,omitempty"`
	AllowSupportingFiles bool     `json:"allow_supporting_files,omitempty"`
}

type SkillFile struct {
	Name, Path, Root, Relative, Digest string
	Data                               []byte
}

type SkillPackage struct {
	Name, Description              string
	Disabled, AllowSupportingFiles bool
	Instructions                   SkillFile
	Supporting                     map[string]SkillFile
	Digest                         string
}

type Registry struct {
	servers   map[string]MCPServer
	journeys  map[string]Journey
	versions  map[string]Journey
	canonical map[string][]byte
	order     []string
}

// Load compiles one candidate configuration and optional retained configurations.
// The candidate supplies routing defaults and live MCP connection settings.
func Load(path string, retained ...string) (*Registry, error) {
	registry, err := loadOne(path)
	if err != nil {
		return nil, err
	}
	for _, oldPath := range retained {
		old, err := loadOne(oldPath)
		if err != nil {
			return nil, err
		}
		for digest, journey := range old.versions {
			if existing, ok := registry.canonical[digest]; ok && !bytes.Equal(existing, old.canonical[digest]) {
				return nil, fmt.Errorf("definition digest collision %s", digest)
			}
			if _, ok := registry.versions[digest]; !ok {
				registry.versions[digest] = journey
				registry.canonical[digest] = bytes.Clone(old.canonical[digest])
			}
		}
	}
	return registry, nil
}

func loadOne(path string) (*Registry, error) {
	data, err := readLimited(path, MaxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read journey config: %w", err)
	}
	var raw struct {
		MCPServers    []MCPServer        `json:"mcp_servers"`
		SkillPackages []SkillDeclaration `json:"skill_packages,omitempty"`
		Journeys      []Journey          `json:"journeys"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode journey config (JSON syntax): %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode journey config: trailing value")
	}

	registry := &Registry{servers: make(map[string]MCPServer), journeys: make(map[string]Journey), versions: make(map[string]Journey), canonical: make(map[string][]byte)}
	for _, server := range raw.MCPServers {
		if server.ID == "" || server.Endpoint == "" {
			return nil, fmt.Errorf("MCP server id and endpoint are required")
		}
		if _, exists := registry.servers[server.ID]; exists {
			return nil, fmt.Errorf("duplicate MCP server %q", server.ID)
		}
		endpoint, err := expandEndpoint(server.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("MCP server %q: %w", server.ID, err)
		}
		server.Endpoint = endpoint
		seenHeaders := make(map[string]bool)
		for i, name := range server.ForwardHeaders {
			canonical := textproto.CanonicalMIMEHeaderKey(name)
			if canonical == "" || strings.ContainsAny(name, "\r\n:") || seenHeaders[canonical] {
				return nil, fmt.Errorf("MCP server %q has invalid or duplicate forwarded header %q", server.ID, name)
			}
			seenHeaders[canonical] = true
			server.ForwardHeaders[i] = canonical
		}
		if server.CallIDHeader != "" {
			server.CallIDHeader = textproto.CanonicalMIMEHeaderKey(server.CallIDHeader)
			if server.CallIDHeader == "" {
				return nil, fmt.Errorf("MCP server %q has invalid call ID header", server.ID)
			}
		}
		if len(server.Tools) == 0 {
			return nil, fmt.Errorf("MCP server %q has empty checked-in tool catalog", server.ID)
		}
		for name, policy := range server.Policies {
			declared := false
			for _, tool := range server.Tools {
				declared = declared || tool == name
			}
			if !declared {
				return nil, fmt.Errorf("policy for undeclared tool %q", name)
			}
			if policy.Class != "read_only" && policy.Class != "effectful" && policy.Class != "deduplicated" {
				return nil, fmt.Errorf("invalid tool policy %q", name)
			}
			if policy.Class == "deduplicated" && (policy.DeduplicationEvidence == "" || server.CallIDHeader == "") {
				return nil, fmt.Errorf("deduplicated tool requires reviewed evidence and call ID header")
			}
		}
		seenTools := make(map[string]bool, len(server.Tools))
		for _, name := range server.Tools {
			if strings.TrimSpace(name) == "" || seenTools[name] {
				return nil, fmt.Errorf("MCP server %q has invalid or duplicate catalog tool %q", server.ID, name)
			}
			seenTools[name] = true
		}
		registry.servers[server.ID] = server
	}

	root, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}
	packages := make(map[string]SkillPackage, len(raw.SkillPackages))
	for _, declaration := range raw.SkillPackages {
		if declaration.Name == "" || declaration.Root == "" {
			return nil, fmt.Errorf("skill name and root are required")
		}
		if _, ok := packages[declaration.Name]; ok {
			return nil, fmt.Errorf("duplicate skill %q", declaration.Name)
		}
		compiled, err := compileSkill(root, declaration)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", declaration.Name, err)
		}
		packages[declaration.Name] = compiled
	}
	for _, journey := range raw.Journeys {
		if journey.ID == "" || journey.Description == "" || journey.SystemPrompt == "" {
			return nil, fmt.Errorf("journey id, description, and system prompt are required")
		}
		if _, exists := registry.journeys[journey.ID]; exists {
			return nil, fmt.Errorf("duplicate journey %q", journey.ID)
		}
		seenKeywords := map[string]bool{}
		for i, keyword := range journey.Routing.Keywords {
			keyword = strings.ToLower(strings.TrimSpace(keyword))
			if keyword == "" || seenKeywords[keyword] {
				return nil, fmt.Errorf("journey %q has invalid routing keyword", journey.ID)
			}
			seenKeywords[keyword] = true
			journey.Routing.Keywords[i] = keyword
		}
		server, exists := registry.servers[journey.MCP.Server]
		if !exists {
			return nil, fmt.Errorf("journey %q references unknown MCP server %q", journey.ID, journey.MCP.Server)
		}
		if len(journey.MCP.Tools) == 0 {
			return nil, fmt.Errorf("journey %q has empty tool allowlist", journey.ID)
		}
		seenTools := make(map[string]bool)
		catalog := make(map[string]bool, len(server.Tools))
		for _, name := range server.Tools {
			catalog[name] = true
		}
		for _, name := range journey.MCP.Tools {
			if strings.TrimSpace(name) == "" || seenTools[name] {
				return nil, fmt.Errorf("journey %q has invalid or duplicate tool %q", journey.ID, name)
			}
			if !catalog[name] {
				return nil, fmt.Errorf("journey %q tool %q is absent from MCP server %q catalog", journey.ID, name, server.ID)
			}
			seenTools[name] = true
		}
		promptPath, err := confinedPath(root, journey.SystemPrompt)
		if err != nil {
			return nil, fmt.Errorf("journey %q prompt: %w", journey.ID, err)
		}
		prompt, err := readLimited(promptPath, MaxPromptBytes)
		if err != nil {
			return nil, fmt.Errorf("journey %q prompt: %w", journey.ID, err)
		}
		journey.Prompt = string(prompt)
		journey.Policies = selectedPolicies(server.Policies, journey.MCP.Tools)
		seenSkills := make(map[string]bool)
		for _, name := range journey.SkillNames {
			skill, ok := packages[name]
			if !ok || seenSkills[name] {
				return nil, fmt.Errorf("journey %q references missing or duplicate skill %q", journey.ID, name)
			}
			seenSkills[name] = true
			journey.Skills = append(journey.Skills, cloneSkill(skill))
		}
		canonical, err := json.Marshal(struct {
			ID, Description, Server, Prompt string
			Tools                           []string
			Policies                        map[string]ToolPolicy
			Skills                          []skillIdentity
			Routing                         Routing
		}{
			ID: journey.ID, Description: journey.Description, Server: journey.MCP.Server,
			Prompt: journey.Prompt, Tools: append([]string(nil), journey.MCP.Tools...), Policies: clonePolicies(journey.Policies),
			Skills: skillIdentities(journey.Skills), Routing: cloneRouting(journey.Routing),
		})
		if err != nil {
			return nil, err
		}
		journey.Digest = fmt.Sprintf("%x", sha256.Sum256(canonical))
		registry.journeys[journey.ID] = journey
		if existing, ok := registry.canonical[journey.Digest]; ok && !bytes.Equal(existing, canonical) {
			return nil, fmt.Errorf("definition digest collision %s", journey.Digest)
		}
		registry.versions[journey.Digest] = journey
		registry.canonical[journey.Digest] = bytes.Clone(canonical)
		registry.order = append(registry.order, journey.ID)
	}
	if len(registry.order) == 0 {
		return nil, fmt.Errorf("at least one journey is required")
	}
	return registry, nil
}

func expandEndpoint(value string) (string, error) {
	if !strings.HasPrefix(value, "${") || !strings.HasSuffix(value, "}") {
		return value, nil
	}
	name := strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")
	if name == "" {
		return "", fmt.Errorf("empty endpoint environment variable")
	}
	resolved, ok := os.LookupEnv(name)
	if !ok || resolved == "" {
		return "", fmt.Errorf("endpoint environment variable %s is unset", name)
	}
	return resolved, nil
}

func confinedPath(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("absolute path is not allowed")
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes config directory")
	}
	path := filepath.Join(root, clean)
	current := root
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symbolic links are not allowed")
		}
	}
	return path, nil
}

func (r *Registry) Journey(id string) (Journey, bool) {
	journey, ok := r.journeys[id]
	return cloneJourney(journey), ok
}

func (r *Registry) DefaultJourney() (Journey, bool) {
	if r == nil || len(r.order) == 0 {
		return Journey{}, false
	}
	return r.Journey(r.order[0])
}

func (r *Registry) Server(id string) (MCPServer, bool) {
	server, ok := r.servers[id]
	return cloneServer(server), ok
}
