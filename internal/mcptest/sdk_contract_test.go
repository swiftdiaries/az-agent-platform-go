package mcptest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// methodCounter wraps an HTTP handler and counts JSON-RPC methods by name, so
// tests can prove how many requests the SDK client really made.
type methodCounter struct {
	next http.Handler
	mu   sync.Mutex
	n    map[string]int
}

func (c *methodCounter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil && r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &msg) == nil && msg.Method != "" {
			c.mu.Lock()
			if c.n == nil {
				c.n = map[string]int{}
			}
			c.n[msg.Method]++
			c.mu.Unlock()
		}
	}
	c.next.ServeHTTP(w, r)
}

func (c *methodCounter) count(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[method]
}

// contractServer starts a real streamable-HTTP MCP server around srv and
// returns a connected client session plus the method counter.
func contractServer(t *testing.T, srv *mcp.Server) (*mcp.ClientSession, *methodCounter) {
	t.Helper()
	counter := &methodCounter{next: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)}
	ts := httptest.NewServer(counter)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "contract-client", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, counter
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func addNoopTool(srv *mcp.Server, name string) {
	mcp.AddTool(srv, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
		return textResult(name), nil, nil
	})
}

func pagedServer(names ...string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "contract", Version: "1.2.3"}, &mcp.ServerOptions{PageSize: 2})
	for _, n := range names {
		addNoopTool(srv, n)
	}
	return srv
}

var fiveTools = []string{"t1", "t2", "t3", "t4", "t5"}

func toolNames(tools []*mcp.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name)
	}
	return out
}

// ServerOptions.PageSize paginates tools/list; ListTools returns one page and
// ListToolsResult.NextCursor is the continuation token (passed back through
// ListToolsParams.Cursor).
func TestSDKContractServerPaging(t *testing.T) {
	session, counter := contractServer(t, pagedServer(fiveTools...))
	ctx := context.Background()

	first, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(first.Tools); !reflect.DeepEqual(got, fiveTools[:2]) {
		t.Fatalf("page 1 = %v", got)
	}
	if first.NextCursor == "" {
		t.Fatal("page 1 has empty NextCursor")
	}

	second, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(second.Tools); !reflect.DeepEqual(got, fiveTools[2:4]) {
		t.Fatalf("page 2 = %v", got)
	}
	if second.NextCursor == "" {
		t.Fatal("page 2 has empty NextCursor")
	}

	third, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: second.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(third.Tools); !reflect.DeepEqual(got, fiveTools[4:]) {
		t.Fatalf("page 3 = %v", got)
	}
	if third.NextCursor != "" {
		t.Fatalf("last page NextCursor = %q, want empty", third.NextCursor)
	}
	if n := counter.count("tools/list"); n != 3 {
		t.Fatalf("server saw %d tools/list requests, want 3", n)
	}
}

// ClientSession.Tools is an iter.Seq2[*Tool, error] that follows NextCursor
// itself, so a manual cursor loop is not needed.
func TestSDKContractToolsIterator(t *testing.T) {
	session, counter := contractServer(t, pagedServer(fiveTools...))

	var got []string
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, tool.Name)
	}
	if !reflect.DeepEqual(got, fiveTools) {
		t.Fatalf("iterated tools = %v, want %v", got, fiveTools)
	}
	if n := counter.count("tools/list"); n != 3 {
		t.Fatalf("server saw %d tools/list requests, want 3 (paging proven by request count)", n)
	}
}

func TestSDKContractInitializeResult(t *testing.T) {
	session, _ := contractServer(t, pagedServer(fiveTools...))

	res := session.InitializeResult()
	if res == nil || res.ServerInfo == nil {
		t.Fatalf("InitializeResult = %+v", res)
	}
	if res.ServerInfo.Name != "contract" || res.ServerInfo.Version != "1.2.3" {
		t.Fatalf("ServerInfo = %+v", res.ServerInfo)
	}
}

