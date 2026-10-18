package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/swiftdiaries/az-agent-platform-go/internal/credential"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/tenantdir"
)

// DefaultSnapshotTTL is how long a conversation's pinned snapshot is reused
// without re-listing when Options.TTL is zero.
const DefaultSnapshotTTL = 10 * time.Minute

var (
	// ErrBusinessRejected marks a tool result the server flagged isError.
	ErrBusinessRejected = errors.New("MCP business rejection")
	// ErrAuthRequired marks a 401 on a call, or a 401/403 while connecting or
	// listing tools, from the integration endpoint.
	ErrAuthRequired = errors.New("MCP authentication required")
	// ErrNotPermitted marks a 403 from the integration endpoint on tools/call.
	ErrNotPermitted = errors.New("MCP operation not permitted")
	// ErrEndpointUnavailable marks an endpoint that is not allowlist-resolved
	// or that could not be reached or initialized. Messages never contain the
	// URL.
	ErrEndpointUnavailable = errors.New("MCP endpoint unavailable")
	// ErrNotBound marks a call to an alias the binding did not resolve.
	ErrNotBound = errors.New("tool is not bound")
)

// Scope identifies the integration a binding talks to.
type Scope struct {
	ProductVersion string
	// Endpoint can only be produced by a tenantdir.Allowlist.
	Endpoint tenantdir.Endpoint
}

// Options tunes Open.
type Options struct {
	// TTL bounds how long a pinned snapshot is reused without re-listing.
	// Zero means DefaultSnapshotTTL; negative means always re-list.
	TTL time.Duration
}

// Binding is an open MCP session plus the journeys resolved against the
// catalog snapshot it saw. It is safe for sequential use.
type Binding struct {
	session        *mcp.ClientSession
	closeOnce      sync.Once
	closeErr       error
	resolutions    []Resolution
	byAlias        map[string]Tool
	digest         string
	productVersion string
	serverName     string
	serverVersion  string
	broken         atomic.Bool
}

var tracer = func() trace.Tracer { return otel.Tracer("az-agent-platform/catalog") }

// Open connects to scope.Endpoint, captures or revalidates the conversation's
// catalog snapshot in the journal and resolves every journey of set against it.
func Open(ctx context.Context, scope Scope, cred credential.Credential, store *journal.Store, owner journal.Owner, set *definitions.Set, opts Options) (*Binding, error) {
	if !scope.Endpoint.Valid() {
		return nil, fmt.Errorf("%w: endpoint is not allowlisted", ErrEndpointUnavailable)
	}
	if set == nil {
		return nil, errors.New("catalog: journey set is required")
	}
	if store == nil {
		return nil, errors.New("catalog: journal store is required")
	}
	ctx, span := tracer().Start(ctx, "catalog.resolve")
	defer span.End()
	span.SetAttributes(attribute.String("mcp.endpoint.id", scope.Endpoint.ID()), attribute.String("product.version", scope.ProductVersion))

	cfg := set.Catalog()
	httpClient := &http.Client{
		Transport: &headerTransport{
			base: http.DefaultTransport, credential: cred, callIDHeader: cfg.CallIDHeader, lifetime: ctx,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "az-agent-platform", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: scope.Endpoint.URL(), HTTPClient: httpClient}, nil)
	if err != nil {
		return nil, unavailable("connect MCP server", err)
	}
	b, err := open(ctx, session, scope, store, owner, set, cfg, opts)
	if err != nil {
		session.Close()
		return nil, err
	}
	span.SetAttributes(attribute.String("catalog.digest", b.digest))
	return b, nil
}

