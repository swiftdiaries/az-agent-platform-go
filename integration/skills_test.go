package integration_test

import (
	"context"
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
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	platformskills "github.com/swiftdiaries/az-agent-platform-go/internal/skills"
)

func TestSkillCatalogAndControlledReads(t *testing.T) {
	root := t.TempDir()
	registry, err := definitions.Load(writeDefinitionBundle(t, root, "prompt\n"))
	if err != nil {
		t.Fatal(err)
	}
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

	registry, err = definitions.Load(writeDefinitionBundle(t, t.TempDir(), "prompt\n"))
	if err != nil {
		t.Fatal(err)
	}
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
}

func TestSkillPolicyFailsClosed(t *testing.T) {
	root := t.TempDir()
	path := writeDefinitionBundle(t, root, "prompt\n")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(string(data[:]))
	data = []byte(strings.Replace(string(data), `"allow_supporting_files":true`, `"allow_supporting_files":false`, 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	registry, err := definitions.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	journey, _ := registry.Journey("planner")
	if _, err := platformskills.New(journey).Read("planner", "guide.md"); !errors.Is(err, platformskills.ErrUnavailable) {
		t.Fatalf("disabled resource policy = %v", err)
	}

	data = []byte(strings.Replace(string(data), `"allow_supporting_files":false`, `"allow_supporting_files":true,"disabled":true`, 1))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	registry, err = definitions.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	journey, _ = registry.Journey("planner")
	source := platformskills.New(journey)
	if len(source.Catalog()) != 0 {
		t.Fatal("disabled skill appeared in catalog")
	}
	if _, err := source.Read("planner", "SKILL.md"); !errors.Is(err, platformskills.ErrUnavailable) {
		t.Fatalf("disabled skill read = %v", err)
	}
}

func writeTwoJourneyBundle(t *testing.T, root, endpoint string) string {
	t.Helper()
	for _, dir := range []string{"prompts", "skills/planner", "skills/shift-swap"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"prompts/planner.md": "vacation system\n", "prompts/shift-swap.md": "shift system\n",
		"skills/planner/SKILL.md":    "---\nname: planner\ndescription: Plan travel\n---\nvacation skill body\n",
		"skills/shift-swap/SKILL.md": "---\nname: shift-swap\ndescription: Exchange a shift\n---\nshift skill body\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := fmt.Sprintf(`{"mcp_servers":[{"id":"s","endpoint":%q,"tools":["lookup_destination","lookup_shift"],"policies":{"lookup_destination":{"class":"read_only"},"lookup_shift":{"class":"read_only"}}}],"skill_packages":[{"name":"planner","root":"skills/planner"},{"name":"shift-swap","root":"skills/shift-swap"}],"journeys":[{"id":"vacation-planner","description":"travel","system_prompt":"prompts/planner.md","mcp":{"server":"s","tools":["lookup_destination"]},"skills":["planner"]},{"id":"shift-swap","description":"exchange work time","system_prompt":"prompts/shift-swap.md","mcp":{"server":"s","tools":["lookup_shift"]},"skills":["shift-swap"]}]}`, endpoint)
	path := filepath.Join(root, "journeys.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJourneyDefinitionRoutesAndMaterializesContext(t *testing.T) {
	sdk := mcp.NewServer(&mcp.Implementation{Name: "journeys", Version: "1"}, nil)
	mcp.AddTool(sdk, &mcp.Tool{Name: "lookup_destination"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return nil, struct{}{}, nil
	})
	mcp.AddTool(sdk, &mcp.Tool{Name: "lookup_shift"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return nil, struct{}{}, nil
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk }, nil))
	t.Cleanup(endpoint.Close)
	registry, err := definitions.Load(writeTwoJourneyBundle(t, t.TempDir(), endpoint.URL))
	if err != nil {
		t.Fatal(err)
	}
	var requests []agentruntime.ModelRequest
	runner := agentruntime.NewRunner(registry, platformmcp.NewClient(), modelFunc(func(_ context.Context, request agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		requests = append(requests, request)
		return agentruntime.ModelResponse{Text: request.Handoff.JourneyID + " answer"}, nil
	}))
	store := journal.New(database(t))
	run := func(id, text, target string) {
		t.Helper()
		command := journal.Command{ThreadID: "thread", RunID: id, CommunicationID: id, Principal: "alice", Text: text, TargetJourney: target}
		if _, _, err := store.Admit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
		owner := claimForTest(t, store, command, false)
		input := agentruntime.RunInput{Store: store, Owner: owner, ThreadID: "thread", RunID: id, Text: text, TargetJourney: target}
		journey, digest, err := runner.Binding(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		input.DefinitionDigest, input.TargetJourney = digest, journey
		input.History, err = store.Start(t.Context(), owner, journey, digest)
		if err != nil {
			t.Fatal(err)
		}
		output, err := runner.Run(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Finish(t.Context(), owner, journal.RunCompleted, output.History, output.Answer, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	run("vacation", "plan Kyoto", "")
	run("shift", "swap my Friday shift", "")
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d", len(requests))
	}
	wants := []struct{ journey, prompt, skill, body, tool string }{
		{"vacation-planner", "vacation system", "planner", "vacation skill body", "lookup_destination"},
		{"shift-swap", "shift system", "shift-swap", "shift skill body", "lookup_shift"},
	}
	for i, want := range wants {
		request := requests[i]
		if request.Handoff.JourneyID != want.journey || request.Instructions != want.prompt || !slices.Equal(toolNames(request.Tools), []string{want.tool, "request_user_input"}) {
			t.Fatalf("journey %d isolation: %#v", i, request)
		}
		if len(request.SkillCatalog) != 1 || request.SkillCatalog[0].Name != want.skill || len(request.SkillMaterial) != 1 || !strings.Contains(string(request.SkillMaterial[0].Body), want.body) {
			t.Fatalf("journey %d skills: %#v %#v", i, request.SkillCatalog, request.SkillMaterial)
		}
		if request.DefinitionDigest == "" || request.Provenance.Definition.ID != want.journey || request.Provenance.Definition.Digest != request.DefinitionDigest || request.Provenance.System.Digest == "" || request.Provenance.Handoff.Digest == "" || request.Provenance.Catalog.Digest == "" || request.Provenance.Context.Digest == "" || request.Provenance.History.Digest == "" || len(request.Provenance.Pending) != 1 || request.Provenance.Pending[0].ID == "" || len(request.Provenance.IncludedInput) != 1 || request.Provenance.IncludedInput[0] != request.Provenance.Pending[0] {
			t.Fatalf("journey %d provenance: %#v", i, request.Provenance)
		}
	}
	if requests[0].DefinitionDigest == requests[1].DefinitionDigest {
		t.Fatal("journeys share a definition digest")
	}
	if !slices.Equal(requests[1].Context, []string{"plan Kyoto", "vacation-planner answer"}) {
		t.Fatalf("selected prior context = %#v", requests[1].Context)
	}
	input := agentruntime.RunInput{Store: store, ThreadID: "new", Text: "swap", TargetJourney: "vacation-planner"}
	if journey, _, err := runner.Binding(t.Context(), input); err != nil || journey != "vacation-planner" {
		t.Fatalf("explicit route = %q, %v", journey, err)
	}
}

func TestVersionAwaitingSessionUsesRetainedProviderBundle(t *testing.T) {
	sdk := mcp.NewServer(&mcp.Implementation{Name: "versions", Version: "1"}, nil)
	mcp.AddTool(sdk, &mcp.Tool{Name: "lookup"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return nil, struct{}{}, nil
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk }, nil))
	t.Cleanup(endpoint.Close)
	makeVersion := func(prompt, body string) string {
		root := t.TempDir()
		path := writeDefinitionBundle(t, root, prompt)
		config, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		config = []byte(strings.Replace(string(config), "http://example", endpoint.URL, 1))
		if err := os.WriteFile(path, config, 0600); err != nil {
			t.Fatal(err)
		}
		skillPath := filepath.Join(root, "skills/planner/SKILL.md")
		skill, err := os.ReadFile(skillPath)
		if err != nil {
			t.Fatal(err)
		}
		skill = []byte(strings.Replace(string(skill), "Use the destination tool.", body, 1))
		if err := os.WriteFile(skillPath, skill, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	oldPath, newPath := makeVersion("old system\n", "old skill body"), makeVersion("new system\n", "new skill body")
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
	store := journal.New(database(t))
	if err := store.ActivateDefinition(t.Context(), "planner", "", oldJourney.Digest, registry.HasDigest); err != nil {
		t.Fatal(err)
	}
	var requests []agentruntime.ModelRequest
	model := modelFunc(func(_ context.Context, request agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		requests = append(requests, request)
		if request.Handoff.Text == "ask" && len(request.ToolResults) == 0 {
			return agentruntime.ModelResponse{ToolCall: questionCall("question")}, nil
		}
		return agentruntime.ModelResponse{Text: "done"}, nil
	})
	service := platform.New(agentruntime.NewRunner(registry, platformmcp.NewClient(), model), store)
	command := platform.Command{ThreadID: "retained", RunID: "wait", CommunicationID: "wait", Principal: "alice", Text: "ask", TargetJourney: "planner"}
	if _, err := service.Submit(t.Context(), platform.Submission{Command: command}); err != nil {
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
	if err := store.ActivateDefinition(t.Context(), "planner", oldJourney.Digest, newJourney.Digest, registry.HasDigest); err != nil {
		t.Fatal(err)
	}

	replacement := platform.New(agentruntime.NewRunner(registry, platformmcp.NewClient(), model), store)
	t.Cleanup(replacement.Close)
	reply := platform.Command{ThreadID: "retained", RunID: "reply", CommunicationID: "reply", Principal: "alice", InteractionID: interaction.ID, ReplyKind: "clarification", ReplyJSON: `{"answers":{"choice":{"option":"A"}}}`}
	if _, err := replacement.Submit(t.Context(), platform.Submission{Command: reply}); err != nil {
		t.Fatal(err)
	}
	awaitState(t, replacement, "retained", "reply", journal.RunCompleted)
	newCommand := platform.Command{ThreadID: "fresh", RunID: "fresh", CommunicationID: "fresh", Principal: "alice", Text: "plan", TargetJourney: "planner"}
	if _, err := replacement.Submit(t.Context(), platform.Submission{Command: newCommand}); err != nil {
		t.Fatal(err)
	}
	awaitState(t, replacement, "fresh", "fresh", journal.RunCompleted)
	if len(requests) != 3 {
		t.Fatalf("provider request count = %d", len(requests))
	}
	for _, request := range requests[:2] {
		if request.DefinitionDigest != oldJourney.Digest || request.Instructions != "old system" || len(request.SkillMaterial) != 1 || !strings.Contains(string(request.SkillMaterial[0].Body), "old skill body") {
			t.Fatalf("awaiting session fell forward: %#v", request)
		}
	}
	request := requests[2]
	if request.DefinitionDigest != newJourney.Digest || request.Instructions != "new system" || len(request.SkillMaterial) != 1 || !strings.Contains(string(request.SkillMaterial[0].Body), "new skill body") {
		t.Fatalf("new session missed current: %#v", request)
	}
}
