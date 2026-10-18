package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/mcptool"
	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	"github.com/swiftdiaries/az-agent-platform-go/internal/credential"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/tenantdir"
)

type Client struct{}

var ErrBusinessRejected = errors.New("MCP business rejection")

var ErrAuthRequired = errors.New("MCP authentication required")

// ErrEndpointUnavailable marks an endpoint that is not allowlist-resolved or
// that could not be reached or initialized. Messages never contain the URL.
var ErrEndpointUnavailable = errors.New("MCP endpoint unavailable")

func NewClient() *Client { return &Client{} }

type Bound struct {
	session *protocol.ClientSession
	tools   []tool.FuncTool
	byName  map[string]tool.FuncTool
}

// Bind connects to endpoint, which can only be produced by a tenantdir
// Allowlist. Credentials are forwarded to every bound server, so the transport
// is never built from anything else, and redirects are never followed.
func (c *Client) Bind(ctx context.Context, server definitions.MCPServer, endpoint tenantdir.Endpoint, allowed []string, cred credential.Credential) (*Bound, error) {
	if !endpoint.Valid() {
		return nil, fmt.Errorf("%w: endpoint is not allowlisted", ErrEndpointUnavailable)
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("empty tool allowlist")
	}
	httpClient := &http.Client{
		Transport: &headerTransport{
			base: http.DefaultTransport, credential: cred, callIDHeader: server.CallIDHeader, lifetime: ctx,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	ctx, span := otel.Tracer("az-agent-platform/mcp").Start(ctx, "mcp.discover")
	span.SetAttributes(attribute.String("mcp.server.id", server.ID), attribute.String("mcp.endpoint.id", endpoint.ID()))
	defer span.End()
	session, err := mcptool.Connect(ctx, &protocol.StreamableClientTransport{Endpoint: endpoint.URL(), HTTPClient: httpClient})
	if err != nil {
		return nil, unavailable("connect MCP server", err)
	}
	all, err := mcptool.ListTools(ctx, session)
	if err != nil {
		session.Close()
		return nil, unavailable("list MCP tools", err)
	}
	wanted := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		wanted[name] = true
	}
	bound := &Bound{session: session, byName: make(map[string]tool.FuncTool)}
	for _, listed := range all {
		if !wanted[listed.Name()] {
			continue
		}
		callable, ok := listed.(tool.FuncTool)
		if !ok {
			session.Close()
			return nil, fmt.Errorf("allowed MCP tool %q is not callable", listed.Name())
		}
		bound.tools = append(bound.tools, callable)
		bound.byName[callable.Name()] = callable
	}
	for _, name := range allowed {
		if _, ok := bound.byName[name]; !ok {
			session.Close()
			return nil, fmt.Errorf("allowed MCP tool %q was not registered by server", name)
		}
	}
	return bound, nil
}

// unavailable classifies a transport or protocol failure as a dependency
// failure, keeping ErrAuthRequired's identity. The cause text can embed the URL
// (net/http errors do), so it is deliberately not included.
func unavailable(op string, err error) error {
	if errors.Is(err, ErrAuthRequired) {
		return fmt.Errorf("%s: %w", op, ErrAuthRequired)
	}
	return fmt.Errorf("%w: %s failed", ErrEndpointUnavailable, op)
}

// ServerInfo returns the name and version the server reported at initialize.
func (b *Bound) ServerInfo() (name, version string) {
	if b.session == nil {
		return "", ""
	}
	if result := b.session.InitializeResult(); result != nil && result.ServerInfo != nil {
		return result.ServerInfo.Name, result.ServerInfo.Version
	}
	return "", ""
}

func (b *Bound) Tools() []tool.FuncTool { return slices.Clone(b.tools) }

func (b *Bound) Call(ctx context.Context, callID, name string, arguments []byte) (any, error) {
	_, ok := b.byName[name]
	if !ok {
		return nil, fmt.Errorf("tool %q is not allowed", name)
	}
	ctx, span := otel.Tracer("az-agent-platform/mcp").Start(ctx, "mcp.call")
	span.SetAttributes(attribute.String("call.id", callID), attribute.String("tool.name", name))
	defer span.End()
	result, err := b.session.CallTool(context.WithValue(ctx, callIDKey{}, callID), &protocol.CallToolParams{Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		return nil, err
	}
	if result.IsError {
		return nil, ErrBusinessRejected
	}
	return result, nil
}

func (b *Bound) Close() error { return b.session.Close() }

type callIDKey struct{}

type headerTransport struct {
	lifetime     context.Context
	base         http.RoundTripper
	credential   credential.Credential
	callIDHeader string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(t.lifetime, cancel)
	if t.lifetime.Err() != nil {
		cancel()
	}
	release := func() { stop(); cancel() }
	clone := req.Clone(ctx)
	clone.Header = req.Header.Clone()
	t.credential.Apply(clone)
	if t.callIDHeader != "" {
		if callID, _ := req.Context().Value(callIDKey{}).(string); callID != "" {
			clone.Header.Set(t.callIDHeader, callID)
		}
	}
	// Inject only W3C trace context; prompts, tool arguments, credentials and
	// business identifiers never enter propagation headers.
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(clone.Header))
	response, err := t.base.RoundTrip(clone)
	if err != nil {
		release()
	} else {
		response.Body = &sessionBody{ReadCloser: response.Body, release: release}
	}
	if err == nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
		response.Body.Close()
		return nil, ErrAuthRequired
	}
	return response, err
}

// SDK session cleanup/reconnect can outlive the request context. Keep HTTP bodies
// usable until Close, but cancel every session request when the owning run ends.
type sessionBody struct {
	io.ReadCloser
	release func()
}

func (b *sessionBody) Close() error {
	defer b.release()
	return b.ReadCloser.Close()
}
