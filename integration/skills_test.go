package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/credential"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	platformskills "github.com/swiftdiaries/az-agent-platform-go/internal/skills"
)

// writeThreeJourneyBundle writes the two-journey bundle plus "plain", and gives two
// of the three skills a supporting file: vacation-planner has guide.md, shift-swap has
// notes.md and plain has only SKILL.md.
func writeThreeJourneyBundle(t *testing.T, root string) string {
	t.Helper()
	path := writeTwoJourneyBundle(t, root)
	if err := os.MkdirAll(filepath.Join(root, "skills/plain"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"skills/plain/SKILL.md":      "---\nname: plain\ndescription: Answer plainly\n---\nplain skill body\n",
		"skills/planner/guide.md":    "Prefer direct routes.\n",
		"skills/shift-swap/notes.md": "Swaps need a manager.\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config := string(data) + "  - id: plain\n    skill: skills/plain\n    tools:\n      - op: lookup_plain\n        class: read_only\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// results maps the tool results of a request by call ID to their JSON.
func results(t *testing.T, request agentruntime.ModelRequest) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, result := range request.ToolResults {
		out[result.CallID] = jsonOf(t, result.Value)
	}
	return out
}

func TestSkillCatalogAndControlledReads(t *testing.T) {
	// The skill package itself: metadata, immutable bodies, declared files only, and
	// a file changed after compilation is detected.
	t.Run("package", func(t *testing.T) {
		root := t.TempDir()
		registry := compileSet(t, writeDefinitionBundle(t, root, "prompt\n"))
		journey, _ := registry.Journey("planner")
		source := platformskills.New(journey)

		if err := os.WriteFile(filepath.Join(root, "skills/planner/guide.md"), []byte("changed\n"), 0600); err != nil {
			t.Fatal(err)
		}
		catalog := source.Catalog()
		if len(catalog) != 1 || catalog[0].Name != "planner" || catalog[0].Description != "Plan a trip" || catalog[0].Digest == "" {
			t.Fatalf("metadata catalog = %#v", catalog)
		}
		if _, err := source.Read("planner", "guide.md"); !errors.Is(err, platformskills.ErrChanged) {
			t.Fatalf("mutated resource read = %v", err)
		}
		if _, err := source.Read("planner", "other.md"); !errors.Is(err, platformskills.ErrUnavailable) {
			t.Fatalf("undeclared resource read = %v", err)
		}

		registry = compileSet(t, writeDefinitionBundle(t, t.TempDir(), "prompt\n"))
		journey, _ = registry.Journey("planner")
		source = platformskills.New(journey)
		instructions, err := source.Read("planner", "SKILL.md")
		if err != nil || instructions.Name != "planner" || instructions.Resource != "SKILL.md" || instructions.SkillDigest == "" || instructions.FileDigest == "" || len(instructions.Body) == 0 {
			t.Fatalf("instructions = %#v, %v", instructions, err)
		}
		instructions.Body[0] = 'X'
		again, err := source.Read("planner", "SKILL.md")
		if err != nil || again.Body[0] != '-' {
			t.Fatalf("read returned mutable body: %#v %v", again, err)
		}
		guide, err := source.Read("planner", "guide.md")
		if err != nil || string(guide.Body) != "Prefer direct routes.\n" {
			t.Fatalf("supporting read = %#v, %v", guide, err)
		}
	})

	root := t.TempDir()
	registry := compileSet(t, writeThreeJourneyBundle(t, root))
	vacation, _ := registry.Journey("vacation-planner")
	guide, err := platformskills.New(vacation).Read("planner", "guide.md")
	if err != nil {
		t.Fatal(err)
	}
	pool := database(t)
	store := journal.New(pool)
	model := &scriptedModel{}
	runner := agentruntime.NewRunner(storedDefinitions(t, pool, registry), model, testTenancy(t, keyedEndpoint(t, "lookup_destination", "lookup_shift", "lookup_plain")))
	builtins := []string{"request_user_input", "activate_journey"}
	read := func(id, journey, resource string) func(agentruntime.ModelRequest) agentruntime.ModelResponse {
		return callStep(id, "read_skill_resource", `{"journey":"`+journey+`","resource":"`+resource+`"}`)
	}

	t.Run("reads are scoped to active journeys", func(t *testing.T) {
		model.script(
			// Nothing is active: the tool is not offered, so a call to it is inactive.
			read("early", "vacation-planner", "guide.md"),
			activateStep("plain"),
			// plain has no supporting files: still not offered.
			read("plain-skill", "plain", "SKILL.md"),
			activateStep("vacation-planner"),
			func(agentruntime.ModelRequest) agentruntime.ModelResponse {
				return agentruntime.ModelResponse{ToolCalls: []agentruntime.ToolCall{
					{CallID: "guide", Name: "read_skill_resource", Arguments: json.RawMessage(`{"journey":"vacation-planner","resource":"guide.md"}`)},
					{CallID: "inactive", Name: "read_skill_resource", Arguments: json.RawMessage(`{"journey":"shift-swap","resource":"notes.md"}`)},
					{CallID: "instructions", Name: "read_skill_resource", Arguments: json.RawMessage(`{"journey":"vacation-planner","resource":"SKILL.md"}`)},
					{CallID: "undeclared", Name: "read_skill_resource", Arguments: json.RawMessage(`{"journey":"vacation-planner","resource":"other.md"}`)},
					{CallID: "no-files", Name: "read_skill_resource", Arguments: json.RawMessage(`{"journey":"plain","resource":"guide.md"}`)},
					{CallID: "traversal", Name: "read_skill_resource", Arguments: json.RawMessage(`{"journey":"vacation-planner","resource":"../shift-swap/notes.md"}`)},
					{CallID: "unknown", Name: "read_skill_resource", Arguments: json.RawMessage(`{"journey":"missing","resource":"guide.md"}`)},
				}}
			},
			textStep("done"),
		)
		if output, err := converse(t, store, runner, "reads", "reads-1", "plan", ""); err != nil || output.Answer != "done" {
			t.Fatalf("run = %#v, %v", output, err)
		}
		requests := model.taken()
		if len(requests) != 6 {
			t.Fatalf("model requests = %d", len(requests))
		}
		hasRead := func(request agentruntime.ModelRequest) bool {
			return slices.Contains(toolNames(request.Tools), "read_skill_resource")
		}
		// Offered only once an active journey has supporting files.
		for i, want := range []bool{false, false, false, false, true, true} {
			if hasRead(requests[i]) != want {
				t.Fatalf("request %d tools = %v, read_skill_resource offered want %v", i, toolNames(requests[i].Tools), want)
			}
		}
		if !slices.Equal(toolNames(requests[0].Tools), builtins) {
			t.Fatalf("request 0 tools = %v", toolNames(requests[0].Tools))
		}
		if got := results(t, requests[1])["early"]; got != `{"error":"tool_not_active"}` {
			t.Fatalf("read while nothing active = %s", got)
		}
		if got := results(t, requests[3])["plain-skill"]; got != `{"error":"tool_not_active"}` {
			t.Fatalf("read with no supporting files offered = %s", got)
		}
		final := results(t, requests[5])
		if got := final["guide"]; got != `"Prefer direct routes.\n"` {
			t.Fatalf("active journey read = %s", got)
		}
		for _, id := range []string{"inactive", "instructions", "undeclared", "no-files", "traversal", "unknown"} {
			if got := final[id]; got != `{"error":"resource_unavailable"}` {
				t.Fatalf("read %s = %s", id, got)
			}
		}
		// Only the successful read is recorded as skill provenance.
		if want := []agentruntime.MaterialProvenance{{ID: "vacation-planner/guide.md", Digest: guide.FileDigest}}; !slices.Equal(requests[5].Provenance.Skills, want) || len(requests[4].Provenance.Skills) != 0 {
			t.Fatalf("skill provenance = %#v (before the read %#v), want %#v", requests[5].Provenance.Skills, requests[4].Provenance.Skills, want)
		}
		var operations int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_operations").Scan(&operations); err != nil || operations != 0 {
			t.Fatalf("resource reads created %d operations, %v", operations, err)
		}
	})

	t.Run("a file changed on disk after compilation is ignored by the stored copy", func(t *testing.T) {
		// The run loads the set from the store, whose copy is immutable and never re-read
		// from disk; the disk-level ErrChanged check is covered by the "package" subtest.
		if err := os.WriteFile(filepath.Join(root, "skills/planner/guide.md"), []byte("changed\n"), 0600); err != nil {
			t.Fatal(err)
		}
		model.script(read("guide", "vacation-planner", "guide.md"), textStep("done"))
		output, err := converse(t, store, runner, "changed", "changed-1", "plan", "vacation-planner")
		if err != nil || output.Answer != "done" {
			t.Fatalf("run over a stored set whose disk file changed = %#v, %v", output, err)
		}
		requests := model.taken()
		if len(requests) != 2 {
			t.Fatalf("model requests = %d, want 2", len(requests))
		}
		if got := results(t, requests[1])["guide"]; got != `"Prefer direct routes.\n"` {
			t.Fatalf("read returned %s, want the stored content", got)
		}
		// The instructions come from the compiled definition, not the disk.
		if !strings.Contains(requests[0].Instructions, "vacation system") {
			t.Fatalf("instructions = %q", requests[0].Instructions)
		}
	})
}

// TestSkillPolicyFailsClosed covers what remains of the old resource policy:
// supporting files are every regular file in the bundle, and anything else
// (a path outside the bundle, a missing file, a bundle with only SKILL.md) is
// unavailable.
func TestSkillPolicyFailsClosed(t *testing.T) {
	root := t.TempDir()
	registry := compileSet(t, writeDefinitionBundle(t, root, "prompt\n"))
	journey, _ := registry.Journey("planner")
	source := platformskills.New(journey)
	if _, err := source.Read("planner", "guide.md"); err != nil {
		t.Fatalf("bundle file read = %v", err)
	}
	for _, resource := range []string{"../../journeys.yaml", "../planner/guide.md", "/etc/passwd", "missing.md", "reference/missing.md"} {
		if _, err := source.Read("planner", resource); !errors.Is(err, platformskills.ErrUnavailable) {
			t.Fatalf("read %q = %v", resource, err)
		}
	}
	if _, err := source.Read("other", "SKILL.md"); !errors.Is(err, platformskills.ErrUnavailable) {
		t.Fatalf("unknown skill read = %v", err)
	}

	bare := t.TempDir()
	path := writeDefinitionBundle(t, bare, "prompt\n")
	if err := os.Remove(filepath.Join(bare, "skills/planner/guide.md")); err != nil {
		t.Fatal(err)
	}
	journey, _ = compileSet(t, path).Journey("planner")
	if len(journey.Skills) != 1 || len(journey.Skills[0].Supporting) != 0 || journey.Skills[0].AllowSupportingFiles {
		t.Fatalf("SKILL.md-only bundle has resources: %#v", journey.Skills)
	}
	source = platformskills.New(journey)
	if len(source.Catalog()) != 1 {
		t.Fatalf("catalog = %#v", source.Catalog())
	}
	if _, err := source.Read("planner", "SKILL.md"); err != nil {
		t.Fatalf("instructions read = %v", err)
	}
	if _, err := source.Read("planner", "guide.md"); !errors.Is(err, platformskills.ErrUnavailable) {
		t.Fatalf("resource of SKILL.md-only bundle = %v", err)
	}
}

func TestSkillReadRejectsReplacedAncestors(t *testing.T) {
	t.Run("compiled root", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "bundle")
		registry := compileSet(t, writeDefinitionBundle(t, root, "prompt\n"))
		journey, _ := registry.Journey("planner")
		real := filepath.Join(parent, "bundle-real")
		if err := os.Rename(root, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, root); err != nil {
			t.Fatal(err)
		}
		if _, err := platformskills.New(journey).Read("planner", "SKILL.md"); !errors.Is(err, platformskills.ErrChanged) {
			t.Fatalf("replaced compiled root read = %v", err)
		}
	})
	t.Run("above compiled root", func(t *testing.T) {
		parent := t.TempDir()
		ancestor := filepath.Join(parent, "canonical")
		root := filepath.Join(ancestor, "bundle")
		registry := compileSet(t, writeDefinitionBundle(t, root, "prompt\n"))
		journey, _ := registry.Journey("planner")
		real := filepath.Join(parent, "canonical-real")
		if err := os.Rename(ancestor, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, ancestor); err != nil {
			t.Fatal(err)
		}
		if _, err := platformskills.New(journey).Read("planner", "SKILL.md"); !errors.Is(err, platformskills.ErrChanged) {
			t.Fatalf("replaced root ancestor read = %v", err)
		}
	})
	t.Run("package", func(t *testing.T) {
		root := t.TempDir()
		registry := compileSet(t, writeDefinitionBundle(t, root, "prompt\n"))
		journey, _ := registry.Journey("planner")
		packagePath := filepath.Join(root, "skills/planner")
		realPath := filepath.Join(root, "skills/planner-real")
		if err := os.Rename(packagePath, realPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realPath, packagePath); err != nil {
			t.Fatal(err)
		}
		if _, err := platformskills.New(journey).Read("planner", "SKILL.md"); !errors.Is(err, platformskills.ErrChanged) {
			t.Fatalf("replaced package read = %v", err)
		}
	})
	t.Run("intermediate", func(t *testing.T) {
		root := t.TempDir()
		path := writeDefinitionBundle(t, root, "prompt\n")
		if err := os.Mkdir(filepath.Join(root, "skills/planner/reference"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(root, "skills/planner/guide.md"), filepath.Join(root, "skills/planner/reference/guide.md")); err != nil {
			t.Fatal(err)
		}
		registry := compileSet(t, path)
		journey, _ := registry.Journey("planner")
		intermediate := filepath.Join(root, "skills/planner/reference")
		real := filepath.Join(root, "skills/planner/reference-real")
		if err := os.Rename(intermediate, real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, intermediate); err != nil {
			t.Fatal(err)
		}
		if _, err := platformskills.New(journey).Read("planner", "reference/guide.md"); !errors.Is(err, platformskills.ErrChanged) {
			t.Fatalf("replaced intermediate read = %v", err)
		}
	})
}