// Tool.Meta and Tool.Annotations round-trip through tools/list. Meta values
// come back JSON-decoded (slices as []any), so compare after normalizing both
// sides through JSON.
func TestSDKContractToolMeta(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "contract", Version: "1"}, nil)
	meta := mcp.Meta{"operation_key": "billing.refund", "tags": []any{"billing", "write"}, "limit": 3}
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, Title: "Refund"}
	mcp.AddTool(srv, &mcp.Tool{Name: "refund", Meta: meta, Annotations: annotations},
		func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
			return textResult("ok"), nil, nil
		})
	session, _ := contractServer(t, srv)

	var listed *mcp.Tool
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		listed = tool
	}
	if listed == nil || listed.Name != "refund" {
		t.Fatalf("listed = %+v", listed)
	}

	normalize := func(v any) any {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got, want := normalize(listed.Meta), normalize(meta); !reflect.DeepEqual(got, want) {
		t.Fatalf("Meta = %#v, want %#v", got, want)
	}
	// After decoding, string and []any values are directly addressable.
	if listed.Meta["operation_key"] != "billing.refund" {
		t.Fatalf("operation_key = %#v", listed.Meta["operation_key"])
	}
	if tags, ok := listed.Meta["tags"].([]any); !ok || !reflect.DeepEqual(tags, []any{"billing", "write"}) {
		t.Fatalf("tags = %#v (%T)", listed.Meta["tags"], listed.Meta["tags"])
	}
	if listed.Annotations == nil || !listed.Annotations.ReadOnlyHint || !listed.Annotations.IdempotentHint || listed.Annotations.Title != "Refund" {
		t.Fatalf("Annotations = %+v", listed.Annotations)
	}
}

// Finding: the SDK does NOT reject tool names outside [A-Za-z0-9_.-]. Both
// Server.AddTool and the generic AddTool accept them; the SDK only logs an
// error-level record ("AddTool: invalid tool name ...") through
// ServerOptions.Logger. The names are listed and callable verbatim, so no
// substitute registration path is needed for remote names such as
// "billing/refund". (Empty or >128-char names are also only logged.)
func TestSDKContractRemoteNamesVerbatim(t *testing.T) {
	names := []string{"billing/refund", "lookup destination", "ns:op"}

	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "contract", Version: "1"}, &mcp.ServerOptions{Logger: logger})

	// Register the first two with the generic AddTool, the last with the raw
	// Server.AddTool; neither panics.
	for _, name := range names[:2] {
		mcp.AddTool(srv, &mcp.Tool{Name: name}, func(_ context.Context, req *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return textResult("called " + req.Params.Name), nil, nil
		})
	}
	srv.AddTool(&mcp.Tool{Name: names[2], InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return textResult("called " + req.Params.Name), nil
		})

	session, _ := contractServer(t, srv)
	ctx := context.Background()

	var listed []string
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		listed = append(listed, tool.Name)
	}
	if !reflect.DeepEqual(sortedCopy(listed), sortedCopy(names)) {
		t.Fatalf("listed = %v, want verbatim %v", listed, names)
	}

	for _, name := range names {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err != nil {
			t.Fatalf("CallTool(%q): %v", name, err)
		}
		if res.IsError {
			t.Fatalf("CallTool(%q) IsError: %+v", name, res.Content)
		}
		text, _ := res.Content[0].(*mcp.TextContent)
		if text == nil || text.Text != "called "+name {
			t.Fatalf("CallTool(%q) content = %+v", name, res.Content)
		}
	}

	// The only signal is a logged error per invalid name.
	logged := logBuf.String()
	for _, name := range names {
		if !strings.Contains(logged, "invalid tool name") || !strings.Contains(logged, name) {
			t.Fatalf("expected an 'invalid tool name' log for %q, log:\n%s", name, logged)
		}
	}
}

// A tool handler reads inbound HTTP headers from req.Extra.Header.
func TestSDKContractCallToolHeaders(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "contract", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "whoami"}, func(_ context.Context, req *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		if req.Extra == nil {
			return nil, nil, io.ErrUnexpectedEOF
		}
		return textResult(req.Extra.Header.Get("X-Contract-Header") + "|" + req.Extra.Header.Get("Authorization")), nil, nil
	})
	counter := &methodCounter{next: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)}
	ts := httptest.NewServer(counter)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpClient := &http.Client{Transport: headerTransport{
		"X-Contract-Header": "call-42",
		"Authorization":     "Bearer secret",
	}}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "contract-client", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %+v", res.Content)
	}
	text, _ := res.Content[0].(*mcp.TextContent)
	if text == nil || text.Text != "call-42|Bearer secret" {
		t.Fatalf("content = %+v", res.Content)
	}
}

type headerTransport map[string]string

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
