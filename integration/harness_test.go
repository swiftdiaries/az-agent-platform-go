package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/catalog"
	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/credential"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"github.com/swiftdiaries/az-agent-platform-go/internal/service"
	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
	"github.com/swiftdiaries/az-agent-platform-go/internal/tenantdir"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	testTenant     = "tenant-a"
	testPrincipal  = "tenant-a/alice" // StaticBearerTokens tenant form; this is the chat principal
	testEndpointID = "test-mcp"
)

// acceptanceTokens maps the acceptance bearer tokens to tenant-form principals.
var acceptanceTokens = chat.StaticBearerTokens{"token": testPrincipal, "token-b": "tenant-b/bob", "token-c": "tenant-c/carol"}

// testTenancy routes testTenant to one allowlisted endpoint at url with default settings.
func testTenancy(t *testing.T, url string) agentruntime.Tenancy {
	t.Helper()
	allow, err := tenantdir.NewAllowlist(map[string]string{testEndpointID: url})
	if err != nil {
		t.Fatal(err)
	}
	source, err := settings.NewMutable(settings.Document{})
	if err != nil {
		t.Fatal(err)
	}
	return agentruntime.Tenancy{
		Directory: tenantdir.NewMutable(map[string]tenantdir.Entry{testTenant: {ProductVersion: "vN", EndpointID: testEndpointID}}),
		Endpoints: allow,
		Settings:  source,
	}
}

