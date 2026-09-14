package definitions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
)

type MCPServer struct {
	ID             string   `json:"id"`
	Endpoint       string   `json:"endpoint"`
	ForwardHeaders []string `json:"forward_headers"`
	CallIDHeader   string   `json:"call_id_header"`
}

type MCPBinding struct {
	Server string   `json:"server"`
	Tools  []string `json:"tools"`
}

type Journey struct {
	ID           string     `json:"id"`
	Description  string     `json:"description"`
	SystemPrompt string     `json:"system_prompt"`
	MCP          MCPBinding `json:"mcp"`
	Prompt       string     `json:"-"`
}

type Registry struct {
	servers  map[string]MCPServer
	journeys map[string]Journey
	order    []string
}

func Load(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read journey config: %w", err)
	}
	var raw struct {
		MCPServers []MCPServer `json:"mcp_servers"`
		Journeys   []Journey   `json:"journeys"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode journey config (JSON syntax): %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode journey config: trailing value")
	}

	registry := &Registry{servers: make(map[string]MCPServer), journeys: make(map[string]Journey)}
	for _, server := range raw.MCPServers {
		if server.ID == "" || server.Endpoint == "" {
			return nil, fmt.Errorf("MCP server id and endpoint are required")
		}
		if _, exists := registry.servers[server.ID]; exists {
			return nil, fmt.Errorf("duplicate MCP server %q", server.ID)
		}
		endpoint, err := expandEndpoint(server.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("MCP server %q: %w", server.ID, err)
		}
		server.Endpoint = endpoint
		seenHeaders := make(map[string]bool)
		for i, name := range server.ForwardHeaders {
			canonical := textproto.CanonicalMIMEHeaderKey(name)
			if canonical == "" || strings.ContainsAny(name, "\r\n:") || seenHeaders[canonical] {
				return nil, fmt.Errorf("MCP server %q has invalid or duplicate forwarded header %q", server.ID, name)
			}
			seenHeaders[canonical] = true
			server.ForwardHeaders[i] = canonical
		}
		if server.CallIDHeader != "" {
			server.CallIDHeader = textproto.CanonicalMIMEHeaderKey(server.CallIDHeader)
			if server.CallIDHeader == "" {
				return nil, fmt.Errorf("MCP server %q has invalid call ID header", server.ID)
			}
		}
		registry.servers[server.ID] = server
	}

	root := filepath.Dir(path)
	for _, journey := range raw.Journeys {
		if journey.ID == "" || journey.Description == "" || journey.SystemPrompt == "" {
			return nil, fmt.Errorf("journey id, description, and system prompt are required")
		}
		if _, exists := registry.journeys[journey.ID]; exists {
			return nil, fmt.Errorf("duplicate journey %q", journey.ID)
		}
		if _, exists := registry.servers[journey.MCP.Server]; !exists {
			return nil, fmt.Errorf("journey %q references unknown MCP server %q", journey.ID, journey.MCP.Server)
		}
		if len(journey.MCP.Tools) == 0 {
			return nil, fmt.Errorf("journey %q has empty tool allowlist", journey.ID)
		}
		seenTools := make(map[string]bool)
		for _, name := range journey.MCP.Tools {
			if strings.TrimSpace(name) == "" || seenTools[name] {
				return nil, fmt.Errorf("journey %q has invalid or duplicate tool %q", journey.ID, name)
			}
			seenTools[name] = true
		}
		promptPath, err := confinedPath(root, journey.SystemPrompt)
		if err != nil {
			return nil, fmt.Errorf("journey %q prompt: %w", journey.ID, err)
		}
		prompt, err := os.ReadFile(promptPath)
		if err != nil {
			return nil, fmt.Errorf("journey %q prompt: %w", journey.ID, err)
		}
		journey.Prompt = string(prompt)
		registry.journeys[journey.ID] = journey
		registry.order = append(registry.order, journey.ID)
	}
	if len(registry.order) == 0 {
		return nil, fmt.Errorf("at least one journey is required")
	}
	return registry, nil
}

func expandEndpoint(value string) (string, error) {
	if !strings.HasPrefix(value, "${") || !strings.HasSuffix(value, "}") {
		return value, nil
	}
	name := strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")
	if name == "" {
		return "", fmt.Errorf("empty endpoint environment variable")
	}
	resolved, ok := os.LookupEnv(name)
	if !ok || resolved == "" {
		return "", fmt.Errorf("endpoint environment variable %s is unset", name)
	}
	return resolved, nil
}

func confinedPath(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("absolute path is not allowed")
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes config directory")
	}
	path := filepath.Join(root, clean)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes config directory")
	}
	return resolved, nil
}

func (r *Registry) Journey(id string) (Journey, bool) {
	journey, ok := r.journeys[id]
	return journey, ok
}

func (r *Registry) DefaultJourney() (Journey, bool) {
	if r == nil || len(r.order) == 0 {
		return Journey{}, false
	}
	return r.Journey(r.order[0])
}

func (r *Registry) Server(id string) (MCPServer, bool) {
	server, ok := r.servers[id]
	return server, ok
}
