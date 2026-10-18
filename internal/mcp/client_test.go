package mcp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/credential"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/mcptest"
	"github.com/swiftdiaries/az-agent-platform-go/internal/tenantdir"
)

func allowlisted(t *testing.T, id, url string) tenantdir.Endpoint {
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

func TestBindRejectsUnallowlistedEndpoint(t *testing.T) {
	var requests atomic.Int64
	sentinel := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer sentinel.Close()

	// The zero Endpoint is the only thing a caller can build without an
	// Allowlist; the sentinel URL cannot be expressed as an Endpoint at all.
	var zero tenantdir.Endpoint
	_, err := NewClient().Bind(t.Context(), definitions.MCPServer{ID: "s"}, zero, []string{"lookup"}, credential.Credential{})
	if !errors.Is(err, ErrEndpointUnavailable) {
		t.Fatalf("Bind error = %v, want ErrEndpointUnavailable", err)
	}
	if strings.Contains(err.Error(), sentinel.URL) {
		t.Fatalf("error leaks a URL: %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("sentinel received %d requests, want 0", got)
	}
}

func TestBindDoesNotFollowRedirects(t *testing.T) {
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

	probe, _ := http.NewRequest(http.MethodGet, "http://probe.invalid/", nil)
	probe.Header.Set("Cookie", "s=sentinel")
	cred := credential.FromRequest(probe, []string{"Cookie"})

	endpoint := allowlisted(t, "redirecting", redirector.URL)
	_, err := NewClient().Bind(t.Context(), definitions.MCPServer{ID: "s"}, endpoint, []string{"lookup"}, cred)
	if err == nil {
		t.Fatal("Bind followed or accepted a redirect")
	}
	if redirectorHits.Load() == 0 || credentialHits.Load() == 0 {
		t.Fatalf("allowlisted endpoint saw %d requests, %d with the credential; want the credential delivered there", redirectorHits.Load(), credentialHits.Load())
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("redirect target received %d requests, want 0 (credential must not be re-sent)", got)
	}
	if !errors.Is(err, ErrEndpointUnavailable) {
		t.Fatalf("Bind error = %v, want ErrEndpointUnavailable", err)
	}
}

func TestBindRecordsServerInfo(t *testing.T) {
	server := mcptest.New(t, mcptest.Options{Name: "java-mcp", Version: "v7"}, mcptest.Catalog{Tools: []mcptest.Tool{{Name: "lookup"}}})
	endpoint := allowlisted(t, "java", server.URL())
	bound, err := NewClient().Bind(t.Context(), definitions.MCPServer{ID: "s"}, endpoint, []string{"lookup"}, credential.Credential{})
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if name, version := bound.ServerInfo(); name != "java-mcp" || version != "v7" {
		t.Fatalf("ServerInfo = %q %q, want java-mcp v7", name, version)
	}
}