func open(ctx context.Context, session *mcp.ClientSession, scope Scope, store *journal.Store, owner journal.Owner, set *definitions.Set, cfg definitions.CatalogConfig, opts Options) (*Binding, error) {
	var serverName, serverVersion string
	if result := session.InitializeResult(); result != nil && result.ServerInfo != nil {
		serverName, serverVersion = result.ServerInfo.Name, result.ServerInfo.Version
	}
	info, err := json.Marshal(struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}{serverName, serverVersion})
	if err != nil {
		return nil, err
	}
	if err := store.RecordServerInfo(ctx, owner, info); err != nil {
		return nil, err
	}

	ttl := opts.TTL
	if ttl == 0 {
		ttl = DefaultSnapshotTTL
	}
	pin, pinned, err := store.ConversationPin(ctx, owner)
	if err != nil {
		return nil, err
	}
	var digest string
	var descriptors json.RawMessage
	if pinned && ttl > 0 && pin.Age < ttl &&
		pin.ProductVersion == scope.ProductVersion && pin.EndpointID == scope.Endpoint.ID() && pin.ServerVersion == serverVersion {
		if err := store.RecordRunCatalog(ctx, owner, pin.Digest); err != nil {
			return nil, err
		}
		digest, descriptors = pin.Digest, pin.Descriptors
	} else {
		listed := []*mcp.Tool{}
		for t, err := range session.Tools(ctx, nil) {
			if err != nil {
				return nil, unavailable("list MCP tools", err)
			}
			listed = append(listed, t)
		}
		descriptors, err = json.Marshal(listed)
		if err != nil {
			return nil, fmt.Errorf("encode catalog descriptors: %w", err)
		}
		digest, err = store.CommitSnapshot(ctx, owner, journal.CatalogSnapshot{
			ProductVersion: scope.ProductVersion, EndpointID: scope.Endpoint.ID(), ServerVersion: serverVersion, Descriptors: descriptors,
		})
		if err != nil {
			return nil, err
		}
	}

	tools, err := ParseDescriptors(descriptors)
	if err != nil {
		return nil, fmt.Errorf("catalog snapshot is malformed: %w", err)
	}
	snapshot := Snapshot{Digest: digest, ProductVersion: scope.ProductVersion, EndpointID: scope.Endpoint.ID(), ServerVersion: serverVersion, Tools: tools}
	b := &Binding{
		session: session, byAlias: map[string]Tool{}, digest: digest, productVersion: scope.ProductVersion,
		serverName: serverName, serverVersion: serverVersion,
	}
	journeys := set.Journeys()
	resolved := make([]Resolution, len(journeys))
	for i, journey := range journeys {
		resolved[i] = Resolve(journey, snapshot, cfg.OperationMetaKey)
	}
	b.resolutions = CheckPolicies(resolved)
	for _, res := range b.resolutions {
		if !res.Available() {
			continue
		}
		for _, t := range res.Tools {
			if prior, ok := b.byAlias[t.Alias]; ok {
				if prior.RemoteName != t.RemoteName {
					return nil, fmt.Errorf("catalog: alias %q resolves to different tools across journeys", t.Alias)
				}
				continue
			}
			b.byAlias[t.Alias] = t
		}
	}
	return b, nil
}

// Resolutions returns one resolution per journey, in set order.
func (b *Binding) Resolutions() []Resolution { return slices.Clone(b.resolutions) }

// Resolution returns the resolution of one journey.
func (b *Binding) Resolution(journeyID string) (Resolution, bool) {
	for _, r := range b.resolutions {
		if r.JourneyID == journeyID {
			return r, true
		}
	}
	return Resolution{}, false
}

// Tool returns a resolved tool of an available journey.
func (b *Binding) Tool(journeyID, alias string) (Tool, bool) {
	r, ok := b.Resolution(journeyID)
	if !ok || !r.Available() {
		return Tool{}, false
	}
	for _, t := range r.Tools {
		if t.Alias == alias {
			return t, true
		}
	}
	return Tool{}, false
}

// Tools returns the model-facing tools of the listed available journeys,
// de-duplicated by alias, in journey order then alias order.
func (b *Binding) Tools(journeyIDs ...string) []tool.Tool {
	var out []tool.Tool
	seen := map[string]bool{}
	for _, r := range b.resolutions {
		if !r.Available() || !slices.Contains(journeyIDs, r.JourneyID) {
			continue
		}
		for _, t := range r.Tools {
			if seen[t.Alias] {
				continue
			}
			seen[t.Alias] = true
			var schema any
			if len(t.InputSchema) > 0 {
				if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
					schema = nil
				}
			}
			out = append(out, schemaTool{name: t.Alias, description: t.Description, schema: schema})
		}
	}
	return out
}

type schemaTool struct {
	name, description string
	schema            any
}

func (s schemaTool) Name() string        { return s.name }
func (s schemaTool) Description() string { return s.description }
func (s schemaTool) Schema() any         { return s.schema }
func (s schemaTool) ReturnSchema() any   { return nil }

// Call invokes the tool bound to alias, sending the remote name.
func (b *Binding) Call(ctx context.Context, callID, alias string, arguments []byte) (any, error) {
	t, ok := b.byAlias[alias]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotBound, alias)
	}
	if b.broken.Load() {
		return nil, ErrAuthRequired
	}
	ctx, span := tracer().Start(ctx, "mcp.call")
	defer span.End()
	span.SetAttributes(attribute.String("call.id", callID), attribute.String("tool.alias", alias), attribute.String("operation.key", t.OperationKey))
	result, err := b.session.CallTool(context.WithValue(ctx, callIDKey{}, callID), &mcp.CallToolParams{Name: t.RemoteName, Arguments: json.RawMessage(arguments)})
	if err != nil {
		if errors.Is(err, ErrAuthRequired) {
			b.broken.Store(true)
			return nil, fmt.Errorf("call MCP tool: %w", ErrAuthRequired)
		}
		if errors.Is(err, ErrNotPermitted) {
			return nil, fmt.Errorf("call MCP tool: %w", ErrNotPermitted)
		}
		return nil, err
	}
	if result.IsError {
		return nil, ErrBusinessRejected
	}
	return result, nil
}

// Snapshot returns the catalog digest and product version of the binding.
func (b *Binding) Snapshot() (digest, productVersion string) { return b.digest, b.productVersion }

// ServerInfo returns the name and version the server reported at initialize.
func (b *Binding) ServerInfo() (name, version string) { return b.serverName, b.serverVersion }

// Close closes the MCP session. It is idempotent.
func (b *Binding) Close() error {
	b.closeOnce.Do(func() { b.closeErr = b.session.Close() })
	return b.closeErr
}