// testEndpoint is the allowlisted endpoint (id testEndpointID) for url, the only
// way a test can obtain a valid tenantdir.Endpoint for a direct Bind.
func testEndpoint(t *testing.T, url string) tenantdir.Endpoint {
	t.Helper()
	allow, err := tenantdir.NewAllowlist(map[string]string{testEndpointID: url})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := allow.Endpoint(testEndpointID)
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

// scoped resolves in's tenancy through runner.Scope (recording it on the run)
// and returns the input ready for Run. Tenant defaults to testTenant.
func scoped(t *testing.T, runner *agentruntime.Runner, in agentruntime.RunInput) agentruntime.RunInput {
	t.Helper()
	if in.Tenant == "" {
		in.Tenant = testTenant
	}
	scope, err := runner.Scope(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	in.ProductVersion, in.Endpoint, in.Settings = scope.Entry.ProductVersion, scope.Endpoint, scope.Settings
	return in
}

// testMetaKey is the _meta key under which the test servers publish operation keys
// (the writers set catalog.operation_meta_key to the same value).
const testMetaKey = "operation_key"

// keyedTool is an inline go-sdk tool that publishes op under _meta[testMetaKey].
// name is the remote name; the model sees definitions.AliasFor(op).
func keyedTool(name, op string) *mcp.Tool {
	return &mcp.Tool{Name: name, Meta: mcp.Meta{testMetaKey: op}, InputSchema: map[string]any{"type": "object"}}
}

// openTestCatalog opens a catalog Binding for set against endpoint as a freshly
// admitted and claimed run (a journal owner is required to pin a snapshot).
func openTestCatalog(ctx context.Context, t *testing.T, endpoint tenantdir.Endpoint, cred credential.Credential, set *definitions.Set) (*catalog.Binding, error) {
	t.Helper()
	store := journal.New(database(t))
	command := journal.Command{ThreadID: "catalog", RunID: "catalog", CommunicationID: "catalog", Principal: testPrincipal, Tenant: testTenant, Text: "plan"}
	if _, _, err := store.Admit(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	owner := claimForTest(t, store, command, false)
	binding, err := catalog.Open(ctx, catalog.Scope{ProductVersion: "vN", Endpoint: endpoint}, cred, store, owner, set, catalog.Options{})
	if binding != nil {
		t.Cleanup(func() { binding.Close() })
	}
	return binding, err
}

func withDescription(tool *mcp.Tool, description string) *mcp.Tool {
	tool.Description = description
	return tool
}

type acceptanceReplica struct {
	app *service.Service
	url string
	// stop shuts the replica down; it is idempotent and also runs on test cleanup.
	stop func()
}

func startAcceptanceReplica(t *testing.T, pool *pgxpool.Pool, set *definitions.Set, model agentruntime.Model, tenancy agentruntime.Tenancy) acceptanceReplica {
	t.Helper()
	app, err := service.New(service.Dependencies{
		Pool: pool, Definitions: set, Authenticator: acceptanceTokens, Model: model,
		CredentialHeaders: acceptanceCredentialHeaders, CostGate: tenancy.Cost,
		Directory: tenancy.Directory, Endpoints: tenancy.Endpoints, Settings: tenancy.Settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- app.Serve(listener) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := app.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				t.Error(err)
			}
			if err := <-serveErr; err != nil {
				t.Error(err)
			}
			app.Close()
		})
	}
	t.Cleanup(stop)
	return acceptanceReplica{app: app, url: "http://" + listener.Addr().String(), stop: stop}
}

func acceptancePost(t *testing.T, endpoint string, input aguitypes.RunAgentInput) *http.Response {
	t.Helper()
	return acceptancePostWithHeaders(t, endpoint, input, nil)
}

// acceptancePostWithHeaders is acceptancePost plus extra request headers, added
// after the harness defaults (so an extra Authorization would replace the bearer).
func acceptancePostWithHeaders(t *testing.T, endpoint string, input aguitypes.RunAgentInput, extra http.Header) *http.Response {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for name, values := range extra {
		req.Header[http.CanonicalHeaderKey(name)] = slices.Clone(values)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func acceptanceReconnect(t *testing.T, endpoint, thread, run string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, endpoint+"?threadId="+thread+"&runId="+run, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func acceptanceThread(t *testing.T, pool *pgxpool.Pool, external string) string {
	t.Helper()
	return acceptanceThreadFor(t, pool, testPrincipal, external)
}

// acceptanceThreadFor is acceptanceThread for an arbitrary chat principal.
func acceptanceThreadFor(t *testing.T, pool *pgxpool.Pool, principal, external string) string {
	t.Helper()
	var thread string
	if err := pool.QueryRow(t.Context(), "SELECT id FROM chat_threads WHERE principal=$2 AND external_id=$1", external, principal).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	return thread
}

// threeReplicas starts replicas A, B and C as separate in-process Service
// instances sharing one database, definition set and model.
func threeReplicas(t *testing.T, pool *pgxpool.Pool, set *definitions.Set, model agentruntime.Model, tenancy agentruntime.Tenancy) (a, b, c acceptanceReplica) {
	t.Helper()
	a = startAcceptanceReplica(t, pool, set, model, tenancy)
	b = startAcceptanceReplica(t, pool, set, model, tenancy)
	c = startAcceptanceReplica(t, pool, set, model, tenancy)
	return a, b, c
}

// acceptanceCredentialHeaders are the header names the acceptance replicas copy
// into a run's Credential (AGENT_PLATFORM_CREDENTIAL_HEADERS). Wired by H1.3.
var acceptanceCredentialHeaders = []string{"Authorization", "Cookie"}

// sentinelDBHits returns "table.column" for every text-like column of every base
// table in the current schema whose contents contain any needle, and the number
// of columns scanned.
func sentinelDBHits(t *testing.T, pool *pgxpool.Pool, needles ...string) (hits []string, scanned int) {
	t.Helper()
	ctx := t.Context()
	rows, err := pool.Query(ctx, `
		SELECT c.table_name, c.column_name, c.data_type
		FROM information_schema.columns c
		JOIN information_schema.tables tb
		  ON tb.table_schema = c.table_schema AND tb.table_name = c.table_name
		WHERE c.table_schema = current_schema() AND tb.table_type = 'BASE TABLE'
		  AND c.data_type IN ('text', 'character varying', 'character', 'json', 'jsonb', 'bytea')
		ORDER BY c.table_name, c.ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	type column struct{ table, name, dataType string }
	var columns []column
	for rows.Next() {
		var col column
		if err := rows.Scan(&col.table, &col.name, &col.dataType); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		columns = append(columns, col)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, col := range columns {
		table := pgx.Identifier{col.table}.Sanitize()
		name := pgx.Identifier{col.name}.Sanitize()
		scanned++
		for _, needle := range needles {
			query := "SELECT count(*) FROM " + table + " WHERE " + name + "::text LIKE '%' || $1 || '%'"
			if col.dataType == "bytea" {
				query = "SELECT count(*) FROM " + table + " WHERE position(convert_to($1,'UTF8') in " + name + ") > 0"
			}
			var count int
			if err := pool.QueryRow(ctx, query, needle).Scan(&count); err != nil {
				t.Fatalf("scanning %s.%s: %v", col.table, col.name, err)
			}
			if count > 0 {
				hits = append(hits, col.table+"."+col.name)
			}
		}
	}
	return hits, scanned
}

// requireNoSentinelInDB fails if any scanned DB column contains a needle, or if
// the scan was vacuous.
func requireNoSentinelInDB(t *testing.T, pool *pgxpool.Pool, needles ...string) {
	t.Helper()
	hits, scanned := sentinelDBHits(t, pool, needles...)
	if scanned == 0 {
		t.Fatal("DB sentinel scan covered zero columns")
	}
	if len(hits) > 0 {
		t.Fatalf("sentinel found in DB columns: %v", slices.Compact(hits))
	}
}

// lockedBuffer is a goroutine-safe bytes.Buffer.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// teeHandler fans every record out to all of its handlers.
type teeHandler []slog.Handler

func (h teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h teeHandler) Handle(ctx context.Context, record slog.Record) error {
	var errs []error
	for _, handler := range h {
		if handler.Enabled(ctx, record.Level) {
			errs = append(errs, handler.Handle(ctx, record.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (h teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(teeHandler, len(h))
	for i, handler := range h {
		out[i] = handler.WithAttrs(attrs)
	}
	return out
}

func (h teeHandler) WithGroup(name string) slog.Handler {
	out := make(teeHandler, len(h))
	for i, handler := range h {
		out[i] = handler.WithGroup(name)
	}
	return out
}

// captureSlog installs a debug-level slog default that records JSON and text
// renderings, restoring the previous default on cleanup. Call it before
// starting replicas. It returns the JSON and text buffers.
func captureSlog(t *testing.T) (jsonOut, textOut *lockedBuffer) {
	t.Helper()
	jsonOut, textOut = &lockedBuffer{}, &lockedBuffer{}
	options := &slog.HandlerOptions{Level: slog.LevelDebug}
	previous := slog.Default()
	slog.SetDefault(slog.New(teeHandler{slog.NewJSONHandler(jsonOut, options), slog.NewTextHandler(textOut, options)}))
	t.Cleanup(func() { slog.SetDefault(previous) })
	// Positive control: the capture really sees records.
	const canary = "e2e-slog-canary-5d07"
	slog.Info("slog capture control", "canary", canary)
	if !strings.Contains(jsonOut.String(), canary) || !strings.Contains(textOut.String(), canary) {
		t.Fatalf("slog capture control missing: json=%q text=%q", jsonOut.String(), textOut.String())
	}
	return jsonOut, textOut
}

// captureSpans installs a recording global tracer provider, restoring the
// previous one on cleanup. Call it before starting replicas.
func captureSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})
	return recorder
}

// waitForEndedSpans polls (up to 5s) until ended spans with every given name exist.
func waitForEndedSpans(t *testing.T, recorder *tracetest.SpanRecorder, names ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		seen := map[string]bool{}
		for _, span := range recorder.Ended() {
			seen[span.Name()] = true
		}
		var missing []string
		for _, name := range names {
			if !seen[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ended spans missing %v; have %v", missing, seen)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// spanDump renders everything observable on a span (name, attributes, events,
// links, status, resource) as one string.
func spanDump(span sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "name=%s\n", span.Name())
	for _, attr := range span.Attributes() {
		fmt.Fprintf(&b, "attr %s=%s\n", attr.Key, attr.Value.Emit())
	}
	for _, event := range span.Events() {
		fmt.Fprintf(&b, "event %s\n", event.Name)
		for _, attr := range event.Attributes {
			fmt.Fprintf(&b, "event-attr %s=%s\n", attr.Key, attr.Value.Emit())
		}
	}
	for _, link := range span.Links() {
		for _, attr := range link.Attributes {
			fmt.Fprintf(&b, "link-attr %s=%s\n", attr.Key, attr.Value.Emit())
		}
	}
	fmt.Fprintf(&b, "status %s %s\n", span.Status().Code, span.Status().Description)
	if res := span.Resource(); res != nil {
		for _, attr := range res.Attributes() {
			fmt.Fprintf(&b, "resource %s=%s\n", attr.Key, attr.Value.Emit())
		}
	}
	return b.String()
}

// sentinelSpanHits returns "span name: needle" for every ended span whose dump contains a needle.
func sentinelSpanHits(spans []sdktrace.ReadOnlySpan, needles ...string) (hits []string) {
	for _, span := range spans {
		dump := spanDump(span)
		for _, needle := range needles {
			if strings.Contains(dump, needle) {
				hits = append(hits, span.Name()+": "+needle)
			}
		}
	}
	return hits
}
