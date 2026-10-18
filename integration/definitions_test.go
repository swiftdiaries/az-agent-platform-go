package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// writeBundleConfig writes the planner bundle (a skill whose SKILL.md body is
// prompt, a guide.md supporting file and a journeys.yaml) with one tool selecting
// operation key op. class "" or "effectful" leaves the tool unclassified
// (effectful).
func writeBundleConfig(t *testing.T, root, prompt, op, class string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "skills/planner"), 0700); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: planner\ndescription: Plan a trip\n---\n" + prompt
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
		"journeys:\n  - id: planner\n    skill: skills/planner\n    tools:\n" + tool
	path := filepath.Join(root, "journeys.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// keyedEndpoint serves one inline MCP tool per operation key (remote name equal to
// the key) and returns the endpoint URL.
func keyedEndpoint(t *testing.T, ops ...string) string {
	t.Helper()
	sdk := mcp.NewServer(&mcp.Implementation{Name: "keyed", Version: "1"}, nil)
	for _, op := range ops {
		mcp.AddTool(sdk, keyedTool(op, op), func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	}
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk }, nil))
	t.Cleanup(endpoint.Close)
	return endpoint.URL
}

// scriptedModel answers each request with the next scripted step and records the
// requests. It fails the request once the script is exhausted.
type scriptedModel struct {
	mu       sync.Mutex
	requests []agentruntime.ModelRequest
	steps    []func(agentruntime.ModelRequest) agentruntime.ModelResponse
}

// script replaces the steps and forgets the recorded requests.
func (m *scriptedModel) script(steps ...func(agentruntime.ModelRequest) agentruntime.ModelResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps, m.requests = steps, nil
}

func (m *scriptedModel) taken() []agentruntime.ModelRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

func (m *scriptedModel) Complete(_ context.Context, request agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	index := len(m.requests)
	m.requests = append(m.requests, request)
	if index >= len(m.steps) {
		return agentruntime.ModelResponse{}, errors.New("model script exhausted")
	}
	return m.steps[index](request), nil
}

func textStep(text string) func(agentruntime.ModelRequest) agentruntime.ModelResponse {
	return func(agentruntime.ModelRequest) agentruntime.ModelResponse {
		return agentruntime.ModelResponse{Text: text}
	}
}

// callStep makes the model call tool name with arguments.
func callStep(id, name, arguments string) func(agentruntime.ModelRequest) agentruntime.ModelResponse {
	return func(agentruntime.ModelRequest) agentruntime.ModelResponse {
		return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{CallID: id, Name: name, Arguments: json.RawMessage(arguments)}}
	}
}

func activateStep(journey string) func(agentruntime.ModelRequest) agentruntime.ModelResponse {
	return callStep("activate-"+journey, "activate_journey", `{"journey":"`+journey+`"}`)
}

