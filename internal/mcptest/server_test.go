package mcptest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/swiftdiaries/az-agent-platform-go/internal/mcptest"
)

// identityTransport adds fixed headers to every outbound request.
type identityTransport map[string]string

func (h identityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, s *mcptest.Server, headers map[string]string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   s.URL(),
			HTTPClient: &http.Client{Transport: identityTransport(headers)},
		}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func tenant(name string) map[string]string {
	return map[string]string{"X-Mcptest-Tenant": name}
}

func identity(tenantName, principal string) map[string]string {
	return map[string]string{"X-Mcptest-Tenant": tenantName, "X-Mcptest-Principal": principal}
}

func listAll(t *testing.T, session *mcp.ClientSession) []*mcp.Tool {
	t.Helper()
	var out []*mcp.Tool
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		out = append(out, tool)
	}
	return out
}

func names(tools []*mcp.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name)
	}
	return out
}

func call(session *mcp.ClientSession, tool string) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("result = %+v", res)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %#v", res.Content[0])
	}
	return text.Text
}

func simple(names ...string) mcptest.Catalog {
	c := mcptest.Catalog{}
	for _, n := range names {
		c.Tools = append(c.Tools, mcptest.Tool{Name: n})
	}
	return c
}

func TestServerPaging(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{PageSize: 2}, simple("t1", "t2", "t3", "t4", "t5"))
	session := connect(t, s, nil)
	ctx := context.Background()

	var sizes []int
	cursor := ""
	for {
		page, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(page.Tools))
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !reflect.DeepEqual(sizes, []int{2, 2, 1}) {
		t.Fatalf("page sizes = %v", sizes)
	}
	if n := s.Count("tools/list"); n != 3 {
		t.Fatalf("tools/list count = %d", n)
	}

	s.Reset()
	if got := names(listAll(t, session)); !reflect.DeepEqual(got, []string{"t1", "t2", "t3", "t4", "t5"}) {
		t.Fatalf("iterated = %v", got)
	}
	if n := s.Count("tools/list"); n != 3 {
		t.Fatalf("tools/list count after iteration = %d", n)
	}
}

func TestServerMetaAndAnnotations(t *testing.T) {
	cat := mcptest.Catalog{Tools: []mcptest.Tool{{
		Name:         "refund",
		Description:  "refund a payment",
		OperationKey: "billing.refund",
		Tags:         []string{"billing", "write"},
		Meta:         map[string]any{"limit": 3},
		Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Refund"},
	}, {Name: "plain"}}}

	for _, metaKey := range []string{"", "x-op"} {
		s := mcptest.New(t, mcptest.Options{MetaKey: metaKey}, cat)
		tools := listAll(t, connect(t, s, nil))
		if len(tools) != 2 {
			t.Fatalf("tools = %v", names(tools))
		}
		key := metaKey
		if key == "" {
			key = mcptest.DefaultMetaKey
		}
		byName := map[string]*mcp.Tool{}
		for _, tool := range tools {
			byName[tool.Name] = tool
		}
		refund := byName["refund"]
		if refund.Description != "refund a payment" || refund.Meta[key] != "billing.refund" {
			t.Fatalf("refund = %+v meta=%v", refund, refund.Meta)
		}
		if metaKey != "" {
			if _, ok := refund.Meta[mcptest.DefaultMetaKey]; ok {
				t.Fatalf("default key leaked: %v", refund.Meta)
			}
		}
		if tags, _ := refund.Meta["tags"].([]any); !reflect.DeepEqual(tags, []any{"billing", "write"}) {
			t.Fatalf("tags = %#v", refund.Meta["tags"])
		}
		if limit, _ := refund.Meta["limit"].(float64); limit != 3 {
			t.Fatalf("limit = %#v", refund.Meta["limit"])
		}
		if refund.Annotations == nil || !refund.Annotations.ReadOnlyHint || refund.Annotations.Title != "Refund" {
			t.Fatalf("annotations = %+v", refund.Annotations)
		}
		if plain := byName["plain"]; len(plain.Meta) != 0 {
			t.Fatalf("plain meta = %v", plain.Meta)
		}
		schema, _ := json.Marshal(byName["plain"].InputSchema)
		if string(schema) != `{"type":"object"}` {
			t.Fatalf("default schema = %s", schema)
		}
	}
}

