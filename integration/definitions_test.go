package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

// writeDefinitionBundle writes a one-journey ("planner") bundle whose single
// tool is the read-only operation "lookup".
func writeDefinitionBundle(t *testing.T, root, prompt string) string {
	t.Helper()
	return writeBundleConfig(t, root, prompt, "lookup", "read_only")
}

// writeBundleConfig writes the planner bundle (prompt, skill with guide.md and a
// journeys.yaml) with one tool selecting operation key op. class "" or
// "effectful" leaves the tool unclassified (effectful).
func writeBundleConfig(t *testing.T, root, prompt, op, class string) string {
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
	tool := "      - op: " + op + "\n"
	if class != "" && class != "effectful" {
		tool += "        class: " + class + "\n"
	}
	config := "catalog:\n  operation_meta_key: " + testMetaKey + "\n  call_id_header: X-Platform-Call-ID\n" +
		"journeys:\n  - id: planner\n    skill: skills/planner\n    system_prompt: prompts/planner.md\n    tools:\n" + tool
	path := filepath.Join(root, "journeys.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// compileSet compiles path and, when retained paths are given, retains their
// journey versions in the returned set.
func compileSet(t *testing.T, path string, retained ...string) *definitions.Set {
	t.Helper()
	current, err := definitions.Compile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) == 0 {
		return current
	}
	var old []*definitions.Set
	for _, p := range retained {
		set, err := definitions.Compile(p)
		if err != nil {
			t.Fatal(err)
		}
		old = append(old, set)
	}
	set, err := definitions.Retain(current, old...)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestVersionActivationPreservesPinnedSessions(t *testing.T) {
	ctx := context.Background()
	oldRoot, newRoot := t.TempDir(), t.TempDir()
	oldPath := writeDefinitionBundle(t, oldRoot, "old prompt\n")
	newPath := writeDefinitionBundle(t, newRoot, "new prompt\n")
	oldRegistry := compileSet(t, oldPath)
	registry := compileSet(t, newPath, oldPath)
	oldJourney, _ := oldRegistry.Journey("planner")
	newJourney, _ := registry.Journey("planner")
	pool := database(t)
	store := journal.New(pool)
	if err := store.ActivateDefinition(ctx, "planner", "", oldJourney.Digest, oldRegistry.JourneyIDForDigest); err != nil {
		t.Fatal(err)
	}

	command := journal.Command{ThreadID: "old-thread", RunID: "old-run", CommunicationID: "old-command", Principal: "alice", Text: "plan"}
	if _, _, err := store.Admit(ctx, command); err != nil {
		t.Fatal(err)
	}
	owner := claimForTest(t, store, command, true)
	resolved, err := store.ResolveDefinition(ctx, owner, "planner", newJourney.Digest, registry.JourneyIDForDigest)
	if err != nil || resolved != oldJourney.Digest {
		t.Fatalf("old current resolution = %q, %v", resolved, err)
	}
	if err := store.ActivateDefinition(ctx, "planner", oldJourney.Digest, newJourney.Digest, registry.JourneyIDForDigest); err != nil {
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
	if resolved, err = store.ResolveDefinition(ctx, continuedOwner, "planner", newJourney.Digest, registry.JourneyIDForDigest); err != nil || resolved != oldJourney.Digest {
		t.Fatalf("completed session fell forward = %q, %v", resolved, err)
	}
	newCommand := journal.Command{ThreadID: "new-thread", RunID: "new-run", CommunicationID: "new-command", Principal: "alice", Text: "plan"}
	if _, _, err := store.Admit(ctx, newCommand); err != nil {
		t.Fatal(err)
	}
	newOwner := claimForTest(t, store, newCommand, false)
	if resolved, err = store.ResolveDefinition(ctx, newOwner, "planner", oldJourney.Digest, registry.JourneyIDForDigest); err != nil || resolved != newJourney.Digest {
		t.Fatalf("new session ignored current = %q, %v", resolved, err)
	}
	if err := store.ActivateDefinition(ctx, "planner", oldJourney.Digest, oldJourney.Digest, registry.JourneyIDForDigest); !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("stale activation = %v", err)
	}
	if current, err := store.CurrentDefinition(ctx, "planner"); err != nil || current != newJourney.Digest {
		t.Fatalf("failed activation changed current = %q, %v", current, err)
	}

	candidateOnly := compileSet(t, newPath)
	if ready, err := store.DefinitionsReady(ctx, candidateOnly.JourneyIDForDigest); err != nil || ready {
		t.Fatalf("missing retained bundle readiness = %v, %v", ready, err)
	}
	if ready, err := store.DefinitionsReady(ctx, registry.JourneyIDForDigest); err != nil || !ready {
		t.Fatalf("complete retained bundle readiness = %v, %v", ready, err)
	}
}

func TestCrossJourneyActivationCannotMutateCurrentOrPin(t *testing.T) {
	ctx := context.Background()
	oldRoot, currentRoot, secondJourneyRoot := t.TempDir(), t.TempDir(), t.TempDir()
	oldPath := writeDefinitionBundle(t, oldRoot, "old prompt\n")
	currentPath := writeDefinitionBundle(t, currentRoot, "current prompt\n")
	secondJourneyPath := writeTwoJourneyBundle(t, secondJourneyRoot)
	oldRegistry := compileSet(t, oldPath)
	secondJourneyRegistry := compileSet(t, secondJourneyPath)
	registry := compileSet(t, currentPath, oldPath, secondJourneyPath)
	oldPlanner, _ := oldRegistry.Journey("planner")
	currentPlanner, _ := registry.Journey("planner")
	shiftSwap, _ := secondJourneyRegistry.Journey("shift-swap")
	pool := database(t)
	store := journal.New(pool)
	if err := store.ActivateDefinition(ctx, "planner", "", oldPlanner.Digest, registry.JourneyIDForDigest); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateDefinition(ctx, "planner", oldPlanner.Digest, shiftSwap.Digest, registry.JourneyIDForDigest); !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("cross-journey activation = %v", err)
	}
	var current string
	if err := pool.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current WHERE journey_id='planner'").Scan(&current); err != nil || current != oldPlanner.Digest {
		t.Fatalf("cross-journey activation changed current = %q, %v", current, err)
	}
	var pins int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_sessions WHERE journey_id='planner'").Scan(&pins); err != nil || pins != 0 {
		t.Fatalf("cross-journey activation created pins = %d, %v", pins, err)
	}
	if err := store.ActivateDefinition(ctx, "planner", oldPlanner.Digest, currentPlanner.Digest, registry.JourneyIDForDigest); err != nil {
		t.Fatalf("retained activation = %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current WHERE journey_id='planner'").Scan(&current); err != nil || current != currentPlanner.Digest {
		t.Fatalf("retained activation current = %q, %v", current, err)
	}

	if _, err := pool.Exec(ctx, "UPDATE agent_definition_current SET definition_digest=$1 WHERE journey_id='planner'", shiftSwap.Digest); err != nil {
		t.Fatal(err)
	}
	if ready, err := store.DefinitionsReady(ctx, registry.JourneyIDForDigest); err != nil || ready {
		t.Fatalf("mismatched current readiness = %v, %v", ready, err)
	}
	command := journal.Command{ThreadID: "mismatched-current", RunID: "mismatched-current", CommunicationID: "mismatched-current", Principal: "alice", Text: "plan"}
	if _, _, err := store.Admit(ctx, command); err != nil {
		t.Fatal(err)
	}
	owner := claimForTest(t, store, command, false)
	if _, err := store.ResolveDefinition(ctx, owner, "planner", currentPlanner.Digest, registry.JourneyIDForDigest); !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("mismatched current resolution = %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_sessions WHERE thread_id=$1", command.ThreadID).Scan(&pins); err != nil || pins != 0 {
		t.Fatalf("mismatched current created pin = %d, %v", pins, err)
	}
}

