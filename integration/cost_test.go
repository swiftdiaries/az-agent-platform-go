package integration_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/cost"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/mcptest"
	"github.com/swiftdiaries/az-agent-platform-go/internal/modeltest"
	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
)

// costFixture is one replica wired to a scripted model and a scripted cost gate.
type costFixture struct {
	pool     *pgxpool.Pool
	endpoint *mcptest.Server
	model    *modeltest.Model
	gate     *cost.Scripted
	url      string
}

func newCostFixture(t *testing.T, steps ...modeltest.Step) *costFixture {
	t.Helper()
	pool := database(t)
	endpoint := mcptest.New(t, mcptest.Options{Name: "cost", Version: "vN"}, mcptest.Catalog{Tools: []mcptest.Tool{
		{Name: "lookup", OperationKey: "travel.lookup", Result: "Kyoto is available"},
	}})
	model := modeltest.New(steps...)
	gate := cost.NewScripted(nil)
	tenancy := testTenancy(t, endpoint.URL())
	tenancy.Cost = gate
	replica := startAcceptanceReplica(t, pool, acceptanceSet(t, "read_only"), model, tenancy)
	return &costFixture{pool: pool, endpoint: endpoint, model: model, gate: gate, url: replica.url}
}

func (f *costFixture) post(t *testing.T, thread, run string) (body, internalThread, internalRun string) {
	t.Helper()
	return f.postJourney(t, thread, run, "planner")
}