func TestServerCatalogSelection(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{Version: "v1"},
		mcptest.Catalog{Tenant: "A", Tools: []mcptest.Tool{{Name: "billing/refund", OperationKey: "refund"}}},
		mcptest.Catalog{Tenant: "B", Tools: []mcptest.Tool{{Name: "lookup destination", OperationKey: "refund"}}},
		mcptest.Catalog{Tenant: "A", Principal: "alice", Tools: []mcptest.Tool{{Name: "alice-only"}}},
		mcptest.Catalog{Tools: []mcptest.Tool{{Name: "wild"}}},
		mcptest.Catalog{Tenant: "A", Version: "v2", Tools: []mcptest.Tool{{Name: "a-v2"}}},
		mcptest.Catalog{Tenant: "Z", Version: "v2", Tools: []mcptest.Tool{{Name: "z-v2"}}},
	)

	cases := []struct {
		name string
		id   map[string]string
		want []string
	}{
		{"tenant A", tenant("A"), []string{"billing/refund"}},
		{"tenant B", tenant("B"), []string{"lookup destination"}},
		{"principal overrides tenant", identity("A", "alice"), []string{"alice-only"}},
		{"other principal falls back to tenant", identity("A", "bob"), []string{"billing/refund"}},
		{"no identity falls to wildcard", nil, []string{"wild"}},
		{"unknown tenant falls to wildcard", tenant("nope"), []string{"wild"}},
		{"version-gated tenant invisible at v1", tenant("Z"), []string{"wild"}},
	}
	for _, tc := range cases {
		session := connect(t, s, tc.id)
		if got := names(listAll(t, session)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: tools = %v, want %v", tc.name, got, tc.want)
		}
		if v := session.InitializeResult().ServerInfo.Version; v != "v1" {
			t.Errorf("%s: version = %q", tc.name, v)
		}
	}

	s.SetVersion("v2")
	session := connect(t, s, tenant("A"))
	if got := names(listAll(t, session)); !reflect.DeepEqual(got, []string{"a-v2"}) {
		t.Fatalf("v2 tenant A = %v", got)
	}
	if v := session.InitializeResult().ServerInfo.Version; v != "v2" {
		t.Fatalf("v2 version = %q", v)
	}
	// v1-only catalogs for B are Version "" so still served at v2.
	if got := names(listAll(t, connect(t, s, tenant("B")))); !reflect.DeepEqual(got, []string{"lookup destination"}) {
		t.Fatalf("v2 tenant B = %v", got)
	}
	if got := names(listAll(t, connect(t, s, tenant("Z")))); !reflect.DeepEqual(got, []string{"z-v2"}) {
		t.Fatalf("v2 tenant Z = %v", got)
	}
}

func TestServerCatalogSelectionNoMatchAndAddCatalog(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{}, mcptest.Catalog{Tenant: "A", Tools: []mcptest.Tool{{Name: "a"}}})
	if got := listAll(t, connect(t, s, tenant("unknown"))); len(got) != 0 {
		t.Fatalf("unknown tenant tools = %v", names(got))
	}
	if v := connect(t, s, nil).InitializeResult().ServerInfo; v.Name != "mcptest" || v.Version != "v1" {
		t.Fatalf("defaults = %+v", v)
	}

	s.AddCatalog(mcptest.Catalog{Tenant: "A", Tools: []mcptest.Tool{{Name: "a2"}, {Name: "a3"}}}) // replaces
	s.AddCatalog(mcptest.Catalog{Tenant: "unknown", Tools: []mcptest.Tool{{Name: "u"}}})
	got := names(listAll(t, connect(t, s, tenant("A"))))
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"a2", "a3"}) {
		t.Fatalf("replaced A = %v", got)
	}
	if got := names(listAll(t, connect(t, s, tenant("unknown")))); !reflect.DeepEqual(got, []string{"u"}) {
		t.Fatalf("added = %v", got)
	}
}

