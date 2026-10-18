package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/cost"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/mcptest"
	"github.com/swiftdiaries/az-agent-platform-go/internal/modeltest"
	"github.com/swiftdiaries/az-agent-platform-go/internal/provider/litellm"
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
		registry := acceptanceSet(t, "read_only")
		model := modeltest.New(
			modeltest.Call("travel_lookup", map[string]any{"destination": "Kyoto"}),
			modeltest.Text(answer),
		)
		a, b, c := threeReplicas(t, pool, registry, model, testTenancy(t, endpoint.URL()))

		body := e2eReadBody(t, "POST on A", acceptancePost(t, a.url, aguitypes.RunAgentInput{
			ThreadID: thread, RunID: run,
			Messages:       []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
			ForwardedProps: map[string]any{"journeyId": "planner"},
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
			if tool.Name == "travel_lookup" {
				offered = true
			}
		}
		if !offered {
			t.Fatalf("model request 0 did not offer tool travel_lookup: %#v", requestsSeen[0].Tools)
		}
		// The model sees the alias travel_lookup (MCP received the remote name lookup,
		// asserted above); the single result is tied to that call by CallID, which
		// modeltest derives from the request index, and by its alias Name.
		if results := requestsSeen[1].ToolResults; len(results) != 1 || results[0].CallID != "modeltest-call-0" || results[0].Name != "travel_lookup" || results[0].Value == nil {
			t.Fatalf("model request 1 tool results = %#v, want exactly one result for travel_lookup call modeltest-call-0", results)
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
		registry := acceptanceSet(t, "read_only")
		model := modeltest.New(
			modeltest.Call("travel_lookup", map[string]any{"destination": "Kyoto"}),
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
			Messages:       []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
			ForwardedProps: map[string]any{"journeyId": "planner"},
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
			modeltest.Call("travel_lookup", map[string]any{"destination": "Kyoto"}),
			modeltest.Text(answerA),
			modeltest.Call("travel_lookup", map[string]any{"destination": "Kyoto"}),
			modeltest.Text(answerB),
		)
		a, b, c := threeReplicas(t, pool, acceptanceSet(t, "read_only"), model, tenancy)

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
				Messages:       []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
				ForwardedProps: map[string]any{"journeyId": "planner"},
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
			Messages:       []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
			ForwardedProps: map[string]any{"journeyId": "planner"},
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
		const (
			remoteA = "travel/lookup"
			remoteB = "travel lookup"
			alias   = "travel_lookup"
		)
		pool := database(t)
		// Five tools per tenant at page size 2 is three tools/list pages. Only the
		// target is selected by the journey; the others differ in op key, tags and
		// annotations, and one has no operation key at all.
		catalogFor := func(target, suffix, result string) mcptest.Catalog {
			readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
			destructive := false
			return mcptest.Catalog{Tools: []mcptest.Tool{
				{Name: target, OperationKey: "travel.lookup", Tags: []string{"travel"}, Annotations: readOnly, Result: result},
				{Name: "hotel" + suffix + "book", OperationKey: "hotel.book", Tags: []string{"hotel"}, Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive}},
				{Name: "leave" + suffix + "request", OperationKey: "leave.request", Tags: []string{"leave", "hr"}},
				{Name: "scratch notes" + suffix + "b"},
				{Name: "visa" + suffix + "check", OperationKey: "visa.check", Tags: []string{"visa", "legacy"}, Annotations: readOnly},
			}}
		}
		vN := mcptest.New(t, mcptest.Options{Name: "e2e-vN", Version: "vN", PageSize: 2}, catalogFor(remoteA, "/", "Kyoto via vN"))
		vN1 := mcptest.New(t, mcptest.Options{Name: "e2e-vN1", Version: "vN+1", PageSize: 2}, catalogFor(remoteB, " ", "Kyoto via vN+1"))
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
			}),
			Endpoints: allow,
			Settings:  source,
		}
		// Four sequential runs (A1, B1, A2, B2), each one tool call then an answer.
		model := modeltest.New(
			modeltest.Call(alias, map[string]any{"destination": "Kyoto"}), modeltest.Text("A1 done"),
			modeltest.Call(alias, map[string]any{"destination": "Kyoto"}), modeltest.Text("B1 done"),
			modeltest.Call(alias, map[string]any{"destination": "Kyoto"}), modeltest.Text("A2 done"),
			modeltest.Call(alias, map[string]any{"destination": "Kyoto"}), modeltest.Text("B2 done"),
		)
		a, b, c := threeReplicas(t, pool, acceptanceSet(t, "read_only"), model, tenancy)

		type tenant struct {
			name, token, principal, thread, remote, other string
			server                                        *mcptest.Server
			serverVersion, version, endpoint              string
		}
		tenants := []tenant{
			{name: "A", token: "token", principal: "tenant-a/alice", thread: "e2e-s3-a", remote: remoteA, other: remoteB, server: vN, serverVersion: "vN", version: "vN", endpoint: "java-vn"},
			{name: "B", token: "token-b", principal: "tenant-b/bob", thread: "e2e-s3-b", remote: remoteB, other: remoteA, server: vN1, serverVersion: "vN+1", version: "vN+1", endpoint: "java-vn1"},
		}
		post := func(tn tenant, replica acceptanceReplica, run, answer string) {
			t.Helper()
			body := e2eReadBody(t, "tenant "+tn.name+" "+run, acceptancePostWithHeaders(t, replica.url, aguitypes.RunAgentInput{
				ThreadID: tn.thread, RunID: run,
				Messages:       []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
				ForwardedProps: map[string]any{"journeyId": "planner"},
			}, http.Header{"Authorization": {"Bearer " + tn.token}}))
			if !strings.Contains(body, answer) || !strings.Contains(body, "RUN_FINISHED") {
				t.Fatalf("tenant %s %s lacks final answer or RUN_FINISHED: %s", tn.name, run, body)
			}
		}
		toolCalls := func(tn tenant) []mcptest.Request {
			var calls []mcptest.Request
			for _, request := range tn.server.Requests() {
				if request.RPCMethod == "tools/call" {
					calls = append(calls, request)
				}
			}
			return calls
		}

		// Run 1: the first message on a new thread captures the paginated snapshot.
		post(tenants[0], a, "e2e-s3-a-run1", "A1 done")
		post(tenants[1], b, "e2e-s3-b-run1", "B1 done")

		initialized := map[string]int{}
		for _, tn := range tenants {
			if got := tn.server.Count("tools/list"); got != 3 {
				t.Errorf("tenant %s tools/list count after run 1 = %d, want 3 (5 tools, page size 2)", tn.name, got)
			}
			if got := tn.server.Count("initialize"); got < 1 {
				t.Errorf("tenant %s initialize count after run 1 = %d, want >= 1", tn.name, got)
			}
			initialized[tn.name] = tn.server.Count("initialize")
			calls := toolCalls(tn)
			if len(calls) != 1 || calls[0].Tool != tn.remote || calls[0].Header.Get("Authorization") != "Bearer "+tn.token {
				t.Errorf("tenant %s tools/call after run 1 = %+v, want exactly one for %q with its own credential", tn.name, calls, tn.remote)
			}
			for _, request := range tn.server.Requests() {
				if request.Tool == tn.other {
					t.Errorf("tenant %s endpoint received the other tenant's remote name %q", tn.name, tn.other)
				}
				if request.Header.Get("Authorization") != "" && request.Header.Get("Authorization") != "Bearer "+tn.token {
					t.Errorf("tenant %s endpoint %s request carried Authorization %q", tn.name, request.RPCMethod, request.Header.Get("Authorization"))
				}
			}
		}

		// The model was offered exactly the alias (plus product tools), identically for both tenants.
		if got := model.Count(); got != 4 {
			t.Fatalf("model requests after run 1 = %d, want 4", got)
		}
		offeredNames := func(request agentruntime.ModelRequest) []string {
			names := make([]string, 0, len(request.Tools))
			for _, tool := range request.Tools {
				names = append(names, tool.Name)
			}
			slices.Sort(names)
			return names
		}
		offeredA, offeredB := offeredNames(model.Requests()[0]), offeredNames(model.Requests()[2])
		if !slices.Contains(offeredA, alias) || !slices.Equal(offeredA, offeredB) {
			t.Errorf("model tool names: tenant A = %v, tenant B = %v, want identical lists containing %q", offeredA, offeredB, alias)
		}
		for i, request := range model.Requests() {
			if got := offeredNames(request); !slices.Equal(got, offeredA) {
				t.Errorf("model request %d tools = %v, want %v", i, got, offeredA)
			}
			dump := fmt.Sprintf("%+v", request)
			for _, leaked := range []string{remoteA, remoteB, "hotel", "leave", "visa", "scratch notes"} {
				if strings.Contains(dump, leaked) {
					t.Errorf("model request %d mentions %q", i, leaked)
				}
			}
		}

		store := journal.New(pool)
		firstDigest := map[string]string{}
		for _, tn := range tenants {
			threadID := acceptanceThreadFor(t, pool, tn.principal, tn.thread)
			snapshot, err := store.Snapshot(t.Context(), threadID, tn.principal)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Runs) != 1 || snapshot.Runs[0].State != journal.RunCompleted {
				t.Fatalf("tenant %s durable runs = %#v, want one completed run", tn.name, snapshot.Runs)
			}
			digest := snapshot.Runs[0].CatalogDigest
			if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
				t.Fatalf("tenant %s run catalog digest = %q, want 64 hex", tn.name, digest)
			}
			firstDigest[tn.name] = digest

			var conversationDigest string
			if err := pool.QueryRow(t.Context(), `SELECT catalog_digest FROM agent_conversations WHERE id=$1`, threadID).Scan(&conversationDigest); err != nil || conversationDigest != digest {
				t.Errorf("tenant %s conversation catalog_digest = %q (%v), want run digest %q", tn.name, conversationDigest, err, digest)
			}

			var (
				rows                                             int
				snapTenant, snapPrincipal, version, endpoint, sv string
				descriptors                                      []byte
			)
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_catalog_snapshots WHERE principal=$1`, tn.principal).Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("tenant %s snapshot rows = %d (%v), want 1", tn.name, rows, err)
			}
			if err := pool.QueryRow(t.Context(),
				`SELECT tenant, principal, product_version, endpoint_id, server_version, descriptors FROM agent_catalog_snapshots WHERE digest=$1`, digest,
			).Scan(&snapTenant, &snapPrincipal, &version, &endpoint, &sv, &descriptors); err != nil {
				t.Fatalf("tenant %s snapshot %s: %v", tn.name, digest, err)
			}
			if snapPrincipal != tn.principal || version != tn.version || endpoint != tn.endpoint || sv != tn.serverVersion {
				t.Errorf("tenant %s snapshot = %q/%q/%q/%q, want %q/%q/%q/%q", tn.name, snapPrincipal, version, endpoint, sv, tn.principal, tn.version, tn.endpoint, tn.serverVersion)
			}
			var tools []struct {
				Name string         `json:"name"`
				Meta map[string]any `json:"_meta"`
			}
			if err := json.Unmarshal(descriptors, &tools); err != nil || len(tools) != 5 {
				t.Fatalf("tenant %s snapshot descriptors = %s (%v), want 5", tn.name, descriptors, err)
			}
			var names []string
			var visaTags any
			var targetKey any
			for _, tool := range tools {
				names = append(names, tool.Name)
				switch {
				case strings.HasPrefix(tool.Name, "visa"):
					visaTags = tool.Meta["tags"]
				case tool.Name == tn.remote:
					targetKey = tool.Meta["operation_key"]
				}
			}
			if !slices.Contains(names, tn.remote) || slices.Contains(names, tn.other) {
				t.Errorf("tenant %s snapshot tool names = %v, want its own remote name %q only", tn.name, names, tn.remote)
			}
			if tags, _ := visaTags.([]any); !slices.Equal(tags, []any{"visa", "legacy"}) {
				t.Errorf("tenant %s snapshot did not preserve a non-selected tool's _meta tags: %v", tn.name, visaTags)
			}
			if targetKey != "travel.lookup" {
				t.Errorf("tenant %s snapshot target _meta operation_key = %v, want travel.lookup", tn.name, targetKey)
			}

			latest, ok, err := store.LatestSnapshot(t.Context(), snapTenant, tn.principal)
			if err != nil || !ok || latest.Digest != digest {
				t.Errorf("tenant %s LatestSnapshot = %q ok=%v err=%v, want %q", tn.name, latest.Digest, ok, err, digest)
			}
		}
		if firstDigest["A"] == firstDigest["B"] {
			t.Errorf("tenants share catalog digest %q, want different digests", firstDigest["A"])
		}
		var snapshotRows int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_catalog_snapshots`).Scan(&snapshotRows); err != nil || snapshotRows != 2 {
			t.Errorf("snapshot rows after run 1 = %d (%v), want 2", snapshotRows, err)
		}

		// Run 2: a second message on the same thread, through a different replica, revalidates without tools/list.
		post(tenants[0], b, "e2e-s3-a-run2", "A2 done")
		post(tenants[1], c, "e2e-s3-b-run2", "B2 done")

		for _, tn := range tenants {
			if got := tn.server.Count("tools/list"); got != 3 {
				t.Errorf("tenant %s tools/list count after run 2 = %d, want unchanged 3", tn.name, got)
			}
			if got := tn.server.Count("initialize"); got <= initialized[tn.name] {
				t.Errorf("tenant %s initialize count after run 2 = %d, want > %d (revalidation handshake)", tn.name, got, initialized[tn.name])
			}
			calls := toolCalls(tn)
			if len(calls) != 2 {
				t.Fatalf("tenant %s tools/call count after run 2 = %d, want 2", tn.name, len(calls))
			}
			for _, call := range calls {
				if call.Tool != tn.remote || call.Header.Get("Authorization") != "Bearer "+tn.token {
					t.Errorf("tenant %s tools/call = %q with %q, want %q with its own credential", tn.name, call.Tool, call.Header.Get("Authorization"), tn.remote)
				}
			}
			if got := tn.server.Calls(tn.remote); got != 2 {
				t.Errorf("tenant %s remote %q calls = %d, want 2", tn.name, tn.remote, got)
			}

			threadID := acceptanceThreadFor(t, pool, tn.principal, tn.thread)
			snapshot, err := store.Snapshot(t.Context(), threadID, tn.principal)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Runs) != 2 {
				t.Fatalf("tenant %s durable runs = %#v, want two", tn.name, snapshot.Runs)
			}
			for _, run := range snapshot.Runs {
				if run.State != journal.RunCompleted || run.CatalogDigest != firstDigest[tn.name] {
					t.Errorf("tenant %s run %s = state %q digest %q, want completed with digest %q", tn.name, run.RunID, run.State, run.CatalogDigest, firstDigest[tn.name])
				}
			}
			var changed int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_events WHERE thread_id=$1 AND kind='catalog_changed'`, threadID).Scan(&changed); err != nil || changed != 0 {
				t.Errorf("tenant %s catalog_changed events = %d (%v), want 0", tn.name, changed, err)
			}
		}
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_catalog_snapshots`).Scan(&snapshotRows); err != nil || snapshotRows != 2 {
			t.Errorf("snapshot rows after run 2 = %d (%v), want 2 (revalidation must not re-capture)", snapshotRows, err)
		}
		if got := model.Count(); got != 8 {
			t.Errorf("model requests = %d, want 8", got)
		}

		// The journal stores the alias, never a remote name.
		var operations, wrongAlias, wrongKey int
		if err := pool.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE alias <> $1 OR alias ~ '[/ ]' OR alias = ANY($2)), count(*) FILTER (WHERE operation_key <> 'travel.lookup') FROM agent_operations`,
			alias, []string{remoteA, remoteB}).Scan(&operations, &wrongAlias, &wrongKey); err != nil {
			t.Fatal(err)
		}
		if operations != 4 || wrongAlias != 0 || wrongKey != 0 {
			t.Errorf("agent_operations: %d rows, %d with a non-alias name, %d with another operation key; want 4, 0, 0", operations, wrongAlias, wrongKey)
		}
		var eventLeaks int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_events WHERE tool_name <> '' AND tool_name <> $1 AND tool_name <> ALL($2)`,
			alias, []string{"request_user_input", "read_skill_resource", "activate_journey"}).Scan(&eventLeaks); err != nil || eventLeaks != 0 {
			t.Errorf("agent_events with a tool_name other than the alias = %d (%v), want 0", eventLeaks, err)
		}
	})
	t.Run("S4", func(t *testing.T) {
		stage(t, 4, "H4b")
		// After activate_journey only that journey's aliases are offered; per-tenant and per-user limits evict LRU journeys, pinned ones stay, and the active set survives restarts.
		const (
			threadA, threadB = "e2e-s4-a", "e2e-s4-b"
			notExecuted      = `{"error":"not_executed","message":"not executed; call again after activation"}`
		)
		pool := database(t)
		set := e2eJourneySet(t)
		vN := mcptest.New(t, mcptest.Options{Name: "e2e-s4-vN", Version: "vN"}, e2eCatalog(func(o e2eOp) string { return o.remoteA }))
		vN1 := mcptest.New(t, mcptest.Options{Name: "e2e-s4-vN1", Version: "vN+1"}, e2eCatalog(func(o e2eOp) string { return o.remoteB }))
		allow, err := tenantdir.NewAllowlist(map[string]string{"java-vn": vN.URL(), "java-vn1": vN1.URL()})
		if err != nil {
			t.Fatal(err)
		}
		// Tenant A keeps the default limit of 2; tenant B is limited to 1.
		one := 1
		settingsDoc := func(alice *int) settings.Document {
			doc := settings.Document{Tenants: map[string]settings.Layer{"tenant-b": {MaxActiveJourneys: &one}}}
			if alice != nil {
				doc.Users = map[string]map[string]settings.Layer{"tenant-a": {testPrincipal: {MaxActiveJourneys: alice}}}
			}
			return doc
		}
		source, err := settings.NewMutable(settingsDoc(nil))
		if err != nil {
			t.Fatal(err)
		}
		tenancy := agentruntime.Tenancy{
			Directory: tenantdir.NewMutable(map[string]tenantdir.Entry{
				"tenant-a": {ProductVersion: "vN", EndpointID: "java-vn"},
				"tenant-b": {ProductVersion: "vN+1", EndpointID: "java-vn1"},
			}),
			Endpoints: allow,
			Settings:  source,
		}

		activate := func(journey string) modeltest.Step {
			return modeltest.Call("activate_journey", map[string]any{"journey": journey})
		}
		// Model script; the comment on each step is its global request index.
		model := modeltest.New(
			// Run A1: activations on a fresh thread (limit 2).
			activate("planner"), // 0
			modeltest.Func(func(_ context.Context, _ agentruntime.ModelRequest) (agentruntime.ModelResponse, error) { // 1: activation plus a sibling call
				return agentruntime.ModelResponse{ToolCalls: []agentruntime.ToolCall{
					{CallID: "s4-act-hotels", Name: "activate_journey", Arguments: json.RawMessage(`{"journey":"hotels"}`)},
					{CallID: "s4-sibling", Name: "travel_lookup", Arguments: json.RawMessage(`{}`)},
				}}, nil
			}),
			modeltest.Func(func(_ context.Context, _ agentruntime.ModelRequest) (agentruntime.ModelResponse, error) { // 2: the sibling was never dispatched
				if got := vN.Calls("travel/lookup"); got != 0 {
					t.Errorf("MCP travel/lookup calls before the model re-issued the sibling = %d, want 0", got)
				}
				return agentruntime.ModelResponse{ToolCalls: []agentruntime.ToolCall{{CallID: "s4-lookup", Name: "travel_lookup", Arguments: json.RawMessage(`{}`)}}}, nil
			}),
			activate("visas"),         // 3: third journey evicts the LRU one (hotels; the dispatched travel_lookup bumped planner)
			modeltest.Text("A1 done"), // 4
			// Run A2: alice's user layer narrows the limit to 1.
			activate("planner"),       // 5
			modeltest.Text("A2 done"), // 6
			// Run A3: build a pending approval for booking, which is also the LRU journey.
			activate("booking"),                        // 7
			activate("planner"),                        // 8: re-activation moves planner to MRU
			modeltest.Call("book_reserve", struct{}{}), // 9: effectful, waits for approval
			modeltest.Text("booked"),                   // 10: run A4 after the approval reply
			// Run B1: tenant B, limit 1.
			activate("planner"),                        // 11
			activate("hotels"),                         // 12: evicts planner
			modeltest.Call("hotel_search", struct{}{}), // 13
			modeltest.Text("B1 done"),                  // 14
			// Run A5: after restarting every replica.
			modeltest.Text("after restart"), // 15
		)
		a, b, c := threeReplicas(t, pool, set, model, tenancy)

		const tokenA, tokenB = "token", "token-b"
		chat := func(replica acceptanceReplica, token, thread, run, text string) string {
			t.Helper()
			body := e2eReadBody(t, run, acceptancePostWithHeaders(t, replica.url, aguitypes.RunAgentInput{
				ThreadID: thread, RunID: run,
				Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: text}},
			}, http.Header{"Authorization": {"Bearer " + token}}))
			if !strings.Contains(body, "RUN_FINISHED") {
				t.Fatalf("%s lacks RUN_FINISHED: %s", run, body)
			}
			return body
		}
		activeOf := func(principal, external string) []string {
			t.Helper()
			var raw []byte
			if err := pool.QueryRow(t.Context(), `SELECT active_journeys FROM agent_conversations WHERE id=$1`, acceptanceThreadFor(t, pool, principal, external)).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var ids []string
			if err := json.Unmarshal(raw, &ids); err != nil {
				t.Fatal(err)
			}
			return ids
		}
		requireActive := func(label, principal, external string, want ...string) {
			t.Helper()
			if got := activeOf(principal, external); !slices.Equal(got, want) {
				t.Errorf("%s: agent_conversations.active_journeys = %v, want %v", label, got, want)
			}
		}
		// expect checks one recorded request: its journey aliases, the active set
		// (LRU first) and that exactly the active journeys' instructions are present.
		expect := func(index int, aliases []string, active ...string) {
			t.Helper()
			request := model.Requests()[index]
			if got := e2eJourneyAliases(request); !slices.Equal(got, aliases) {
				t.Errorf("request %d journey aliases = %v, want %v", index, got, aliases)
			}
			if !slices.Equal(request.ActiveJourneys, active) && (len(request.ActiveJourneys) != 0 || len(active) != 0) {
				t.Errorf("request %d ActiveJourneys = %v, want %v", index, request.ActiveJourneys, active)
			}
			if !strings.Contains(request.Instructions, "## Available journeys") {
				t.Errorf("request %d instructions lack the available journeys section", index)
			}
			for _, id := range []string{"planner", "hotels", "visas", "booking"} {
				if got, want := strings.Contains(request.Instructions, "## Active journey: "+id+"\n"), slices.Contains(active, id); got != want {
					t.Errorf("request %d instructions contain active section for %s = %v, want %v", index, id, got, want)
				}
			}
		}
		result := func(index int, callID, want string) {
			t.Helper()
			if got := e2eResult(t, model.Requests()[index], callID); got != want {
				t.Errorf("request %d result for %s = %s, want %s", index, callID, got, want)
			}
		}

		// Run A1 (limit 2).
		chat(a, tokenA, threadA, "e2e-s4-a1", "start")
		if got := model.Count(); got != 5 {
			t.Fatalf("model requests after A1 = %d, want 5", got)
		}
		expect(0, nil)
		expect(1, []string{"travel_lookup"}, "planner")
		result(1, "modeltest-call-0", `{"activated":"planner"}`)
		expect(2, []string{"hotel_search", "travel_lookup"}, "planner", "hotels")
		result(2, "s4-act-hotels", `{"activated":"hotels"}`)
		result(2, "s4-sibling", notExecuted)
		expect(3, []string{"hotel_search", "travel_lookup"}, "hotels", "planner") // dispatching travel_lookup bumped planner
		if got := e2eResult(t, model.Requests()[3], "s4-lookup"); strings.Contains(got, `"error"`) {
			t.Errorf("executed travel_lookup result = %s, want a tool result", got)
		}
		expect(4, []string{"travel_lookup", "visa_check"}, "planner", "visas")
		result(4, "modeltest-call-3", `{"activated":"visas","evicted":["hotels"]}`)
		if got := vN.Calls("travel/lookup"); got != 1 {
			t.Errorf("MCP travel/lookup calls = %d, want 1 (the sibling must not run)", got)
		}
		requireActive("after A1", testPrincipal, threadA, "planner", "visas")

		// Run A2: a user-level setting narrows A's limit to 1; the next run start evicts down.
		if err := source.Set(settingsDoc(&one)); err != nil {
			t.Fatal(err)
		}
		chat(b, tokenA, threadA, "e2e-s4-a2", "narrow")
		expect(5, []string{"visa_check"}, "visas") // planner (LRU) was evicted at run start
		expect(6, []string{"travel_lookup"}, "planner")
		result(6, "modeltest-call-5", `{"activated":"planner","evicted":["visas"]}`)
		requireActive("after A2", testPrincipal, threadA, "planner")
		var resolved struct {
			MaxActiveJourneys int `json:"max_active_journeys"`
		}
		var recorded []byte
		if err := pool.QueryRow(t.Context(), `SELECT settings FROM agent_runs WHERE id=(SELECT id FROM chat_runs WHERE external_id='e2e-s4-a2')`).Scan(&recorded); err != nil || json.Unmarshal(recorded, &resolved) != nil || resolved.MaxActiveJourneys != 1 {
			t.Errorf("run A2 recorded settings = %s (%v), want max_active_journeys 1", recorded, err)
		}

		// Run A3 (limit back to 2): booking becomes the least recently used journey and has a pending approval.
		if err := source.Set(settingsDoc(nil)); err != nil {
			t.Fatal(err)
		}
		body := e2eReadBody(t, "A3", acceptancePostWithHeaders(t, c.url, aguitypes.RunAgentInput{
			ThreadID: threadA, RunID: "e2e-s4-a3",
			Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "book"}},
		}, http.Header{"Authorization": {"Bearer " + tokenA}}))
		if !strings.Contains(body, "interaction.requested") {
			t.Fatalf("A3 lacks the approval request: %s", body)
		}
		expect(7, []string{"travel_lookup"}, "planner")
		expect(8, []string{"book_reserve", "travel_lookup"}, "planner", "booking")
		result(8, "modeltest-call-7", `{"activated":"booking"}`)
		expect(9, []string{"book_reserve", "travel_lookup"}, "booking", "planner")
		result(9, "modeltest-call-8", `{"activated":"planner"}`)
		requireActive("after A3", testPrincipal, threadA, "booking", "planner")
		if got := vN.Calls("book/reserve"); got != 0 {
			t.Errorf("MCP book/reserve calls before approval = %d, want 0", got)
		}
		var pending int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_interactions WHERE thread_id=$1 AND state='pending' AND journey_id='booking'`, acceptanceThreadFor(t, pool, testPrincipal, threadA)).Scan(&pending); err != nil || pending != 1 {
			t.Fatalf("pending booking approvals = %d (%v), want 1", pending, err)
		}

		// Run A4: the limit drops to 1 while the approval is pending. The unpinned planner is evicted; the pinned, least recently used booking stays.
		if err := source.Set(settingsDoc(&one)); err != nil {
			t.Fatal(err)
		}
		snapshot, err := journal.New(pool).Snapshot(t.Context(), acceptanceThreadFor(t, pool, testPrincipal, threadA), testPrincipal)
		if err != nil {
			t.Fatal(err)
		}
		var interaction *journal.Interaction
		for _, run := range snapshot.Runs {
			if run.Interaction != nil {
				interaction = run.Interaction
			}
		}
		if interaction == nil {
			t.Fatalf("no pending interaction in snapshot: %#v", snapshot.Runs)
		}
		replyBody := e2eReadBody(t, "A4", acceptancePostWithHeaders(t, a.url, aguitypes.RunAgentInput{
			ThreadID: threadA, RunID: "e2e-s4-a4",
			ForwardedProps: map[string]any{"interactionReply": map[string]any{
				"interactionId": interaction.ID, "kind": "approval",
				"answer": map[string]any{"decision": "approve", "binding": interaction.Call.Binding},
			}},
		}, http.Header{"Authorization": {"Bearer " + tokenA}}))
		if !strings.Contains(replyBody, "RUN_FINISHED") || !strings.Contains(replyBody, "booked") {
			t.Fatalf("A4 did not finish with the answer: %s", replyBody)
		}
		expect(10, []string{"book_reserve"}, "booking")
		requireActive("after A4", testPrincipal, threadA, "booking")
		if got := vN.Calls("book/reserve"); got != 1 {
			t.Errorf("MCP book/reserve calls after approval = %d, want exactly 1", got)
		}

		// Run B1: tenant B (limit 1): the second activation evicts the first.
		chat(b, tokenB, threadB, "e2e-s4-b1", "start")
		expect(11, nil)
		expect(12, []string{"travel_lookup"}, "planner")
		result(12, "modeltest-call-11", `{"activated":"planner"}`)
		expect(13, []string{"hotel_search"}, "hotels")
		result(13, "modeltest-call-12", `{"activated":"hotels","evicted":["planner"]}`)
		requireActive("after B1", "tenant-b/bob", threadB, "hotels")
		if vN1.Calls("hotel search") != 1 || vN1.Calls("travel lookup") != 0 || vN.Calls("hotel/search") != 0 {
			t.Errorf("MCP calls: vN+1 hotel search = %d, vN+1 travel lookup = %d, vN hotel/search = %d; want 1, 0, 0", vN1.Calls("hotel search"), vN1.Calls("travel lookup"), vN.Calls("hotel/search"))
		}
		if err := pool.QueryRow(t.Context(), `SELECT settings FROM agent_runs WHERE id=(SELECT id FROM chat_runs WHERE external_id='e2e-s4-b1')`).Scan(&recorded); err != nil || json.Unmarshal(recorded, &resolved) != nil || resolved.MaxActiveJourneys != 1 {
			t.Errorf("run B1 recorded settings = %s (%v), want max_active_journeys 1", recorded, err)
		}
		historySize := func(principal, external string) int {
			t.Helper()
			var size int
			if err := pool.QueryRow(t.Context(), `SELECT jsonb_array_length(history) FROM agent_conversations WHERE id=$1`, acceptanceThreadFor(t, pool, principal, external)).Scan(&size); err != nil {
				t.Fatal(err)
			}
			return size
		}
		historyA := historySize(testPrincipal, threadA)
		if historyA == 0 {
			t.Fatal("conversation A has no history")
		}

		// Restart every replica: the active set and history come from the database alone.
		for _, replica := range []acceptanceReplica{a, b, c} {
			replica.stop()
		}
		a, b, c = threeReplicas(t, pool, set, model, tenancy)
		_, _ = a, c
		requireActive("after restart A", testPrincipal, threadA, "booking")
		requireActive("after restart B", "tenant-b/bob", threadB, "hotels")
		if got := historySize(testPrincipal, threadA); got != historyA {
			t.Errorf("history length after restart = %d, want %d", got, historyA)
		}
		chat(b, tokenA, threadA, "e2e-s4-a5", "still there?")
		expect(15, []string{"book_reserve"}, "booking")
		if !strings.Contains(strings.Join(model.Requests()[15].Messages, "\n"), "start") {
			t.Errorf("request 15 lacks the conversation history from before the restart: %v", model.Requests()[15].Messages)
		}
		if got := model.Count(); got != 16 {
			t.Errorf("model requests = %d, want 16", got)
		}
		requireActive("after A5", testPrincipal, threadA, "booking")
	})
	t.Run("S5", func(t *testing.T) {
		stage(t, 5, "H4b")
		// A correction admitted via replica B while A's tool is blocked reaches every later provider request, and a reconnect on C is gap-free.
		const (
			thread     = "e2e-s5"
			correction = "use Osaka instead"
		)
		pool := database(t)
		endpoint := mcptest.New(t, mcptest.Options{Name: "e2e-s5", Version: "vN"}, e2eCatalog(func(o e2eOp) string { return o.remoteA }))
		started, release := endpoint.Block("travel/lookup")
		t.Cleanup(release)
		model := modeltest.New(
			modeltest.Call("travel_lookup", map[string]any{"destination": "Kyoto"}), // 0: blocks in MCP
			modeltest.Call("travel_lookup", map[string]any{"destination": "Osaka"}), // 1: first request after the correction
			modeltest.Text("corrected Osaka"),                                       // 2
		)
		a, b, c := threeReplicas(t, pool, e2eJourneySet(t), model, testTenancy(t, endpoint.URL()))

		first := acceptancePost(t, a.url, aguitypes.RunAgentInput{
			ThreadID: thread, RunID: "e2e-s5-a",
			Messages:       []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip"}},
			ForwardedProps: map[string]any{"journeyId": "planner"},
		})
		defer first.Body.Close()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the MCP tool did not start")
		}
		steering := acceptancePost(t, b.url, aguitypes.RunAgentInput{
			ThreadID: thread, RunID: "e2e-s5-b",
			Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: correction}},
		})
		defer steering.Body.Close()
		if steering.StatusCode != http.StatusOK {
			t.Fatalf("steering admission = %d", steering.StatusCode)
		}
		live := acceptanceReconnect(t, c.url, thread, "e2e-s5-b")
		defer live.Body.Close()
		if live.StatusCode != http.StatusOK {
			t.Fatalf("reconnect on C = %d", live.StatusCode)
		}
		snapshot, err := journal.New(pool).Snapshot(t.Context(), acceptanceThread(t, pool, thread), testPrincipal)
		if err != nil || len(snapshot.Runs) != 1 || snapshot.Runs[0].PendingCommands != 1 {
			t.Fatalf("durable steering receipt = %#v, %v", snapshot, err)
		}
		if got := model.Count(); got != 1 {
			t.Fatalf("model requests while the tool is blocked = %d, want 1", got)
		}
		// A second reconnect on C resumes from the newest event journalled so far.
		var cursor int64
		if err := pool.QueryRow(t.Context(), `SELECT max(sequence) FROM agent_events WHERE thread_id=$1`, acceptanceThread(t, pool, thread)).Scan(&cursor); err != nil || cursor == 0 {
			t.Fatalf("newest event sequence = %d (%v)", cursor, err)
		}
		resumed := e2eReconnectFrom(t, c.url, thread, "e2e-s5-b", "token", cursor)
		defer resumed.Body.Close()
		if resumed.StatusCode != http.StatusOK {
			t.Fatalf("resume on C = %d", resumed.StatusCode)
		}
		release()

		streams := map[string]string{}
		for name, response := range map[string]*http.Response{"A": first, "B (steering)": steering, "C (reconnect)": live, "C (resumed)": resumed} {
			body := e2eReadBody(t, "stream "+name, response)
			if !strings.Contains(body, "corrected Osaka") || !strings.Contains(body, "RUN_FINISHED") {
				t.Fatalf("stream %s lacks the final answer or RUN_FINISHED: %s", name, body)
			}
			streams[name] = body
		}

		requests := model.Requests()
		if len(requests) != 3 {
			t.Fatalf("model requests = %d, want 3", len(requests))
		}
		if strings.Contains(strings.Join(requests[0].Messages, "\n"), correction) {
			t.Errorf("request 0 already contains the correction: %v", requests[0].Messages)
		}
		for i := 1; i < len(requests); i++ {
			if !strings.Contains(strings.Join(requests[i].Messages, "\n"), correction) {
				t.Errorf("request %d lacks the correction %q: %v", i, correction, requests[i].Messages)
			}
		}
		if pending := requests[1].PendingCommands; len(pending) != 1 || pending[0].Text != correction {
			t.Errorf("request 1 pending commands = %#v, want the correction", pending)
		}
		// Every round was built from the same activated journey, so the correction survived rebuilding the agent.
		for i, request := range requests {
			if !slices.Equal(request.ActiveJourneys, []string{"planner"}) || !slices.Equal(e2eJourneyAliases(request), []string{"travel_lookup"}) {
				t.Errorf("request %d active journeys = %v aliases = %v, want [planner] / [travel_lookup]", i, request.ActiveJourneys, e2eJourneyAliases(request))
			}
		}
		if got := endpoint.Calls("travel/lookup"); got != 2 {
			t.Errorf("MCP travel/lookup calls = %d, want 2", got)
		}

		// Gap-free replay: A saw one contiguous run of event ids. Each reconnect on C
		// starts at its snapshot id (the larger of the cursor and the watermark) and
		// then sees exactly the later events, in order, with no gap and no duplicate.
		idsA := e2eEventIDs(t, streams["A"])
		if len(idsA) < 5 || !e2eContiguous(idsA) {
			t.Fatalf("stream A event ids = %v, want at least 5 contiguous ids", idsA)
		}
		for name, first := range map[string]int64{"B (steering)": 0, "C (reconnect)": 0, "C (resumed)": cursor} {
			ids := e2eEventIDs(t, streams[name])
			if len(ids) == 0 || !e2eContiguous(ids) || ids[len(ids)-1] != idsA[len(idsA)-1] || ids[0] < first {
				t.Errorf("stream %s event ids = %v, want contiguous from %d to %d", name, ids, first, idsA[len(idsA)-1])
				continue
			}
			if tail := idsA[slices.Index(idsA, ids[0]):]; !slices.Equal(ids, tail) {
				t.Errorf("stream %s event ids = %v, want the suffix of A's %v", name, ids, tail)
			}
		}
		if ids := e2eEventIDs(t, streams["C (resumed)"]); ids[0] != cursor || len(ids) < 2 {
			t.Errorf("resumed stream event ids = %v, want to start at the cursor %d and continue", ids, cursor)
		}
	})
	t.Run("S6", func(t *testing.T) {
		stage(t, 6, "H5")
		// Approval wait, tenant moved to vN+1, approve: superseded plus a new interaction, then dispatched exactly once on vN+1; clarification replies survive the flip.
		const (
			threadApproval, threadClarify = "e2e-s6-approval", "e2e-s6-clarify"
			token                         = "token"
		)
		pool := database(t)
		set := e2eJourneySet(t)
		vN := mcptest.New(t, mcptest.Options{Name: "e2e-s6-vN", Version: "vN"}, e2eCatalog(func(o e2eOp) string { return o.remoteA }))
		vN1 := mcptest.New(t, mcptest.Options{Name: "e2e-s6-vN1", Version: "vN+1"}, e2eCatalog(func(o e2eOp) string { return o.remoteB }))
		allow, err := tenantdir.NewAllowlist(map[string]string{"java-vn": vN.URL(), "java-vn1": vN1.URL()})
		if err != nil {
			t.Fatal(err)
		}
		source, err := settings.NewMutable(settings.Document{})
		if err != nil {
			t.Fatal(err)
		}
		onVN := tenantdir.Entry{ProductVersion: "vN", EndpointID: "java-vn"}
		onVN1 := tenantdir.Entry{ProductVersion: "vN+1", EndpointID: "java-vn1"}
		directory := tenantdir.NewMutable(map[string]tenantdir.Entry{"tenant-a": onVN})
		tenancy := agentruntime.Tenancy{Directory: directory, Endpoints: allow, Settings: source}

		// Model script; the comment on each step is its global request index.
		model := modeltest.New(
			// Approval thread, run 1 (on vN).
			modeltest.Call("activate_journey", map[string]any{"journey": "booking"}), // 0
			modeltest.Call("book_reserve", struct{}{}),                               // 1: effectful, waits for approval
			// Approval thread, run 3: the new approval is granted on vN+1. Runs 2 (supersede) makes no model request.
			modeltest.Text("booked"), // 2
			// Clarification thread.
			modeltest.Func(func(_ context.Context, _ agentruntime.ModelRequest) (agentruntime.ModelResponse, error) { // 3
				return agentruntime.ModelResponse{ToolCalls: []agentruntime.ToolCall{{
					CallID: "s6-clarify", Name: "request_user_input",
					Arguments: json.RawMessage(`{"questions":[{"id":"seat","header":"Seat","question":"Which seat?","options":[{"label":"Aisle","description":"Aisle seat"},{"label":"Window","description":"Window seat"}]}]}`),
				}}}, nil
			}),
			modeltest.Text("clarified"), // 4
		)
		a, b, c := threeReplicas(t, pool, set, model, tenancy)

		post := func(replica acceptanceReplica, thread, run string, input aguitypes.RunAgentInput) string {
			t.Helper()
			input.ThreadID, input.RunID = thread, run
			return e2eReadBody(t, run, acceptancePostWithHeaders(t, replica.url, input, http.Header{"Authorization": {"Bearer " + token}}))
		}
		type pendingInteraction struct{ id, callID, binding string }
		pending := func(thread string) pendingInteraction {
			t.Helper()
			var p pendingInteraction
			if err := pool.QueryRow(t.Context(), `SELECT id, artifact->'call'->>'callId', COALESCE(artifact->'call'->>'binding','') FROM agent_interactions WHERE thread_id=$1 AND state='pending'`,
				acceptanceThreadFor(t, pool, testPrincipal, thread)).Scan(&p.id, &p.callID, &p.binding); err != nil {
				t.Fatalf("no pending interaction on %s: %v", thread, err)
			}
			return p
		}
		marker := func(id string) (m *string, state, product string) {
			t.Helper()
			var p *string
			if err := pool.QueryRow(t.Context(), `SELECT marker, state, COALESCE(product_version,'') FROM agent_interactions WHERE id=$1`, id).Scan(&m, &state, &p); err != nil {
				t.Fatal(err)
			}
			if p != nil {
				product = *p
			}
			return m, state, product
		}
		requireMarker := func(label, id, want, wantState string) {
			t.Helper()
			m, state, _ := marker(id)
			if m == nil || *m != want || state != wantState {
				t.Errorf("%s: interaction %s marker/state = %v/%s, want %s/%s", label, id, m, state, want, wantState)
			}
		}
		approve := func(replica acceptanceReplica, thread, run string, p pendingInteraction) string {
			t.Helper()
			return post(replica, thread, run, aguitypes.RunAgentInput{ForwardedProps: map[string]any{"interactionReply": map[string]any{
				"interactionId": p.id, "kind": "approval",
				"answer": map[string]any{"decision": "approve", "binding": p.binding},
			}}})
		}

		// Run 1: the approval wait opens on vN; nothing is dispatched.
		body := post(a, threadApproval, "e2e-s6-run1", aguitypes.RunAgentInput{Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "book"}}})
		if !strings.Contains(body, "interaction.requested") {
			t.Fatalf("run 1 lacks the approval request: %s", body)
		}
		old := pending(threadApproval)
		if m, _, product := marker(old.id); m == nil || *m != "requested" || product != "vN" {
			t.Fatalf("old approval marker/product_version = %v/%s, want requested/vN", m, product)
		}
		if vN.Calls("book/reserve") != 0 || vN1.Calls("book reserve") != 0 {
			t.Fatalf("MCP calls before approval: vN %d, vN+1 %d, want 0, 0", vN.Calls("book/reserve"), vN1.Calls("book reserve"))
		}

		// The tenant moves to vN+1 while the approval is pending.
		directory.Set("tenant-a", onVN1)

		// Run 2 (another replica): approving the old interaction supersedes it and opens a new approval; zero MCP calls.
		body = approve(b, threadApproval, "e2e-s6-run2", old)
		if !strings.Contains(body, "interaction.requested") || strings.Contains(body, "RUN_ERROR") {
			t.Fatalf("run 2 did not wait on a new approval: %s", body)
		}
		requireMarker("old approval after flip", old.id, "superseded", "consumed")
		fresh := pending(threadApproval)
		if fresh.id == old.id || fresh.callID == old.callID || fresh.binding == old.binding || fresh.binding == "" {
			t.Errorf("new approval %#v must differ from the old %#v in id, call id and binding", fresh, old)
		}
		if m, _, product := marker(fresh.id); m == nil || *m != "requested" || product != "vN+1" {
			t.Errorf("new approval marker/product_version = %v/%s, want requested/vN+1", m, product)
		}
		var superseded int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_events WHERE thread_id=$1 AND kind='interaction.superseded'`, acceptanceThreadFor(t, pool, testPrincipal, threadApproval)).Scan(&superseded); err != nil || superseded != 1 {
			t.Errorf("interaction.superseded events = %d (%v), want 1", superseded, err)
		}
		if vN.Calls("book/reserve") != 0 || vN1.Calls("book reserve") != 0 {
			t.Errorf("MCP calls after supersede: vN %d, vN+1 %d, want 0, 0", vN.Calls("book/reserve"), vN1.Calls("book reserve"))
		}
		if got := model.Count(); got != 2 {
			t.Errorf("model requests after supersede = %d, want 2 (the supersede run asks the model nothing)", got)
		}

		// Run 3 (third replica): approving the new interaction dispatches exactly once on vN+1.
		body = approve(c, threadApproval, "e2e-s6-run3", fresh)
		if !strings.Contains(body, "RUN_FINISHED") || !strings.Contains(body, "booked") {
			t.Fatalf("run 3 did not finish with the answer: %s", body)
		}
		if vN1.Calls("book reserve") != 1 || vN.Calls("book/reserve") != 0 {
			t.Errorf("MCP calls after approval: vN+1 book reserve %d, vN book/reserve %d, want 1, 0", vN1.Calls("book reserve"), vN.Calls("book/reserve"))
		}
		requireMarker("new approval", fresh.id, "dispatched", "consumed")
		requireMarker("old approval stays superseded", old.id, "superseded", "consumed")

		// Clarification thread: opened on vN, answered after the flip to vN+1.
		directory.Set("tenant-a", onVN)
		body = post(a, threadClarify, "e2e-s6-clarify1", aguitypes.RunAgentInput{Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "seat?"}}})
		if !strings.Contains(body, "interaction.requested") {
			t.Fatalf("clarification run lacks the question: %s", body)
		}
		question := pending(threadClarify)
		var artifact struct {
			Kind      string `json:"kind"`
			Questions []struct {
				Header  string `json:"header"`
				Options []struct{ Label, Description string }
			} `json:"questions"`
		}
		var raw []byte
		if err := pool.QueryRow(t.Context(), `SELECT artifact FROM agent_interactions WHERE id=$1`, question.id).Scan(&raw); err != nil || json.Unmarshal(raw, &artifact) != nil {
			t.Fatalf("clarification artifact %s: %v", raw, err)
		}
		if len(artifact.Questions) != 1 || len(artifact.Questions[0].Header) > 12 || len(artifact.Questions[0].Options) < 2 || len(artifact.Questions[0].Options) > 3 {
			t.Fatalf("clarification questions = %s, want 1 question, header <= 12 chars, 2-3 options", raw)
		}
		for _, option := range artifact.Questions[0].Options {
			if option.Label == "" || option.Description == "" {
				t.Errorf("clarification option %#v lacks label or description", option)
			}
		}
		if m, state, _ := marker(question.id); m != nil || state != "pending" {
			t.Errorf("clarification marker/state = %v/%s, want NULL/pending", m, state)
		}
		directory.Set("tenant-a", onVN1)
		body = post(b, threadClarify, "e2e-s6-clarify2", aguitypes.RunAgentInput{ForwardedProps: map[string]any{"interactionReply": map[string]any{
			"interactionId": question.id, "kind": "clarification",
			"answer": map[string]any{"answers": map[string]any{"seat": map[string]any{"option": "Window"}}},
		}}})
		if !strings.Contains(body, "RUN_FINISHED") || !strings.Contains(body, "clarified") {
			t.Fatalf("clarification reply did not finish with the answer: %s", body)
		}
		if got := model.Count(); got != 5 {
			t.Fatalf("model requests = %d, want 5", got)
		}
		if got := e2eResult(t, model.Requests()[4], "s6-clarify"); !strings.Contains(got, "Window") {
			t.Errorf("request 4 result for s6-clarify = %s, want it to carry the answer Window", got)
		}
		if m, state, _ := marker(question.id); m != nil || state != "consumed" {
			t.Errorf("answered clarification marker/state = %v/%s, want NULL/consumed", m, state)
		}
	})
	t.Run("S7", func(t *testing.T) {
		stage(t, 7, "H6")
		// The real litellm provider against a scripted Responses stub gets attribution headers without credentials, maps 429 budget_exceeded and Admit denials to limit_exceeded, and agent_usage matches reported tokens.
		const (
			threadOK, threadBudget, threadDeny = "e2e-s7-ok", "e2e-s7-budget", "e2e-s7-deny"
			runOK, runBudget, runDeny          = "e2e-s7-ok-run", "e2e-s7-budget-run", "e2e-s7-deny-run"
			s7Sentinel                         = "azap-s7-sentinel-4d27b1"
			s7Model                            = "stub-model"
		)
		first := modeltest.Usage{InputTokens: 120, OutputTokens: 30, CachedTokens: 40}
		second := modeltest.Usage{InputTokens: 200, OutputTokens: 45, CachedTokens: 64}

		pool := database(t)
		endpoint := mcptest.New(t, mcptest.Options{Name: "e2e-s7", Version: "vN"}, mcptest.Catalog{Tools: []mcptest.Tool{
			{Name: "lookup", OperationKey: "travel.lookup", Result: "Kyoto is available"},
		}})
		stub := modeltest.NewResponsesServer(
			modeltest.RespondCall("s7-call", "travel_lookup", json.RawMessage(`{"destination":"Kyoto"}`)).WithUsage(first),
			modeltest.RespondText("Kyoto is available for your trip").WithUsage(second),
			modeltest.RespondBudgetExceeded(30*time.Second),
		)
		t.Cleanup(stub.Close)
		provider, err := litellm.New(litellm.Config{BaseURL: stub.URL(), APIKey: "stub-key", Model: s7Model})
		if err != nil {
			t.Fatal(err)
		}
		headers, err := cost.NewHeaders(cost.DefaultHeaderNames())
		if err != nil {
			t.Fatal(err)
		}
		gate := cost.NewScripted(headers)
		tenancy := testTenancy(t, endpoint.URL())
		tenancy.Cost = gate
		a, b, c := threeReplicas(t, pool, acceptanceSet(t, "read_only"), provider, tenancy)

		post := func(replica acceptanceReplica, thread, run, cookie string) string {
			t.Helper()
			return e2eReadBody(t, run, acceptancePostWithHeaders(t, replica.url, aguitypes.RunAgentInput{
				ThreadID: thread, RunID: run,
				Messages:       []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
				ForwardedProps: map[string]any{"journeyId": "planner"},
			}, http.Header{"Cookie": {"azap_session=" + cookie}}))
		}
		store := journal.New(pool)

		// 1. A run with a tool round: two model calls, two usage rows.
		body := post(a, threadOK, runOK, s7Sentinel)
		if !strings.Contains(body, "Kyoto is available for your trip") || !strings.Contains(body, "RUN_FINISHED") {
			t.Fatalf("run 1 lacks final answer or RUN_FINISHED: %s", body)
		}
		internalOK := acceptanceThread(t, pool, threadOK)
		idOK := e2eInternalRun(t, pool, internalOK, runOK)
		if got := e2eRunState(t, pool, internalOK, idOK); got != string(journal.RunCompleted) {
			t.Fatalf("run 1 state = %q, want completed", got)
		}
		if got := endpoint.Count("tools/call"); got != 1 {
			t.Fatalf("MCP tools/call count after run 1 = %d, want 1", got)
		}
		rows := e2eUsageRows(t, pool, internalOK, idOK)
		want := []e2eUsageRow{
			{seq: 1, model: s7Model, input: first.InputTokens, output: first.OutputTokens, cached: first.CachedTokens, tenant: testTenant, principal: testPrincipal},
			{seq: 2, model: s7Model, input: second.InputTokens, output: second.OutputTokens, cached: second.CachedTokens, tenant: testTenant, principal: testPrincipal},
		}
		if !slices.Equal(rows, want) {
			t.Fatalf("agent_usage rows for run 1 = %+v, want %+v", rows, want)
		}
		report, err := cost.Usage(t.Context(), store, cost.Scope{Tenant: testTenant}, cost.Window{})
		if err != nil {
			t.Fatal(err)
		}
		wantTotal := cost.ModelUsage{Calls: 2, Input: first.InputTokens + second.InputTokens, Output: first.OutputTokens + second.OutputTokens, Cached: first.CachedTokens + second.CachedTokens}
		wantModel := wantTotal
		wantModel.Model = s7Model
		if report.Total != wantTotal || len(report.Models) != 1 || report.Models[0] != wantModel {
			t.Fatalf("cost.Usage report = %+v, want total %+v and one model %+v", report, wantTotal, wantModel)
		}

		// 2. Attribution headers on every stub request; the credential sentinel reaches MCP but never the stub.
		if got := stub.Count(); got != 2 {
			t.Fatalf("stub requests after run 1 = %d, want 2", got)
		}
		for i, request := range stub.Requests() {
			for name, wantValue := range map[string]string{
				"X-Agent-Tenant":       testTenant,
				"X-Agent-Principal":    testPrincipal,
				"X-Agent-Conversation": internalOK,
				"X-Agent-Run":          idOK,
			} {
				if got := request.Header.Get(name); got != wantValue {
					t.Errorf("stub request %d %s = %q, want %q", i, name, got, wantValue)
				}
			}
			if got := request.Header.Get("Authorization"); got != "Bearer stub-key" {
				t.Errorf("stub request %d Authorization changed: got %q", i, got)
			}
			if strings.Contains(fmt.Sprint(request.Header), s7Sentinel) || strings.Contains(string(request.Body), s7Sentinel) {
				t.Errorf("credential sentinel leaked into stub request %d", i)
			}
		}
		sentToMCP := 0
		for _, request := range endpoint.Requests() {
			if request.RPCMethod != "" && strings.Contains(request.Header.Get("Cookie"), s7Sentinel) {
				sentToMCP++
			}
		}
		if sentToMCP == 0 {
			t.Error("the MCP endpoint never received the credential sentinel")
		}

		// 3. The stub answers 429 budget_exceeded with Retry-After 30: terminal, one request, no retry.
		body = post(b, threadBudget, runBudget, s7Sentinel)
		e2eRequireLimitExceeded(t, "budget run", body)
		if !strings.Contains(body, `"retryAfterSeconds":30`) {
			t.Errorf("budget run SSE lacks the limit_exceeded event with retryAfterSeconds 30: %s", body)
		}
		internalBudget := acceptanceThread(t, pool, threadBudget)
		idBudget := e2eInternalRun(t, pool, internalBudget, runBudget)
		if got := e2eRunState(t, pool, internalBudget, idBudget); got != string(journal.RunLimitExceeded) {
			t.Errorf("budget run state = %q, want limit_exceeded", got)
		}
		var reason, kind, retry string
		if err := pool.QueryRow(t.Context(), `SELECT reason, detail->>'kind', detail->>'retry_after_seconds' FROM agent_events WHERE thread_id=$1 AND run_id=$2 AND kind='run.limit_exceeded'`,
			internalBudget, idBudget).Scan(&reason, &kind, &retry); err != nil {
			t.Fatalf("budget run has no run.limit_exceeded event: %v", err)
		}
		if reason != "budget" || kind != "budget" || retry != "30" {
			t.Errorf("run.limit_exceeded reason/kind/retry_after_seconds = %q/%q/%q, want budget/budget/30", reason, kind, retry)
		}
		if got := e2eStubRequestsFor(stub, idBudget); got != 1 {
			t.Errorf("stub requests for the budget run = %d, want exactly 1 (no retry)", got)
		}
		if got := stub.Count(); got != 3 {
			t.Errorf("stub requests in total = %d, want 3", got)
		}
		if got := e2eOutcomeUnknown(t, pool, internalBudget, idBudget); got != 0 {
			t.Errorf("budget run has %d outcome_unknown operations/attempts, want 0", got)
		}
		if rows := e2eUsageRows(t, pool, internalBudget, idBudget); len(rows) != 0 {
			t.Errorf("budget run has agent_usage rows %+v, want none", rows)
		}

		// 4. A scripted Admit denial (every run but the two above, i.e. only this one): the run ends limit_exceeded before any model or MCP request.
		gate.SetAdmit(func(attribution cost.Attribution, _ settings.Cost) (cost.Decision, error) {
			if attribution.Run != idOK && attribution.Run != idBudget {
				return cost.Deny(cost.LimitBudget, 0), nil
			}
			return cost.Allow(), nil
		})
		t.Cleanup(func() { gate.SetAdmit(nil) })
		stubBefore, mcpBefore := stub.Count(), len(endpoint.Requests())
		body = post(c, threadDeny, runDeny, s7Sentinel)
		gate.SetAdmit(nil)
		e2eRequireLimitExceeded(t, "denied run", body)
		internalDeny := acceptanceThread(t, pool, threadDeny)
		idDeny := e2eInternalRun(t, pool, internalDeny, runDeny)
		if got := e2eRunState(t, pool, internalDeny, idDeny); got != string(journal.RunLimitExceeded) {
			t.Errorf("denied run state = %q, want limit_exceeded", got)
		}
		if got := stub.Count(); got != stubBefore {
			t.Errorf("stub requests after the denied run = %d, want unchanged at %d", got, stubBefore)
		}
		if got := len(endpoint.Requests()); got != mcpBefore {
			t.Errorf("MCP requests after the denied run = %d, want unchanged at %d", got, mcpBefore)
		}
		if got := e2eStubRequestsFor(stub, idDeny); got != 0 {
			t.Errorf("stub requests for the denied run = %d, want 0", got)
		}
		if got := e2eOutcomeUnknown(t, pool, internalDeny, idDeny); got != 0 {
			t.Errorf("denied run has %d outcome_unknown operations/attempts, want 0", got)
		}
		if rows := e2eUsageRows(t, pool, internalDeny, idDeny); len(rows) != 0 {
			t.Errorf("denied run has agent_usage rows %+v, want none", rows)
		}
		var admitted bool
		for _, attribution := range gate.Admits() {
			if attribution.Run == idDeny {
				admitted = true
				if attribution.Tenant != testTenant || attribution.Principal != testPrincipal || attribution.Conversation != internalDeny {
					t.Errorf("Admit attribution for the denied run = %+v", attribution)
				}
			}
		}
		if !admitted {
			t.Error("the gate never saw Admit for the denied run")
		}
		for _, attribution := range gate.Accesses() {
			if attribution.Run == idDeny {
				t.Error("Access was requested for the denied run, want Admit only")
			}
		}
		// Usage totals are unchanged by the two limit_exceeded runs.
		after, err := cost.Usage(t.Context(), store, cost.Scope{Tenant: testTenant}, cost.Window{})
		if err != nil {
			t.Fatal(err)
		}
		if after.Total != wantTotal {
			t.Errorf("usage total after limit_exceeded runs = %+v, want %+v", after.Total, wantTotal)
		}
	})
	t.Run("S8", func(t *testing.T) {
		stage(t, 8, "H6b")
		// Design corrections: one alias across tenants and a version flip (M4), history truncation (M5), idle definition re-pin (M6), dispatch keeps a journey active (M9) and 403 as a tool result (M15). Each block has its own database, definition set and replicas.
		t.Run("M4 alias", e2eS8Alias)
		t.Run("M5 history budget", e2eS8HistoryBudget)
		t.Run("M6 definition re-pin", e2eS8Repin)
		t.Run("M9 dispatch touches", e2eS8Touch)
		t.Run("M15 not permitted", e2eS8NotPermitted)
	})
	t.Run("S9", func(t *testing.T) {
		stage(t, 9, "H7")
		// An apply-then-drop-response effect yields outcome_unknown with no retry and a pinned journey, and the trace carries no content, tool descriptions or remote names.
	})
}