// writeTwoJourneyBundle writes two journeys (vacation-planner, shift-swap) whose
// prompts are their SKILL.md bodies, and a base conversation prompt.
func writeTwoJourneyBundle(t *testing.T, root string) string {
	t.Helper()
	for _, dir := range []string{"skills/planner", "skills/shift-swap"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"conversation.md":            "base prompt\n",
		"skills/planner/SKILL.md":    "---\nname: planner\ndescription: Plan travel\n---\nvacation system\n",
		"skills/shift-swap/SKILL.md": "---\nname: shift-swap\ndescription: Exchange a shift\n---\nshift system\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := `conversation_prompt: conversation.md
catalog:
  operation_meta_key: operation_key
  call_id_header: X-Platform-Call-ID
journeys:
  - id: vacation-planner
    skill: skills/planner
    tools:
      - op: lookup_destination
        class: read_only
  - id: shift-swap
    skill: skills/shift-swap
    tools:
      - op: lookup_shift
        class: read_only
`
	path := filepath.Join(root, "journeys.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJourneyDefinitionTargetsAndMaterializesContext(t *testing.T) {
	registry := compileSet(t, writeTwoJourneyBundle(t, t.TempDir()))
	vacation, _ := registry.Journey("vacation-planner")
	shift, _ := registry.Journey("shift-swap")
	var requests []agentruntime.ModelRequest
	pool := database(t)
	runner := agentruntime.NewRunner(storedDefinitions(t, pool, registry), modelFunc(func(_ context.Context, request agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		requests = append(requests, request)
		return agentruntime.ModelResponse{Text: strings.Join(request.ActiveJourneys, "+") + " answer"}, nil
	}), testTenancy(t, keyedEndpoint(t, "lookup_destination", "lookup_shift")))
	store := journal.New(pool)
	run := func(id, text, target string) {
		t.Helper()
		command := journal.Command{ThreadID: "thread", RunID: id, CommunicationID: id, Principal: "alice", Tenant: testTenant, Text: text, TargetJourney: target}
		if _, _, err := store.Admit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
		owner := claimForTest(t, store, command, false)
		input := scoped(t, runner, agentruntime.RunInput{Store: store, Owner: owner, ThreadID: "thread", RunID: id, Text: text, TargetJourney: target})
		journey, digest, err := runner.Binding(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		input.DefinitionDigest, input.TargetJourney = digest, journey
		input.History, err = store.Start(t.Context(), owner, journey, digest)
		if err != nil {
			t.Fatal(err)
		}
		if id == "vacation" {
			steering := journal.Command{ThreadID: "thread", RunID: "vacation-correction", CommunicationID: "vacation-correction", Principal: "alice", Tenant: testTenant, Text: "Kyoto instead", TargetJourney: target}
			if receipt, fresh, err := store.Admit(t.Context(), steering); err != nil || fresh || receipt.ExecutionRunID != id {
				t.Fatalf("steering admission = %#v, %v, %v", receipt, fresh, err)
			}
		}
		output, err := runner.Run(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(t.Context(), owner, journal.RunCompleted, output.History, output.Answer, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	run("vacation", "plan Kyoto", "vacation-planner")
	run("shift", "swap my Friday shift", "shift-swap")
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d", len(requests))
	}
	wants := []struct {
		active     []string
		tools      []string
		bodies     []string
		absent     string
		provenance []agentruntime.MaterialProvenance
	}{
		{[]string{"vacation-planner"}, []string{"lookup_destination", "request_user_input", "activate_journey"}, []string{"vacation system"}, "shift system", []agentruntime.MaterialProvenance{{ID: "vacation-planner", Digest: vacation.Digest}}},
		// The same conversation keeps the first journey active (the limit is 2).
		{[]string{"vacation-planner", "shift-swap"}, []string{"lookup_destination", "lookup_shift", "request_user_input", "activate_journey"}, []string{"vacation system", "shift system"}, "", []agentruntime.MaterialProvenance{{ID: "vacation-planner", Digest: vacation.Digest}, {ID: "shift-swap", Digest: shift.Digest}}},
	}
	for i, want := range wants {
		request := requests[i]
		if !slices.Equal(request.ActiveJourneys, want.active) || !slices.Equal(toolNames(request.Tools), want.tools) || !strings.HasPrefix(request.Instructions, "base prompt") {
			t.Fatalf("journey %d isolation: active %v tools %v instructions %q", i, request.ActiveJourneys, toolNames(request.Tools), request.Instructions)
		}
		for _, body := range want.bodies {
			if !strings.Contains(request.Instructions, body) {
				t.Fatalf("journey %d instructions lack %q: %q", i, body, request.Instructions)
			}
		}
		if want.absent != "" && strings.Contains(request.Instructions, want.absent) {
			t.Fatalf("journey %d instructions contain inactive %q: %q", i, want.absent, request.Instructions)
		}
		wantPending := 1
		if i == 0 {
			wantPending = 2
		}
		if request.DefinitionDigest != registry.Digest() || request.Provenance.Definition != (agentruntime.MaterialProvenance{ID: "set", Digest: registry.Digest()}) ||
			request.Provenance.System.Digest == "" || request.Provenance.Catalog.Digest == "" || request.Provenance.History.Digest == "" ||
			!slices.Equal(request.Provenance.Active, want.provenance) ||
			len(request.Provenance.Pending) != wantPending || request.Provenance.Pending[0].ID == "" || len(request.Provenance.IncludedInput) != wantPending || request.Provenance.IncludedInput[0] != request.Provenance.Pending[0] {
			t.Fatalf("journey %d provenance: %#v", i, request.Provenance)
		}
	}
	if vacation.Digest == shift.Digest || requests[0].Provenance.System.Digest == requests[1].Provenance.System.Digest {
		t.Fatal("journeys share a digest")
	}
	// One history per thread: the shift run receives the vacation run's turns
	// (user text, steering, answer) in order, then its own user text.
	next := 0
	for _, want := range []string{"plan Kyoto", "Kyoto instead", "vacation-planner answer", "swap my Friday shift"} {
		found := false
		for ; next < len(requests[1].Messages); next++ {
			if strings.Contains(requests[1].Messages[next], want) {
				found, next = true, next+1
				break
			}
		}
		if !found {
			t.Fatalf("shift run history lacks %q in order: %#v", want, requests[1].Messages)
		}
	}
	command := journal.Command{ThreadID: "new", RunID: "new", CommunicationID: "new", Principal: "alice", Tenant: testTenant, Text: "swap", TargetJourney: "vacation-planner"}
	if _, _, err := store.Admit(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	input := agentruntime.RunInput{Store: store, Owner: claimForTest(t, store, command, false), ThreadID: "new", Text: "swap", TargetJourney: "vacation-planner"}
	if journey, _, err := runner.Binding(t.Context(), input); err != nil || journey != "vacation-planner" {
		t.Fatalf("explicit target = %q, %v", journey, err)
	}
	// Without a target there is no journey to bind: the conversation's active set decides.
	input.TargetJourney = ""
	if journey, _, err := runner.Binding(t.Context(), input); err != nil || journey != "" {
		t.Fatalf("untargeted binding = %q, %v", journey, err)
	}
}

func TestVersionAwaitingSessionUsesStoredProviderBundle(t *testing.T) {
	sdk := mcp.NewServer(&mcp.Implementation{Name: "versions", Version: "1"}, nil)
	mcp.AddTool(sdk, keyedTool("lookup", "lookup"), func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return nil, struct{}{}, nil
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk }, nil))
	t.Cleanup(endpoint.Close)
	makeVersion := func(body string) string {
		return writeDefinitionBundle(t, t.TempDir(), body+"\n")
	}
	oldPath, newPath := makeVersion("old skill body"), makeVersion("new skill body")
	oldRegistry := compileSet(t, oldPath)
	registry := compileSet(t, newPath)
	oldJourney, _ := oldRegistry.Journey("planner")
	newJourney, _ := registry.Journey("planner")
	pool := database(t)
	store := journal.New(pool)
	defs := storedDefinitions(t, pool, oldRegistry, registry) // the old set becomes current
	var requests []agentruntime.ModelRequest
	model := modelFunc(func(_ context.Context, request agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		requests = append(requests, request)
		if slices.Contains(request.Messages, "ask") && len(request.ToolResults) == 0 {
			return agentruntime.ModelResponse{ToolCall: questionCall("question")}, nil
		}
		return agentruntime.ModelResponse{Text: "done"}, nil
	})
	service := platform.New(agentruntime.NewRunner(defs, model, testTenancy(t, endpoint.URL)), store)
	command := platform.Command{ThreadID: "retained", RunID: "wait", CommunicationID: "wait", Principal: "alice", Tenant: testTenant, Text: "ask", TargetJourney: "planner"}
	if _, err := service.Submit(t.Context(), command, credential.Credential{}); err != nil {
		t.Fatal(err)
	}
	waiting := awaitState(t, service, "retained", "wait", journal.RunAwaitingInput)
	var interaction *journal.Interaction
	for _, run := range waiting.Runs {
		if run.RunID == "wait" {
			interaction = run.Interaction
		}
	}
	if interaction == nil {
		t.Fatal("missing clarification")
	}
	service.Close()
	if err := store.ActivateDefinition(t.Context(), oldRegistry.Digest(), registry.Digest(), servedOnly(oldRegistry.Digest(), registry.Digest())); err != nil {
		t.Fatal(err)
	}

	replacement := platform.New(agentruntime.NewRunner(defs, model, testTenancy(t, endpoint.URL)), store)
	t.Cleanup(replacement.Close)
	reply := platform.Command{ThreadID: "retained", RunID: "reply", CommunicationID: "reply", Principal: "alice", Tenant: testTenant, InteractionID: interaction.ID, ReplyKind: "clarification", ReplyJSON: `{"answers":{"choice":{"option":"A"}}}`}
	if _, err := replacement.Submit(t.Context(), reply, credential.Credential{}); err != nil {
		t.Fatal(err)
	}
	awaitState(t, replacement, "retained", "reply", journal.RunCompleted)
	newCommand := platform.Command{ThreadID: "fresh", RunID: "fresh", CommunicationID: "fresh", Principal: "alice", Tenant: testTenant, Text: "plan", TargetJourney: "planner"}
	if _, err := replacement.Submit(t.Context(), newCommand, credential.Credential{}); err != nil {
		t.Fatal(err)
	}
	awaitState(t, replacement, "fresh", "fresh", journal.RunCompleted)
	if len(requests) != 3 {
		t.Fatalf("provider request count = %d", len(requests))
	}
	clarification := requests[1].Provenance
	clarificationPayload, _ := json.Marshal(journal.Reply{Answers: map[string]journal.Answer{"choice": {Option: "A"}}})
	clarificationDigest := fmt.Sprintf("%x", sha256.Sum256(clarificationPayload))
	if len(clarification.Pending) != 1 || len(clarification.IncludedInput) != 1 || clarification.Pending[0] != clarification.IncludedInput[0] || clarification.Pending[0].ID != "reply" || clarification.Pending[0].Digest != clarificationDigest || !json.Valid(requests[1].PendingCommands[0].Payload) {
		t.Fatalf("clarification reply provenance = %#v", clarification)
	}
	for _, request := range requests[:2] {
		if request.DefinitionDigest != oldRegistry.Digest() || !strings.Contains(request.Instructions, "old skill body") || strings.Contains(request.Instructions, "new skill body") ||
			!slices.Equal(request.ActiveJourneys, []string{"planner"}) || !slices.Equal(request.Provenance.Active, []agentruntime.MaterialProvenance{{ID: "planner", Digest: oldJourney.Digest}}) {
			t.Fatalf("awaiting session fell forward: %#v", request)
		}
	}
	request := requests[2]
	if request.DefinitionDigest != registry.Digest() || !strings.Contains(request.Instructions, "new skill body") || strings.Contains(request.Instructions, "old skill body") ||
		!slices.Equal(request.ActiveJourneys, []string{"planner"}) || !slices.Equal(request.Provenance.Active, []agentruntime.MaterialProvenance{{ID: "planner", Digest: newJourney.Digest}}) {
		t.Fatalf("new session missed current: %#v", request)
	}
}

func TestSkillRuntimeLoadsOnlySelectedMaterial(t *testing.T) {
	registry := compileSet(t, writeThreeJourneyBundle(t, t.TempDir()))
	vacation, _ := registry.Journey("vacation-planner")
	shift, _ := registry.Journey("shift-swap")
	plain, _ := registry.Journey("plain")
	pool := database(t)
	store := journal.New(pool)
	model := &scriptedModel{}
	runner := agentruntime.NewRunner(storedDefinitions(t, pool, registry), model, testTenancy(t, keyedEndpoint(t, "lookup_destination", "lookup_shift", "lookup_plain")))
	// The model activates three journeys in turn; the limit is 2, so the third
	// activation evicts the least recently used one.
	model.script(activateStep("vacation-planner"), activateStep("shift-swap"), activateStep("plain"), textStep("done"))
	output, err := converse(t, store, runner, "select", "select-1", "plan", "")
	if err != nil || output.Answer != "done" {
		t.Fatalf("skill selection run = %#v err=%v", output, err)
	}
	requests := model.taken()
	if len(requests) != 4 {
		t.Fatalf("model requests = %d", len(requests))
	}
	lines := map[string]string{
		"vacation-planner": "- vacation-planner: Plan travel",
		"shift-swap":       "- shift-swap: Exchange a shift",
		"plain":            "- plain: Answer plainly",
	}
	bodies := map[string]string{"vacation-planner": "vacation system", "shift-swap": "shift system", "plain": "plain skill body"}
	digests := map[string]string{"vacation-planner": vacation.Digest, "shift-swap": shift.Digest, "plain": plain.Digest}
	for i, active := range [][]string{nil, {"vacation-planner"}, {"vacation-planner", "shift-swap"}, {"shift-swap", "plain"}} {
		request := requests[i]
		if !slices.Equal(request.ActiveJourneys, active) {
			t.Fatalf("request %d active = %v, want %v", i, request.ActiveJourneys, active)
		}
		var wantProvenance []agentruntime.MaterialProvenance
		for id := range lines {
			// Every available journey contributes its metadata line, active or not.
			if !strings.Contains(request.Instructions, lines[id]) {
				t.Fatalf("request %d instructions lack %q: %q", i, lines[id], request.Instructions)
			}
			isActive := slices.Contains(active, id)
			if strings.Contains(request.Instructions, bodies[id]) != isActive || strings.Contains(request.Instructions, "## Active journey: "+id) != isActive {
				t.Fatalf("request %d: journey %s active=%v but instructions = %q", i, id, isActive, request.Instructions)
			}
		}
		for _, id := range active {
			wantProvenance = append(wantProvenance, agentruntime.MaterialProvenance{ID: id, Digest: digests[id]})
		}
		if !slices.Equal(request.Provenance.Active, wantProvenance) || len(request.Provenance.Skills) != 0 {
			t.Fatalf("request %d provenance: active %#v skills %#v", i, request.Provenance.Active, request.Provenance.Skills)
		}
	}
	if got := results(t, requests[3])["activate-plain"]; got != `{"activated":"plain","evicted":["vacation-planner"]}` {
		t.Fatalf("eviction result = %s", got)
	}
	// Eviction is durable and applies to the next run of the conversation.
	if got := activeSet(t, pool, "select"); !slices.Equal(got, []string{"shift-swap", "plain"}) {
		t.Fatalf("persisted set = %v", got)
	}
}
