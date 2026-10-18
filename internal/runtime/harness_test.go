package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"

	"github.com/swiftdiaries/az-agent-platform-go/internal/catalog"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
)

// The loop tests drive runHarness with the real journal and definitions, a scripted
// model and a fake catalog, so that no MCP server is needed.

type schemaOnlyTool struct{ name string }

func (t schemaOnlyTool) Name() string        { return t.name }
func (t schemaOnlyTool) Description() string { return "tool " + t.name }
func (t schemaOnlyTool) Schema() any         { return map[string]any{"type": "object"} }
func (t schemaOnlyTool) ReturnSchema() any   { return nil }

type fakeCatalog struct {
	mu          sync.Mutex
	journeys    map[string][]catalog.Tool // journey -> tools, in config order
	unavailable map[string]bool
	calls       []string
	onCall      func(alias string)
}

func (f *fakeCatalog) Resolution(id string) (catalog.Resolution, bool) {
	tools, ok := f.journeys[id]
	if !ok {
		return catalog.Resolution{}, false
	}
	if f.unavailable[id] {
		return catalog.Resolution{JourneyID: id, Unavailable: []catalog.Unavailable{{Reason: catalog.ReasonMissing, Operation: id + ".op"}}}, true
	}
	return catalog.Resolution{JourneyID: id, Tools: tools}, true
}

func (f *fakeCatalog) Tool(id, alias string) (catalog.Tool, bool) {
	resolution, ok := f.Resolution(id)
	if !ok || !resolution.Available() {
		return catalog.Tool{}, false
	}
	for _, t := range resolution.Tools {
		if t.Alias == alias {
			return t, true
		}
	}
	return catalog.Tool{}, false
}

func (f *fakeCatalog) Tools(ids ...string) []tool.Tool {
	var out []tool.Tool
	seen := map[string]bool{}
	for _, id := range []string{"alpha", "beta"} { // config order
		resolution, ok := f.Resolution(id)
		if !ok || !resolution.Available() || !slices.Contains(ids, id) {
			continue
		}
		for _, t := range resolution.Tools {
			if !seen[t.Alias] {
				seen[t.Alias] = true
				out = append(out, schemaOnlyTool{t.Alias})
			}
		}
	}
	return out
}

func (f *fakeCatalog) Call(_ context.Context, _, alias string, _ []byte) (any, error) {
	f.mu.Lock()
	f.calls = append(f.calls, alias)
	hook := f.onCall
	f.mu.Unlock()
	if hook != nil {
		hook(alias)
	}
	return map[string]string{"alias": alias, "status": "ok"}, nil
}

func (f *fakeCatalog) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

type scriptedModel struct {
	mu       sync.Mutex
	requests []ModelRequest
	script   []func(ModelRequest) ModelResponse
}

func (m *scriptedModel) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	m.mu.Lock()
	index := len(m.requests)
	m.requests = append(m.requests, request)
	m.mu.Unlock()
	if index >= len(m.script) {
		return ModelResponse{}, errors.New("model script exhausted")
	}
	return m.script[index](request), nil
}

func toolCall(id, name, arguments string) ToolCall {
	return ToolCall{CallID: id, Name: name, Arguments: json.RawMessage(arguments)}
}

func respond(calls ...ToolCall) func(ModelRequest) ModelResponse {
	return func(ModelRequest) ModelResponse { return ModelResponse{ToolCalls: calls} }
}

func answer(text string) func(ModelRequest) ModelResponse {
	return func(ModelRequest) ModelResponse { return ModelResponse{Text: text} }
}

func requestToolNames(request ModelRequest) []string {
	names := make([]string, len(request.Tools))
	for i, t := range request.Tools {
		names[i] = t.Name
	}
	return names
}