// postJourney posts a Chat run whose forwardedProps.journeyId is journey; an
// empty journey sends no journeyId (an untargeted run).
func (f *costFixture) postJourney(t *testing.T, thread, run, journey string) (body, internalThread, internalRun string) {
	t.Helper()
	props := map[string]any{}
	if journey != "" {
		props["journeyId"] = journey
	}
	body = e2eReadBody(t, run, acceptancePost(t, f.url, aguitypes.RunAgentInput{
		ThreadID: thread, RunID: run,
		Messages:       []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
		ForwardedProps: props,
	}))
	internalThread = acceptanceThread(t, f.pool, thread)
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM chat_runs WHERE thread_id=$1 AND external_id=$2`, internalThread, run).Scan(&internalRun); err != nil {
		t.Fatal(err)
	}
	return body, internalThread, internalRun
}

func (f *costFixture) state(t *testing.T, thread, run string) string {
	t.Helper()
	return e2eRunState(t, f.pool, thread, run)
}

func requireLimitSSE(t *testing.T, body string) {
	t.Helper()
	if !strings.Contains(body, "RUN_ERROR") || !strings.Contains(body, `"code":"limit_exceeded"`) || strings.Contains(body, "RUN_FINISHED") {
		t.Fatalf("expected RUN_ERROR code limit_exceeded and no RUN_FINISHED: %s", body)
	}
}

func TestCostAdmitDeniedLimitExceeded(t *testing.T) {
	f := newCostFixture(t, modeltest.Text("never"))
	f.gate.SetAdmit(func(cost.Attribution, settings.Cost) (cost.Decision, error) {
		return cost.Deny(cost.LimitBudget, 12*time.Second), nil
	})
	body, thread, run := f.post(t, "cost-deny", "cost-deny-run")
	requireLimitSSE(t, body)
	if !strings.Contains(body, `"retryAfterSeconds":12`) || !strings.Contains(body, `"kind":"budget"`) {
		t.Errorf("limit_exceeded custom event lacks kind/retryAfterSeconds: %s", body)
	}
	if got := f.state(t, thread, run); got != string(journal.RunLimitExceeded) {
		t.Fatalf("state = %q, want limit_exceeded", got)
	}
	if n := f.model.Count(); n != 0 {
		t.Errorf("model requests = %d, want 0", n)
	}
	if n := len(f.endpoint.Requests()); n != 0 {
		t.Errorf("MCP requests = %d, want 0", n)
	}
	if got := len(f.gate.Admits()); got != 1 {
		t.Errorf("Admit calls = %d, want 1", got)
	}
	if got := len(f.gate.Accesses()); got != 0 {
		t.Errorf("Access calls after a denial = %d, want 0", got)
	}
	if rows := e2eUsageRows(t, f.pool, thread, run); len(rows) != 0 {
		t.Errorf("usage rows = %d, want 0", len(rows))
	}
	// A reconnect projects the same terminal state from the snapshot.
	again := e2eReadBody(t, "reconnect", acceptanceReconnect(t, f.url, "cost-deny", "cost-deny-run"))
	requireLimitSSE(t, again)
}

func TestCostModelLimitExceeded(t *testing.T) {
	f := newCostFixture(t, modeltest.Fail(&cost.LimitError{Kind: cost.LimitRate, RetryAfter: 5 * time.Second}), modeltest.Text("never"))
	body, thread, run := f.post(t, "cost-model", "cost-model-run")
	requireLimitSSE(t, body)
	if !strings.Contains(body, `"retryAfterSeconds":5`) || !strings.Contains(body, `"kind":"rate"`) {
		t.Errorf("limit_exceeded custom event lacks kind/retryAfterSeconds: %s", body)
	}
	if got := f.state(t, thread, run); got != string(journal.RunLimitExceeded) {
		t.Fatalf("state = %q, want limit_exceeded", got)
	}
	if n := f.model.Count(); n != 1 {
		t.Errorf("model requests = %d, want exactly 1 (no retry)", n)
	}
	var reason, kind, retry string
	if err := f.pool.QueryRow(t.Context(), `SELECT reason, detail->>'kind', detail->>'retry_after_seconds' FROM agent_events WHERE thread_id=$1 AND run_id=$2 AND kind='run.limit_exceeded'`, thread, run).Scan(&reason, &kind, &retry); err != nil {
		t.Fatalf("no run.limit_exceeded event: %v", err)
	}
	if reason != "rate" || kind != "rate" || retry != "5" {
		t.Errorf("event reason/kind/retry = %q/%q/%q, want rate/rate/5", reason, kind, retry)
	}
	if n := e2eOutcomeUnknown(t, f.pool, thread, run); n != 0 {
		t.Errorf("outcome_unknown rows = %d, want 0", n)
	}
	if rows := e2eUsageRows(t, f.pool, thread, run); len(rows) != 0 {
		t.Errorf("usage rows for a failed call = %d, want 0", len(rows))
	}
}

func TestCostUsageRecordedPerModelCall(t *testing.T) {
	first := modeltest.Usage{InputTokens: 11, OutputTokens: 3, CachedTokens: 2}
	second := modeltest.Usage{InputTokens: 21, OutputTokens: 5, CachedTokens: 7}
	f := newCostFixture(t,
		modeltest.Call("travel_lookup", json.RawMessage(`{"destination":"Kyoto"}`)).WithUsage(first),
		modeltest.Text("Kyoto is available for your trip").WithUsage(second),
	)
	body, thread, run := f.post(t, "cost-usage", "cost-usage-run")
	if !strings.Contains(body, "RUN_FINISHED") {
		t.Fatalf("run did not finish: %s", body)
	}
	if n := f.model.Count(); n != 2 {
		t.Fatalf("model requests = %d, want 2", n)
	}
	rows := e2eUsageRows(t, f.pool, thread, run)
	want := []modeltest.Usage{first, second}
	if len(rows) != len(want) {
		t.Fatalf("usage rows = %+v, want %d", rows, len(want))
	}
	for i, r := range rows {
		if r.seq != i+1 || r.input != want[i].InputTokens || r.output != want[i].OutputTokens || r.cached != want[i].CachedTokens || r.tenant != testTenant || r.principal != testPrincipal {
			t.Errorf("row %d = %+v, want seq %d tokens %+v", i, r, i+1, want[i])
		}
	}
	// Attribution is built once per run and Access once per Admit.
	if len(f.gate.Admits()) != 1 || len(f.gate.Accesses()) != 1 {
		t.Errorf("admits/accesses = %d/%d, want 1/1", len(f.gate.Admits()), len(f.gate.Accesses()))
	}
	a := f.gate.Admits()[0]
	if a.Tenant != testTenant || a.Principal != testPrincipal || a.Conversation != thread || a.Run != run || a.Journey != "planner" {
		t.Errorf("attribution = %+v", a)
	}
	if f.model.Requests()[0].Access != (cost.Access{}) {
		// Open inner gate decorates nothing; the zero Access is what the model sees.
		t.Errorf("Open gate produced a non-zero Access")
	}
}

// usageJourneys returns agent_usage.journey_id (NULL as nil) for every row of a run.
func (f *costFixture) usageJourneys(t *testing.T, thread, run string) []*string {
	t.Helper()
	rows, err := f.pool.Query(t.Context(), `SELECT journey_id FROM agent_usage WHERE thread_id=$1 AND run_id=$2 ORDER BY seq`, thread, run)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []*string
	for rows.Next() {
		var j *string
		if err := rows.Scan(&j); err != nil {
			t.Fatal(err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCostAttributionRejectsUnknownJourney(t *testing.T) {
	const bogus = "evil-journey-x"
	f := newCostFixture(t, modeltest.Text("never"))
	body, thread, run := f.postJourney(t, "cost-bogus", "cost-bogus-run", bogus)
	if strings.Contains(body, "RUN_FINISHED") {
		t.Fatalf("run with an unknown journey finished: %s", body)
	}
	if got := f.state(t, thread, run); got != string(journal.RunFailed) {
		t.Fatalf("state = %q, want failed", got)
	}
	if got := len(f.gate.Admits()); got != 0 {
		t.Errorf("Admit calls = %d, want 0 (attribution must be validated first)", got)
	}
	if got := len(f.gate.Accesses()); got != 0 {
		t.Errorf("Access calls = %d, want 0", got)
	}
	if n := f.model.Count(); n != 0 {
		t.Errorf("model requests = %d, want 0", n)
	}
	if n := len(f.endpoint.Requests()); n != 0 {
		t.Errorf("MCP requests = %d, want 0", n)
	}
	var n int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_usage WHERE journey_id=$1`, bogus).Scan(&n); err != nil || n != 0 {
		t.Errorf("usage rows carrying %q = %d (err %v), want 0", bogus, n, err)
	}
}