func TestDeclarativeJourneyRouting(t *testing.T) {
	root := t.TempDir()
	path := writeTwoJourneyBundle(t, root)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config := strings.ReplaceAll(string(data), "id: vacation-planner", "id: alpha")
	config = strings.ReplaceAll(config, "id: shift-swap", "id: beta")
	config = strings.Replace(config, "routing: {keywords: [shift, swap], priority: 10}", "routing: {keywords: [quasar], priority: 7}", 1)
	if !strings.Contains(config, "quasar") || !strings.Contains(config, "id: alpha") {
		t.Fatal("routing edit did not apply")
	}
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	registry := compileSet(t, path)
	runner := agentruntime.NewRunner(registry, nil, agentruntime.Tenancy{}) // Binding only: no tenancy needed
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
		_, err := store.ResolveDefinition(context.Background(), owner, "planner", strings.Repeat("a", 64), func(string) (string, bool) { return "planner", true })
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
	old := compileSet(t, oldPath)
	copyRegistry := compileSet(t, copyPath)
	first, _ := old.Journey("planner")
	second, _ := copyRegistry.Journey("planner")
	if first.Digest == "" || first.Digest != second.Digest {
		t.Fatalf("root-dependent digest: %q != %q", first.Digest, second.Digest)
	}

	first.Tools[0].Ops[0] = "mutated"
	first.Tools[0].Class = definitions.ClassEffectful
	first.Skills[0].Instructions.Data[0] = 'X'
	again, _, ok := old.Pinned(second.Digest)
	if !ok || again.Tools[0].Ops[0] != "lookup" || again.Tools[0].Class != definitions.ClassReadOnly || string(again.Skills[0].Instructions.Data) != "---\nname: planner\ndescription: Plan a trip\n---\nUse the destination tool.\n" {
		t.Fatalf("caller mutation changed compiled definition: %#v", again)
	}

	newRoot := t.TempDir()
	newPath := writeDefinitionBundle(t, newRoot, "new prompt\n")
	registry := compileSet(t, newPath, oldPath)
	current, _ := registry.Journey("planner")
	retained, _, ok := registry.Pinned(second.Digest)
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
			data = []byte(strings.Replace(string(data), "skill: skills/planner", "skill: ../planner", 1))
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
			if _, err := definitions.Compile(path); err == nil {
				t.Fatalf("accepted %s", test.name)
			}
		})
	}
}