func TestServerHeaderCapture(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{}, simple("lookup"))
	session := connect(t, s, map[string]string{
		"Authorization":       "Bearer secret",
		"Cookie":              "session=abc",
		"X-Custom":            "c1",
		"X-Mcptest-Tenant":    "T",
		"X-Mcptest-Principal": "P",
	})
	if _, err := call(session, "lookup"); err != nil {
		t.Fatal(err)
	}

	var sawList, sawCall, sawInit bool
	for _, r := range s.Requests() {
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Cookie") != "session=abc" || r.Header.Get("X-Custom") != "c1" {
			t.Fatalf("request %+v missing headers", r)
		}
		if r.Tenant != "T" || r.Principal != "P" {
			t.Fatalf("identity = %q/%q", r.Tenant, r.Principal)
		}
		for k := range r.Header {
			if k == "X-Mcptest-Inflight" {
				t.Fatalf("internal header leaked: %v", r.Header)
			}
		}
		switch {
		case r.RPCMethod == "initialize":
			sawInit = r.HTTPMethod == http.MethodPost
		case r.RPCMethod == "tools/call":
			sawCall = r.Tool == "lookup" && r.Status == http.StatusOK
		case r.RPCMethod == "tools/list":
			sawList = true
		}
	}
	if !sawInit || !sawCall {
		t.Fatalf("requests = %+v", s.Requests())
	}
	_ = sawList

	// Mutating a returned copy must not affect the server's record.
	got := s.Requests()
	got[0].Header.Set("Authorization", "mutated")
	if s.Requests()[0].Header.Get("Authorization") != "Bearer secret" {
		t.Fatal("Requests exposes internal headers")
	}

	// Close() (session DELETE) is recorded with an empty RPC method.
	_ = session.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var del bool
		for _, r := range s.Requests() {
			if r.HTTPMethod == http.MethodDelete && r.RPCMethod == "" {
				del = true
			}
		}
		if del {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no DELETE recorded: %+v", s.Requests())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServerCounters(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{}, simple("a", "b"))
	session := connect(t, s, nil)
	listAll(t, session)
	for _, tool := range []string{"a", "a", "b"} {
		if _, err := call(session, tool); err != nil {
			t.Fatal(err)
		}
	}
	if s.Count("initialize") != 1 || s.Count("tools/list") != 1 || s.Count("tools/call") != 3 {
		t.Fatalf("counts init=%d list=%d call=%d", s.Count("initialize"), s.Count("tools/list"), s.Count("tools/call"))
	}
	if s.Calls("a") != 2 || s.Calls("b") != 1 || s.Applied("a") != 2 || s.Applied("b") != 1 || s.Calls("missing") != 0 {
		t.Fatalf("calls a=%d b=%d applied a=%d b=%d", s.Calls("a"), s.Calls("b"), s.Applied("a"), s.Applied("b"))
	}
	s.Reset()
	if s.Count("tools/call") != 0 || s.Calls("a") != 0 || s.Applied("a") != 0 || len(s.Requests()) != 0 {
		t.Fatal("Reset did not clear observations")
	}
	// Catalogs survive Reset.
	if _, err := call(session, "a"); err != nil {
		t.Fatal(err)
	}
	if s.Applied("a") != 1 {
		t.Fatalf("applied after reset = %d", s.Applied("a"))
	}
}

func TestServerBlock(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{}, simple("slow"))
	session := connect(t, s, nil)

	started, release := s.Block("slow")
	done := make(chan error, 1)
	go func() {
		res, err := call(session, "slow")
		if err == nil && res.IsError {
			err = errors.New("unexpected tool error")
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("started never fired")
	}
	select {
	case err := <-done:
		t.Fatalf("call finished while blocked: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if s.Applied("slow") != 0 {
		t.Fatal("effect applied while blocked")
	}
	release()
	release() // idempotent
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not finish after release")
	}
	if s.Applied("slow") != 1 {
		t.Fatalf("applied = %d", s.Applied("slow"))
	}

	// Context cancellation unblocks a still-blocked call.
	started2, release2 := s.Block("slow")
	defer release2()
	ctx, cancel := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{}})
		done2 <- err
	}()
	select {
	case <-started2:
	case <-time.After(5 * time.Second):
		t.Fatal("second started never fired")
	}
	cancel()
	select {
	case err := <-done2:
		if err == nil {
			t.Fatal("cancelled call returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not unblock the call")
	}
	if s.Applied("slow") != 1 {
		t.Fatalf("cancelled call applied effect: %d", s.Applied("slow"))
	}
}

