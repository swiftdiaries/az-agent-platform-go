package mcptest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultMetaKey is the default _meta key under which [Tool.OperationKey] is
// published. It is configurable ([Options.MetaKey]) because the real key is an
// integration input.
const DefaultMetaKey = "operation_key"

const (
	// TenantHeader is the header the default [Options.Identify] reads the
	// tenant from.
	TenantHeader = "X-Mcptest-Tenant"
	// PrincipalHeader is the header the default [Options.Identify] reads the
	// principal from.
	PrincipalHeader = "X-Mcptest-Principal"

	// inflightHeader is an internal header the wrapper adds to the request it
	// dispatches so a tool handler (via req.Extra.Header) can find the
	// in-flight HTTP request it is serving. It is never recorded.
	inflightHeader = "X-Mcptest-Inflight"
)

// Tool describes one remote tool served by a [Catalog].
type Tool struct {
	// Name is the remote tool name, served verbatim (it may contain '/', ' '
	// or ':').
	Name        string
	Description string
	// OperationKey, when non-empty, is written to _meta[Options.MetaKey].
	OperationKey string
	// Tags, when non-empty, are written to _meta["tags"].
	Tags []string
	// Meta holds extra _meta entries, merged with OperationKey and Tags
	// (which win on conflict).
	Meta        map[string]any
	Annotations *mcp.ToolAnnotations
	// InputSchema defaults to {"type":"object"}. It must be a value the SDK's
	// Server.AddTool accepts (for example a map[string]any or
	// *jsonschema.Schema).
	InputSchema any
	// Result is the text content of a successful call; it defaults to
	// "<Name> ok".
	Result string
}

// Catalog is the set of tools served to a (Tenant, Principal) at a Version.
type Catalog struct {
	// Tenant and Principal select who the catalog is served to; "" is a
	// wildcard.
	Tenant, Principal string
	// Version restricts the catalog to the server's current version (see
	// [Server.SetVersion]); "" serves at any version.
	Version string
	Tools   []Tool
}

// Options configures [New].
type Options struct {
	// Name is the server info name; default "mcptest".
	Name string
	// Version is the initial server/product version reported in the initialize
	// result's ServerInfo.Version and used to select catalogs; default "v1".
	Version string
	// PageSize is the tools/list page size; zero means [mcp.DefaultPageSize].
	PageSize int
	// MetaKey is the _meta key for operation keys; default [DefaultMetaKey].
	MetaKey string
	// Identify maps an inbound HTTP request to (tenant, principal) for catalog
	// selection and request recording. The default reads the TenantHeader and
	// PrincipalHeader headers (absent means "").
	Identify func(*http.Request) (tenant, principal string)
}

// Request records one inbound HTTP request.
type Request struct {
	HTTPMethod string
	// RPCMethod is the JSON-RPC method of a POST body (for example
	// "initialize", "tools/list", "tools/call"), or "" for GET/DELETE or an
	// unparseable body. Batches are not decoded.
	RPCMethod string
	// Tool is the params.name of a tools/call request, else "".
	Tool              string
	Tenant, Principal string
	// Header is a clone of the inbound request headers.
	Header http.Header
	// Status is the HTTP status the client received. It is 0 while the
	// request is in flight and for responses that were dropped by
	// [Server.DropResponseAfterApply].
	Status int
}

// Server is a go-sdk streamable-HTTP MCP test server. Create one with [New].
type Server struct {
	opts     Options
	identify func(*http.Request) (string, string)
	ts       *httptest.Server
	logger   *slog.Logger
	closed   sync.Once

	mu       sync.Mutex
	version  string
	catalogs []*Catalog
	cache    map[*Catalog]*mcp.Server
	empty    *mcp.Server

	requests []*Request
	counts   map[string]int
	calls    map[string]int
	applied  map[string]int
	controls map[string]*controls
	inflight map[string]*inflight
	nextID   int
}

type controls struct {
	failN   int
	unauthN int // <0: always
	dropN   int
	isError *string
	block   *blocker
}

type inflight struct {
	buffering bool
	drop      atomic.Bool
}

