package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/mcptool"
	protocol "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
)

type Client struct{}

var ErrAuthRequired = errors.New("MCP authentication required")

func NewClient() *Client { return &Client{} }

type Bound struct {
	session *protocol.ClientSession
	tools   []tool.FuncTool
	byName  map[string]tool.FuncTool
}

func (c *Client) Bind(ctx context.Context, server definitions.MCPServer, allowed []string, source http.Header) (*Bound, error) {
	if len(allowed) == 0 {
		return nil, fmt.Errorf("empty tool allowlist")
	}
	forwarded := make(http.Header)
	for _, name := range server.ForwardHeaders {
		for _, value := range source.Values(name) {
			forwarded.Add(name, value)
		}
	}
	httpClient := &http.Client{
		Transport: &headerTransport{
			base: http.DefaultTransport, headers: forwarded, callIDHeader: server.CallIDHeader,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	ctx, span := otel.Tracer("az-agent-platform/mcp").Start(ctx, "mcp.discover")
	span.SetAttributes(attribute.String("mcp.server.id", server.ID))
	defer span.End()
	session, err := mcptool.Connect(ctx, &protocol.StreamableClientTransport{Endpoint: server.Endpoint, HTTPClient: httpClient})
	if err != nil {
		return nil, fmt.Errorf("connect MCP server: %w", err)
	}
	all, err := mcptool.ListTools(ctx, session)
	if err != nil {
		session.Close()
		return nil, err
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

func (b *Bound) Tools() []tool.FuncTool { return slices.Clone(b.tools) }

func (b *Bound) Call(ctx context.Context, callID, name string, arguments []byte) (any, error) {
	callable, ok := b.byName[name]
	if !ok {
		return nil, fmt.Errorf("tool %q is not allowed", name)
	}
	ctx, span := otel.Tracer("az-agent-platform/mcp").Start(ctx, "mcp.call")
	span.SetAttributes(attribute.String("call.id", callID), attribute.String("tool.name", name))
	defer span.End()
	return callable.Call(context.WithValue(ctx, callIDKey{}, callID), string(arguments))
}

func (b *Bound) Close() error { return b.session.Close() }

type callIDKey struct{}

type headerTransport struct {
	base         http.RoundTripper
	headers      http.Header
	callIDHeader string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	for name, values := range t.headers {
		clone.Header.Del(name)
		for _, value := range values {
			clone.Header.Add(name, value)
		}
	}
	if t.callIDHeader != "" {
		if callID, _ := req.Context().Value(callIDKey{}).(string); callID != "" {
			clone.Header.Set(t.callIDHeader, callID)
		}
	}
	response, err := t.base.RoundTrip(clone)
	if err == nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
		response.Body.Close()
		return nil, ErrAuthRequired
	}
	return response, err
}
