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
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
		registry := acceptanceSet(t, "read_only")
		model := modeltest.New(
			modeltest.Call("travel_lookup", map[string]any{"destination": "Kyoto"}),
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
				Messages: []aguitypes.Message{{Role: aguitypes.RoleUser, Content: "plan a trip to Kyoto"}},
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
			alias, []string{"request_user_input", "load_skill", "read_skill_resource", "activate_journey"}).Scan(&eventLeaks); err != nil || eventLeaks != 0 {
			t.Errorf("agent_events with a tool_name other than the alias = %d (%v), want 0", eventLeaks, err)
		}
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
