package definitions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkillBudgetFiles(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "skills", "planner"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "prompt.md"), []byte("prompt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skills", "planner", "SKILL.md"), []byte("---\nname: planner\ndescription: Plan\n---\nUse the planner.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skills", "planner", "guide.md"), []byte("Prefer direct routes.\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

// writeSkillBudgetConfigWithJourneys writes a config whose journeys all share
// one skill bundle. The file name is unique per index so several can live in
// one directory.
func writeSkillBudgetConfigWithJourneys(t *testing.T, root, file string, first, count int) string {
	t.Helper()
	path := filepath.Join(root, file)
	var config strings.Builder
	config.WriteString("catalog:\n  operation_meta_key: op\njourneys:\n")
	for i := first; i < first+count; i++ {
		fmt.Fprintf(&config, "  - id: journey-%03d\n    skill: skills/planner\n    system_prompt: prompt.md\n    tools:\n      - op: lookup\n", i)
	}
	if err := os.WriteFile(path, []byte(config.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSkillSnapshotsAreSharedAndCallerImmutable(t *testing.T) {
	root := t.TempDir()
	writeSkillBudgetFiles(t, root)
	path := writeSkillBudgetConfigWithJourneys(t, root, "journeys.yaml", 0, 64)
	set, err := Compile(path)
	if err != nil {
		t.Fatal(err)
	}
	if set.skillFiles != 2 {
		t.Fatalf("skill file budget counted journey references: got %d", set.skillFiles)
	}

	first := set.journeys["journey-000"].Skills[0]
	second := set.journeys["journey-001"].Skills[0]
	if &first.Instructions.Data[0] != &second.Instructions.Data[0] {
		t.Fatal("journey compilation copied instruction bodies")
	}
	firstGuide := first.Supporting["guide.md"]
	secondGuide := second.Supporting["guide.md"]
	if &firstGuide.Data[0] != &secondGuide.Data[0] {
		t.Fatal("journey compilation copied supporting bodies")
	}

	digest := set.journeys["journey-000"].Digest
	copy, _, ok := set.Pinned(digest)
	if !ok {
		t.Fatal("compiled version is missing")
	}
	copy.Skills[0].Instructions.Data[0] = 'X'
	copy.Skills[0].Supporting["guide.md"].Data[0] = 'X'
	again := set.journeys["journey-000"].Skills[0]
	if again.Instructions.Data[0] != '-' || again.Supporting["guide.md"].Data[0] != 'P' {
		t.Fatal("caller mutation changed the immutable set snapshot")
	}
}

func TestSkillBudgetRejectsTooManyRetainedBundles(t *testing.T) {
	root := t.TempDir()
	writeSkillBudgetFiles(t, root)
	bundleCount := MaxCompiledSkillFiles / 2
	sets := make([]*Set, bundleCount+1)
	for i := range sets {
		path := writeSkillBudgetConfigWithJourneys(t, root, fmt.Sprintf("journeys-%03d.yaml", i), i, 1)
		set, err := Compile(path)
		if err != nil {
			t.Fatal(err)
		}
		sets[i] = set
	}

	retained, err := Retain(sets[0], sets[1:bundleCount]...)
	if err != nil {
		t.Fatalf("rejected retained bundles at aggregate file limit: %v", err)
	}
	if retained.skillFiles != MaxCompiledSkillFiles {
		t.Fatalf("retained skill file budget = %d, want %d", retained.skillFiles, MaxCompiledSkillFiles)
	}
	retained, err = Retain(sets[0], sets[1:]...)
	if err == nil {
		t.Fatal("accepted retained skill bundles beyond aggregate file limit")
	}
	if !strings.Contains(err.Error(), "retained skill bundles exceed aggregate limits") {
		t.Fatalf("unexpected aggregate limit error: %v", err)
	}
	if retained != nil {
		t.Fatal("returned a set after exceeding the retained budget")
	}

	if _, err := Retain(sets[0], sets[1], sets[1]); err != nil {
		t.Fatalf("reloading an already retained bundle consumed budget: %v", err)
	}
}

func TestCompileRejectsAggregateSkillLimitInOneSet(t *testing.T) {
	root := t.TempDir()
	writeSkillBudgetFiles(t, root)
	var config strings.Builder
	config.WriteString("catalog:\n  operation_meta_key: op\njourneys:\n")
	for i := 0; i < MaxCompiledSkillFiles/2+1; i++ {
		dir := filepath.Join(root, "skills", fmt.Sprintf("s%03d", i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		skill := fmt.Sprintf("---\nname: s%03d\ndescription: Plan\n---\nBody.\n", i)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skill), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "guide.md"), []byte("g\n"), 0600); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&config, "  - id: j%03d\n    skill: skills/s%03d\n    system_prompt: prompt.md\n    tools:\n      - op: lookup\n", i, i)
	}
	path := filepath.Join(root, "journeys.yaml")
	if err := os.WriteFile(path, []byte(config.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if set, err := Compile(path); err == nil || set != nil || !strings.Contains(err.Error(), "exceed aggregate limits") {
		t.Fatalf("aggregate skill limit not enforced in one set: %v", err)
	}
}