// converse admits, claims, binds and runs one command of principal alice and, when
// the run succeeds, finishes it. A failure is returned as is (the run is not finished).
func converse(t *testing.T, store *journal.Store, runner *agentruntime.Runner, thread, run, text, target string) (agentruntime.RunOutput, error) {
	t.Helper()
	command := journal.Command{ThreadID: thread, RunID: run, CommunicationID: run, Principal: "alice", Tenant: testTenant, Text: text, TargetJourney: target}
	if _, _, err := store.Admit(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	return runOwned(t, store, runner, claimForTest(t, store, command, false), command, target)
}

// runOwned binds, starts, runs and finishes command for an owner that already holds it.
func runOwned(t *testing.T, store *journal.Store, runner *agentruntime.Runner, owner journal.Owner, command journal.Command, target string) (agentruntime.RunOutput, error) {
	t.Helper()
	input := scoped(t, runner, agentruntime.RunInput{Store: store, Owner: owner, ThreadID: command.ThreadID, RunID: command.RunID, Principal: command.Principal, Text: command.Text, TargetJourney: target})
	journey, digest, err := runner.Binding(t.Context(), input)
	if err != nil {
		return agentruntime.RunOutput{}, err
	}
	input.DefinitionDigest, input.TargetJourney = digest, journey
	if input.History, err = store.Start(t.Context(), owner, journey, digest); err != nil {
		return agentruntime.RunOutput{}, err
	}
	output, err := runner.Run(t.Context(), input)
	if err != nil {
		return output, err
	}
	if err := store.Finish(t.Context(), owner, journal.RunCompleted, output.History, output.Answer, "", ""); err != nil {
		t.Fatal(err)
	}
	return output, nil
}

// jsonOf renders a tool result value as JSON.
func jsonOf(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// activeSet reads a conversation's persisted active journeys (least recently used first).
func activeSet(t *testing.T, pool *pgxpool.Pool, thread string) []string {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(t.Context(), "SELECT active_journeys FROM agent_conversations WHERE id=$1", thread).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		t.Fatal(err)
	}
	return ids
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

func TestVersionActivationRepinsIdleSessions(t *testing.T) {
	ctx := context.Background()
	oldRoot, newRoot := t.TempDir(), t.TempDir()
	oldPath := writeDefinitionBundle(t, oldRoot, "old prompt\n")
	newPath := writeDefinitionBundle(t, newRoot, "new prompt\n")
	oldRegistry := compileSet(t, oldPath)
	registry := compileSet(t, newPath, oldPath)
	oldDigest, newDigest := oldRegistry.Digest(), registry.Digest()
	if oldDigest == newDigest {
		t.Fatal("old and new set digests must differ")
	}
	pool := database(t)
	store := journal.New(pool)
	if err := store.ActivateDefinition(ctx, "", oldDigest, oldRegistry.Serves); err != nil {
		t.Fatal(err)
	}
	changes := func(thread string) (count int, oldSeen, newSeen string) {
		t.Helper()
		if err := pool.QueryRow(ctx, "SELECT count(*), COALESCE(min(detail->>'old_digest'),''), COALESCE(min(detail->>'new_digest'),'') FROM agent_events WHERE thread_id=$1 AND kind='definition_changed'", thread).Scan(&count, &oldSeen, &newSeen); err != nil {
			t.Fatal(err)
		}
		return count, oldSeen, newSeen
	}

	// The first run resolves before the activation and the activation lands while it
	// runs: the run stays on the set it resolved.
	model := &scriptedModel{}
	runner := agentruntime.NewRunner(registry, model, testTenancy(t, keyedEndpoint(t, "lookup")))
	model.script(func(r agentruntime.ModelRequest) agentruntime.ModelResponse {
		if err := store.ActivateDefinition(ctx, oldDigest, newDigest, registry.Serves); err != nil {
			t.Errorf("activation during the run: %v", err)
		}
		return textStep("done")(r)
	})
	command := journal.Command{ThreadID: "old-thread", RunID: "old-run", CommunicationID: "old-command", Principal: "alice", Tenant: testTenant, Text: "plan", TargetJourney: "planner"}
	if _, _, err := store.Admit(ctx, command); err != nil {
		t.Fatal(err)
	}
	owner := claimForTest(t, store, command, true)
	if output, err := runOwned(t, store, runner, owner, command, "planner"); err != nil || output.Answer != "done" {
		t.Fatalf("run through activation: %#v, %v", output, err)
	}
	requireSet := func(label, wantDigest, wantBody string) {
		t.Helper()
		requests := model.taken()
		if len(requests) != 1 || requests[0].DefinitionDigest != wantDigest || requests[0].Provenance.Definition.Digest != wantDigest ||
			!strings.Contains(requests[0].Instructions, wantBody) || !slices.Equal(requests[0].ActiveJourneys, []string{"planner"}) {
			t.Fatalf("%s request = %#v", label, requests)
		}
	}
	requireSet("old session", oldDigest, "old prompt")
	if count, _, _ := changes("old-thread"); count != 0 {
		t.Fatalf("definition_changed events within the first run = %d", count)
	}

	// The next run of the same idle conversation re-pins to the current set.
	continued := journal.Command{ThreadID: "old-thread", RunID: "old-next", CommunicationID: "old-next", Principal: "alice", Tenant: testTenant, Text: "again", TargetJourney: "planner"}
	if _, _, err := store.Admit(ctx, continued); err != nil {
		t.Fatal(err)
	}
	continuedOwner := claimForTest(t, store, continued, false)
	model.script(textStep("again"))
	if _, err := runOwned(t, store, runner, continuedOwner, continued, "planner"); err != nil {
		t.Fatal(err)
	}
	requireSet("re-pinned session", newDigest, "new prompt")
	if count, from, to := changes("old-thread"); count != 1 || from != oldDigest || to != newDigest {
		t.Fatalf("definition_changed = %d %q -> %q, want 1 %q -> %q", count, from, to, oldDigest, newDigest)
	}
	var pin string
	if err := pool.QueryRow(ctx, "SELECT definition_digest FROM agent_conversations WHERE id='old-thread'").Scan(&pin); err != nil || pin != newDigest {
		t.Fatalf("conversation pin = %q, %v", pin, err)
	}

	newCommand := journal.Command{ThreadID: "new-thread", RunID: "new-run", CommunicationID: "new-command", Principal: "alice", Tenant: testTenant, Text: "plan", TargetJourney: "planner"}
	if _, _, err := store.Admit(ctx, newCommand); err != nil {
		t.Fatal(err)
	}
	newOwner := claimForTest(t, store, newCommand, false)
	model.script(textStep("fresh"))
	if _, err := runOwned(t, store, runner, newOwner, newCommand, "planner"); err != nil {
		t.Fatal(err)
	}
	requireSet("new session", newDigest, "new prompt")
	if count, _, _ := changes("new-thread"); count != 0 {
		t.Fatalf("a new conversation emitted %d definition_changed events", count)
	}
	// Each conversation keeps its own active set.
	if got := activeSet(t, pool, "old-thread"); !slices.Equal(got, []string{"planner"}) {
		t.Fatalf("old session active set = %v", got)
	}
	if err := store.ActivateDefinition(ctx, oldDigest, oldDigest, registry.Serves); !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("stale activation = %v", err)
	}
	if current, err := store.CurrentDefinition(ctx); err != nil || current != newDigest {
		t.Fatalf("failed activation changed current = %q, %v", current, err)
	}

	// No conversation is non-idle, so the old bundle is no longer needed.
	candidateOnly := compileSet(t, newPath)
	if ready, err := store.DefinitionsReady(ctx, candidateOnly.Serves); err != nil || !ready {
		t.Fatalf("readiness with only the new set and no non-idle conversation = %v, %v", ready, err)
	}
	if ready, err := store.DefinitionsReady(ctx, registry.Serves); err != nil || !ready {
		t.Fatalf("complete retained bundle readiness = %v, %v", ready, err)
	}
	if ready, err := store.DefinitionsReady(ctx, func(string) bool { return false }); err != nil || ready {
		t.Fatalf("readiness serving nothing = %v, %v", ready, err)
	}
}

// A conversation with a pending interaction is non-idle: it stays pinned to the old
// set, and the old set must stay served until it settles.
func TestVersionActivationNonIdleSessionNeedsOldSet(t *testing.T) {
	ctx := context.Background()
	oldPath := writeDefinitionBundle(t, t.TempDir(), "old prompt\n")
	newPath := writeDefinitionBundle(t, t.TempDir(), "new prompt\n")
	oldRegistry := compileSet(t, oldPath)
	registry := compileSet(t, newPath, oldPath)
	oldDigest, newDigest := oldRegistry.Digest(), registry.Digest()
	pool := database(t)
	store := journal.New(pool)
	if err := store.ActivateDefinition(ctx, "", oldDigest, oldRegistry.Serves); err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{}
	runner := agentruntime.NewRunner(registry, model, testTenancy(t, keyedEndpoint(t, "lookup")))
	model.script(callStep("ask", "request_user_input", `{"questions":[{"id":"q","header":"Where","question":"Which city?","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}]}]}`))
	command := journal.Command{ThreadID: "waiting-thread", RunID: "waiting-run", CommunicationID: "waiting-command", Principal: "alice", Tenant: testTenant, Text: "plan", TargetJourney: "planner"}
	if _, _, err := store.Admit(ctx, command); err != nil {
		t.Fatal(err)
	}
	owner := claimForTest(t, store, command, true)
	input := scoped(t, runner, agentruntime.RunInput{Store: store, Owner: owner, ThreadID: command.ThreadID, RunID: command.RunID, Principal: command.Principal, Text: command.Text, TargetJourney: "planner"})
	journey, digest, err := runner.Binding(ctx, input)
	if err != nil || digest != oldDigest {
		t.Fatalf("binding = %q %q, %v", journey, digest, err)
	}
	input.DefinitionDigest, input.TargetJourney = digest, journey
	if input.History, err = store.Start(ctx, owner, journey, digest); err != nil {
		t.Fatal(err)
	}
	if output, err := runner.Run(ctx, input); err != nil || !output.Waiting {
		t.Fatalf("run = %#v, %v", output, err)
	}
	if err := store.ActivateDefinition(ctx, oldDigest, newDigest, registry.Serves); err != nil {
		t.Fatal(err)
	}
	candidateOnly := compileSet(t, newPath)
	if ready, err := store.DefinitionsReady(ctx, candidateOnly.Serves); err != nil || ready {
		t.Fatalf("readiness with only the new set while a conversation is non-idle on the old one = %v, %v", ready, err)
	}
	if ready, err := store.DefinitionsReady(ctx, registry.Serves); err != nil || !ready {
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
	// This replica retains the old bundle but not the two-journey bundle.
	registry := compileSet(t, currentPath, oldPath)
	oldDigest, currentDigest, unservedDigest := oldRegistry.Digest(), registry.Digest(), secondJourneyRegistry.Digest()
	if registry.Serves(unservedDigest) {
		t.Fatal("replica unexpectedly serves the two-journey set")
	}
	pool := database(t)
	store := journal.New(pool)
	if err := store.ActivateDefinition(ctx, "", oldDigest, registry.Serves); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateDefinition(ctx, oldDigest, unservedDigest, registry.Serves); !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("unserved activation = %v", err)
	}
	var current string
	if err := pool.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current").Scan(&current); err != nil || current != oldDigest {
		t.Fatalf("unserved activation changed current = %q, %v", current, err)
	}
	var pins int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_conversations WHERE definition_digest IS NOT NULL").Scan(&pins); err != nil || pins != 0 {
		t.Fatalf("unserved activation created pins = %d, %v", pins, err)
	}
	if err := store.ActivateDefinition(ctx, oldDigest, currentDigest, registry.Serves); err != nil {
		t.Fatalf("retained activation = %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current").Scan(&current); err != nil || current != currentDigest {
		t.Fatalf("retained activation current = %q, %v", current, err)
	}

	if _, err := pool.Exec(ctx, "UPDATE agent_definition_current SET definition_digest=$1", unservedDigest); err != nil {
		t.Fatal(err)
	}
	if ready, err := store.DefinitionsReady(ctx, registry.Serves); err != nil || ready {
		t.Fatalf("unserved current readiness = %v, %v", ready, err)
	}
	command := journal.Command{ThreadID: "mismatched-current", RunID: "mismatched-current", CommunicationID: "mismatched-current", Principal: "alice", Tenant: testTenant, Text: "plan"}
	if _, _, err := store.Admit(ctx, command); err != nil {
		t.Fatal(err)
	}
	owner := claimForTest(t, store, command, false)
	if _, err := store.ResolveDefinition(ctx, owner, currentDigest, registry.Serves); !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("unserved current resolution = %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_conversations WHERE id=$1 AND definition_digest IS NOT NULL", command.ThreadID).Scan(&pins); err != nil || pins != 0 {
		t.Fatalf("unserved current created pin = %d, %v", pins, err)
	}

	// Activating a journey changes only the activating conversation's active set: not
	// another conversation's set, not a definition pin, not the current pointer.
	two := compileSet(t, secondJourneyPath)
	twoPool := database(t)
	twoStore := journal.New(twoPool)
	if err := twoStore.ActivateDefinition(ctx, "", two.Digest(), two.Serves); err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{}
	runner := agentruntime.NewRunner(two, model, testTenancy(t, keyedEndpoint(t, "lookup_destination", "lookup_shift")))
	pinOf := func(thread string) string {
		t.Helper()
		var pin *string
		if err := twoPool.QueryRow(ctx, "SELECT definition_digest FROM agent_conversations WHERE id=$1", thread).Scan(&pin); err != nil || pin == nil {
			t.Fatalf("pin of %s = %v, %v", thread, pin, err)
		}
		return *pin
	}
	currentOf := func() string {
		t.Helper()
		var current string
		if err := twoPool.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current").Scan(&current); err != nil {
			t.Fatal(err)
		}
		return current
	}
	model.script(textStep("hello"))
	if _, err := converse(t, twoStore, runner, "bystander", "bystander-1", "hello", "vacation-planner"); err != nil {
		t.Fatal(err)
	}
	bystanderPin, bystanderSet := pinOf("bystander"), activeSet(t, twoPool, "bystander")
	if bystanderPin != two.Digest() || !slices.Equal(bystanderSet, []string{"vacation-planner"}) {
		t.Fatalf("bystander pin %q set %v", bystanderPin, bystanderSet)
	}
	var actorPinBefore string
	model.script(
		func(agentruntime.ModelRequest) agentruntime.ModelResponse {
			actorPinBefore = pinOf("actor")
			return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{CallID: "activate", Name: "activate_journey", Arguments: json.RawMessage(`{"journey":"shift-swap"}`)}}
		},
		textStep("activated"),
	)
	if _, err := converse(t, twoStore, runner, "actor", "actor-1", "swap", ""); err != nil {
		t.Fatal(err)
	}
	if got := activeSet(t, twoPool, "actor"); !slices.Equal(got, []string{"shift-swap"}) {
		t.Fatalf("actor active set = %v", got)
	}
	if got := activeSet(t, twoPool, "bystander"); !slices.Equal(got, bystanderSet) {
		t.Fatalf("activation changed another conversation's set: %v", got)
	}
	if got := pinOf("bystander"); got != bystanderPin {
		t.Fatalf("activation changed another conversation's pin: %q", got)
	}
	if got := pinOf("actor"); got != actorPinBefore || got != two.Digest() {
		t.Fatalf("activation changed the activating conversation's pin: %q -> %q", actorPinBefore, got)
	}
	if got := currentOf(); got != two.Digest() {
		t.Fatalf("activation changed the current definition: %q", got)
	}
}

