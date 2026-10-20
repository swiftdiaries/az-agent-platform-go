package definitions

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
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

func (src *source) compileSkill(declaration skillDeclaration) (SkillPackage, error) {
	packageRoot, err := src.confine("", declaration.Root)
	if err != nil {
		return SkillPackage{}, err
	}
	info, err := fs.Stat(src.fsys, packageRoot)
	if err != nil || !info.IsDir() {
		return SkillPackage{}, fmt.Errorf("package root is not a directory")
	}
	instructionPath, err := src.confine(packageRoot, "SKILL.md")
	if err != nil {
		return SkillPackage{}, err
	}
	instructions, err := src.read(instructionPath, MaxSkillBytes)
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
		Instructions:         src.snapshot("SKILL.md", instructions),
		Supporting:           make(map[string]SkillFile, len(declaration.SupportingFiles)),
	}
	seen := make(map[string]bool)
	for _, name := range declaration.SupportingFiles {
		if name == "" || name == "SKILL.md" || seen[name] {
			return SkillPackage{}, fmt.Errorf("invalid or duplicate supporting file %q", name)
		}
		seen[name] = true
		path, err := src.confine(packageRoot, filepath.FromSlash(name))
		if err != nil {
			return SkillPackage{}, fmt.Errorf("supporting file %q: %w", name, err)
		}
		data, err := src.read(path, MaxSupportingBytes)
		if err != nil {
			return SkillPackage{}, fmt.Errorf("supporting file %q: %w", name, err)
		}
		compiled.Supporting[name] = src.snapshot(name, data)
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

// skillBody returns the text after the closing frontmatter line of a SKILL.md
// that skillMetadata accepted, trimmed of surrounding whitespace.
func skillBody(data []byte) string {
	end := bytes.Index(data[4:], []byte("\n---\n"))
	if !bytes.HasPrefix(data, []byte("---\n")) || end < 0 {
		return ""
	}
	return strings.TrimSpace(string(data[4+end+5:]))
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

type skillDeclaration struct {
	Name                 string
	Root                 string
	SupportingFiles      []string
	Disabled             bool
	AllowSupportingFiles bool
}

type SkillFile struct {
	Name, Digest string
	Data         []byte
}

type SkillPackage struct {
	Name, Description              string
	Disabled, AllowSupportingFiles bool
	Instructions                   SkillFile
	Supporting                     map[string]SkillFile
	Digest                         string
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
	out.Tools = cloneSelectors(in.Tools)
	out.Deny = append([]string(nil), in.Deny...)
	out.Skills = make([]SkillPackage, len(in.Skills))
	for i, skill := range in.Skills {
		out.Skills[i] = cloneSkill(skill)
	}
	return out
}