func TestServerFailTransient(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{}, simple("pay"))
	session := connect(t, s, nil)
	s.FailTransient("pay", 1)

	if res, err := call(session, "pay"); err == nil {
		t.Fatalf("first call succeeded: %+v", res)
	}
	if s.Applied("pay") != 0 {
		t.Fatal("transient failure applied the effect")
	}
	res, err := call(session, "pay")
	if err != nil {
		t.Fatal(err)
	}
	if resultText(t, res) != "pay ok" {
		t.Fatalf("result = %+v", res)
	}
	if s.Applied("pay") != 1 || s.Calls("pay") != 2 {
		t.Fatalf("applied=%d calls=%d", s.Applied("pay"), s.Calls("pay"))
	}
	var saw503 bool
	for _, r := range s.Requests() {
		if r.Tool == "pay" && r.Status == http.StatusServiceUnavailable {
			saw503 = true
		}
	}
	if !saw503 {
		t.Fatalf("no 503 recorded: %+v", s.Requests())
	}
}

func TestServerDropResponseAfterApply(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{}, simple("pay"))
	session := connect(t, s, nil)
	s.DropResponseAfterApply("pay", 1)

	res, err := call(session, "pay")
	if err == nil {
		t.Fatalf("dropped call returned a result: %+v", res)
	}
	// The SDK client reports a dead connection as "rejected by transport ...
	// EOF"; a server reply would instead surface a result or a tool/RPC error.
	if !strings.Contains(err.Error(), "rejected by transport") || !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("error %q is not a connection-level failure", err)
	}
	for _, r := range s.Requests() {
		if r.Tool == "pay" && r.Status != 0 {
			t.Fatalf("server wrote a response (status %d) for the dropped call", r.Status)
		}
	}
	if res != nil {
		t.Fatalf("result = %+v", res)
	}
	if s.Applied("pay") != 1 || s.Calls("pay") != 1 {
		t.Fatalf("applied=%d calls=%d", s.Applied("pay"), s.Calls("pay"))
	}

	// Controls are consumed: a fresh session's call is answered normally.
	session2 := connect(t, s, nil)
	res, err = call(session2, "pay")
	if err != nil {
		t.Fatal(err)
	}
	if resultText(t, res) != "pay ok" || s.Applied("pay") != 2 {
		t.Fatalf("second call: %+v applied=%d", res, s.Applied("pay"))
	}
}

func TestServerIsError(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{}, simple("reject"))
	session := connect(t, s, nil)
	s.IsError("reject", "private rejection")

	res, err := call(session, "reject")
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || resultText(t, res) != "private rejection" {
		t.Fatalf("result = %+v", res)
	}
	if s.Applied("reject") != 0 || s.Calls("reject") != 1 {
		t.Fatalf("applied=%d calls=%d", s.Applied("reject"), s.Calls("reject"))
	}

	// A pending drop is not consumed by a call that never applies; its
	// buffered response is replayed intact.
	s.DropResponseAfterApply("reject", 1)
	res, err = call(session, "reject")
	if err != nil || !res.IsError || resultText(t, res) != "private rejection" {
		t.Fatalf("buffered replay: %+v %v", res, err)
	}

	s.ClearControls("reject")
	res, err = call(session, "reject")
	if err != nil || res.IsError || resultText(t, res) != "reject ok" || s.Applied("reject") != 1 {
		t.Fatalf("after clear: %+v %v applied=%d", res, err, s.Applied("reject"))
	}
}

