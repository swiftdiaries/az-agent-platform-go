package skills

import (
	"errors"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
)

const memSkill = "---\nname: planner\ndescription: Plan time off\n---\nUse the planner.\n"

func memJourney(t *testing.T) definitions.Journey {
	t.Helper()
	set, err := definitions.CompileBundle(definitions.Bundle{
		definitions.BundleConfig:  []byte("catalog:\n  operation_meta_key: op\njourneys:\n  - id: j\n    skill: skills/planner\n    tools:\n      - op: lookup\n"),
		"skills/planner/SKILL.md": []byte(memSkill),
		"skills/planner/ref/g.md": []byte("guide\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := set.Journey("j")
	return j
}

func TestSourceInMemoryRead(t *testing.T) {
	src := New(memJourney(t))
	m, err := src.Read("planner", "")
	if err != nil || string(m.Body) != memSkill || m.Resource != "SKILL.md" {
		t.Fatalf("SKILL.md: %+v %v", m, err)
	}
	m, err = src.Read("planner", "ref/g.md")
	if err != nil || string(m.Body) != "guide\n" || m.FileDigest == "" {
		t.Fatalf("supporting: %+v %v", m, err)
	}
	m.Body[0] = 'X'
	if again, _ := src.Read("planner", "ref/g.md"); string(again.Body) != "guide\n" {
		t.Fatal("returned body aliases the source")
	}
}

func TestSourceInMemoryTampered(t *testing.T) {
	j := memJourney(t)
	j.Skills[0].Instructions.Data[len(j.Skills[0].Instructions.Data)-2] ^= 1
	f := j.Skills[0].Supporting["ref/g.md"]
	f.Data[0] ^= 1
	if _, err := New(j).Read("planner", ""); !errors.Is(err, ErrChanged) {
		t.Fatalf("SKILL.md err = %v", err)
	}
	if _, err := New(j).Read("planner", "ref/g.md"); !errors.Is(err, ErrChanged) {
		t.Fatalf("supporting err = %v", err)
	}
}
