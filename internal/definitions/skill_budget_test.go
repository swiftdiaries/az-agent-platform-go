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

func writeSkillBudgetConfig(t *testing.T, root string, index int) string {
	t.Helper()
	path := filepath.Join(root, fmt.Sprintf("journeys-%03d.json", index))
	config := fmt.Sprintf(`{"mcp_servers":[{"id":"s","endpoint":"http://example","tools":["lookup"]}],"skill_packages":[{"name":"planner","root":"skills/planner","supporting_files":["guide.md"],"allow_supporting_files":true}],"journeys":[{"id":"journey-%03d","description":"Plan","system_prompt":"prompt.md","mcp":{"server":"s","tools":["lookup"]},"skills":["planner"]}]}`, index)
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeSkillBudgetConfigWithJourneys(t *testing.T, root string, count int) string {
	t.Helper()
	path := filepath.Join(root, "journeys.json")
	var config strings.Builder
	config.WriteString(`{"mcp_servers":[{"id":"s","endpoint":"http://example","tools":["lookup"]}],"skill_packages":[{"name":"planner","root":"skills/planner","supporting_files":["guide.md"],"allow_supporting_files":true}],"journeys":[`)
	for i := 0; i < count; i++ {
		if i > 0 {
			config.WriteByte(',')
		}
		fmt.Fprintf(&config, `{"id":"journey-%03d","description":"Plan","system_prompt":"prompt.md","mcp":{"server":"s","tools":["lookup"]},"skills":["planner"]}`, i)
	}
	config.WriteString(`]}`)
	if err := os.WriteFile(path, []byte(config.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSkillSnapshotsAreSharedAndCallerImmutable(t *testing.T) {
	root := t.TempDir()
	writeSkillBudgetFiles(t, root)
	path := writeSkillBudgetConfigWithJourneys(t, root, 64)
	registry, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if registry.skillFiles != 2 {
		t.Fatalf("skill file budget counted journey references: got %d", registry.skillFiles)
	}

	first := registry.journeys["journey-000"].Skills[0]
	second := registry.journeys["journey-001"].Skills[0]
	if &first.Instructions.Data[0] != &second.Instructions.Data[0] {
		t.Fatal("journey compilation copied instruction bodies")
	}
	firstGuide := first.Supporting["guide.md"]
	secondGuide := second.Supporting["guide.md"]
	if &firstGuide.Data[0] != &secondGuide.Data[0] {
		t.Fatal("journey compilation copied supporting bodies")
	}

	digest := registry.journeys["journey-000"].Digest
	copy, ok := registry.Version(digest)
	if !ok {
		t.Fatal("compiled version is missing")
	}
	copy.Skills[0].Instructions.Data[0] = 'X'
	copy.Skills[0].Supporting["guide.md"].Data[0] = 'X'
	again := registry.journeys["journey-000"].Skills[0]
	if again.Instructions.Data[0] != '-' || again.Supporting["guide.md"].Data[0] != 'P' {
		t.Fatal("caller mutation changed the immutable registry snapshot")
	}
}

func TestSkillBudgetRejectsTooManyRetainedBundles(t *testing.T) {
	root := t.TempDir()
	writeSkillBudgetFiles(t, root)
	bundleCount := MaxCompiledSkillFiles / 2
	paths := make([]string, bundleCount+1)
	for i := range paths {
		paths[i] = writeSkillBudgetConfig(t, root, i)
	}

	registry, err := Load(paths[0], paths[1:bundleCount]...)
	if err != nil {
		t.Fatalf("rejected retained bundles at aggregate file limit: %v", err)
	}
	if registry.skillFiles != MaxCompiledSkillFiles {
		t.Fatalf("retained skill file budget = %d, want %d", registry.skillFiles, MaxCompiledSkillFiles)
	}
	registry, err = Load(paths[0], paths[1:]...)
	if err == nil {
		t.Fatal("accepted retained skill bundles beyond aggregate file limit")
	}
	if !strings.Contains(err.Error(), "retained skill bundles exceed aggregate limits") {
		t.Fatalf("unexpected aggregate limit error: %v", err)
	}
	if registry != nil {
		t.Fatal("returned a registry after exceeding the retained budget")
	}

	if _, err := Load(paths[0], paths[1], paths[1]); err != nil {
		t.Fatalf("reloading an already retained bundle consumed budget: %v", err)
	}
}
