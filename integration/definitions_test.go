package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
)

func writeDefinitionBundle(t *testing.T, root, prompt string) string {
	t.Helper()
	for _, dir := range []string{"prompts", "skills/planner"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "prompts/planner.md"), []byte(prompt), 0600); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: planner\ndescription: Plan a trip\n---\nUse the destination tool.\n"
	if err := os.WriteFile(filepath.Join(root, "skills/planner/SKILL.md"), []byte(skill), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skills/planner/guide.md"), []byte("Prefer direct routes.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := `{"mcp_servers":[{"id":"s","endpoint":"http://example","forward_headers":["Authorization"],"tools":["lookup"],"policies":{"lookup":{"class":"read_only"}}}],"skill_packages":[{"name":"planner","root":"skills/planner","supporting_files":["guide.md"],"allow_supporting_files":true}],"journeys":[{"id":"planner","description":"Plan a trip","system_prompt":"prompts/planner.md","mcp":{"server":"s","tools":["lookup"]},"skills":["planner"]}]}`
	path := filepath.Join(root, "journeys.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVersionActivationPreservesPinnedSessions(t *testing.T) {
	ctx := context.Background()
	oldRoot, newRoot := t.TempDir(), t.TempDir()
	oldPath := writeDefinitionBundle(t, oldRoot, "old prompt\n")
	newPath := writeDefinitionBundle(t, newRoot, "new prompt\n")
	oldRegistry, err := definitions.Load(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := definitions.Load(newPath, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	oldJourney, _ := oldRegistry.Journey("planner")
	newJourney, _ := registry.Journey("planner")
	pool := database(t)
	store := journal.New(pool)
	if err := store.ActivateDefinition(ctx, "planner", "", oldJourney.Digest, oldRegistry.HasDigest); err != nil {
		t.Fatal(err)
	}

	command := journal.Command{ThreadID: "old-thread", RunID: "old-run", CommunicationID: "old-command", Principal: "alice", Text: "plan"}
	if _, _, err := store.Admit(ctx, command); err != nil {
		t.Fatal(err)
	}
	owner := claimForTest(t, store, command, true)
	resolved, err := store.ResolveDefinition(ctx, command.ThreadID, "planner", newJourney.Digest)
	if err != nil || resolved != oldJourney.Digest {
		t.Fatalf("old current resolution = %q, %v", resolved, err)
	}
	if _, err := store.Start(ctx, owner, "planner", resolved); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, owner, journal.RunCompleted, []byte(`[]`), "done", "", ""); err != nil {
		t.Fatal(err)
	}

	if err := store.ActivateDefinition(ctx, "planner", oldJourney.Digest, newJourney.Digest, registry.HasDigest); err != nil {
		t.Fatal(err)
	}
	if resolved, err = store.ResolveDefinition(ctx, "old-thread", "planner", newJourney.Digest); err != nil || resolved != oldJourney.Digest {
		t.Fatalf("completed session fell forward = %q, %v", resolved, err)
	}
	if resolved, err = store.ResolveDefinition(ctx, "new-thread", "planner", oldJourney.Digest); err != nil || resolved != newJourney.Digest {
		t.Fatalf("new session ignored current = %q, %v", resolved, err)
	}
	if err := store.ActivateDefinition(ctx, "planner", oldJourney.Digest, oldJourney.Digest, registry.HasDigest); !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("stale activation = %v", err)
	}
	if current, err := store.CurrentDefinition(ctx, "planner"); err != nil || current != newJourney.Digest {
		t.Fatalf("failed activation changed current = %q, %v", current, err)
	}

	candidateOnly, err := definitions.Load(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := store.DefinitionsReady(ctx, candidateOnly.HasDigest); err != nil || ready {
		t.Fatalf("missing retained bundle readiness = %v, %v", ready, err)
	}
	if ready, err := store.DefinitionsReady(ctx, registry.HasDigest); err != nil || !ready {
		t.Fatalf("complete retained bundle readiness = %v, %v", ready, err)
	}
}

func TestDefinitionCompilationIsImmutableAndContentAddressed(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	oldPath := writeDefinitionBundle(t, a, "old prompt\n")
	copyPath := writeDefinitionBundle(t, b, "old prompt\n")
	old, err := definitions.Load(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	copyRegistry, err := definitions.Load(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := old.Journey("planner")
	second, _ := copyRegistry.Journey("planner")
	if first.Digest == "" || first.Digest != second.Digest {
		t.Fatalf("root-dependent digest: %q != %q", first.Digest, second.Digest)
	}

	first.MCP.Tools[0] = "mutated"
	first.Policies["lookup"] = definitions.ToolPolicy{Class: "effectful"}
	first.Skills[0].Instructions.Data[0] = 'X'
	again, _ := old.Version(second.Digest)
	if again.MCP.Tools[0] != "lookup" || again.Policies["lookup"].Class != "read_only" || string(again.Skills[0].Instructions.Data) != "---\nname: planner\ndescription: Plan a trip\n---\nUse the destination tool.\n" {
		t.Fatalf("caller mutation changed compiled definition: %#v", again)
	}

	newRoot := t.TempDir()
	newPath := writeDefinitionBundle(t, newRoot, "new prompt\n")
	registry, err := definitions.Load(newPath, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := registry.Journey("planner")
	retained, ok := registry.Version(second.Digest)
	if !ok || retained.Prompt != "old prompt\n" || current.Digest == retained.Digest {
		t.Fatalf("retained versions: current=%#v old=%#v", current, retained)
	}
}

func TestDefinitionCompilationRejectsUnsafePackages(t *testing.T) {
	cases := []struct {
		name string
		edit func(*testing.T, string)
	}{
		{"missing skill", func(t *testing.T, root string) { os.Remove(filepath.Join(root, "skills/planner/SKILL.md")) }},
		{"symlink", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "skills/planner/SKILL.md")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, "prompts/planner.md"), filepath.Join(root, "skills/planner/SKILL.md")); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized prompt", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "prompts/planner.md"), []byte(strings.Repeat("x", definitions.MaxPromptBytes+1)), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"traversal", func(t *testing.T, root string) {
			path := filepath.Join(root, "journeys.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), `"root":"skills/planner"`, `"root":"../planner"`, 1))
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := writeDefinitionBundle(t, root, "prompt\n")
			test.edit(t, root)
			if _, err := definitions.Load(path); err == nil {
				t.Fatal(fmt.Sprintf("accepted %s", test.name))
			}
		})
	}
}
