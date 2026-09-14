package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
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
	resolved, err := store.ResolveDefinition(ctx, owner, "planner", newJourney.Digest)
	if err != nil || resolved != oldJourney.Digest {
		t.Fatalf("old current resolution = %q, %v", resolved, err)
	}
	if err := store.ActivateDefinition(ctx, "planner", oldJourney.Digest, newJourney.Digest, registry.HasDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(ctx, owner, "planner", resolved); err != nil {
		t.Fatalf("binding winner did not stay pinned through activation: %v", err)
	}
	if err := store.Finish(ctx, owner, journal.RunCompleted, []byte(`[]`), "done", "", ""); err != nil {
		t.Fatal(err)
	}

	continued := journal.Command{ThreadID: "old-thread", RunID: "old-next", CommunicationID: "old-next", Principal: "alice", Text: "again"}
	if _, _, err := store.Admit(ctx, continued); err != nil {
		t.Fatal(err)
	}
	continuedOwner := claimForTest(t, store, continued, false)
	if resolved, err = store.ResolveDefinition(ctx, continuedOwner, "planner", newJourney.Digest); err != nil || resolved != oldJourney.Digest {
		t.Fatalf("completed session fell forward = %q, %v", resolved, err)
	}
	newCommand := journal.Command{ThreadID: "new-thread", RunID: "new-run", CommunicationID: "new-command", Principal: "alice", Text: "plan"}
	if _, _, err := store.Admit(ctx, newCommand); err != nil {
		t.Fatal(err)
	}
	newOwner := claimForTest(t, store, newCommand, false)
	if resolved, err = store.ResolveDefinition(ctx, newOwner, "planner", oldJourney.Digest); err != nil || resolved != newJourney.Digest {
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

func TestDeclarativeJourneyRouting(t *testing.T) {
	root := t.TempDir()
	path := writeTwoJourneyBundle(t, root, "http://example")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config := strings.ReplaceAll(string(data), `"id":"vacation-planner"`, `"id":"alpha"`)
	config = strings.ReplaceAll(config, `"id":"shift-swap"`, `"id":"beta"`)
	config = strings.Replace(config, `"routing":{"keywords":["shift","swap"],"priority":10}`, `"routing":{"keywords":["quasar"],"priority":7}`, 1)
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	registry, err := definitions.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := agentruntime.NewRunner(registry, platformmcp.NewClient(), nil)
	store := journal.New(database(t))
	bind := func(thread, text, target, want string) {
		command := journal.Command{ThreadID: thread, RunID: thread, CommunicationID: thread, Principal: "alice", Text: text, TargetJourney: target}
		if _, _, err := store.Admit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
		owner := claimForTest(t, store, command, false)
		got, _, err := runner.Binding(t.Context(), agentruntime.RunInput{Store: store, Owner: owner, ThreadID: thread, Text: text, TargetJourney: target})
		if err != nil || got != want {
			t.Fatalf("route %q = %q, %v", text, got, err)
		}
	}
	bind("inferred", "please quasar now", "", "beta")
	bind("default", "unmatched", "", "alpha")
	bind("explicit", "please quasar now", "alpha", "alpha")
}

func TestResolveDefinitionRejectsOwnerExpiredWhileWaitingForRollout(t *testing.T) {
	pool := database(t)
	store := journal.New(pool)
	command := journal.Command{ThreadID: "definition-owner", RunID: "definition-owner", CommunicationID: "definition-owner", Principal: "alice", Text: "plan"}
	if _, _, err := store.Admit(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	owner, err := store.Claim(t.Context(), command, "stale-owner", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(t.Context())
	if _, err := gate.Exec(t.Context(), "SELECT pg_advisory_xact_lock(789134628)"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.ResolveDefinition(context.Background(), owner, "planner", strings.Repeat("a", 64))
		done <- err
	}()
	waitDatabase(t, pool, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted)")
	if _, err := pool.Exec(t.Context(), "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", command.RunID); err != nil {
		t.Fatal(err)
	}
	if err := store.Reap(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := gate.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, journal.ErrOwnership) {
		t.Fatalf("stale definition pin = %v", err)
	}
	var sessions int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_sessions WHERE thread_id=$1", command.ThreadID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("stale owner inserted %d sessions: %v", sessions, err)
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

func TestDefinitionRetainsVersionServerBinding(t *testing.T) {
	oldPath := writeDefinitionBundle(t, t.TempDir(), "old prompt\n")
	newPath := writeDefinitionBundle(t, t.TempDir(), "new prompt\n")
	for path, name := range map[string]string{oldPath: "legacy", newPath: "current"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.ReplaceAll(string(data), `"id":"s"`, fmt.Sprintf(`"id":%q`, name)))
		data = []byte(strings.ReplaceAll(string(data), `"server":"s"`, fmt.Sprintf(`"server":%q`, name)))
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	old, err := definitions.Load(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	oldJourney, _ := old.Journey("planner")
	registry, err := definitions.Load(newPath, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	server, ok := registry.ServerFor(oldJourney.Digest)
	if !ok || server.ID != "legacy" || !registry.HasDigest(oldJourney.Digest) {
		t.Fatalf("retained binding = %#v, %v", server, ok)
	}
}

func TestDefinitionRejectsAggregateSkillLimits(t *testing.T) {
	for _, test := range []struct {
		name, body string
		files      int
	}{
		{name: "bytes", files: 9, body: strings.Repeat("x", definitions.MaxSupportingBytes)},
		{name: "file count", files: definitions.MaxCompiledSkillFiles + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "skills", "planner"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "prompt.md"), []byte("prompt"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "skills", "planner", "SKILL.md"), []byte("---\nname: planner\ndescription: Plan\n---\nbody\n"), 0600); err != nil {
				t.Fatal(err)
			}
			files := make([]string, test.files)
			for i := range files {
				files[i] = fmt.Sprintf("file-%04d.txt", i)
				if err := os.WriteFile(filepath.Join(root, "skills", "planner", files[i]), []byte(test.body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			config, err := json.Marshal(struct {
				MCPServers    []definitions.MCPServer        `json:"mcp_servers"`
				SkillPackages []definitions.SkillDeclaration `json:"skill_packages"`
				Journeys      []definitions.Journey          `json:"journeys"`
			}{
				MCPServers:    []definitions.MCPServer{{ID: "s", Endpoint: "http://example", Tools: []string{"lookup"}}},
				SkillPackages: []definitions.SkillDeclaration{{Name: "planner", Root: "skills/planner", SupportingFiles: files, AllowSupportingFiles: true}},
				Journeys:      []definitions.Journey{{ID: "planner", Description: "Plan", SystemPrompt: "prompt.md", MCP: definitions.MCPBinding{Server: "s", Tools: []string{"lookup"}}, SkillNames: []string{"planner"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "journeys.json")
			if err := os.WriteFile(path, config, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := definitions.Load(path); err == nil {
				t.Fatal("aggregate skill limit accepted")
			}
		})
	}
}