type blocker struct {
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func (b *blocker) doRelease() { b.releaseOnce.Do(func() { close(b.release) }) }

// New starts a test server serving catalogs and registers its Close with
// t.Cleanup.
func New(t testing.TB, opts Options, catalogs ...Catalog) *Server {
	t.Helper()
	if opts.Name == "" {
		opts.Name = "mcptest"
	}
	if opts.Version == "" {
		opts.Version = "v1"
	}
	if opts.MetaKey == "" {
		opts.MetaKey = DefaultMetaKey
	}
	s := &Server{
		opts:     opts,
		identify: opts.Identify,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		version:  opts.Version,
		cache:    map[*Catalog]*mcp.Server{},
		counts:   map[string]int{},
		calls:    map[string]int{},
		applied:  map[string]int{},
		controls: map[string]*controls{},
		inflight: map[string]*inflight{},
	}
	if s.identify == nil {
		s.identify = func(r *http.Request) (string, string) {
			return r.Header.Get(TenantHeader), r.Header.Get(PrincipalHeader)
		}
	}
	for _, c := range catalogs {
		s.addCatalogLocked(c)
	}
	sdk := mcp.NewStreamableHTTPHandler(s.serverFor, nil)
	s.ts = httptest.NewServer(&handler{s: s, next: sdk})
	t.Cleanup(s.Close)
	return s
}

// URL returns the MCP endpoint URL.
func (s *Server) URL() string { return s.ts.URL }

// Close releases every Block, drops open connections and shuts the server
// down. It is idempotent and also runs from t.Cleanup.
func (s *Server) Close() {
	s.closed.Do(func() {
		s.mu.Lock()
		for _, c := range s.controls {
			if c.block != nil {
				c.block.doRelease()
			}
		}
		s.mu.Unlock()
		s.ts.CloseClientConnections()
		s.ts.Close()
	})
}

// SetVersion changes the version reported by later initialize calls and used
// to select catalogs for new sessions. Existing sessions are unaffected.
func (s *Server) SetVersion(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = v
	s.invalidateLocked()
}

// AddCatalog adds a catalog, replacing any existing catalog with the same
// (Tenant, Principal, Version). It applies to new sessions.
func (s *Server) AddCatalog(c Catalog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addCatalogLocked(c)
}

func (s *Server) addCatalogLocked(c Catalog) {
	cp := c
	cp.Tools = append([]Tool(nil), c.Tools...)
	for i, old := range s.catalogs {
		if old.Tenant == c.Tenant && old.Principal == c.Principal && old.Version == c.Version {
			s.catalogs[i] = &cp
			s.invalidateLocked()
			return
		}
	}
	s.catalogs = append(s.catalogs, &cp)
	s.invalidateLocked()
}

func (s *Server) invalidateLocked() {
	s.cache = map[*Catalog]*mcp.Server{}
	s.empty = nil
}

// selectLocked returns the most specific catalog for the identity at the
// current version, or nil. Specificity: exact tenant+principal, then tenant
// with wildcard principal, then principal with wildcard tenant, then full
// wildcard; a version-pinned catalog beats an any-version one.
func (s *Server) selectLocked(tenant, principal string) *Catalog {
	var best *Catalog
	bestScore := -1
	for _, c := range s.catalogs {
		if (c.Tenant != "" && c.Tenant != tenant) || (c.Principal != "" && c.Principal != principal) {
			continue
		}
		if c.Version != "" && c.Version != s.version {
			continue
		}
		score := 0
		if c.Tenant != "" {
			score += 2
		}
		if c.Principal != "" {
			score++
		}
		score *= 2
		if c.Version != "" {
			score++
		}
		if score > bestScore {
			best, bestScore = c, score
		}
	}
	return best
}

// serverFor is the streamable handler's getServer: it runs once per new MCP
// session and returns the cached mcp.Server for the selected catalog.
func (s *Server) serverFor(r *http.Request) *mcp.Server {
	tenant, principal := s.identify(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	cat := s.selectLocked(tenant, principal)
	if cat == nil {
		if s.empty == nil {
			s.empty = s.buildLocked(nil)
		}
		return s.empty
	}
	srv, ok := s.cache[cat]
	if !ok {
		srv = s.buildLocked(cat)
		s.cache[cat] = srv
	}
	return srv
}

func (s *Server) buildLocked(c *Catalog) *mcp.Server {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: s.opts.Name, Version: s.version},
		&mcp.ServerOptions{PageSize: s.opts.PageSize, Logger: s.logger, HasTools: true},
	)
	if c == nil {
		return srv
	}
	for _, t := range c.Tools {
		meta := mcp.Meta{}
		for k, v := range t.Meta {
			meta[k] = v
		}
		if t.OperationKey != "" {
			meta[s.opts.MetaKey] = t.OperationKey
		}
		if len(t.Tags) > 0 {
			meta["tags"] = t.Tags
		}
		if len(meta) == 0 {
			meta = nil
		}
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		srv.AddTool(&mcp.Tool{
			Name: t.Name, Description: t.Description, Meta: meta,
			Annotations: t.Annotations, InputSchema: schema,
		}, s.toolHandler(t))
	}
	return srv
}

