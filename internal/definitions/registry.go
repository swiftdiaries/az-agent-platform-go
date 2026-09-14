package definitions

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type skillIdentity struct {
	Name, Description, Digest      string
	Disabled, AllowSupportingFiles bool
}

func skillFootprint(skill SkillPackage) (int, int) {
	bytes := len(skill.Instructions.Data)
	for _, file := range skill.Supporting {
		bytes += len(file.Data)
	}
	return bytes, 1 + len(skill.Supporting)
}

func compileSkill(configRoot string, declaration SkillDeclaration) (SkillPackage, error) {
	packageRoot, err := confinedPath(configRoot, declaration.Root)
	if err != nil {
		return SkillPackage{}, err
	}
	info, err := os.Stat(packageRoot)
	if err != nil || !info.IsDir() {
		return SkillPackage{}, fmt.Errorf("package root is not a directory")
	}
	instructionPath, err := confinedPath(packageRoot, "SKILL.md")
	if err != nil {
		return SkillPackage{}, err
	}
	instructions, err := readLimited(instructionPath, MaxSkillBytes)
	if err != nil {
		return SkillPackage{}, err
	}
	name, description, err := skillMetadata(instructions)
	if err != nil {
		return SkillPackage{}, err
	}
	if name != declaration.Name {
		return SkillPackage{}, fmt.Errorf("frontmatter name %q does not match declaration", name)
	}
	compiled := SkillPackage{
		Name: name, Description: description, Disabled: declaration.Disabled,
		AllowSupportingFiles: declaration.AllowSupportingFiles,
		Instructions:         snapshotFile("SKILL.md", configRoot, instructionPath, instructions),
		Supporting:           make(map[string]SkillFile, len(declaration.SupportingFiles)),
	}
	seen := make(map[string]bool)
	for _, name := range declaration.SupportingFiles {
		if name == "" || name == "SKILL.md" || seen[name] {
			return SkillPackage{}, fmt.Errorf("invalid or duplicate supporting file %q", name)
		}
		seen[name] = true
		path, err := confinedPath(packageRoot, filepath.FromSlash(name))
		if err != nil {
			return SkillPackage{}, fmt.Errorf("supporting file %q: %w", name, err)
		}
		data, err := readLimited(path, MaxSupportingBytes)
		if err != nil {
			return SkillPackage{}, fmt.Errorf("supporting file %q: %w", name, err)
		}
		compiled.Supporting[name] = snapshotFile(name, configRoot, path, data)
	}
	canonical, err := json.Marshal(struct {
		Name, Description              string
		Disabled, AllowSupportingFiles bool
		Instructions                   []byte
		Supporting                     map[string][]byte
	}{compiled.Name, compiled.Description, compiled.Disabled, compiled.AllowSupportingFiles, bytes.Clone(instructions), supportingBytes(compiled.Supporting)})
	if err != nil {
		return SkillPackage{}, err
	}
	compiled.Digest = fmt.Sprintf("%x", sha256.Sum256(canonical))
	return compiled, nil
}

func skillMetadata(data []byte) (string, string, error) {
	if len(data) > MaxSkillBytes || !bytes.HasPrefix(data, []byte("---\n")) {
		return "", "", fmt.Errorf("SKILL.md requires bounded frontmatter")
	}
	end := bytes.Index(data[4:], []byte("\n---\n"))
	if end < 0 || end+4 > MaxSkillMetadata {
		return "", "", fmt.Errorf("SKILL.md frontmatter is invalid or too large")
	}
	var name, description string
	for _, line := range strings.Split(string(data[4:4+end]), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = strings.Trim(strings.TrimSpace(value), "\"'")
		case "description":
			description = strings.Trim(strings.TrimSpace(value), "\"'")
		}
	}
	if name == "" || description == "" || len(name) > 64 || len(description) > 1024 {
		return "", "", fmt.Errorf("SKILL.md name and description are required and bounded")
	}
	return name, description, nil
}

func readLimited(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, fmt.Errorf("file is not regular or exceeds %d bytes", limit)
	}
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, err
	}
	return data, nil
}