func TestServerUnauthorized(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{}, simple("pay"))
	s.Unauthorized("pay", 2)

	// The SDK client ends its session after a 401, so use a fresh session
	// for each attempt.
	for i := 0; i < 2; i++ {
		if res, err := call(connect(t, s, nil), "pay"); err == nil {
			t.Fatalf("call %d succeeded: %+v", i, res)
		}
	}
	if s.Applied("pay") != 0 {
		t.Fatal("unauthorized call applied the effect")
	}
	var n401 int
	for _, r := range s.Requests() {
		if r.Tool == "pay" && r.Status == http.StatusUnauthorized {
			n401++
		}
	}
	if n401 != 2 {
		t.Fatalf("401 count = %d: %+v", n401, s.Requests())
	}
	if _, err := call(connect(t, s, nil), "pay"); err != nil {
		t.Fatalf("third call: %v", err)
	}

	s.Unauthorized("pay", -1)
	for i := 0; i < 3; i++ {
		if _, err := call(connect(t, s, nil), "pay"); err == nil {
			t.Fatal("n<0 should always fail")
		}
	}
	s.ClearControls("pay")
	if _, err := call(connect(t, s, nil), "pay"); err != nil {
		t.Fatalf("after clear: %v", err)
	}
	if s.Applied("pay") != 2 || s.Calls("pay") != 7 {
		t.Fatalf("applied=%d calls=%d", s.Applied("pay"), s.Calls("pay"))
	}
}

func TestServerVerbatimNames(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{}, mcptest.Catalog{Tools: []mcptest.Tool{
		{Name: "billing/refund", Result: "refunded"},
		{Name: "lookup destination"},
		{Name: "ns:op"},
	}})
	session := connect(t, s, nil)
	if got := names(listAll(t, session)); !reflect.DeepEqual(got, []string{"billing/refund", "lookup destination", "ns:op"}) {
		t.Fatalf("listed = %v", got)
	}
	for name, want := range map[string]string{
		"billing/refund":     "refunded",
		"lookup destination": "lookup destination ok",
		"ns:op":              "ns:op ok",
	} {
		res, err := call(session, name)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if resultText(t, res) != want {
			t.Fatalf("%q result = %+v", name, res)
		}
		if s.Calls(name) != 1 || s.Applied(name) != 1 {
			t.Fatalf("%q calls=%d applied=%d", name, s.Calls(name), s.Applied(name))
		}
		var found bool
		for _, r := range s.Requests() {
			found = found || (r.RPCMethod == "tools/call" && r.Tool == name)
		}
		if !found {
			t.Fatalf("no verbatim Request.Tool %q", name)
		}
	}
}

func TestServerForbidden(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{}, simple("pay", "look"))
	s.Forbid("pay")

	// The session survives a 403 (RoundTrip errors do not end it), but a
	// fresh one per attempt keeps this test independent of the client.
	for i := 0; i < 2; i++ {
		if res, err := call(connect(t, s, nil), "pay"); err == nil {
			t.Fatalf("call %d succeeded: %+v", i, res)
		}
	}
	if s.Applied("pay") != 0 {
		t.Fatal("forbidden call applied the effect")
	}
	var n403 int
	for _, r := range s.Requests() {
		if r.Tool == "pay" && r.Status == http.StatusForbidden {
			n403++
		}
	}
	if n403 != 2 || s.Calls("pay") != 2 || s.Count("tools/call") != 2 {
		t.Fatalf("403=%d calls=%d: %+v", n403, s.Calls("pay"), s.Requests())
	}
}

func TestServerForbiddenOtherToolsServed(t *testing.T) {
	s := mcptest.New(t, mcptest.Options{}, simple("pay", "look"))
	s.Forbid("pay")
	session := connect(t, s, nil)
	if got := names(listAll(t, session)); !reflect.DeepEqual(got, []string{"look", "pay"}) {
		t.Fatalf("listed = %v", got)
	}
	if _, err := call(session, "look"); err != nil || s.Applied("look") != 1 {
		t.Fatalf("look: err=%v applied=%d", err, s.Applied("look"))
	}
	s.ClearControls("pay")
	if _, err := call(session, "pay"); err != nil || s.Applied("pay") != 1 {
		t.Fatalf("after clear: err=%v applied=%d", err, s.Applied("pay"))
	}
}