// toolHandler is the go-sdk tool handler. Order: Block, IsError, apply.
func (s *Server) toolHandler(t Tool) mcp.ToolHandler {
	result := t.Result
	if result == "" {
		result = t.Name + " ok"
	}
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.mu.Lock()
		var blk *blocker
		var errText *string
		if c := s.controls[t.Name]; c != nil {
			blk, errText = c.block, c.isError
		}
		var inf *inflight
		if req.Extra != nil {
			inf = s.inflight[req.Extra.Header.Get(inflightHeader)]
		}
		s.mu.Unlock()

		if blk != nil {
			blk.startedOnce.Do(func() { close(blk.started) })
			select {
			case <-blk.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if errText != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: *errText}}}, nil
		}

		s.mu.Lock()
		s.applied[t.Name]++
		if inf != nil && inf.buffering {
			if c := s.controls[t.Name]; c != nil && c.dropN > 0 {
				c.dropN--
				inf.drop.Store(true)
			}
		}
		s.mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil
	}
}

func (s *Server) ctl(tool string) *controls {
	c := s.controls[tool]
	if c == nil {
		c = &controls{}
		s.controls[tool] = c
	}
	return c
}

// Block makes calls to tool wait inside the handler, before the effect is
// applied. started is closed when a call first reaches the handler; release
// lets blocked and later calls proceed and is idempotent (it also runs on
// Close). A call whose context ends while blocked fails without applying.
// Calling Block again replaces the previous block.
func (s *Server) Block(tool string) (started <-chan struct{}, release func()) {
	b := &blocker{started: make(chan struct{}), release: make(chan struct{})}
	s.mu.Lock()
	c := s.ctl(tool)
	if c.block != nil {
		c.block.doRelease()
	}
	c.block = b
	s.mu.Unlock()
	return b.started, b.doRelease
}

// FailTransient makes the next n tools/call HTTP requests for tool answer
// HTTP 503 before reaching the SDK: the effect is not applied.
func (s *Server) FailTransient(tool string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctl(tool).failN = n
}

// DropResponseAfterApply makes the next n calls to tool apply their effect
// (Applied is incremented) and then lose the HTTP response: the connection is
// closed without writing any response, so the client sees a transport error.
//
// Mechanism: for tools/call requests of a tool with a pending drop, the
// wrapper hands the SDK a buffering ResponseWriter and tags the dispatched
// request with an internal header that the tool handler can map back to the
// in-flight request. After the apply point the handler marks the request
// dropped; when the SDK finishes, the wrapper discards the buffer and calls
// panic(http.ErrAbortHandler), which net/http turns into a connection close
// without a response. (Hijacking was not needed.) A call that does not reach
// the apply point (Block cancelled, IsError) is answered normally and does not
// consume the drop.
func (s *Server) DropResponseAfterApply(tool string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctl(tool).dropN = n
}