// TestDefinitionRetainsVersionCatalogBinding checks that a retained journey
// version keeps its own tool selection and class, not the current set's.
func TestDefinitionRetainsVersionCatalogBinding(t *testing.T) {
	oldPath := writeBundleConfig(t, t.TempDir(), "old prompt\n", "legacy_lookup", "read_only")
	newPath := writeBundleConfig(t, t.TempDir(), "new prompt\n", "current_lookup", "")
	old := compileSet(t, oldPath)
	oldJourney, _ := old.Journey("planner")
	registry := compileSet(t, newPath, oldPath)
	retained, _, ok := registry.Pinned(oldJourney.Digest)
	if !ok || !registry.HasDigest(oldJourney.Digest) || len(retained.Tools) != 1 || retained.Tools[0].Ops[0] != "legacy_lookup" || retained.Tools[0].Class != definitions.ClassReadOnly {
		t.Fatalf("retained binding = %#v, %v", retained.Tools, ok)
	}
	current, _ := registry.Journey("planner")
	if current.Tools[0].Ops[0] != "current_lookup" || current.Tools[0].Class != "" {
		t.Fatalf("current tools = %#v", current.Tools)
	}
	pinnedCurrent, _, ok := registry.Pinned(current.Digest)
	if !ok || pinnedCurrent.Tools[0].Ops[0] != "current_lookup" {
		t.Fatalf("current pinned = %#v, %v", pinnedCurrent.Tools, ok)
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
			for i := 0; i < test.files; i++ {
				if err := os.WriteFile(filepath.Join(root, "skills", "planner", fmt.Sprintf("file-%04d.txt", i)), []byte(test.body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			config := "catalog:\n  operation_meta_key: operation_key\njourneys:\n  - id: planner\n    skill: skills/planner\n    system_prompt: prompt.md\n    tools:\n      - op: lookup\n"
			path := filepath.Join(root, "journeys.yaml")
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := definitions.Compile(path); err == nil {
				t.Fatal("aggregate skill limit accepted")
			}
		})
	}
}
