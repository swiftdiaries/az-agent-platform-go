package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/mcptest"
	"github.com/swiftdiaries/az-agent-platform-go/internal/modeltest"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
	"github.com/swiftdiaries/az-agent-platform-go/internal/tenantdir"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// stage gates subtest k on the e2eReachedStage progress ratchet.
func stage(t *testing.T, k int, hop string) {
	t.Helper()
	if k > e2eReachedStage {
		t.Skip("pending: " + hop)
	}
}

// e2eReadBody reads a whole SSE response body and requires status 200.
func e2eReadBody(t *testing.T, name string, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s: reading body: %v", name, err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s: status = %d, body: %s", name, response.StatusCode, body)
	}
	return string(body)
}

// e2eReconnect is acceptanceReconnect for an arbitrary bearer token.
func e2eReconnect(t *testing.T, endpoint, thread, run, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, endpoint+"?threadId="+thread+"&runId="+run, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestE2EConversationAgent(t *testing.T) {
	t.Run("S0", func(t *testing.T) {
		stage(t, 0, "H0")
		const (
			thread = "e2e-s0"
			run    = "e2e-s0-run"
			answer = "Kyoto is available for your trip"
		)
		pool := database(t)
		endpoint := mcptest.New(t, mcptest.Options{Name: "e2e", Version: "vN"}, mcptest.Catalog{Tools: []mcptest.Tool{
			{Name: "lookup", OperationKey: "travel.lookup", Result: "Kyoto is available"},
		}})
		registry := acceptanceRegistry(t, "read_only")
		model := modeltest.New(
			modeltest.Call("lookup", map[string]any{"destination": "Kyoto"}),
			modeltest.Text(answer),
		)
		a, b, c := threeReplicas(t, pool, registry, model, testTenancy(t, endpoint.URL()))

		body := e2eReadBody(t, "POST on A", acceptancePost(t, a.url, aguitypes.RunAgentInput{
			ThreadID: thread, RunID: run,
			Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
		}))
		if !strings.Contains(body, answer) || !strings.Contains(body, "RUN_FINISHED") {
			t.Fatalf("POST on A lacks final answer or RUN_FINISHED: %s", body)
		}

		for name, replica := range map[string]acceptanceReplica{"B": b, "C": c} {
			replay := e2eReadBody(t, "reconnect on "+name, acceptanceReconnect(t, replica.url, thread, run))
			if !strings.Contains(replay, answer) || !strings.Contains(replay, "RUN_FINISHED") {
				t.Fatalf("reconnect on %s lacks final answer or RUN_FINISHED: %s", name, replay)
			}
		}

		if got := endpoint.Calls("lookup"); got != 1 {
			t.Fatalf("MCP lookup calls = %d, want 1", got)
		}
		if got := endpoint.Applied("lookup"); got != 1 {
			t.Fatalf("MCP lookup applied = %d, want 1", got)
		}
		if got := endpoint.Count("tools/call"); got != 1 {
			t.Fatalf("MCP tools/call count = %d, want 1", got)
		}
		var toolCall *mcptest.Request
		requests := endpoint.Requests()
		for i := range requests {
			if requests[i].RPCMethod == "tools/call" {
				toolCall = &requests[i]
				break
			}
		}
		if toolCall == nil {
			t.Fatal("no tools/call request recorded by the MCP endpoint")
		}
		if toolCall.Tool != "lookup" {
			t.Fatalf("tools/call tool = %q, want lookup", toolCall.Tool)
		}
		if toolCall.Header.Get("Authorization") != "Bearer token" {
			t.Fatal("tools/call did not carry the forwarded Authorization credential")
		}

		if got := model.Count(); got != 2 {
			t.Fatalf("model requests = %d, want 2", got)
		}
		requestsSeen := model.Requests()
		offered := false
		for _, tool := range requestsSeen[0].Tools {
			if tool.Name == "lookup" {
				offered = true
			}
		}
		if !offered {
			t.Fatalf("model request 0 did not offer tool lookup: %#v", requestsSeen[0].Tools)
		}
		// Current code does not populate ToolResult.Name (runtime/run.go:199 sets
		// only CallID and Value), so tie the single result to the lookup call by
		// its CallID, which modeltest derives from the request index. Later hops
		// that populate Name should tighten this to Name == "lookup".
		if results := requestsSeen[1].ToolResults; len(results) != 1 || results[0].CallID != "modeltest-call-0" || results[0].Value == nil {
			t.Fatalf("model request 1 tool results = %#v, want exactly one result for call modeltest-call-0", results)
		}

		threadID := acceptanceThread(t, pool, thread)
		snapshot, err := journal.New(pool).Snapshot(t.Context(), threadID, testPrincipal)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Runs) != 1 || snapshot.Runs[0].State != journal.RunCompleted {
			t.Fatalf("durable runs = %#v, want one completed run", snapshot.Runs)
		}
	})
	t.Run("S1", func(t *testing.T) {
		stage(t, 1, "H1")
		// A sentinel credential never appears in any DB text/jsonb column, events, slog output or spans, while MCP still receives it.
		const (
			thread        = "e2e-s1"
			run           = "e2e-s1-run"
			answer        = "Kyoto is available for your trip"
			s1Sentinel    = "azap-s1-sentinel-9c41e7"
			s1DecoyValue  = "azap-s1-decoy-3b82d5"
			decoyHeader   = "X-Azap-Decoy"
			sessionCookie = "azap_session="
		)
		needles := []string{s1Sentinel, s1DecoyValue}

		// Capture before any replica starts so nothing is missed.
		slogJSON, slogText := captureSlog(t)
		recorder := captureSpans(t)

		pool := database(t)
		endpoint := mcptest.New(t, mcptest.Options{Name: "e2e", Version: "vN"}, mcptest.Catalog{Tools: []mcptest.Tool{
			{Name: "lookup", OperationKey: "travel.lookup", Result: "Kyoto is available"},
		}})
		registry := acceptanceRegistry(t, "read_only")
		model := modeltest.New(
			modeltest.Call("lookup", map[string]any{"destination": "Kyoto"}),
			modeltest.Text(answer),
		)
		a, b, c := threeReplicas(t, pool, registry, model, testTenancy(t, endpoint.URL()))

		// collected is every platform-side response surface that must stay clean.
		collected := map[string]string{}
		collectHeaders := func(name string, response *http.Response) {
			var sb strings.Builder
			for key, values := range response.Header {
				fmt.Fprintf(&sb, "%s: %s\n", key, strings.Join(values, ", "))
			}
			collected[name+" response headers"] = sb.String()
		}

		postResponse := acceptancePostWithHeaders(t, a.url, aguitypes.RunAgentInput{
			ThreadID: thread, RunID: run,
			Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
		}, http.Header{
			"Cookie":    {sessionCookie + s1Sentinel},
			decoyHeader: {s1DecoyValue},
		})
		collectHeaders("POST on A", postResponse)
		body := e2eReadBody(t, "POST on A", postResponse)
		if !strings.Contains(body, answer) || !strings.Contains(body, "RUN_FINISHED") {
			t.Fatalf("POST on A lacks final answer or RUN_FINISHED: %s", body)
		}
		collected["POST on A body"] = body

		for name, replica := range map[string]acceptanceReplica{"B": b, "C": c} {
			response := acceptanceReconnect(t, replica.url, thread, run)
			collectHeaders("reconnect on "+name, response)
			replay := e2eReadBody(t, "reconnect on "+name, response)
			if !strings.Contains(replay, answer) || !strings.Contains(replay, "RUN_FINISHED") {
				t.Fatalf("reconnect on %s lacks final answer or RUN_FINISHED: %s", name, replay)
			}
			collected["reconnect on "+name+" body"] = replay
		}

		snapshot, err := journal.New(pool).Snapshot(t.Context(), acceptanceThread(t, pool, thread), testPrincipal)
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Runs) != 1 || snapshot.Runs[0].State != journal.RunCompleted {
			t.Fatalf("durable runs = %#v, want one completed run", snapshot.Runs)
		}

		// MCP still receives the credential: every JSON-RPC request carries the
		// Cookie, exactly one tools/call for lookup, and the decoy never arrives.
		lookups, rpcRequests := 0, 0
		for _, request := range endpoint.Requests() {
			if got := request.Header.Get(decoyHeader); got != "" || strings.Contains(fmt.Sprint(request.Header), s1DecoyValue) {
				t.Fatalf("MCP %s request carried the non-configured decoy header: %v", request.RPCMethod, request.Header)
			}
			if request.RPCMethod == "" {
				continue
			}
			rpcRequests++
			if !strings.Contains(request.Header.Get("Cookie"), s1Sentinel) {
				t.Errorf("MCP %s request did not carry the Cookie credential; Cookie = %q", request.RPCMethod, request.Header.Get("Cookie"))
			}
			if request.RPCMethod == "tools/call" && request.Tool == "lookup" {
				lookups++
			}
		}
		if rpcRequests == 0 {
			t.Fatal("MCP endpoint recorded no JSON-RPC requests")
		}
		if lookups != 1 {
			t.Fatalf("MCP tools/call lookup requests = %d, want 1", lookups)
		}

		// The credential must never reach the model.
		if got := model.Count(); got != 2 {
			t.Fatalf("model requests = %d, want 2", got)
		}
		for i, request := range model.Requests() {
			collected[fmt.Sprintf("model request %d (%%+v)", i)] = fmt.Sprintf("%+v", request)
			request.OnDispatch = nil
			if encoded, err := json.Marshal(request); err == nil {
				collected[fmt.Sprintf("model request %d (json)", i)] = string(encoded)
			}
		}

		// Leak scans, after the run completed and the reconnects returned.
		for name, text := range collected {
			for _, needle := range needles {
				if strings.Contains(text, needle) {
					t.Errorf("%q leaked into %s", needle, name)
				}
			}
		}
		for name, text := range map[string]string{"slog JSON": slogJSON.String(), "slog text": slogText.String()} {
			if text == "" {
				t.Errorf("%s capture is empty", name)
			}
			for _, needle := range needles {
				if strings.Contains(text, needle) {
					t.Errorf("%q leaked into %s output", needle, name)
				}
			}
		}
		waitForEndedSpans(t, recorder, "chat.accept", "mcp.call")
		if hits := sentinelSpanHits(recorder.Ended(), needles...); len(hits) > 0 {
			t.Errorf("sentinel leaked into spans: %v", hits)
		}
		requireNoSentinelInDB(t, pool, needles...)

		// Positive controls: the scanners do detect a planted sentinel.
		if _, err := pool.Exec(t.Context(), `CREATE TABLE s1_scan_probe (v jsonb)`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO s1_scan_probe (v) VALUES ($1::jsonb)`, `{"note":"`+s1Sentinel+`"}`); err != nil {
			t.Fatal(err)
		}
		if hits, _ := sentinelDBHits(t, pool, s1Sentinel); !slices.Contains(hits, "s1_scan_probe.v") {
			t.Fatalf("DB scan control did not find the planted sentinel; hits = %v", hits)
		}
		probeRecorder := tracetest.NewSpanRecorder()
		probeProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(probeRecorder))
		_, probeSpan := probeProvider.Tracer("s1-control").Start(t.Context(), "probe", trace.WithAttributes(attribute.String("k", s1Sentinel)))
		probeSpan.End()
		if hits := sentinelSpanHits(probeRecorder.Ended(), s1Sentinel); len(hits) != 1 {
			t.Fatalf("span scan control hits = %v, want 1", hits)
		}
	})
	t.Run("S2", func(t *testing.T) {
		stage(t, 2, "H2")
		// Tenant A routes to vN and tenant B to vN+1; a non-allowlisted directory endpoint yields dependency_unavailable with zero requests.
		const (
			answerA = "Kyoto via vN is available"
			answerB = "Kyoto via vN+1 is available"
		)
		pool := database(t)
		newEndpoint := func(version, result string) *mcptest.Server {
			return mcptest.New(t, mcptest.Options{Name: "e2e-" + version, Version: version}, mcptest.Catalog{Tools: []mcptest.Tool{
				{Name: "lookup", OperationKey: "travel.lookup", Result: result},
			}})
		}
		vN := newEndpoint("vN", "Kyoto via vN")
		vN1 := newEndpoint("vN+1", "Kyoto via vN+1")
		rogue := newEndpoint("rogue", "Kyoto via rogue")

		// rogue is named by tenant C's directory entry but is on no allowlist.
		allow, err := tenantdir.NewAllowlist(map[string]string{"java-vn": vN.URL(), "java-vn1": vN1.URL()})
		if err != nil {
			t.Fatal(err)
		}
		source, err := settings.NewMutable(settings.Document{})
		if err != nil {
			t.Fatal(err)
		}
		tenancy := agentruntime.Tenancy{
			Directory: tenantdir.NewMutable(map[string]tenantdir.Entry{
				"tenant-a": {ProductVersion: "vN", EndpointID: "java-vn"},
				"tenant-b": {ProductVersion: "vN+1", EndpointID: "java-vn1"},
				"tenant-c": {ProductVersion: "vN+2", EndpointID: "java-rogue"},
			}),
			Endpoints: allow,
			Settings:  source,
		}

		// Two completed runs (A then B); tenant C must never reach the model.
		model := modeltest.New(
			modeltest.Call("lookup", map[string]any{"destination": "Kyoto"}),
			modeltest.Text(answerA),
			modeltest.Call("lookup", map[string]any{"destination": "Kyoto"}),
			modeltest.Text(answerB),
		)
		a, b, c := threeReplicas(t, pool, acceptanceRegistry(t, "read_only"), model, tenancy)

		type tenantRun struct {
			name, token, principal, thread, run, answer string
			post                                        acceptanceReplica
			reconnects                                  map[string]acceptanceReplica
		}
		runs := []tenantRun{
			{name: "A", token: "token", principal: "tenant-a/alice", thread: "e2e-s2-a", run: "e2e-s2-a-run", answer: answerA,
				post: a, reconnects: map[string]acceptanceReplica{"B": b, "C": c}},
			{name: "B", token: "token-b", principal: "tenant-b/bob", thread: "e2e-s2-b", run: "e2e-s2-b-run", answer: answerB,
				post: b, reconnects: map[string]acceptanceReplica{"A": a, "C": c}},
		}
		// Sequential, so the scripted model indices are deterministic.
		for _, r := range runs {
			body := e2eReadBody(t, "tenant "+r.name+" POST", acceptancePostWithHeaders(t, r.post.url, aguitypes.RunAgentInput{
				ThreadID: r.thread, RunID: r.run,
				Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
			}, http.Header{"Authorization": {"Bearer " + r.token}}))
			if !strings.Contains(body, r.answer) || !strings.Contains(body, "RUN_FINISHED") {
				t.Fatalf("tenant %s POST lacks final answer or RUN_FINISHED: %s", r.name, body)
			}
			for name, replica := range r.reconnects {
				replay := e2eReadBody(t, "tenant "+r.name+" reconnect on "+name, e2eReconnect(t, replica.url, r.thread, r.run, r.token))
				if !strings.Contains(replay, r.answer) || !strings.Contains(replay, "RUN_FINISHED") {
					t.Fatalf("tenant %s reconnect on %s lacks final answer or RUN_FINISHED: %s", r.name, name, replay)
				}
			}
		}

		// Tenant C's directory entry names a non-allowlisted endpoint.
		const threadC, runC = "e2e-s2-c", "e2e-s2-c-run"
		bodyC := e2eReadBody(t, "tenant C POST", acceptancePostWithHeaders(t, c.url, aguitypes.RunAgentInput{
			ThreadID: threadC, RunID: runC,
			Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
		}, http.Header{"Authorization": {"Bearer token-c"}}))
		if !strings.Contains(bodyC, "RUN_ERROR") || !strings.Contains(bodyC, `"code":"dependency_unavailable"`) || strings.Contains(bodyC, "RUN_FINISHED") {
			t.Fatalf("tenant C POST: expected RUN_ERROR dependency_unavailable and no RUN_FINISHED: %s", bodyC)
		}

		// Each endpoint saw exactly one lookup, and only its own tenant's credential.
		for _, check := range []struct {
			name         string
			server       *mcptest.Server
			want, reject string
		}{
			{"vN", vN, "Bearer token", "Bearer token-b"},
			{"vN+1", vN1, "Bearer token-b", "Bearer token"},
		} {
			lookups, rpcRequests := 0, 0
			for _, request := range check.server.Requests() {
				if request.RPCMethod == "" {
					continue
				}
				rpcRequests++
				if got := request.Header.Get("Authorization"); got != check.want {
					t.Errorf("%s %s request Authorization = %q, want %q", check.name, request.RPCMethod, got, check.want)
				}
				if request.Header.Get("Authorization") == check.reject {
					t.Errorf("%s %s request carried the other tenant's credential", check.name, request.RPCMethod)
				}
				if request.RPCMethod == "tools/call" && request.Tool == "lookup" {
					lookups++
				}
			}
			if rpcRequests == 0 {
				t.Errorf("%s recorded no JSON-RPC requests", check.name)
			}
			if lookups != 1 || check.server.Calls("lookup") != 1 {
				t.Errorf("%s lookups = %d (Calls = %d), want exactly 1", check.name, lookups, check.server.Calls("lookup"))
			}
		}

		// Durable scope.
		store := journal.New(pool)
		for _, want := range []struct {
			r                                tenantRun
			version, endpoint, serverVersion string
		}{
			{runs[0], "vN", "java-vn", "vN"},
			{runs[1], "vN+1", "java-vn1", "vN+1"},
		} {
			snapshot, err := store.Snapshot(t.Context(), acceptanceThreadFor(t, pool, want.r.principal, want.r.thread), want.r.principal)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Runs) != 1 {
				t.Fatalf("tenant %s durable runs = %#v, want one", want.r.name, snapshot.Runs)
			}
			run := snapshot.Runs[0]
			if run.State != journal.RunCompleted || run.ProductVersion != want.version || run.EndpointID != want.endpoint {
				t.Errorf("tenant %s run = state %q scope %q/%q, want completed %q/%q", want.r.name, run.State, run.ProductVersion, run.EndpointID, want.version, want.endpoint)
			}
			var info struct{ Name, Version string }
			if err := json.Unmarshal(run.ServerInfo, &info); err != nil || info.Version != want.serverVersion {
				t.Errorf("tenant %s server info = %s (%v), want version %q", want.r.name, run.ServerInfo, err, want.serverVersion)
			}
			var recorded struct {
				MaxActiveJourneys int    `json:"max_active_journeys"`
				Digest            string `json:"digest"`
			}
			if err := json.Unmarshal(run.Settings, &recorded); err != nil || recorded.MaxActiveJourneys != 2 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(recorded.Digest) {
				t.Errorf("tenant %s settings = %s (%v), want max_active_journeys 2 and a 64-hex digest", want.r.name, run.Settings, err)
			}
		}
		snapshotC, err := store.Snapshot(t.Context(), acceptanceThreadFor(t, pool, "tenant-c/carol", threadC), "tenant-c/carol")
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshotC.Runs) != 1 {
			t.Fatalf("tenant C durable runs = %#v, want one", snapshotC.Runs)
		}
		if run := snapshotC.Runs[0]; run.State != journal.RunDependencyUnavailable || run.ProductVersion != "" || run.EndpointID != "" {
			t.Errorf("tenant C run = state %q scope %q/%q, want dependency_unavailable with no scope", run.State, run.ProductVersion, run.EndpointID)
		}

		// The non-allowlisted endpoint received nothing, and the model never saw tenant C.
		if got := len(rogue.Requests()); got != 0 {
			t.Errorf("rogue received %d requests, want 0: %+v", got, rogue.Requests())
		}
		for _, method := range []string{"initialize", "tools/list", "tools/call"} {
			if got := rogue.Count(method); got != 0 {
				t.Errorf("rogue %s count = %d, want 0", method, got)
			}
		}
		if got := model.Count(); got != 4 {
			t.Errorf("model requests = %d, want 4 (tenant C must not reach the model)", got)
		}
	})
	t.Run("S3", func(t *testing.T) {
		stage(t, 3, "H3")
		// A paginated catalog snapshot gives both tenants the same aliases while MCP receives each tenant's own remote names; a second run revalidates without tools/list.
	})
	t.Run("S4", func(t *testing.T) {
		stage(t, 4, "H4b")
		// After activate_journey only that journey's aliases are offered; per-tenant and per-user limits evict LRU journeys, pinned ones stay, and the active set survives restarts.
	})
	t.Run("S5", func(t *testing.T) {
		stage(t, 5, "H4b")
		// A correction admitted via replica B while A's tool is blocked reaches every later provider request, and a reconnect on C is gap-free.
	})
	t.Run("S6", func(t *testing.T) {
		stage(t, 6, "H5")
		// Approval wait, tenant moved to vN+1, approve: superseded plus a new interaction, then dispatched exactly once on vN+1; clarification replies survive the flip.
	})
	t.Run("S7", func(t *testing.T) {
		stage(t, 7, "H6")
		// The real litellm provider against a scripted Responses stub gets attribution headers without credentials, maps 429 budget_exceeded and Admit denials to limit_exceeded, and agent_usage matches reported tokens.
	})
	t.Run("S8", func(t *testing.T) {
		stage(t, 8, "H7")
		// An apply-then-drop-response effect yields outcome_unknown with no retry and a pinned journey, and the trace carries no content, tool descriptions or remote names.
	})
}