const harnessConfig = `conversation_prompt: conversation.md
catalog:
  operation_meta_key: operation_key
  call_id_header: X-Platform-Call-ID
journeys:
  - id: alpha
    skill: skills/alpha
    tools:
      - op: alpha.lookup
        class: read_only
      - op: alpha.book
        class: effectful
  - id: beta
    skill: skills/beta
    tools:
      - op: beta.lookup
        class: read_only
  - id: gamma
    skill: skills/gamma
    tools:
      - op: gamma.lookup
        class: read_only
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func compileHarnessSet(t *testing.T) *definitions.Set {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "journeys.yaml"), harnessConfig)
	writeFile(t, filepath.Join(dir, "conversation.md"), "You are the conversation agent.\n")
	for _, id := range []string{"alpha", "beta", "gamma"} {
		writeFile(t, filepath.Join(dir, "skills", id, "SKILL.md"), "---\nname: "+id+"\ndescription: Does "+id+" things\n---\nBody of "+id+".\n")
	}
	writeFile(t, filepath.Join(dir, "skills", "alpha", "guide.md"), "alpha guide\n")
	set, err := definitions.Compile(filepath.Join(dir, "journeys.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func harnessCatalog() *fakeCatalog {
	return &fakeCatalog{journeys: map[string][]catalog.Tool{
		"alpha": {
			{Alias: "alpha_lookup", OperationKey: "alpha.lookup", Class: definitions.ClassReadOnly},
			{Alias: "alpha_book", OperationKey: "alpha.book", Class: definitions.ClassEffectful, RequiresApproval: true},
		},
		"beta":  {{Alias: "beta_lookup", OperationKey: "beta.lookup", Class: definitions.ClassReadOnly}},
		"gamma": {{Alias: "gamma_lookup", OperationKey: "gamma.lookup", Class: definitions.ClassReadOnly}},
	}}
}

type harness struct {
	t       *testing.T
	pool    *pgxpool.Pool
	store   *journal.Store
	command journal.Command
	owner   journal.Owner
	set     *definitions.Set
	cat     *fakeCatalog
	model   *scriptedModel
	runner  *Runner
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.Database(t)
	if err := journal.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, pool: pool, store: journal.New(pool), set: compileHarnessSet(t), cat: harnessCatalog(), model: &scriptedModel{}}
	h.command = journal.Command{ThreadID: "thread", RunID: "run", CommunicationID: "m1", Principal: "p", Tenant: "tenant", Text: "hello"}
	if _, fresh, err := h.store.Admit(ctx, h.command); err != nil || !fresh {
		t.Fatalf("Admit: %v %v", fresh, err)
	}
	owner, err := h.store.Claim(ctx, h.command, "owner", journal.LeaseDuration)
	if err != nil {
		t.Fatal(err)
	}
	h.owner = owner
	h.runner = NewRunner(h.set, h.model, Tenancy{})
	return h
}

// run starts the run (pinning the set) and runs the conversation with the given limit.
func (h *harness) run(target string, limit int) (RunOutput, error) {
	h.t.Helper()
	return h.runWith(target, limit, 0)
}

func (h *harness) runWith(target string, limit, iteration int) (RunOutput, error) {
	h.t.Helper()
	ctx := context.Background()
	history, err := h.store.Start(ctx, h.owner, target, h.set.Digest())
	if err != nil {
		h.t.Fatal(err)
	}
	in := RunInput{Store: h.store, Owner: h.owner, Iteration: iteration, History: history, ThreadID: h.command.ThreadID, RunID: h.command.RunID, Text: h.command.Text, TargetJourney: target, DefinitionDigest: h.set.Digest()}
	in.Settings.MaxActiveJourneys = limit
	return h.runner.converse(ctx, in, h.set, h.cat)
}

// approvePending approves the thread's pending approval in a new run, as the platform
// does, and makes that run's owner the harness owner. The approved call is not dispatched.
func (h *harness) approvePending() {
	h.t.Helper()
	ctx := context.Background()
	var interactionID, binding string
	if err := h.pool.QueryRow(ctx, "SELECT id,artifact->'call'->>'binding' FROM agent_interactions WHERE thread_id='thread' AND kind='approval' AND state='pending'").Scan(&interactionID, &binding); err != nil {
		h.t.Fatal(err)
	}
	reply := journal.Command{ThreadID: "thread", RunID: "run-approve", CommunicationID: "approve", Principal: "p", Tenant: "tenant", InteractionID: interactionID, ReplyKind: "approval", ReplyJSON: `{"decision":"approve","binding":"` + binding + `"}`}
	if _, fresh, err := h.store.Admit(ctx, reply); err != nil || !fresh {
		h.t.Fatalf("reply: %v %v", fresh, err)
	}
	owner, err := h.store.Claim(ctx, reply, "owner-2", journal.LeaseDuration)
	if err != nil {
		h.t.Fatal(err)
	}
	h.command, h.owner = reply, owner
	h.command.Text = "approve"
}

func (h *harness) active() (active, pinned []string) {
	h.t.Helper()
	active, pinned, err := h.store.ActiveJourneys(context.Background(), h.owner)
	if err != nil {
		h.t.Fatal(err)
	}
	return active, pinned
}

// historyResults decodes the tool results of a run's history by call ID.
func historyResults(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var history []*message.Message
	if err := json.Unmarshal(raw, &history); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, m := range history {
		for _, c := range m.Contents {
			if r, ok := c.(*message.FunctionResultContent); ok {
				out[r.CallID] = r.Result
			}
		}
	}
	return out
}

func asJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHarnessActivationRebuildsAgentAndGatesTools(t *testing.T) {
	h := newHarness(t)
	h.model.script = []func(ModelRequest) ModelResponse{
		// Round 0: activation plus a sibling that must not run.
		respond(toolCall("c1", "activate_journey", `{"journey":"alpha"}`), toolCall("c2", "alpha_lookup", `{}`)),
		// Round 1: alpha is active; one active, one foreign, one unknown tool.
		respond(toolCall("c3", "alpha_lookup", `{}`), toolCall("c4", "beta_lookup", `{}`), toolCall("c5", "mystery", `{not json`)),
		answer("done"),
	}
	out, err := h.run("", 2)
	if err != nil {
		t.Fatal(err)
	}
	if out.Answer != "done" || out.Waiting {
		t.Fatalf("output = %+v", out)
	}
	requests := h.model.requests
	if len(requests) != 3 {
		t.Fatalf("model requests = %d", len(requests))
	}
	// Round 0: nothing active; the available journeys are advertised.
	r0 := requests[0]
	if !slices.Equal(requestToolNames(r0), []string{"request_user_input", "activate_journey"}) || len(r0.ActiveJourneys) != 0 || len(r0.Provenance.Active) != 0 {
		t.Fatalf("round 0 tools/active = %v/%v", requestToolNames(r0), r0.ActiveJourneys)
	}
	for _, want := range []string{"You are the conversation agent.", "## Available journeys", "- alpha: Does alpha things", "- beta: Does beta things", "- gamma: Does gamma things"} {
		if !strings.Contains(r0.Instructions, want) {
			t.Fatalf("round 0 instructions lack %q:\n%s", want, r0.Instructions)
		}
	}
	if strings.Contains(r0.Instructions, "## Active journey") || strings.Contains(r0.Instructions, "Body of alpha") {
		t.Fatalf("round 0 instructions have an active journey:\n%s", r0.Instructions)
	}
	enum := requests[0].Tools[1].Schema
	if !strings.Contains(asJSON(t, enum), `"enum":["alpha","beta","gamma"]`) {
		t.Fatalf("activate_journey schema = %s", asJSON(t, enum))
	}
	// Round 1: the inner agent was rebuilt with alpha's tools, body, guide tool and digests.
	r1 := requests[1]
	if want := []string{"alpha_lookup", "alpha_book", "request_user_input", "activate_journey", "read_skill_resource"}; !slices.Equal(requestToolNames(r1), want) {
		t.Fatalf("round 1 tools = %v, want %v", requestToolNames(r1), want)
	}
	if !strings.Contains(r1.Instructions, "## Active journey: alpha\nBody of alpha.") || !slices.Equal(r1.ActiveJourneys, []string{"alpha"}) {
		t.Fatalf("round 1 instructions/active = %q/%v", r1.Instructions, r1.ActiveJourneys)
	}
	if r1.DefinitionDigest != h.set.Digest() || r1.Provenance.Definition != (MaterialProvenance{ID: "set", Digest: h.set.Digest()}) || r1.Provenance.System.ID != "instructions" || r1.Provenance.System.Digest != digestValue(r1.Instructions) || r1.Provenance.Catalog.ID != "journeys" || r1.Provenance.History.ID != "thread" {
		t.Fatalf("round 1 provenance = %+v", r1.Provenance)
	}
	if len(r1.Provenance.Active) != 1 || r1.Provenance.Active[0].ID != "alpha" || r1.Provenance.Active[0].Digest == "" {
		t.Fatalf("round 1 active provenance = %+v", r1.Provenance.Active)
	}
	if r0.Provenance.System.Digest == r1.Provenance.System.Digest {
		t.Fatal("instructions digest did not change with the active set")
	}
	// Only the active alias was dispatched.
	if got := h.cat.called(); !slices.Equal(got, []string{"alpha_lookup"}) {
		t.Fatalf("dispatched = %v", got)
	}
	results := historyResults(t, out.History)
	if got := asJSON(t, results["c1"]); got != `{"activated":"alpha"}` {
		t.Fatalf("c1 = %s", got)
	}
	if got := asJSON(t, results["c2"]); got != `{"error":"not_executed","message":"not executed; call again after activation"}` {
		t.Fatalf("c2 = %s", got)
	}
	if got := asJSON(t, results["c3"]); got != `{"alias":"alpha_lookup","status":"ok"}` {
		t.Fatalf("c3 = %s", got)
	}
	for _, id := range []string{"c4", "c5"} {
		if got := asJSON(t, results[id]); got != `{"error":"tool_not_active"}` {
			t.Fatalf("%s = %s", id, got)
		}
	}
	// Tool results are named by the alias in the following request.
	var named []string
	for _, r := range requests[2].ToolResults {
		named = append(named, r.CallID+"="+r.Name)
	}
	if !slices.Contains(named, "c3=alpha_lookup") || !slices.Contains(named, "c1=activate_journey") {
		t.Fatalf("tool result names = %v", named)
	}
	if active, _ := h.active(); !slices.Equal(active, []string{"alpha"}) {
		t.Fatalf("persisted active = %v", active)
	}
}

func TestHarnessActivationEvictsLeastRecentlyUsed(t *testing.T) {
	h := newHarness(t)
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "activate_journey", `{"journey":"alpha"}`)),
		respond(toolCall("c2", "activate_journey", `{"journey":"beta"}`)),
		respond(toolCall("c3", "activate_journey", `{"journey":"gamma"}`), toolCall("c4", "activate_journey", `{"journey":"nope"}`), toolCall("c5", "activate_journey", `{"journey":7}`)),
		answer("done"),
	}
	out, err := h.run("", 2)
	if err != nil {
		t.Fatal(err)
	}
	results := historyResults(t, out.History)
	if got := asJSON(t, results["c3"]); got != `{"activated":"gamma","evicted":["alpha"]}` {
		t.Fatalf("c3 = %s", got)
	}
	if got := asJSON(t, results["c4"]); got != `{"error":"journey_unavailable"}` {
		t.Fatalf("c4 = %s", got)
	}
	if got := asJSON(t, results["c5"]); got != `{"error":"invalid_arguments"}` {
		t.Fatalf("c5 = %s", got)
	}
	if last := h.model.requests[3]; !slices.Equal(last.ActiveJourneys, []string{"beta", "gamma"}) {
		t.Fatalf("final active = %v", last.ActiveJourneys)
	}
	if active, _ := h.active(); !slices.Equal(active, []string{"beta", "gamma"}) {
		t.Fatalf("persisted active = %v", active)
	}
}

func TestHarnessUnavailableJourneysAreHiddenNotFatal(t *testing.T) {
	h := newHarness(t)
	h.cat.unavailable = map[string]bool{"beta": true}
	if err := h.store.SetActiveJourneys(context.Background(), h.owner, []string{"beta", "alpha"}); err != nil {
		t.Fatal(err)
	}
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "activate_journey", `{"journey":"beta"}`), toolCall("c2", "beta_lookup", `{}`)),
		answer("done"),
	}
	out, err := h.run("", 2)
	if err != nil {
		t.Fatal(err)
	}
	first := h.model.requests[0]
	// beta is neither advertised nor kept active; alpha stays.
	if strings.Contains(first.Instructions, "- beta:") || !strings.Contains(first.Instructions, "- alpha:") || !slices.Equal(first.ActiveJourneys, []string{"alpha"}) {
		t.Fatalf("first request = %v\n%s", first.ActiveJourneys, first.Instructions)
	}
	if strings.Contains(asJSON(t, first.Tools[len(first.Tools)-2].Schema), "beta") {
		t.Fatalf("beta offered for activation: %s", asJSON(t, first.Tools[len(first.Tools)-2].Schema))
	}
	if got := asJSON(t, historyResults(t, out.History)["c1"]); got != `{"error":"journey_unavailable"}` {
		t.Fatalf("c1 = %s", got)
	}
	if got := asJSON(t, historyResults(t, out.History)["c2"]); got != `{"error":"not_executed","message":"not executed; call again after activation"}` {
		t.Fatalf("c2 = %s", got)
	}
	if active, _ := h.active(); !slices.Equal(active, []string{"alpha"}) {
		t.Fatalf("persisted active = %v", active)
	}
}

func TestHarnessExplicitTarget(t *testing.T) {
	t.Run("is active before round zero without touching history", func(t *testing.T) {
		h := newHarness(t)
		h.model.script = []func(ModelRequest) ModelResponse{answer("done")}
		if _, err := h.run("beta", 2); err != nil {
			t.Fatal(err)
		}
		first := h.model.requests[0]
		if !slices.Equal(first.ActiveJourneys, []string{"beta"}) || !slices.Contains(requestToolNames(first), "beta_lookup") || !strings.Contains(first.Instructions, "## Active journey: beta") {
			t.Fatalf("first request = %v %v", first.ActiveJourneys, requestToolNames(first))
		}
		if len(first.ToolResults) != 0 {
			t.Fatalf("activation leaked into history: %+v", first.ToolResults)
		}
		if active, _ := h.active(); !slices.Equal(active, []string{"beta"}) {
			t.Fatalf("persisted active = %v", active)
		}
	})
	t.Run("unavailable target fails the run", func(t *testing.T) {
		h := newHarness(t)
		h.cat.unavailable = map[string]bool{"beta": true}
		_, err := h.run("beta", 2)
		if !errors.Is(err, ErrJourneyUnavailable) || len(h.model.requests) != 0 {
			t.Fatalf("error/requests = %v/%d", err, len(h.model.requests))
		}
	})
	t.Run("refused activation fails the run", func(t *testing.T) {
		h := newHarness(t)
		// alpha has an approved, undispatched call: it is pinned and fills the only slot.
		h.model.script = []func(ModelRequest) ModelResponse{respond(toolCall("c1", "alpha_book", `{}`))}
		if out, err := h.run("alpha", 1); err != nil || !out.Waiting {
			t.Fatalf("setup: %+v %v", out, err)
		}
		h.approvePending()
		if active, pinned := h.active(); !slices.Equal(active, []string{"alpha"}) || !slices.Equal(pinned, []string{"alpha"}) {
			t.Fatalf("active/pinned = %v/%v", active, pinned)
		}
		// activation of beta at limit 1 is refused, as a result for the model.
		conv := &conversation{set: h.set, cat: h.cat, limit: 1, available: h.set.Journeys()}
		result, err := conv.activate(context.Background(), RunInput{Store: h.store, Owner: h.owner}, `{"journey":"beta"}`)
		if err != nil || asJSON(t, result) != `{"error":"activation_refused","message":"all active journeys are pinned"}` {
			t.Fatalf("activate = %v/%v", asJSON(t, result), err)
		}
		// and a refused explicit target fails the run.
		if _, err := h.runner.converse(context.Background(), RunInput{Store: h.store, Owner: h.owner, TargetJourney: "beta", Settings: settingsWith(1)}, h.set, h.cat); !errors.Is(err, ErrJourneyUnavailable) {
			t.Fatalf("converse error = %v", err)
		}
		if active, _ := h.active(); !slices.Equal(active, []string{"alpha"}) {
			t.Fatalf("refusal changed the active set: %v", active)
		}
	})
	t.Run("invalid limit fails the run", func(t *testing.T) {
		h := newHarness(t)
		if _, err := h.run("", 0); err == nil || len(h.model.requests) != 0 {
			t.Fatalf("error/requests = %v/%d", err, len(h.model.requests))
		}
	})
}

func TestHarnessLoweredLimitEvictsLeastRecentlyUsedAtRunStart(t *testing.T) {
	h := newHarness(t)
	if err := h.store.SetActiveJourneys(context.Background(), h.owner, []string{"alpha", "beta", "gamma"}); err != nil {
		t.Fatal(err)
	}
	h.model.script = []func(ModelRequest) ModelResponse{answer("done")}
	if _, err := h.run("", 2); err != nil {
		t.Fatal(err)
	}
	if first := h.model.requests[0]; !slices.Equal(first.ActiveJourneys, []string{"beta", "gamma"}) {
		t.Fatalf("active = %v", first.ActiveJourneys)
	}
	if active, _ := h.active(); !slices.Equal(active, []string{"beta", "gamma"}) {
		t.Fatalf("persisted active = %v", active)
	}
}

func TestHarnessReadSkillResourceIsScopedToActiveJourneys(t *testing.T) {
	h := newHarness(t)
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "read_skill_resource", `{"journey":"alpha","resource":"guide.md"}`)), // not offered yet: inactive
		respond(toolCall("c2", "activate_journey", `{"journey":"alpha"}`)),
		respond(
			toolCall("c3", "read_skill_resource", `{"journey":"alpha","resource":"guide.md"}`),
			toolCall("c4", "read_skill_resource", `{"journey":"alpha","resource":"missing.md"}`),
			toolCall("c5", "read_skill_resource", `{"journey":"beta","resource":"guide.md"}`),
			toolCall("c6", "read_skill_resource", `{"journey":"alpha"}`),
		),
		answer("done"),
	}
	out, err := h.run("", 2)
	if err != nil {
		t.Fatal(err)
	}
	results := historyResults(t, out.History)
	if got := asJSON(t, results["c1"]); got != `{"error":"tool_not_active"}` {
		t.Fatalf("c1 = %s", got)
	}
	if got := asJSON(t, results["c3"]); got != `"alpha guide\n"` {
		t.Fatalf("c3 = %s", got)
	}
	for id, want := range map[string]string{"c4": `{"error":"resource_unavailable"}`, "c5": `{"error":"resource_unavailable"}`, "c6": `{"error":"invalid_arguments"}`} {
		if got := asJSON(t, results[id]); got != want {
			t.Fatalf("%s = %s, want %s", id, got, want)
		}
	}
	last := h.model.requests[3]
	if len(last.Provenance.Skills) != 1 || last.Provenance.Skills[0].ID != "alpha/guide.md" || last.Provenance.Skills[0].Digest == "" {
		t.Fatalf("skills provenance = %+v", last.Provenance.Skills)
	}
}

func TestHarnessSteeringIsDrainedAndIncludedEveryRound(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	correction := journal.Command{ThreadID: "thread", RunID: "run-b", CommunicationID: "m2", Principal: "p", Tenant: "tenant", Text: "actually Kyoto"}
	h.cat.onCall = func(string) {
		if _, fresh, err := h.store.Admit(ctx, correction); err != nil || fresh {
			t.Errorf("correction admit: fresh=%v err=%v", fresh, err)
		}
	}
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "activate_journey", `{"journey":"alpha"}`)),
		respond(toolCall("c2", "alpha_lookup", `{}`)),
		answer("done"),
	}
	if _, err := h.run("", 2); err != nil {
		t.Fatal(err)
	}
	requests := h.model.requests
	if len(requests) != 3 {
		t.Fatalf("requests = %d", len(requests))
	}
	texts := func(r ModelRequest) []string {
		var out []string
		for _, c := range r.PendingCommands {
			out = append(out, c.Text)
		}
		return out
	}
	// m1 is pending in round 0 only; the correction arrives during the tool call and is
	// drained by the next round's fresh agent, which includes it in its request.
	if !slices.Equal(texts(requests[0]), []string{"hello"}) || len(requests[1].PendingCommands) != 0 || !slices.Equal(texts(requests[2]), []string{"actually Kyoto"}) {
		t.Fatalf("pending = %v / %v / %v", texts(requests[0]), texts(requests[1]), texts(requests[2]))
	}
	if len(requests[2].Provenance.Pending) != 1 || requests[2].Provenance.IncludedInput[0] != requests[2].Provenance.Pending[0] {
		t.Fatalf("provenance = %+v", requests[2].Provenance)
	}
	if !strings.Contains(strings.Join(requests[2].Messages, "\n"), "actually Kyoto") {
		t.Fatalf("correction missing from the provider request: %v", requests[2].Messages)
	}
	snapshot, err := h.store.Snapshot(ctx, "thread", "p")
	if err != nil {
		t.Fatal(err)
	}
	included := map[string]int{}
	for _, event := range snapshot.Events {
		if event.Type == "command.included" {
			included[event.CommunicationID]++
		}
	}
	if included["m1"] != 1 || included["m2"] != 1 {
		t.Fatalf("included events = %v", included)
	}
}

func TestHarnessApprovalOwnedByJourneyAndOperationRecordsIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "activate_journey", `{"journey":"alpha"}`)),
		respond(toolCall("c2", "alpha_book", `{"n":1}`), toolCall("c3", "alpha_lookup", `{}`)),
	}
	out, err := h.run("", 2)
	if err != nil || !out.Waiting {
		t.Fatalf("output/error = %+v/%v", out, err)
	}
	var journey string
	if err := h.pool.QueryRow(ctx, "SELECT journey_id FROM agent_interactions WHERE thread_id='thread' AND kind='approval'").Scan(&journey); err != nil || journey != "alpha" {
		t.Fatalf("approval journey = %q/%v", journey, err)
	}
	if got := h.cat.called(); len(got) != 0 {
		t.Fatalf("dispatched before approval: %v", got)
	}
	h.approvePending()
	if _, pinned := h.active(); !slices.Equal(pinned, []string{"alpha"}) {
		t.Fatalf("pinned while approved but undispatched = %v", pinned)
	}
	h.model.script = append(h.model.script, answer("booked"))
	done, err := h.run("alpha", 2)
	if err != nil || done.Answer != "booked" {
		t.Fatalf("resume: %+v %v", done, err)
	}
	if got := h.cat.called(); !slices.Equal(got, []string{"alpha_book"}) {
		t.Fatalf("dispatched = %v", got)
	}
	var operationJourney string
	if err := h.pool.QueryRow(ctx, "SELECT journey_id FROM agent_operations WHERE thread_id='thread'").Scan(&operationJourney); err != nil || operationJourney != "alpha" {
		t.Fatalf("operation journey = %q/%v", operationJourney, err)
	}
	results := historyResults(t, done.History)
	if got := asJSON(t, results["c2"]); got != `{"alias":"alpha_book","status":"ok"}` {
		t.Fatalf("c2 = %s", got)
	}
	if got := asJSON(t, results["c3"]); got != `{"error":"deferred_for_human_input"}` {
		t.Fatalf("c3 = %s", got)
	}
}

func TestHarnessClarificationHasNoJourney(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	questions := `{"questions":[{"id":"q","header":"Pick","question":"Which?","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}]}]}`
	h.model.script = []func(ModelRequest) ModelResponse{
		respond(toolCall("c1", "activate_journey", `{"journey":"alpha"}`)),
		respond(toolCall("c2", "request_user_input", questions)),
	}
	out, err := h.run("", 2)
	if err != nil || !out.Waiting {
		t.Fatalf("output/error = %+v/%v", out, err)
	}
	var journey *string
	if err := h.pool.QueryRow(ctx, "SELECT journey_id FROM agent_interactions WHERE thread_id='thread' AND kind='clarification'").Scan(&journey); err != nil || journey != nil {
		t.Fatalf("clarification journey = %v/%v", journey, err)
	}
}

func settingsWith(limit int) settings.Settings { return settings.Settings{MaxActiveJourneys: limit} }
