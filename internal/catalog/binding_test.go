package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/swiftdiaries/az-agent-platform-go/internal/credential"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/mcptest"
	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
	"github.com/swiftdiaries/az-agent-platform-go/internal/tenantdir"
)

const callIDHeader = "X-Platform-Call-ID"

type journeySpec struct {
	id  string
	ops []string
}

// compileSet writes a config with one read_only op selector per op key and
// compiles it.
func compileSet(t *testing.T, journeys ...journeySpec) *definitions.Set {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	fmt.Fprintf(&b, "catalog:\n  operation_meta_key: %s\n  call_id_header: %s\njourneys:\n", mcptest.DefaultMetaKey, callIDHeader)
	for _, j := range journeys {
		skillDir := filepath.Join(dir, "skills", j.id)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatal(err)
		}
		skill := fmt.Sprintf("---\nname: %s\ndescription: skill %s\n---\nBody.\n", j.id, j.id)
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "  - id: %s\n    skill: skills/%s\n    tools:\n", j.id, j.id)
		for _, op := range j.ops {
			fmt.Fprintf(&b, "      - op: %s\n        class: read_only\n", op)
		}
	}
	path := filepath.Join(dir, "journeys.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := definitions.Compile(path)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func lookupSet(t *testing.T) *definitions.Set {
	return compileSet(t, journeySpec{"j", []string{"travel.lookup"}})
}

