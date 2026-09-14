package skills

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
)

var (
	ErrUnavailable = errors.New("skill material is unavailable")
	ErrChanged     = errors.New("skill package changed after compilation")
)

type Metadata struct{ Name, Description, Digest string }

type Material struct {
	Name, Resource, SkillDigest, FileDigest string
	Body                                    []byte
}

type Source struct {
	skills map[string]definitions.SkillPackage
}

func New(journey definitions.Journey) *Source {
	source := &Source{skills: make(map[string]definitions.SkillPackage, len(journey.Skills))}
	for _, skill := range journey.Skills {
		copy := skill
		copy.Instructions.Data = bytes.Clone(skill.Instructions.Data)
		copy.Supporting = make(map[string]definitions.SkillFile, len(skill.Supporting))
		for name, file := range skill.Supporting {
			file.Data = bytes.Clone(file.Data)
			copy.Supporting[name] = file
		}
		source.skills[skill.Name] = copy
	}
	return source
}

// Catalog returns selection metadata without touching instruction or resource files.
func (s *Source) Catalog() []Metadata {
	result := make([]Metadata, 0, len(s.skills))
	for _, skill := range s.skills {
		if !skill.Disabled {
			result = append(result, Metadata{Name: skill.Name, Description: skill.Description, Digest: skill.Digest})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// Read returns only compiled, declared material after verifying the package is unchanged.
func (s *Source) Read(name, resource string) (Material, error) {
	skill, ok := s.skills[name]
	if !ok || skill.Disabled {
		return Material{}, ErrUnavailable
	}
	var file definitions.SkillFile
	if resource == "" || resource == "SKILL.md" {
		resource, file = "SKILL.md", skill.Instructions
	} else {
		if !skill.AllowSupportingFiles {
			return Material{}, ErrUnavailable
		}
		file, ok = skill.Supporting[resource]
		if !ok {
			return Material{}, ErrUnavailable
		}
	}
	info, err := os.Lstat(file.Path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(file.Data)) {
		return Material{}, ErrChanged
	}
	current, err := os.ReadFile(file.Path)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(current)) != file.Digest {
		return Material{}, ErrChanged
	}
	return Material{Name: skill.Name, Resource: resource, SkillDigest: skill.Digest, FileDigest: file.Digest, Body: bytes.Clone(file.Data)}, nil
}