func TestCostAttributionValidTarget(t *testing.T) {
	f := newCostFixture(t, modeltest.Text("Kyoto is available").WithUsage(modeltest.Usage{InputTokens: 4, OutputTokens: 2}))
	body, thread, run := f.postJourney(t, "cost-valid", "cost-valid-run", "planner")
	if !strings.Contains(body, "RUN_FINISHED") {
		t.Fatalf("run did not finish: %s", body)
	}
	admits := f.gate.Admits()
	if len(admits) != 1 || admits[0].Journey != "planner" {
		t.Fatalf("admits = %+v, want exactly one with Journey planner", admits)
	}
	js := f.usageJourneys(t, thread, run)
	if len(js) == 0 {
		t.Fatal("no usage rows")
	}
	for i, j := range js {
		if j == nil || *j != "planner" {
			t.Errorf("usage row %d journey_id = %v, want planner", i, j)
		}
	}
}

func TestCostAttributionUntargeted(t *testing.T) {
	f := newCostFixture(t, modeltest.Text("Kyoto is available").WithUsage(modeltest.Usage{InputTokens: 4, OutputTokens: 2}))
	body, thread, run := f.postJourney(t, "cost-untargeted", "cost-untargeted-run", "")
	if !strings.Contains(body, "RUN_FINISHED") {
		t.Fatalf("run did not finish: %s", body)
	}
	admits := f.gate.Admits()
	if len(admits) != 1 || admits[0].Journey != "" {
		t.Fatalf("admits = %+v, want exactly one with empty Journey", admits)
	}
	js := f.usageJourneys(t, thread, run)
	if len(js) == 0 {
		t.Fatal("no usage rows")
	}
	for i, j := range js {
		if j != nil {
			t.Errorf("usage row %d journey_id = %q, want NULL", i, *j)
		}
	}
}