// e2eOp is one operation both E2E MCP endpoints serve under different remote names.
type e2eOp struct{ key, remoteA, remoteB string }

var e2eOps = []e2eOp{
	{"travel.lookup", "travel/lookup", "travel lookup"},
	{"hotel.search", "hotel/search", "hotel search"},
	{"visa.check", "visa/check", "visa check"},
	{"book.reserve", "book/reserve", "book reserve"},
}

// e2eCatalog serves every e2eOps operation under the remote name remote picks.
func e2eCatalog(remote func(e2eOp) string) mcptest.Catalog {
	var tools []mcptest.Tool
	for _, op := range e2eOps {
		tools = append(tools, mcptest.Tool{Name: remote(op), OperationKey: op.key})
	}
	return mcptest.Catalog{Tools: tools}
}

// e2eJourneySet compiles four journeys: planner, hotels and visas are read-only,
// booking is effectful (it needs approval). Each skill body is "BODY <id>".
func e2eJourneySet(t *testing.T) *definitions.Set {
	t.Helper()
	root := t.TempDir()
	config := "catalog:\n  operation_meta_key: " + testMetaKey + "\n  call_id_header: X-Platform-Call-ID\njourneys:\n"
	for _, j := range []struct{ id, op, class string }{
		{"planner", "travel.lookup", "read_only"},
		{"hotels", "hotel.search", "read_only"},
		{"visas", "visa.check", "read_only"},
		{"booking", "book.reserve", ""},
	} {
		dir := filepath.Join(root, "skills", j.id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		skill := "---\nname: " + j.id + "\ndescription: The " + j.id + " journey\n---\nBODY " + j.id + "\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skill), 0600); err != nil {
			t.Fatal(err)
		}
		config += "  - id: " + j.id + "\n    skill: skills/" + j.id + "\n    tools:\n      - op: " + j.op + "\n"
		if j.class != "" {
			config += "        class: " + j.class + "\n"
		}
	}
	path := filepath.Join(root, "journeys.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return compileSet(t, path)
}

// e2eJourneyAliases is the sorted list of journey tool aliases in a model request
// (the request omits the product tools).
func e2eJourneyAliases(request agentruntime.ModelRequest) []string {
	var names []string
	for _, tool := range request.Tools {
		switch tool.Name {
		case "activate_journey", "request_user_input", "read_skill_resource":
		default:
			names = append(names, tool.Name)
		}
	}
	slices.Sort(names)
	return names
}

// e2eResult is the JSON of the tool result with the given call ID in request.
func e2eResult(t *testing.T, request agentruntime.ModelRequest, callID string) string {
	t.Helper()
	for _, result := range request.ToolResults {
		if result.CallID == callID {
			encoded, err := json.Marshal(result.Value)
			if err != nil {
				t.Fatal(err)
			}
			return string(encoded)
		}
	}
	t.Fatalf("request has no tool result for call %q: %#v", callID, request.ToolResults)
	return ""
}

// e2eReconnectFrom is e2eReconnect with a Last-Event-ID cursor.
func e2eReconnectFrom(t *testing.T, endpoint, thread, run, token string, cursor int64) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, endpoint+"?threadId="+thread+"&runId="+run, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Last-Event-ID", fmt.Sprint(cursor))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

var e2eEventID = regexp.MustCompile(`(?m)^id: (\d+)$`)

// e2eEventIDs returns the SSE event ids of body in order.
func e2eEventIDs(t *testing.T, body string) []int64 {
	t.Helper()
	var ids []int64
	for _, match := range e2eEventID.FindAllStringSubmatch(body, -1) {
		var id int64
		if _, err := fmt.Sscan(match[1], &id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// e2eContiguous reports whether ids increase by exactly one.
func e2eContiguous(ids []int64) bool {
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[i-1]+1 {
			return false
		}
	}
	return true
}

// e2eUsageRow is one agent_usage row without its timestamp.
type e2eUsageRow struct {
	seq                   int
	model                 string
	input, output, cached int64
	tenant, principal     string
}

// e2eUsageRows returns the run's agent_usage rows ordered by seq.
func e2eUsageRows(t *testing.T, pool *pgxpool.Pool, thread, run string) []e2eUsageRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT seq, model, input_tokens, output_tokens, cached_tokens, tenant, principal FROM agent_usage WHERE thread_id=$1 AND run_id=$2 ORDER BY seq`, thread, run)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []e2eUsageRow
	for rows.Next() {
		var r e2eUsageRow
		if err := rows.Scan(&r.seq, &r.model, &r.input, &r.output, &r.cached, &r.tenant, &r.principal); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// e2eRunState is the durable agent_runs.state of a run.
func e2eRunState(t *testing.T, pool *pgxpool.Pool, thread, run string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(t.Context(), `SELECT state FROM agent_runs WHERE thread_id=$1 AND id=$2`, thread, run).Scan(&state); err != nil {
		t.Fatalf("run %s: %v", run, err)
	}
	return state
}

// e2eOutcomeUnknown counts the run's operations and attempts recorded as outcome_unknown.
func e2eOutcomeUnknown(t *testing.T, pool *pgxpool.Pool, thread, run string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM agent_operations WHERE thread_id=$1 AND run_id=$2 AND outcome='outcome_unknown')
		+ (SELECT count(*) FROM agent_attempts a JOIN agent_operations o ON o.call_id=a.call_id WHERE o.thread_id=$1 AND o.run_id=$2 AND a.outcome='outcome_unknown')`,
		thread, run).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// e2eStubRequestsFor counts the stub requests attributed to run by X-Agent-Run.
func e2eStubRequestsFor(stub *modeltest.ResponsesServer, run string) int {
	n := 0
	for _, request := range stub.Requests() {
		if request.Header.Get("X-Agent-Run") == run {
			n++
		}
	}
	return n
}

// e2eRequireLimitExceeded requires a RUN_ERROR with code limit_exceeded and no RUN_FINISHED.
func e2eRequireLimitExceeded(t *testing.T, name, body string) {
	t.Helper()
	if !strings.Contains(body, "RUN_ERROR") || !strings.Contains(body, `"code":"limit_exceeded"`) || strings.Contains(body, "RUN_FINISHED") {
		t.Fatalf("%s: expected RUN_ERROR code limit_exceeded and no RUN_FINISHED: %s", name, body)
	}
}

// e2eInternalRun is the internal run ID (agent_runs.id) of a chat run's external ID.
func e2eInternalRun(t *testing.T, pool *pgxpool.Pool, thread, external string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(t.Context(), `SELECT id FROM chat_runs WHERE thread_id=$1 AND external_id=$2`, thread, external).Scan(&id); err != nil {
		t.Fatalf("internal id of run %s: %v", external, err)
	}
	return id
}

// e2eS8Journey is one S8 journey: its id and its YAML tool entries (already
// indented six spaces).
type e2eS8Journey struct{ id, tools string }

// e2eS8Path writes an S8 bundle whose skill bodies are "BODY <id> <prompt>" and
// returns the journeys.yaml path.
func e2eS8Path(t *testing.T, prompt string, journeys ...e2eS8Journey) string {
	t.Helper()
	root := t.TempDir()
	config := "catalog:\n  operation_meta_key: " + testMetaKey + "\n  call_id_header: X-Platform-Call-ID\njourneys:\n"
	for _, j := range journeys {
		dir := filepath.Join(root, "skills", j.id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		skill := "---\nname: " + j.id + "\ndescription: The " + j.id + " journey\n---\nBODY " + j.id + " " + prompt + "\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skill), 0600); err != nil {
			t.Fatal(err)
		}
		config += "  - id: " + j.id + "\n    skill: skills/" + j.id + "\n    tools:\n" + j.tools
	}
	path := filepath.Join(root, "journeys.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// e2eS8Set compiles an S8 bundle.
func e2eS8Set(t *testing.T, prompt string, journeys ...e2eS8Journey) *definitions.Set {
	t.Helper()
	return compileSet(t, e2eS8Path(t, prompt, journeys...))
}

// e2eS8ReadOnly is the YAML tool entry selecting one read_only operation.
func e2eS8ReadOnly(op string) string { return "      - op: " + op + "\n        class: read_only\n" }

// e2eS8Chat posts one chat run as the bearer token and requires RUN_FINISHED unless
// the caller passes allowError.
func e2eS8Chat(t *testing.T, replica acceptanceReplica, token, thread, run string, input aguitypes.RunAgentInput) string {
	t.Helper()
	input.ThreadID, input.RunID = thread, run
	return e2eReadBody(t, run, acceptancePostWithHeaders(t, replica.url, input, http.Header{"Authorization": {"Bearer " + token}}))
}

// e2eS8Say is a user message, optionally targeting a journey.
func e2eS8Say(text, journey string) aguitypes.RunAgentInput {
	input := aguitypes.RunAgentInput{Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: text}}}
	if journey != "" {
		input.ForwardedProps = map[string]any{"journeyId": journey}
	}
	return input
}

// e2eS8Finished requires a finished run whose body contains want.
func e2eS8Finished(t *testing.T, name, body, want string) {
	t.Helper()
	if !strings.Contains(body, "RUN_FINISHED") || !strings.Contains(body, want) || strings.Contains(body, "RUN_ERROR") {
		t.Fatalf("%s did not finish with %q: %s", name, want, body)
	}
}

// e2eS8Tenancy builds a tenancy from endpoint id -> URL and tenant -> directory entry with default settings.
func e2eS8Tenancy(t *testing.T, urls map[string]string, entries map[string]tenantdir.Entry) (agentruntime.Tenancy, *tenantdir.Mutable) {
	t.Helper()
	allow, err := tenantdir.NewAllowlist(urls)
	if err != nil {
		t.Fatal(err)
	}
	source, err := settings.NewMutable(settings.Document{})
	if err != nil {
		t.Fatal(err)
	}
	directory := tenantdir.NewMutable(entries)
	return agentruntime.Tenancy{Directory: directory, Endpoints: allow, Settings: source}, directory
}

// e2eS8Active is agent_conversations.active_journeys (least recently used first).
func e2eS8Active(t *testing.T, pool *pgxpool.Pool, principal, external string) []string {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(t.Context(), `SELECT active_journeys FROM agent_conversations WHERE id=$1`, acceptanceThreadFor(t, pool, principal, external)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

// e2eS8Alias is M4: one alias for a [k.v2, k] selector across two tenants on different alternatives and across a version flip.
func e2eS8Alias(t *testing.T) {
	const (
		alias   = "leave_request"
		remoteA = "leave/request"      // tenant A, vN: only leave.request
		remoteB = "leave request v2"   // tenant B: only leave.request.v2
		remoteN = "leave:request v2 A" // tenant A after the flip: leave.request.v2
	)
	pool := database(t)
	set := e2eS8Set(t, "alias", e2eS8Journey{"leave", "      - op: [leave.request.v2, leave.request]\n        alias: " + alias + "\n        class: read_only\n"})
	epA := mcptest.New(t, mcptest.Options{Name: "e2e-s8-a", Version: "vN"}, mcptest.Catalog{Tools: []mcptest.Tool{{Name: remoteA, OperationKey: "leave.request", Result: "granted"}}})
	epB := mcptest.New(t, mcptest.Options{Name: "e2e-s8-b", Version: "vN+1"}, mcptest.Catalog{Tools: []mcptest.Tool{{Name: remoteB, OperationKey: "leave.request.v2", Result: "granted"}}})
	epN := mcptest.New(t, mcptest.Options{Name: "e2e-s8-n", Version: "vN+2"}, mcptest.Catalog{Tools: []mcptest.Tool{{Name: remoteN, OperationKey: "leave.request.v2", Result: "granted"}}})
	tenancy, directory := e2eS8Tenancy(t,
		map[string]string{"java-a": epA.URL(), "java-b": epB.URL(), "java-n": epN.URL()},
		map[string]tenantdir.Entry{
			"tenant-a": {ProductVersion: "vN", EndpointID: "java-a"},
			"tenant-b": {ProductVersion: "vN+1", EndpointID: "java-b"},
		})
	model := modeltest.New(
		modeltest.Call(alias, struct{}{}), modeltest.Text("A1 done"), // 0, 1: tenant A on leave.request
		modeltest.Call(alias, struct{}{}), modeltest.Text("B1 done"), // 2, 3: tenant B on leave.request.v2
		modeltest.Call(alias, struct{}{}), modeltest.Text("A2 done"), // 4, 5: tenant A after the flip
	)
	a, b, c := threeReplicas(t, pool, set, model, tenancy)

	e2eS8Finished(t, "A1", e2eS8Chat(t, a, "token", "e2e-s8-alias-a", "e2e-s8-alias-a1", e2eS8Say("leave please", "leave")), "A1 done")
	e2eS8Finished(t, "B1", e2eS8Chat(t, b, "token-b", "e2e-s8-alias-b", "e2e-s8-alias-b1", e2eS8Say("leave please", "leave")), "B1 done")
	if epA.Calls(remoteA) != 1 || epB.Calls(remoteB) != 1 || epA.Count("tools/call") != 1 || epB.Count("tools/call") != 1 {
		t.Fatalf("MCP calls: A %q=%d (tools/call %d), B %q=%d (tools/call %d); want one each", remoteA, epA.Calls(remoteA), epA.Count("tools/call"), remoteB, epB.Calls(remoteB), epB.Count("tools/call"))
	}

	// The tenant moves to the version whose catalog has leave.request.v2 under another remote name.
	directory.Set("tenant-a", tenantdir.Entry{ProductVersion: "vN+2", EndpointID: "java-n"})
	e2eS8Finished(t, "A2", e2eS8Chat(t, c, "token", "e2e-s8-alias-a", "e2e-s8-alias-a2", e2eS8Say("again", "")), "A2 done")
	if epN.Calls(remoteN) != 1 || epN.Count("tools/call") != 1 {
		t.Errorf("flipped endpoint %q calls = %d (tools/call %d), want 1", remoteN, epN.Calls(remoteN), epN.Count("tools/call"))
	}
	if epA.Calls(remoteA) != 1 || epA.Count("tools/call") != 1 {
		t.Errorf("old endpoint calls after the flip = %d (tools/call %d), want unchanged 1", epA.Calls(remoteA), epA.Count("tools/call"))
	}

	requests := model.Requests()
	if len(requests) != 6 {
		t.Fatalf("model requests = %d, want 6", len(requests))
	}
	for _, i := range []int{0, 2, 4} {
		if got := e2eJourneyAliases(requests[i]); !slices.Equal(got, []string{alias}) {
			t.Errorf("request %d journey aliases = %v, want [%s]", i, got, alias)
		}
	}
	for i, request := range requests {
		dump := fmt.Sprintf("%+v", request)
		for _, remote := range []string{remoteA, remoteB, remoteN} {
			if strings.Contains(dump, remote) {
				t.Errorf("model request %d mentions remote name %q", i, remote)
			}
		}
	}
	for _, i := range []int{1, 3, 5} {
		if got := e2eResult(t, requests[i], fmt.Sprintf("modeltest-call-%d", i-1)); strings.Contains(got, `"error"`) {
			t.Errorf("request %d result = %s, want a tool result", i, got)
		}
	}
}

// e2eS8HistoryBudget is M5: with a budget that fits about two turns, the fourth provider request omits the oldest turns, keeps every call paired with its result, and leaves one counts-only history_truncated event; the persisted history keeps all four turns.
func e2eS8HistoryBudget(t *testing.T) {
	const (
		sentinel = "azap-s8-history-sentinel-7f3a91"
		// A turn is ~2.5KB of user text (~625 estimated tokens at 4 bytes per token) plus
		// a short answer, so 1700 tokens (6.8KB) fits two turns and not three.
		budget = 1700
	)
	turnText := func(n int) string {
		head := fmt.Sprintf("TURN-%d-MARKER ", n)
		if n == 1 {
			head += sentinel + " "
		}
		return head + strings.Repeat("lorem ipsum dolor ", 140)
	}
	pool := database(t)
	set := e2eS8Set(t, "history", e2eS8Journey{"planner", e2eS8ReadOnly("travel.lookup")})
	endpoint := mcptest.New(t, mcptest.Options{Name: "e2e-s8-history", Version: "vN"}, mcptest.Catalog{Tools: []mcptest.Tool{{Name: "travel/lookup", OperationKey: "travel.lookup", Result: "Kyoto is available"}}})
	tenancy := testTenancy(t, endpoint.URL())
	tenancy.HistoryBudget = budget
	model := modeltest.New(
		modeltest.Text("answer 1"), // 0
		modeltest.Text("answer 2"), // 1
		modeltest.Call("travel_lookup", map[string]any{"destination": "Kyoto"}), // 2: turn 3 holds a call
		modeltest.Text("answer 3"), // 3
		modeltest.Text("answer 4"), // 4
	)
	a, b, c := threeReplicas(t, pool, set, model, tenancy)
	const thread = "e2e-s8-history"
	replicas := []acceptanceReplica{a, b, c, a}
	for i := 1; i <= 4; i++ {
		journey := ""
		if i == 1 {
			journey = "planner"
		}
		e2eS8Finished(t, fmt.Sprintf("turn %d", i), e2eS8Chat(t, replicas[i-1], "token", thread, fmt.Sprintf("e2e-s8-history-%d", i), e2eS8Say(turnText(i), journey)), fmt.Sprintf("answer %d", i))
	}
	requests := model.Requests()
	if len(requests) != 5 {
		t.Fatalf("model requests = %d, want 5", len(requests))
	}

	// The fourth turn's provider request.
	last := requests[4]
	var firstUser, texts string
	calls, results := map[string]bool{}, map[string]bool{}
	for _, m := range last.ProviderMessages {
		if m.Role == message.RoleUser && firstUser == "" {
			firstUser = m.String()
		}
		texts += m.String() + "\n"
		for _, content := range m.Contents {
			switch content := content.(type) {
			case *message.FunctionCallContent:
				calls[content.CallID] = true
			case *message.FunctionResultContent:
				results[content.CallID] = true
			}
		}
	}
	if firstUser == "" || strings.Contains(firstUser, "TURN-1-MARKER") || strings.Contains(firstUser, "TURN-2-MARKER") {
		t.Errorf("request 4 first user message = %.60q, want a turn after the oldest", firstUser)
	}
	if strings.Contains(texts, "TURN-1-MARKER") || strings.Contains(texts, sentinel) || !strings.Contains(texts, "TURN-4-MARKER") {
		t.Errorf("request 4 messages: want turn 1 omitted and turn 4 present")
	}
	if len(calls) == 0 || !maps.Equal(calls, results) {
		t.Errorf("request 4 function calls %v and results %v, want a non-empty call set with a result for each", calls, results)
	}
	// The untrimmed requests keep turn 1.
	if !strings.Contains(strings.Join(requests[1].Messages, "\n"), "TURN-1-MARKER") {
		t.Error("request 1 (two turns) lost turn 1 although it fits the budget")
	}

	// Exactly one counts-only event for the run that trimmed.
	threadID := acceptanceThread(t, pool, thread)
	runID := e2eInternalRun(t, pool, threadID, "e2e-s8-history-4")
	var raw []byte
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*), min(detail::text)::text FROM agent_events WHERE thread_id=$1 AND run_id=$2 AND kind='history_truncated'`, threadID, runID).Scan(&n, &raw); err != nil || n != 1 {
		t.Fatalf("history_truncated events for the fourth run = %d (%v), want 1", n, err)
	}
	var detail struct {
		TurnsDropped, MessagesDropped, EstimateBefore, EstimateAfter, Budget int
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for key := range fields {
		if !slices.Contains([]string{"turns_dropped", "messages_dropped", "estimate_before", "estimate_after", "budget"}, key) {
			t.Errorf("history_truncated detail has unexpected field %q", key)
		}
	}
	number := func(key string) int { v, _ := fields[key].(float64); return int(v) }
	detail.TurnsDropped, detail.MessagesDropped, detail.EstimateBefore, detail.EstimateAfter, detail.Budget =
		number("turns_dropped"), number("messages_dropped"), number("estimate_before"), number("estimate_after"), number("budget")
	if detail.TurnsDropped < 1 || detail.MessagesDropped < 1 || detail.Budget != budget || detail.EstimateBefore <= budget || detail.EstimateAfter > budget || detail.EstimateAfter >= detail.EstimateBefore {
		t.Errorf("history_truncated detail = %s, want turns/messages dropped >= 1, budget %d, before > budget >= after", raw, budget)
	}
	if strings.Contains(string(raw), sentinel) || strings.Contains(string(raw), "TURN-") {
		t.Errorf("history_truncated detail carries turn content: %s", raw)
	}

	// The persisted history is never trimmed.
	var persisted string
	if err := pool.QueryRow(t.Context(), `SELECT history::text FROM agent_conversations WHERE id=$1`, threadID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		if !strings.Contains(persisted, fmt.Sprintf("TURN-%d-MARKER", i)) {
			t.Errorf("persisted history lacks turn %d", i)
		}
	}
	if !strings.Contains(persisted, sentinel) {
		t.Error("persisted history lacks the turn 1 sentinel")
	}
}

// e2eS8Ready reports whether the replica's /readyz answers 200.
func e2eS8Ready(t *testing.T, replica acceptanceReplica) bool {
	t.Helper()
	response, err := http.Get(replica.url + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode == http.StatusOK
}

// e2eS8Repin is M6: an idle conversation re-pins to the current definition set with one definition_changed event; a conversation with a pending approval keeps its pin and keeps the old set required until it settles.
func e2eS8Repin(t *testing.T) {
	ctx := t.Context()
	journeys := []e2eS8Journey{
		{"planner", e2eS8ReadOnly("travel.lookup")},
		{"booking", "      - op: book.reserve\n"}, // effectful: needs approval
	}
	d1Path, d2Path := e2eS8Path(t, "PROMPT-D1", journeys...), e2eS8Path(t, "PROMPT-D2", journeys...)
	d1Only, d2Only, both := compileSet(t, d1Path), compileSet(t, d2Path), compileSet(t, d2Path, d1Path)
	d1, d2 := d1Only.Digest(), d2Only.Digest()
	if d1 == d2 || both.Digest() != d2 || !both.Serves(d1) || d2Only.Serves(d1) {
		t.Fatalf("definition sets: d1 %s d2 %s both %s", d1, d2, both.Digest())
	}

	pool := database(t)
	store := journal.New(pool)
	endpoint := mcptest.New(t, mcptest.Options{Name: "e2e-s8-repin", Version: "vN"}, e2eCatalog(func(o e2eOp) string { return o.remoteA }))
	tenancy := testTenancy(t, endpoint.URL())
	model := modeltest.New(
		modeltest.Text("x1 done"),                  // 0: idle conversation on D1
		modeltest.Call("book_reserve", struct{}{}), // 1: effectful, waits for approval
		modeltest.Text("booked"),                   // 2: the approved continuation (D1)
		modeltest.Text("x2 done"),                  // 3: the idle conversation after the flip (D2)
	)
	const threadX, threadY = "e2e-s8-repin-x", "e2e-s8-repin-y"
	a, b, _ := threeReplicas(t, pool, d1Only, model, tenancy)

	e2eS8Finished(t, "X1", e2eS8Chat(t, a, "token", threadX, "e2e-s8-repin-x1", e2eS8Say("plan", "planner")), "x1 done")
	body := e2eS8Chat(t, b, "token", threadY, "e2e-s8-repin-y1", e2eS8Say("book", "booking"))
	if !strings.Contains(body, "interaction.requested") {
		t.Fatalf("Y1 lacks the approval request: %s", body)
	}
	internalX, internalY := acceptanceThread(t, pool, threadX), acceptanceThread(t, pool, threadY)
	pinOf := func(thread string) string {
		t.Helper()
		var pin string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(definition_digest,'') FROM agent_conversations WHERE id=$1`, thread).Scan(&pin); err != nil {
			t.Fatal(err)
		}
		return pin
	}
	changes := func(thread string) (count int, from, to string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(min(detail->>'old_digest'),''), COALESCE(min(detail->>'new_digest'),'') FROM agent_events WHERE thread_id=$1 AND kind='definition_changed'`, thread).Scan(&count, &from, &to); err != nil {
			t.Fatal(err)
		}
		return count, from, to
	}
	if pinOf(internalX) != d1 || pinOf(internalY) != d1 {
		t.Fatalf("pins before the activation: X %q Y %q, want both D1 %q", pinOf(internalX), pinOf(internalY), d1)
	}
	var interaction, binding string
	if err := pool.QueryRow(ctx, `SELECT id, COALESCE(artifact->'call'->>'binding','') FROM agent_interactions WHERE thread_id=$1 AND state='pending'`, internalY).Scan(&interaction, &binding); err != nil {
		t.Fatalf("no pending approval on Y: %v", err)
	}
	if endpoint.Calls("book/reserve") != 0 {
		t.Fatalf("book/reserve called before approval")
	}

	// D2 cannot become current from a replica that does not serve D1: Y's pending approval references it.
	if err := store.ActivateDefinition(ctx, d1, d2, d2Only.Serves); !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("activation D1 -> D2 with a D2-only set = %v, want ErrDefinition", err)
	}
	if err := store.ActivateDefinition(ctx, d1, d2, both.Serves); err != nil {
		t.Fatalf("activation D1 -> D2 with D1 and D2 served: %v", err)
	}
	if current, err := store.CurrentDefinition(ctx); err != nil || current != d2 {
		t.Fatalf("current definition = %q, %v, want D2", current, err)
	}

	// Replicas that serve only D2 are not Ready while the D1 approval is pending.
	for _, replica := range []acceptanceReplica{a, b} {
		replica.stop()
	}
	a, b, _ = threeReplicas(t, pool, d2Only, model, tenancy)
	if e2eS8Ready(t, a) || e2eS8Ready(t, b) {
		t.Fatal("a D2-only replica is Ready while an approval pinned to D1 is pending")
	}
	if pinOf(internalY) != d1 {
		t.Errorf("Y pin while pending = %q, want D1", pinOf(internalY))
	}
	if count, _, _ := changes(internalY); count != 0 {
		t.Errorf("definition_changed events on Y while pending = %d, want 0", count)
	}

	// Settle the approval on replicas that serve D1 and D2: the continuation still runs on D1.
	for _, replica := range []acceptanceReplica{a, b} {
		replica.stop()
	}
	a, b, _ = threeReplicas(t, pool, both, model, tenancy)
	reply := aguitypes.RunAgentInput{ForwardedProps: map[string]any{"interactionReply": map[string]any{
		"interactionId": interaction, "kind": "approval",
		"answer": map[string]any{"decision": "approve", "binding": binding},
	}}}
	e2eS8Finished(t, "Y2", e2eS8Chat(t, a, "token", threadY, "e2e-s8-repin-y2", reply), "booked")
	if endpoint.Calls("book/reserve") != 1 {
		t.Errorf("book/reserve calls after approval = %d, want 1", endpoint.Calls("book/reserve"))
	}
	if got := model.Requests()[2].DefinitionDigest; got != d1 {
		t.Errorf("approved continuation ran on %q, want D1 %q", got, d1)
	}
	if count, _, _ := changes(internalY); count != 0 {
		t.Errorf("definition_changed events on Y after its continuation = %d, want 0 (a continuation never re-pins)", count)
	}

	// Nothing is pending any more: D2-only replicas are Ready.
	for _, replica := range []acceptanceReplica{a, b} {
		replica.stop()
	}
	a, b, _ = threeReplicas(t, pool, d2Only, model, tenancy)
	if !e2eS8Ready(t, a) || !e2eS8Ready(t, b) {
		t.Fatal("a D2-only replica is not Ready after the approval settled")
	}

	// The idle conversation's next message re-pins to D2 with one definition_changed event.
	e2eS8Finished(t, "X2", e2eS8Chat(t, b, "token", threadX, "e2e-s8-repin-x2", e2eS8Say("again", "")), "x2 done")
	if count, from, to := changes(internalX); count != 1 || from != d1 || to != d2 {
		t.Errorf("definition_changed on X = %d %q -> %q, want 1 %q -> %q", count, from, to, d1, d2)
	}
	request := model.Requests()[3]
	if request.DefinitionDigest != d2 || request.Provenance.Definition.Digest != d2 || !strings.Contains(request.Instructions, "PROMPT-D2") || strings.Contains(request.Instructions, "PROMPT-D1") {
		t.Errorf("X2 request definition %q, instructions lack PROMPT-D2 or carry PROMPT-D1: %.200q", request.DefinitionDigest, request.Instructions)
	}
	if pinOf(internalX) != d2 {
		t.Errorf("X pin after re-pin = %q, want D2", pinOf(internalX))
	}
	var changedRun string
	if err := pool.QueryRow(ctx, `SELECT run_id FROM agent_events WHERE thread_id=$1 AND kind='definition_changed'`, internalX).Scan(&changedRun); err != nil || changedRun != e2eInternalRun(t, pool, internalX, "e2e-s8-repin-x2") {
		t.Errorf("definition_changed run = %q (%v), want the X2 run", changedRun, err)
	}
}

// e2eS8Touch is M9: dispatching a journey's alias keeps it from being the least recently used.
func e2eS8Touch(t *testing.T) {
	pool := database(t)
	set := e2eS8Set(t, "touch",
		e2eS8Journey{"j1", e2eS8ReadOnly("travel.lookup")},
		e2eS8Journey{"j2", e2eS8ReadOnly("hotel.search")},
		e2eS8Journey{"j3", e2eS8ReadOnly("visa.check")},
	)
	endpoint := mcptest.New(t, mcptest.Options{Name: "e2e-s8-touch", Version: "vN"}, e2eCatalog(func(o e2eOp) string { return o.remoteA }))
	tenancy := testTenancy(t, endpoint.URL()) // default limit: 2 active journeys
	activate := func(id string) modeltest.Step {
		return modeltest.Call("activate_journey", map[string]any{"journey": id})
	}
	model := modeltest.New(
		activate("j1"), // 0
		activate("j2"), // 1
		modeltest.Call("travel_lookup", struct{}{}), // 2: dispatch a j1 alias
		activate("j3"),            // 3: j2 is now the least recently used
		modeltest.Text("touched"), // 4
	)
	a, _, _ := threeReplicas(t, pool, set, model, tenancy)
	e2eS8Finished(t, "touch run", e2eS8Chat(t, a, "token", "e2e-s8-touch", "e2e-s8-touch-1", e2eS8Say("go", "")), "touched")
	requests := model.Requests()
	if len(requests) != 5 {
		t.Fatalf("model requests = %d, want 5", len(requests))
	}
	if got := requests[3].ActiveJourneys; !slices.Equal(got, []string{"j2", "j1"}) {
		t.Errorf("request 3 ActiveJourneys = %v, want [j2 j1] (the dispatch moved j1 to most recently used)", got)
	}
	if got := e2eResult(t, requests[4], "modeltest-call-3"); got != `{"activated":"j3","evicted":["j2"]}` {
		t.Errorf("third activation result = %s, want j3 activated and [j2] evicted", got)
	}
	if got := e2eS8Active(t, pool, testPrincipal, "e2e-s8-touch"); !slices.Equal(got, []string{"j1", "j3"}) {
		t.Errorf("active journeys = %v, want [j1 j3]", got)
	}
	if got := endpoint.Calls("travel/lookup"); got != 1 {
		t.Errorf("MCP travel/lookup calls = %d, want 1", got)
	}
}

// e2eS8NotPermitted is M15: a 403 is a tool result the model sees once and the run continues; a 401 still ends the run auth_required.
func e2eS8NotPermitted(t *testing.T) {
	pool := database(t)
	set := e2eS8Set(t, "forbid", e2eS8Journey{"ops", e2eS8ReadOnly("travel.lookup") + e2eS8ReadOnly("hotel.search")})
	endpoint := mcptest.New(t, mcptest.Options{Name: "e2e-s8-forbid", Version: "vN"}, e2eCatalog(func(o e2eOp) string { return o.remoteA }))
	endpoint.Forbid("travel/lookup")
	model := modeltest.New(
		modeltest.Call("travel_lookup", struct{}{}), // 0: 403
		modeltest.Call("hotel_search", struct{}{}),  // 1: a later call in the same run succeeds
		modeltest.Text("ops done"),                  // 2
		modeltest.Call("hotel_search", struct{}{}),  // 3: second thread, 401
	)
	a, b, _ := threeReplicas(t, pool, set, model, testTenancy(t, endpoint.URL()))

	body := e2eS8Chat(t, a, "token", "e2e-s8-forbid-1", "e2e-s8-forbid-run1", e2eS8Say("go", "ops"))
	e2eS8Finished(t, "forbid run", body, "ops done")
	requests := model.Requests()
	if len(requests) != 3 {
		t.Fatalf("model requests after the 403 run = %d, want 3", len(requests))
	}
	if got := e2eResult(t, requests[1], "modeltest-call-0"); got != `{"error":"not_permitted"}` {
		t.Errorf("result of the forbidden call = %s, want {\"error\":\"not_permitted\"}", got)
	}
	if got := e2eResult(t, requests[2], "modeltest-call-1"); strings.Contains(got, `"error"`) {
		t.Errorf("result of the later call = %s, want a tool result", got)
	}
	if got := endpoint.Calls("travel/lookup"); got != 1 {
		t.Errorf("MCP travel/lookup calls = %d, want exactly 1 (no retry)", got)
	}
	if got := endpoint.Calls("hotel/search"); got != 1 {
		t.Errorf("MCP hotel/search calls = %d, want 1", got)
	}
	internal := acceptanceThread(t, pool, "e2e-s8-forbid-1")
	if got := e2eRunState(t, pool, internal, e2eInternalRun(t, pool, internal, "e2e-s8-forbid-run1")); got != string(journal.RunCompleted) {
		t.Errorf("403 run state = %q, want completed", got)
	}

	// A 401 on a separate thread still ends the run auth_required.
	endpoint.Unauthorized("hotel/search", 1)
	body = e2eS8Chat(t, b, "token", "e2e-s8-forbid-2", "e2e-s8-forbid-run2", e2eS8Say("go", "ops"))
	if !strings.Contains(body, `"code":"auth_required"`) || strings.Contains(body, "RUN_FINISHED") {
		t.Fatalf("401 run: expected RUN_ERROR auth_required and no RUN_FINISHED: %s", body)
	}
	internal = acceptanceThread(t, pool, "e2e-s8-forbid-2")
	if got := e2eRunState(t, pool, internal, e2eInternalRun(t, pool, internal, "e2e-s8-forbid-run2")); got != string(journal.RunAuthRequired) {
		t.Errorf("401 run state = %q, want auth_required", got)
	}
	if got := model.Count(); got != 4 {
		t.Errorf("model requests = %d, want 4", got)
	}
}
