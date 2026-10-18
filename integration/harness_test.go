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
	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"github.com/swiftdiaries/az-agent-platform-go/internal/service"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type acceptanceReplica struct {
	app *service.Service
	url string
}

func startAcceptanceReplica(t *testing.T, pool *pgxpool.Pool, registry *definitions.Registry, model agentruntime.Model) acceptanceReplica {
	t.Helper()
	app, err := service.New(service.Dependencies{
		Pool: pool, Registry: registry, Authenticator: chat.StaticBearerTokens{"token": "alice"}, Model: model,
		CredentialHeaders: acceptanceCredentialHeaders,
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
	t.Cleanup(func() {
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
	return acceptanceReplica{app: app, url: "http://" + listener.Addr().String()}
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
	var thread string
	if err := pool.QueryRow(t.Context(), "SELECT id FROM chat_threads WHERE principal='alice' AND external_id=$1", external).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	return thread
}

// threeReplicas starts replicas A, B and C as separate in-process Service
// instances sharing one database, registry and model.
func threeReplicas(t *testing.T, pool *pgxpool.Pool, registry *definitions.Registry, model agentruntime.Model) (a, b, c acceptanceReplica) {
	t.Helper()
	a = startAcceptanceReplica(t, pool, registry, model)
	b = startAcceptanceReplica(t, pool, registry, model)
	c = startAcceptanceReplica(t, pool, registry, model)
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