func endpointFor(t *testing.T, id, url string) tenantdir.Endpoint {
	t.Helper()
	allow, err := tenantdir.NewAllowlist(map[string]string{id: url})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := allow.Endpoint(id)
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func cookieCredential() credential.Credential {
	probe, _ := http.NewRequest(http.MethodGet, "http://probe.invalid/", nil)
	probe.Header.Set("Cookie", "s=sentinel")
	return credential.FromRequest(probe, []string{"Cookie"})
}

type env struct {
	pool   *pgxpool.Pool
	store  *journal.Store
	server *mcptest.Server
	scope  Scope
	set    *definitions.Set
}

func lookupTool() mcptest.Tool {
	return mcptest.Tool{Name: "travel/lookup", Description: "Look up", OperationKey: "travel.lookup"}
}

func newEnv(t *testing.T, opts mcptest.Options, tools ...mcptest.Tool) *env {
	t.Helper()
	if len(tools) == 0 {
		tools = []mcptest.Tool{lookupTool()}
	}
	pool := pgtest.Database(t)
	if err := journal.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	server := mcptest.New(t, opts, mcptest.Catalog{Tools: tools})
	return &env{
		pool:   pool,
		store:  journal.New(pool),
		server: server,
		scope:  Scope{ProductVersion: "2026.2", Endpoint: endpointFor(t, "ep-1", server.URL())},
		set:    lookupSet(t),
	}
}

func (e *env) claim(t *testing.T, run string) journal.Owner {
	t.Helper()
	cmd := journal.Command{ThreadID: "t1", RunID: run, CommunicationID: "c-" + run, Principal: "p", Tenant: "tenant-a", Text: "hi"}
	if _, fresh, err := e.store.Admit(t.Context(), cmd); err != nil || !fresh {
		t.Fatalf("Admit: fresh=%v err=%v", fresh, err)
	}
	owner, err := e.store.Claim(t.Context(), cmd, "owner-1", journal.LeaseDuration)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

// next completes first's run (its command consumed) and claims a new run in
// the same thread.
func (e *env) next(t *testing.T, first journal.Owner, run string) journal.Owner {
	t.Helper()
	if _, err := e.pool.Exec(t.Context(), "UPDATE agent_commands SET included=true WHERE thread_id='t1'"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Finish(t.Context(), first, journal.RunCompleted, nil, "", "", ""); err != nil {
		t.Fatal(err)
	}
	return e.claim(t, run)
}

func (e *env) open(t *testing.T, owner journal.Owner, opts Options) *Binding {
	t.Helper()
	b, err := Open(t.Context(), e.scope, cookieCredential(), e.store, owner, e.set, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func (e *env) run(t *testing.T, id string) journal.Run {
	t.Helper()
	snap, err := e.store.Snapshot(t.Context(), "t1", "p")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range snap.Runs {
		if r.RunID == id {
			return r
		}
	}
	t.Fatalf("run %s not found", id)
	return journal.Run{}
}

func (e *env) changedEvents(t *testing.T) int {
	t.Helper()
	events, err := e.store.Events(t.Context(), "t1", "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range events {
		if ev.Type == "catalog_changed" {
			n++
		}
	}
	return n
}

func TestOpenRejectsUnallowlistedEndpoint(t *testing.T) {
	var requests atomic.Int64
	sentinel := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer sentinel.Close()

	e := newEnv(t, mcptest.Options{})
	owner := e.claim(t, "r1")
	var zero tenantdir.Endpoint
	_, err := Open(t.Context(), Scope{ProductVersion: "1", Endpoint: zero}, credential.Credential{}, e.store, owner, e.set, Options{})
	if !errors.Is(err, ErrEndpointUnavailable) {
		t.Fatalf("Open error = %v, want ErrEndpointUnavailable", err)
	}
	if strings.Contains(err.Error(), sentinel.URL) {
		t.Fatalf("error leaks a URL: %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("sentinel received %d requests, want 0", got)
	}
	if got := len(e.server.Requests()); got != 0 {
		t.Fatalf("server received %d requests, want 0", got)
	}
}

func TestOpenRejectsMissingDependencies(t *testing.T) {
	e := newEnv(t, mcptest.Options{})
	owner := e.claim(t, "r1")
	if _, err := Open(t.Context(), e.scope, credential.Credential{}, e.store, owner, nil, Options{}); err == nil {
		t.Fatal("nil set accepted")
	}
	if _, err := Open(t.Context(), e.scope, credential.Credential{}, nil, owner, e.set, Options{}); err == nil {
		t.Fatal("nil store accepted")
	}
	if got := len(e.server.Requests()); got != 0 {
		t.Fatalf("server received %d requests, want 0", got)
	}
}

func TestOpenDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	var redirectorHits, credentialHits atomic.Int64
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectorHits.Add(1)
		if r.Header.Get("Cookie") == "s=sentinel" {
			credentialHits.Add(1)
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	e := newEnv(t, mcptest.Options{})
	e.scope.Endpoint = endpointFor(t, "redirecting", redirector.URL)
	owner := e.claim(t, "r1")
	_, err := Open(t.Context(), e.scope, cookieCredential(), e.store, owner, e.set, Options{})
	if err == nil {
		t.Fatal("Open followed or accepted a redirect")
	}
	if redirectorHits.Load() == 0 || credentialHits.Load() == 0 {
		t.Fatalf("allowlisted endpoint saw %d requests, %d with the credential; want the credential delivered there", redirectorHits.Load(), credentialHits.Load())
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("redirect target received %d requests, want 0", got)
	}
	if !errors.Is(err, ErrEndpointUnavailable) {
		t.Fatalf("Open error = %v, want ErrEndpointUnavailable", err)
	}
	if strings.Contains(err.Error(), redirector.URL) || strings.Contains(err.Error(), target.URL) {
		t.Fatalf("error leaks a URL: %v", err)
	}
}

func TestOpenRecordsServerInfo(t *testing.T) {
	e := newEnv(t, mcptest.Options{Name: "java-mcp", Version: "v7"})
	owner := e.claim(t, "r1")
	b := e.open(t, owner, Options{})
	if name, version := b.ServerInfo(); name != "java-mcp" || version != "v7" {
		t.Fatalf("ServerInfo = %q %q", name, version)
	}
	var info struct{ Name, Version string }
	if err := json.Unmarshal(e.run(t, "r1").ServerInfo, &info); err != nil {
		t.Fatal(err)
	}
	if info.Name != "java-mcp" || info.Version != "v7" {
		t.Fatalf("recorded server info = %+v", info)
	}
}

func TestOpenCapturesPaginatedSnapshot(t *testing.T) {
	var tools []mcptest.Tool
	for i := range 5 {
		tools = append(tools, mcptest.Tool{
			Name: fmt.Sprintf("tool-%d", i), Description: fmt.Sprintf("d%d", i), OperationKey: fmt.Sprintf("op.%d", i),
			Tags:        []string{"t"},
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "title"},
			Meta:        map[string]any{"extra": "kept", "n": 3},
		})
	}
	e := newEnv(t, mcptest.Options{PageSize: 2}, tools...)
	owner := e.claim(t, "r1")
	b := e.open(t, owner, Options{})

	if got := e.server.Count("tools/list"); got != 3 {
		t.Fatalf("tools/list count = %d, want 3", got)
	}
	pin, ok, err := e.store.ConversationPin(t.Context(), owner)
	if err != nil || !ok {
		t.Fatalf("ConversationPin ok=%v err=%v", ok, err)
	}
	var descriptors []map[string]any
	if err := json.Unmarshal(pin.Descriptors, &descriptors); err != nil {
		t.Fatal(err)
	}
	if len(descriptors) != 5 {
		t.Fatalf("pinned %d descriptors, want 5", len(descriptors))
	}
	for _, d := range descriptors {
		meta, _ := d["_meta"].(map[string]any)
		ann, _ := d["annotations"].(map[string]any)
		if meta["extra"] != "kept" || meta[mcptest.DefaultMetaKey] == nil || meta["tags"] == nil || ann["readOnlyHint"] != true {
			t.Fatalf("descriptor lost data: %v", d)
		}
	}
	digest, version := b.Snapshot()
	if digest == "" || digest != pin.Digest || version != "2026.2" {
		t.Fatalf("Snapshot = %q %q, pin digest %q", digest, version, pin.Digest)
	}
	if got := e.run(t, "r1").CatalogDigest; got != digest {
		t.Fatalf("run CatalogDigest = %q, want %q", got, digest)
	}
	reqs := e.server.Requests()
	if len(reqs) == 0 {
		t.Fatal("no requests")
	}
	for _, r := range reqs {
		if r.Header.Get("Cookie") != "s=sentinel" {
			t.Fatalf("%s %s lacked the credential", r.HTTPMethod, r.RPCMethod)
		}
	}
}

func TestOpenRevalidatesWithoutList(t *testing.T) {
	e := newEnv(t, mcptest.Options{})
	first := e.claim(t, "r1")
	b1 := e.open(t, first, Options{})
	digest, _ := b1.Snapshot()
	inits, lists := e.server.Count("initialize"), e.server.Count("tools/list")

	second := e.next(t, first, "r2")
	b2 := e.open(t, second, Options{})
	if got := e.server.Count("initialize"); got <= inits {
		t.Fatalf("initialize count %d did not grow from %d", got, inits)
	}
	if got := e.server.Count("tools/list"); got != lists {
		t.Fatalf("tools/list count = %d, want unchanged %d", got, lists)
	}
	if d, _ := b2.Snapshot(); d != digest {
		t.Fatalf("second digest = %q, want %q", d, digest)
	}
	if got := e.run(t, "r2").CatalogDigest; got != digest {
		t.Fatalf("r2 CatalogDigest = %q, want %q", got, digest)
	}
	if _, ok := b2.Tool("j", "travel_lookup"); !ok {
		t.Fatal("reused pin did not resolve travel_lookup")
	}
}

func TestOpenRelists(t *testing.T) {
	cases := []struct {
		name        string
		change      func(*env)
		opts        Options
		wantChanged int
	}{
		{"product version", func(e *env) { e.scope.ProductVersion = "2026.3" }, Options{}, 1},
		{"server version", func(e *env) { e.server.SetVersion("v2") }, Options{}, 1},
		{"negative ttl", func(*env) {}, Options{TTL: -1}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, mcptest.Options{})
			first := e.claim(t, "r1")
			b1 := e.open(t, first, tc.opts)
			digest1, _ := b1.Snapshot()
			lists := e.server.Count("tools/list")

			tc.change(e)
			second := e.next(t, first, "r2")
			b2 := e.open(t, second, tc.opts)
			if got := e.server.Count("tools/list"); got <= lists {
				t.Fatalf("tools/list count = %d, want > %d", got, lists)
			}
			digest2, _ := b2.Snapshot()
			if (digest1 != digest2) != (tc.wantChanged == 1) {
				t.Fatalf("digests %q %q, wantChanged=%d", digest1, digest2, tc.wantChanged)
			}
			if got := e.changedEvents(t); got != tc.wantChanged {
				t.Fatalf("catalog_changed events = %d, want %d", got, tc.wantChanged)
			}
		})
	}
}

func TestOpenResolvesAliasesAcrossTenants(t *testing.T) {
	for _, remote := range []string{"travel/lookup", "travel lookup"} {
		e := newEnv(t, mcptest.Options{}, mcptest.Tool{Name: remote, OperationKey: "travel.lookup", Description: "d " + remote})
		b := e.open(t, e.claim(t, "r1"), Options{})
		tools := b.Tools("j")
		if len(tools) != 1 || tools[0].Name() != "travel_lookup" || tools[0].Description() != "d "+remote {
			t.Fatalf("remote %q: Tools = %+v", remote, tools)
		}
		st, ok := tools[0].(tool.SchemaTool)
		if !ok {
			t.Fatalf("tool is not a SchemaTool")
		}
		schema, _ := st.Schema().(map[string]any)
		if schema["type"] != "object" || st.ReturnSchema() != nil {
			t.Fatalf("schema = %v return = %v", st.Schema(), st.ReturnSchema())
		}
		if _, ok := b.Tool("j", "travel_lookup"); !ok {
			t.Fatal("Tool(j, travel_lookup) not found")
		}
	}
}

func TestBindingCallSendsRemoteName(t *testing.T) {
	for _, remote := range []string{"travel/lookup", "travel lookup"} {
		t.Run(remote, func(t *testing.T) {
			e := newEnv(t, mcptest.Options{}, mcptest.Tool{Name: remote, OperationKey: "travel.lookup"})
			b := e.open(t, e.claim(t, "r1"), Options{})
			got, err := b.Call(t.Context(), "call-1", "travel_lookup", []byte(`{"x":1}`))
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := got.(*mcp.CallToolResult); !ok {
				t.Fatalf("result = %T", got)
			}
			if e.server.Calls(remote) != 1 {
				t.Fatalf("Calls(%q) = %d", remote, e.server.Calls(remote))
			}
			for _, r := range e.server.Requests() {
				id := r.Header.Get(callIDHeader)
				if r.RPCMethod == "tools/call" {
					if r.Tool != remote || id != "call-1" {
						t.Fatalf("tools/call tool=%q call id=%q", r.Tool, id)
					}
				} else if id != "" {
					t.Fatalf("%s %s carried call id %q", r.HTTPMethod, r.RPCMethod, id)
				}
			}
		})
	}
}

func TestBindingCallUnknownAlias(t *testing.T) {
	e := newEnv(t, mcptest.Options{})
	b := e.open(t, e.claim(t, "r1"), Options{})
	if _, err := b.Call(t.Context(), "call-1", "travel/lookup", nil); !errors.Is(err, ErrNotBound) {
		t.Fatalf("Call error = %v, want ErrNotBound", err)
	}
	if _, err := b.Call(t.Context(), "call-1", "nope", nil); !errors.Is(err, ErrNotBound) {
		t.Fatalf("Call error = %v, want ErrNotBound", err)
	}
	if got := e.server.Count("tools/call"); got != 0 {
		t.Fatalf("tools/call count = %d, want 0", got)
	}
}

func TestBindingCallIsError(t *testing.T) {
	e := newEnv(t, mcptest.Options{})
	e.server.IsError("travel/lookup", "nope")
	b := e.open(t, e.claim(t, "r1"), Options{})
	if _, err := b.Call(t.Context(), "call-1", "travel_lookup", []byte(`{}`)); !errors.Is(err, ErrBusinessRejected) {
		t.Fatalf("Call error = %v, want ErrBusinessRejected", err)
	}
}

func TestBindingCallAuthRequired(t *testing.T) {
	e := newEnv(t, mcptest.Options{})
	e.server.Unauthorized("travel/lookup", -1)
	b := e.open(t, e.claim(t, "r1"), Options{})
	_, err := b.Call(t.Context(), "call-1", "travel_lookup", []byte(`{}`))
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("Call error = %v, want ErrAuthRequired", err)
	}
	if strings.Contains(err.Error(), e.server.URL()) {
		t.Fatalf("error leaks the URL: %v", err)
	}
	calls := e.server.Calls("travel/lookup")
	requests := len(e.server.Requests())
	if _, err := b.Call(t.Context(), "call-2", "travel_lookup", []byte(`{}`)); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("second Call error = %v, want ErrAuthRequired", err)
	}
	if got := e.server.Calls("travel/lookup"); got != calls {
		t.Fatalf("second call reached the server: %d != %d", got, calls)
	}
	if got := len(e.server.Requests()); got != requests {
		t.Fatalf("second call sent %d new requests", got-requests)
	}
}

func TestOpenUnavailableJourney(t *testing.T) {
	e := newEnv(t, mcptest.Options{})
	e.set = compileSet(t,
		journeySpec{"good", []string{"travel.lookup"}},
		journeySpec{"bad", []string{"travel.book"}},
	)
	b := e.open(t, e.claim(t, "r1"), Options{})
	res, ok := b.Resolution("bad")
	if !ok || res.Available() || len(res.Unavailable) != 1 || res.Unavailable[0].Reason != ReasonMissing {
		t.Fatalf("bad resolution = %+v ok=%v", res, ok)
	}
	if got := b.Tools("bad"); len(got) != 0 {
		t.Fatalf("Tools(bad) = %v", got)
	}
	if _, ok := b.Tool("bad", "travel_book"); ok {
		t.Fatal("Tool(bad, travel_book) found")
	}
	if good, ok := b.Resolution("good"); !ok || !good.Available() {
		t.Fatalf("good resolution = %+v", good)
	}
	if got := b.Tools("good", "bad", "unknown"); len(got) != 1 || got[0].Name() != "travel_lookup" {
		t.Fatalf("Tools = %v", got)
	}
	if got := len(b.Resolutions()); got != 2 {
		t.Fatalf("Resolutions = %d", got)
	}
	if _, err := b.Call(t.Context(), "c", "travel_book", nil); !errors.Is(err, ErrNotBound) {
		t.Fatalf("Call of unavailable alias = %v", err)
	}
}

// compileTagSet writes one journey per spec, each with a single tag selector
// of the given class, so policies are known only after resolution.
func compileTagSet(t *testing.T, specs map[string][2]string) *definitions.Set {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	fmt.Fprintf(&b, "catalog:\n  operation_meta_key: %s\n  call_id_header: %s\njourneys:\n", mcptest.DefaultMetaKey, callIDHeader)
	for _, id := range []string{"tagA", "tagB", "plain"} {
		spec, ok := specs[id]
		if !ok {
			continue
		}
		skillDir := filepath.Join(dir, "skills", id)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatal(err)
		}
		skill := fmt.Sprintf("---\nname: %s\ndescription: skill %s\n---\nBody.\n", id, id)
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skill), 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "  - id: %s\n    skill: skills/%s\n    tools:\n      - tags: [%s]\n        class: %s\n", id, id, spec[0], spec[1])
	}
	path := filepath.Join(dir, "journeys.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := definitions.Compile(path)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestOpenPolicyConflict(t *testing.T) {
	e := newEnv(t, mcptest.Options{},
		mcptest.Tool{Name: "travel/book", OperationKey: "travel.book", Tags: []string{"shared"}},
		mcptest.Tool{Name: "travel/lookup", OperationKey: "travel.lookup", Tags: []string{"solo"}},
	)
	e.set = compileTagSet(t, map[string][2]string{
		"tagA":  {"shared", "effectful"},
		"tagB":  {"shared", "idempotent"},
		"plain": {"solo", "read_only"},
	})
	b := e.open(t, e.claim(t, "r1"), Options{})
	for _, id := range []string{"tagA", "tagB"} {
		res, ok := b.Resolution(id)
		if !ok || res.Available() || len(res.Unavailable) != 1 ||
			res.Unavailable[0] != (Unavailable{ReasonPolicyConflict, "travel.book"}) {
			t.Fatalf("%s resolution = %+v ok=%v", id, res, ok)
		}
		if got := b.Tools(id); len(got) != 0 {
			t.Fatalf("Tools(%s) = %v", id, got)
		}
	}
	if res, ok := b.Resolution("plain"); !ok || !res.Available() {
		t.Fatalf("plain resolution = %+v", res)
	}
	if _, ok := b.Tool("plain", "travel_lookup"); !ok {
		t.Fatal("plain journey lost its tool")
	}
}