func TestDeclarativeJourneyActivation(t *testing.T) {
	path := writeTwoJourneyBundle(t, t.TempDir())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	config := strings.ReplaceAll(string(data), "id: vacation-planner", "id: alpha")
	config = strings.ReplaceAll(config, "id: shift-swap", "id: beta")
	if !strings.Contains(config, "id: alpha") || !strings.Contains(config, "id: beta") || strings.Contains(config, "routing") {
		t.Fatal("journey rename did not apply")
	}
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	registry := compileSet(t, path)
	alpha, _ := registry.Journey("alpha")
	beta, _ := registry.Journey("beta")
	pool := database(t)
	store := journal.New(pool)
	model := &scriptedModel{}
	runner := agentruntime.NewRunner(registry, model, testTenancy(t, keyedEndpoint(t, "lookup_destination", "lookup_shift")))
	builtins := []string{"request_user_input", "activate_journey"}

	t.Run("no target offers only activation", func(t *testing.T) {
		model.script(activateStep("beta"), textStep("ready"))
		output, err := converse(t, store, runner, "free", "free-1", "hello", "")
		if err != nil || output.Answer != "ready" {
			t.Fatalf("run = %#v, %v", output, err)
		}
		requests := model.taken()
		if len(requests) != 2 {
			t.Fatalf("model requests = %d", len(requests))
		}
		first := requests[0]
		if !slices.Equal(toolNames(first.Tools), builtins) || len(first.ActiveJourneys) != 0 || len(first.Provenance.Active) != 0 {
			t.Fatalf("request 0 tools %v active %v provenance %v", toolNames(first.Tools), first.ActiveJourneys, first.Provenance.Active)
		}
		for _, want := range []string{"base prompt", "- alpha: Plan travel", "- beta: Exchange a shift"} {
			if !strings.Contains(first.Instructions, want) {
				t.Fatalf("instructions lack %q: %q", want, first.Instructions)
			}
		}
		for _, absent := range []string{"vacation system", "shift system", "## Active journey"} {
			if strings.Contains(first.Instructions, absent) {
				t.Fatalf("inactive journey material %q in instructions: %q", absent, first.Instructions)
			}
		}
		if first.DefinitionDigest != registry.Digest() || first.Provenance.Definition != (agentruntime.MaterialProvenance{ID: "set", Digest: registry.Digest()}) || first.Provenance.Catalog.Digest == "" || first.Provenance.System.Digest == "" {
			t.Fatalf("request 0 provenance = %#v", first.Provenance)
		}
		var schema struct {
			Properties struct{ Journey struct{ Enum []string } }
		}
		for _, tool := range first.Tools {
			if tool.Name == "activate_journey" {
				raw, _ := json.Marshal(tool.Schema)
				if err := json.Unmarshal(raw, &schema); err != nil {
					t.Fatal(err)
				}
			}
		}
		if !slices.Equal(schema.Properties.Journey.Enum, []string{"alpha", "beta"}) {
			t.Fatalf("activate_journey enum = %v", schema.Properties.Journey.Enum)
		}

		// The model's activation takes effect on the next request of the same run.
		second := requests[1]
		if !slices.Equal(toolNames(second.Tools), []string{"lookup_shift", "request_user_input", "activate_journey"}) || !slices.Equal(second.ActiveJourneys, []string{"beta"}) {
			t.Fatalf("request 1 tools %v active %v", toolNames(second.Tools), second.ActiveJourneys)
		}
		if !strings.Contains(second.Instructions, "## Active journey: beta") || !strings.Contains(second.Instructions, "shift system") || strings.Contains(second.Instructions, "vacation system") {
			t.Fatalf("request 1 instructions = %q", second.Instructions)
		}
		if !slices.Equal(second.Provenance.Active, []agentruntime.MaterialProvenance{{ID: "beta", Digest: beta.Digest}}) {
			t.Fatalf("request 1 active provenance = %#v", second.Provenance.Active)
		}
		if len(second.ToolResults) != 1 || second.ToolResults[0].Name != "activate_journey" || jsonOf(t, second.ToolResults[0].Value) != `{"activated":"beta"}` {
			t.Fatalf("activation result = %#v", second.ToolResults)
		}
		// The activation is durable: a later run of the conversation starts with it.
		if got := activeSet(t, pool, "free"); !slices.Equal(got, []string{"beta"}) {
			t.Fatalf("persisted active set = %v", got)
		}
	})

	t.Run("activation failures are results and never dispatch", func(t *testing.T) {
		model.script(
			func(agentruntime.ModelRequest) agentruntime.ModelResponse {
				return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{CallID: "bad", Name: "activate_journey", Arguments: json.RawMessage(`{"journey":"nope"}`)}}
			},
			// alpha is not active: its alias is a tool_not_active result and is never dispatched.
			func(agentruntime.ModelRequest) agentruntime.ModelResponse {
				return agentruntime.ModelResponse{ToolCall: &agentruntime.ToolCall{CallID: "inactive", Name: "lookup_destination", Arguments: json.RawMessage(`{}`)}}
			},
			textStep("done"),
		)
		if output, err := converse(t, store, runner, "free", "free-2", "again", ""); err != nil || output.Answer != "done" {
			t.Fatalf("run = %#v, %v", output, err)
		}
		requests := model.taken()
		if len(requests) != 3 {
			t.Fatalf("model requests = %d", len(requests))
		}
		// No target: the run starts with the conversation's persisted active set.
		if !slices.Equal(requests[0].ActiveJourneys, []string{"beta"}) || !slices.Equal(toolNames(requests[0].Tools), []string{"lookup_shift", "request_user_input", "activate_journey"}) {
			t.Fatalf("persisted set not restored: active %v tools %v", requests[0].ActiveJourneys, toolNames(requests[0].Tools))
		}
		if got := jsonOf(t, requests[1].ToolResults[len(requests[1].ToolResults)-1].Value); got != `{"error":"journey_unavailable"}` || !slices.Equal(requests[1].ActiveJourneys, []string{"beta"}) {
			t.Fatalf("unknown activation = %s active %v", got, requests[1].ActiveJourneys)
		}
		if got := jsonOf(t, requests[2].ToolResults[len(requests[2].ToolResults)-1].Value); got != `{"error":"tool_not_active"}` {
			t.Fatalf("inactive tool result = %s", got)
		}
		var operations int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_operations").Scan(&operations); err != nil || operations != 0 {
			t.Fatalf("inactive tool was dispatched: %d operations, %v", operations, err)
		}
		if got := activeSet(t, pool, "free"); !slices.Equal(got, []string{"beta"}) {
			t.Fatalf("failed activation changed the active set: %v", got)
		}
	})

	t.Run("model activation adds a second journey", func(t *testing.T) {
		model.script(activateStep("alpha"), textStep("both"))
		if _, err := converse(t, store, runner, "free", "free-3", "more", ""); err != nil {
			t.Fatal(err)
		}
		requests := model.taken()
		second := requests[1]
		if !slices.Equal(second.ActiveJourneys, []string{"beta", "alpha"}) || !slices.Equal(toolNames(second.Tools), []string{"lookup_destination", "lookup_shift", "request_user_input", "activate_journey"}) {
			t.Fatalf("after activating alpha: active %v tools %v", second.ActiveJourneys, toolNames(second.Tools))
		}
		if !strings.Contains(second.Instructions, "vacation system") || !strings.Contains(second.Instructions, "shift system") {
			t.Fatalf("instructions lack an active body: %q", second.Instructions)
		}
	})

	t.Run("explicit target activates before the first round", func(t *testing.T) {
		model.script(textStep("ok"))
		if _, err := converse(t, store, runner, "targeted", "targeted-1", "plan", "alpha"); err != nil {
			t.Fatal(err)
		}
		requests := model.taken()
		if len(requests) != 1 {
			t.Fatalf("model requests = %d", len(requests))
		}
		first := requests[0]
		if !slices.Equal(first.ActiveJourneys, []string{"alpha"}) || !slices.Equal(toolNames(first.Tools), []string{"lookup_destination", "request_user_input", "activate_journey"}) {
			t.Fatalf("target not active on request 0: active %v tools %v", first.ActiveJourneys, toolNames(first.Tools))
		}
		if !strings.Contains(first.Instructions, "vacation system") || strings.Contains(first.Instructions, "shift system") {
			t.Fatalf("instructions = %q", first.Instructions)
		}
		if !slices.Equal(first.Provenance.Active, []agentruntime.MaterialProvenance{{ID: "alpha", Digest: alpha.Digest}}) {
			t.Fatalf("active provenance = %#v", first.Provenance.Active)
		}
		// Targeting adds nothing to history: the model has not been asked to activate.
		if strings.Contains(string(first.History), "activate_journey") || len(first.ToolResults) != 0 {
			t.Fatalf("target activation leaked into history: %s %#v", first.History, first.ToolResults)
		}
		// Targeting a second journey keeps the first (limit 2); re-targeting moves it to MRU.
		for i, step := range []struct {
			target string
			want   []string
		}{{"beta", []string{"alpha", "beta"}}, {"alpha", []string{"beta", "alpha"}}} {
			model.script(textStep("ok"))
			if _, err := converse(t, store, runner, "targeted", fmt.Sprintf("targeted-%d", i+2), "plan", step.target); err != nil {
				t.Fatal(err)
			}
			if got := model.taken()[0].ActiveJourneys; !slices.Equal(got, step.want) {
				t.Fatalf("target %s active = %v, want %v", step.target, got, step.want)
			}
		}
	})

	t.Run("unknown target fails", func(t *testing.T) {
		model.script(textStep("never"))
		if _, err := converse(t, store, runner, "unknown", "unknown-1", "plan", "missing"); err == nil || !strings.Contains(err.Error(), "unknown journey target") {
			t.Fatalf("unknown target = %v", err)
		}
		// Even a caller that skips Binding cannot run an unknown target.
		command := journal.Command{ThreadID: "unknown-direct", RunID: "unknown-2", CommunicationID: "unknown-2", Principal: "alice", Tenant: testTenant, Text: "plan"}
		if _, _, err := store.Admit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
		owner := claimForTest(t, store, command, false)
		input := scoped(t, runner, agentruntime.RunInput{Store: store, Owner: owner, ThreadID: command.ThreadID, RunID: command.RunID, Principal: "alice", Tenant: testTenant, Text: command.Text})
		_, digest, err := runner.Binding(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		input.DefinitionDigest, input.TargetJourney = digest, "missing"
		if _, err := runner.Run(t.Context(), input); !errors.Is(err, journal.ErrDefinition) {
			t.Fatalf("Run with unknown target = %v", err)
		}
		if len(model.taken()) != 0 {
			t.Fatalf("model called for an unknown target: %d", len(model.taken()))
		}
	})

	t.Run("unavailable journeys are hidden and cannot be targeted", func(t *testing.T) {
		// This endpoint publishes only alpha's operation.
		partial := agentruntime.NewRunner(registry, model, testTenancy(t, keyedEndpoint(t, "lookup_destination")))
		model.script(activateStep("beta"), textStep("ok"))
		if _, err := converse(t, store, partial, "partial", "partial-1", "hello", ""); err != nil {
			t.Fatal(err)
		}
		requests := model.taken()
		if strings.Contains(requests[0].Instructions, "- beta:") || !strings.Contains(requests[0].Instructions, "- alpha:") {
			t.Fatalf("unavailable journey in metadata: %q", requests[0].Instructions)
		}
		if got := jsonOf(t, requests[1].ToolResults[0].Value); got != `{"error":"journey_unavailable"}` || len(requests[1].ActiveJourneys) != 0 {
			t.Fatalf("activation of unavailable journey = %s active %v", got, requests[1].ActiveJourneys)
		}
		model.script(textStep("never"))
		if _, err := converse(t, store, partial, "partial-target", "partial-target-1", "hello", "beta"); !errors.Is(err, agentruntime.ErrJourneyUnavailable) {
			t.Fatalf("unavailable target = %v", err)
		}
		if len(model.taken()) != 0 {
			t.Fatalf("model called for an unavailable target: %d", len(model.taken()))
		}
	})
}