func snapshotFile(name, root, path string, data []byte) SkillFile {
	relative, _ := filepath.Rel(root, path)
	return SkillFile{Name: name, Path: path, Root: root, Relative: relative, Digest: fmt.Sprintf("%x", sha256.Sum256(data)), Data: bytes.Clone(data)}
}

func supportingBytes(files map[string]SkillFile) map[string][]byte {
	result := make(map[string][]byte, len(files))
	for name, file := range files {
		result[name] = bytes.Clone(file.Data)
	}
	return result
}

func skillIdentities(skills []SkillPackage) []skillIdentity {
	result := make([]skillIdentity, len(skills))
	for i, skill := range skills {
		result[i] = skillIdentity{skill.Name, skill.Description, skill.Digest, skill.Disabled, skill.AllowSupportingFiles}
	}
	return result
}

func clonePolicies(in map[string]ToolPolicy) map[string]ToolPolicy {
	if in == nil {
		return nil
	}
	out := make(map[string]ToolPolicy, len(in))
	for name, policy := range in {
		out[name] = policy
	}
	return out
}

func selectedPolicies(all map[string]ToolPolicy, tools []string) map[string]ToolPolicy {
	result := make(map[string]ToolPolicy, len(tools))
	for _, name := range tools {
		if policy, ok := all[name]; ok {
			result[name] = policy
		}
	}
	return result
}

func cloneSkill(in SkillPackage) SkillPackage {
	out := in
	out.Instructions.Data = bytes.Clone(in.Instructions.Data)
	out.Supporting = make(map[string]SkillFile, len(in.Supporting))
	for name, file := range in.Supporting {
		file.Data = bytes.Clone(file.Data)
		out.Supporting[name] = file
	}
	return out
}

func cloneJourney(in Journey) Journey {
	out := in
	out.MCP.Tools = append([]string(nil), in.MCP.Tools...)
	out.SkillNames = append([]string(nil), in.SkillNames...)
	out.Routing = cloneRouting(in.Routing)
	out.Policies = clonePolicies(in.Policies)
	out.Skills = make([]SkillPackage, len(in.Skills))
	for i, skill := range in.Skills {
		out.Skills[i] = cloneSkill(skill)
	}
	return out
}

func cloneRouting(in Routing) Routing {
	in.Keywords = append([]string(nil), in.Keywords...)
	return in
}

func cloneServer(in MCPServer) MCPServer {
	out := in
	out.ForwardHeaders = append([]string(nil), in.ForwardHeaders...)
	out.Tools = append([]string(nil), in.Tools...)
	out.Policies = clonePolicies(in.Policies)
	return out
}

func (r *Registry) Version(digest string) (Journey, bool) {
	if r == nil {
		return Journey{}, false
	}
	journey, ok := r.versions[digest]
	return cloneJourney(journey), ok
}

func (r *Registry) HasDigest(digest string) bool {
	_, versionOK := r.versions[digest]
	_, serverOK := r.versionServers[digest]
	return versionOK && serverOK
}

// JourneyIDForDigest returns the immutable definition identity only when the
// complete bundle is available to this replica.
func (r *Registry) JourneyIDForDigest(digest string) (string, bool) {
	journey, versionOK := r.versions[digest]
	_, serverOK := r.versionServers[digest]
	return journey.ID, versionOK && serverOK
}

func (r *Registry) ServerFor(digest string) (MCPServer, bool) {
	server, ok := r.versionServers[digest]
	return cloneServer(server), ok
}

func (r *Registry) Infer(text string) (Journey, bool) {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return Journey{}, false
	}
	bestPriority, bestIndex := 0, -1
	for index, id := range r.order {
		journey := r.journeys[id]
		matched := false
		for _, keyword := range journey.Routing.Keywords {
			matched = matched || strings.Contains(text, keyword)
		}
		if matched && (bestIndex < 0 || journey.Routing.Priority > bestPriority) {
			bestPriority, bestIndex = journey.Routing.Priority, index
		}
	}
	if bestIndex >= 0 {
		return cloneJourney(r.journeys[r.order[bestIndex]]), true
	}
	for _, id := range r.order {
		if r.journeys[id].Routing.Default {
			return cloneJourney(r.journeys[id]), true
		}
	}
	return r.DefaultJourney()
}
