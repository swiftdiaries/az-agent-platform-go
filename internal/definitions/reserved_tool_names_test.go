package definitions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductToolNamesAreRejectedAtCompileBoundary(t *testing.T) {
	for _, name := range []string{"load_skill", "read_skill_resource", "request_user_input"} {
		t.Run("catalog/"+name, func(t *testing.T) {
			path := writeToolNameConfig(t, []string{"lookup", name}, []string{"lookup"})
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "reserved for the product runtime") {
				t.Fatalf("expected reserved catalog tool error, got %v", err)
			}
		})
		t.Run("allowlist/"+name, func(t *testing.T) {
			path := writeToolNameConfig(t, []string{"lookup"}, []string{name})
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "reserved for the product runtime") {
				t.Fatalf("expected reserved allowlist tool error, got %v", err)
			}
		})
	}
}

func TestOrdinaryAndCaseVariantToolNamesRemainAllowed(t *testing.T) {
	path := writeToolNameConfig(t, []string{"lookup", "Load_Skill"}, []string{"lookup"})
	if _, err := Load(path); err != nil {
		t.Fatalf("ordinary and case-variant tool names should remain allowed: %v", err)
	}
}

func TestConfinedPathRejectsRawParentComponents(t *testing.T) {
	root := t.TempDir()
	writeSkillBudgetFiles(t, root)

	for _, tc := range []struct {
		name        string
		declaration SkillDeclaration
	}{
		{name: "package root", declaration: SkillDeclaration{Name: "planner", Root: "skills/../skills/planner"}},
		{name: "supporting file", declaration: SkillDeclaration{Name: "planner", Root: "skills/planner", SupportingFiles: []string{"nested/../guide.md"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := compileSkill(root, tc.declaration); err == nil || !strings.Contains(err.Error(), "parent components are not allowed") {
				t.Fatalf("expected raw parent component rejection, got %v", err)
			}
		})
	}
}

func writeToolNameConfig(t *testing.T, catalog, allowlist []string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prompt.md"), []byte("prompt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"mcp_servers":[{"id":"s","endpoint":"http://example","tools":[%s]}],"journeys":[{"id":"j","description":"test","system_prompt":"prompt.md","mcp":{"server":"s","tools":[%s]}}]}`, quoteTools(catalog), quoteTools(allowlist))
	path := filepath.Join(root, "journeys.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func quoteTools(tools []string) string {
	quoted := make([]string, len(tools))
	for i, tool := range tools {
		quoted[i] = fmt.Sprintf("%q", tool)
	}
	return strings.Join(quoted, ",")
}