func TestResolveDefinitionRejectsOwnerExpiredWhileWaitingForRollout(t *testing.T) {
	pool := database(t)
	store := journal.New(pool)
	command := journal.Command{ThreadID: "definition-owner", RunID: "definition-owner", CommunicationID: "definition-owner", Principal: "alice", Tenant: testTenant, Text: "plan"}
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
		_, err := store.ResolveDefinition(context.Background(), owner, strings.Repeat("a", 64), func(string) bool { return true })
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
	var pinned int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM agent_conversations WHERE id=$1 AND definition_digest IS NOT NULL", command.ThreadID).Scan(&pinned); err != nil || pinned != 0 {
		t.Fatalf("stale owner pinned %d conversations: %v", pinned, err)
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
	if old.Digest() == "" || old.Digest() != copyRegistry.Digest() {
		t.Fatalf("root-dependent set digest: %q != %q", old.Digest(), copyRegistry.Digest())
	}

	first.Tools[0].Ops[0] = "mutated"
	first.Tools[0].Class = definitions.ClassEffectful
	first.Skills[0].Instructions.Data[0] = 'X'
	pinnedSet, ok := old.Version(old.Digest())
	if !ok {
		t.Fatal("compiled set does not serve its own digest")
	}
	again, ok := pinnedSet.Journey("planner")
	if !ok || again.Tools[0].Ops[0] != "lookup" || again.Tools[0].Class != definitions.ClassReadOnly || string(again.Skills[0].Instructions.Data) != "---\nname: planner\ndescription: Plan a trip\n---\nold prompt\n" {
		t.Fatalf("caller mutation changed compiled definition: %#v", again)
	}

	newRoot := t.TempDir()
	newPath := writeDefinitionBundle(t, newRoot, "new prompt\n")
	registry := compileSet(t, newPath, oldPath)
	current, _ := registry.Journey("planner")
	retainedSet, ok := registry.Version(old.Digest())
	if !ok || !registry.Serves(old.Digest()) || registry.Digest() == old.Digest() {
		t.Fatalf("old set not retained distinctly: ok=%v", ok)
	}
	retained, ok := retainedSet.Journey("planner")
	if !ok || retained.Prompt != "old prompt" || current.Digest == retained.Digest {
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
			if err := os.Symlink(filepath.Join(root, "journeys.yaml"), filepath.Join(root, "skills/planner/SKILL.md")); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized skill", func(t *testing.T, root string) {
			skill := "---\nname: planner\ndescription: Plan a trip\n---\n" + strings.Repeat("x", definitions.MaxSkillBytes+1)
			if err := os.WriteFile(filepath.Join(root, "skills/planner/SKILL.md"), []byte(skill), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized conversation prompt", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "conversation.md"), []byte(strings.Repeat("x", definitions.MaxPromptBytes+1)), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "journeys.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append([]byte("conversation_prompt: conversation.md\n"), data...), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"removed system_prompt key", func(t *testing.T, root string) {
			path := filepath.Join(root, "journeys.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), "skill: skills/planner\n", "skill: skills/planner\n    system_prompt: prompt.md\n", 1))
			if err := os.WriteFile(path, data, 0600); err != nil {
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
	retainedSet, ok := registry.Version(old.Digest())
	if !ok || !registry.Serves(old.Digest()) {
		t.Fatal("old set not retained")
	}
	retained, ok := retainedSet.Journey("planner")
	if !ok || retained.Digest != oldJourney.Digest || len(retained.Tools) != 1 || retained.Tools[0].Ops[0] != "legacy_lookup" || retained.Tools[0].Class != definitions.ClassReadOnly {
		t.Fatalf("retained binding = %#v, %v", retained.Tools, ok)
	}
	current, _ := registry.Journey("planner")
	if current.Tools[0].Ops[0] != "current_lookup" || current.Tools[0].Class != "" {
		t.Fatalf("current tools = %#v", current.Tools)
	}
	currentSet, ok := registry.Version(registry.Digest())
	if !ok || !registry.Serves(registry.Digest()) {
		t.Fatal("current set not served")
	}
	pinnedCurrent, ok := currentSet.Journey("planner")
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
			config := "catalog:\n  operation_meta_key: operation_key\njourneys:\n  - id: planner\n    skill: skills/planner\n    tools:\n      - op: lookup\n"
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