// IsError makes calls to tool return a CallToolResult with IsError set and
// text as its content. The effect is not applied.
func (s *Server) IsError(tool string, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctl(tool).isError = &text
}

// Unauthorized makes the next n tools/call HTTP requests for tool answer HTTP
// 401 before reaching the SDK; n < 0 means always.
func (s *Server) Unauthorized(tool string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctl(tool).unauthN = n
}

// ClearControls removes every control for tool, releasing any Block.
func (s *Server) ClearControls(tool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.controls[tool]; c != nil && c.block != nil {
		c.block.doRelease()
	}
	delete(s.controls, tool)
}

// Requests returns copies of every recorded HTTP request in arrival order,
// including notifications, GET and DELETE.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	for i, r := range s.requests {
		out[i] = *r
		out[i].Header = r.Header.Clone()
	}
	return out
}

// Count returns the number of JSON-RPC messages received with rpcMethod.
func (s *Server) Count(rpcMethod string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[rpcMethod]
}

// Calls returns the number of tools/call HTTP requests received for tool,
// including failed ones.
func (s *Server) Calls(tool string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[tool]
}

// Applied returns how many times tool's effect actually executed.
func (s *Server) Applied(tool string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applied[tool]
}

// Reset clears recorded requests and counters, but not controls or catalogs.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
	s.counts = map[string]int{}
	s.calls = map[string]int{}
	s.applied = map[string]int{}
}

// handler is the capture/fault-injection wrapper in front of the SDK handler.
type handler struct {
	s    *Server
	next http.Handler
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s := h.s
	var rpcMethod, tool string
	if r.Method == http.MethodPost && r.Body != nil {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		var env struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &env) == nil { // batches (arrays) fail to decode and are ignored
			rpcMethod = env.Method
			if rpcMethod == "tools/call" {
				tool = env.Params.Name
			}
		}
	}
	tenant, principal := s.identify(r)
	rec := &Request{HTTPMethod: r.Method, RPCMethod: rpcMethod, Tool: tool, Tenant: tenant, Principal: principal, Header: r.Header.Clone()}

	sw := &statusWriter{ResponseWriter: w}
	s.mu.Lock()
	s.requests = append(s.requests, rec)
	if rpcMethod != "" {
		s.counts[rpcMethod]++
	}
	status := 0
	var inf *inflight
	var infID string
	if rpcMethod == "tools/call" {
		s.calls[tool]++
		if c := s.controls[tool]; c != nil {
			switch {
			case c.unauthN != 0:
				if c.unauthN > 0 {
					c.unauthN--
				}
				status = http.StatusUnauthorized
			case c.failN > 0:
				c.failN--
				status = http.StatusServiceUnavailable
			}
			if status == 0 && c.dropN > 0 {
				inf = &inflight{buffering: true}
			}
		}
		if status == 0 {
			if inf == nil {
				inf = &inflight{}
			}
			s.nextID++
			infID = strconv.Itoa(s.nextID)
			s.inflight[infID] = inf
		}
	}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		rec.Status = sw.status
		delete(s.inflight, infID)
		s.mu.Unlock()
	}()

	if status != 0 {
		http.Error(sw, http.StatusText(status), status)
		return
	}

	dispatch := r
	if infID != "" {
		dispatch = r.Clone(r.Context())
		dispatch.Header.Set(inflightHeader, infID)
	}
	if inf == nil || !inf.buffering {
		h.next.ServeHTTP(sw, dispatch)
		return
	}
	bw := &bufferedWriter{header: http.Header{}}
	h.next.ServeHTTP(bw, dispatch)
	if inf.drop.Load() {
		panic(http.ErrAbortHandler)
	}
	bw.replay(sw)
}

// statusWriter records the status code written to the client.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// bufferedWriter holds a whole response so it can be discarded or replayed.
type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedWriter) Header() http.Header { return b.header }

func (b *bufferedWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *bufferedWriter) Flush() {}

func (b *bufferedWriter) replay(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	if b.status != 0 {
		w.WriteHeader(b.status)
	}
	_, _ = w.Write(b.body.Bytes())
}
