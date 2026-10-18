package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/provider/litellm"
	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
	"github.com/swiftdiaries/az-agent-platform-go/internal/telemetry"
	"github.com/swiftdiaries/az-agent-platform-go/internal/tenantdir"
)

func TestLoadConfigRequiresEveryProductionBoundary(t *testing.T) {
	values := validEnvironment()
	for _, name := range []string{"DATABASE_URL", "AGENT_PLATFORM_CONFIG", "AGENT_PLATFORM_PORT", "AGENT_PLATFORM_SHUTDOWN_GRACE", "KEYCLOAK_JWT_ISSUER", "KEYCLOAK_JWKS_URL", "KEYCLOAK_JWT_AUDIENCE", "KEYCLOAK_JWT_SOURCE", "KEYCLOAK_JWT_HEADER", "KEYCLOAK_USER_CLAIM_PATH", "KEYCLOAK_TENANT_CLAIM_PATH", "KEYCLOAK_JWT_ALLOWED_ALGORITHMS", "AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT", "AZ_AGENT_FOUNDRY_DEPLOYMENT", "OTEL_SERVICE_NAME", "OTEL_SERVICE_VERSION", "AGENT_PLATFORM_TENANT_DIRECTORY", "AGENT_PLATFORM_MCP_ENDPOINTS", "AGENT_PLATFORM_SETTINGS"} {
		t.Run(name, func(t *testing.T) {
			missing := mapsClone(values)
			delete(missing, name)
			if _, err := loadConfig(missingValue(missing)); err == nil {
				t.Fatalf("missing %s accepted", name)
			}
		})
	}
}

func TestLoadConfigSelectsLiteLLMWithoutFoundryCredentialConfiguration(t *testing.T) {
	values := validEnvironment()
	delete(values, "AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT")
	delete(values, "AZ_AGENT_FOUNDRY_DEPLOYMENT")
	values[envModelProvider] = "litellm"
	values[litellm.EnvBaseURL] = "http://localhost:4000"
	values[litellm.EnvAPIKey] = "proxy-secret"
	values[litellm.EnvModel] = "azure-alias"
	loaded, err := loadConfig(missingValue(values))
	if err != nil || loaded.modelProvider != "litellm" || loaded.litellm.Model != "azure-alias" {
		t.Fatalf("LiteLLM config/error = %+v/%v", loaded, err)
	}
	model, err := newModel(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := model.(*litellm.Adapter); !ok {
		t.Fatalf("model type = %T", model)
	}
	for _, name := range []string{litellm.EnvBaseURL, litellm.EnvAPIKey, litellm.EnvModel} {
		missing := mapsClone(values)
		delete(missing, name)
		if _, err := loadConfig(missingValue(missing)); err == nil {
			t.Errorf("missing %s accepted", name)
		}
	}
	values[envModelProvider] = "unknown"
	if _, err := loadConfig(missingValue(values)); err == nil {
		t.Error("unknown provider accepted")
	}
}

func TestLoadConfigRejectsInvalidListenerAndDrainValues(t *testing.T) {
	for name, value := range map[string]string{
		"AGENT_PLATFORM_PORT":           "0",
		"AGENT_PLATFORM_SHUTDOWN_GRACE": "0s",
	} {
		t.Run(name, func(t *testing.T) {
			values := validEnvironment()
			values[name] = value
			if _, err := loadConfig(missingValue(values)); err == nil {
				t.Fatalf("invalid %s accepted", name)
			}
		})
	}
}

func TestLoadConfigBuildsExplicitKeycloakMapping(t *testing.T) {
	values := validEnvironment()
	values["AGENT_PLATFORM_RETAINED_CONFIGS"] = " /config/old-a.yaml, /config/old-b.yaml "
	config, err := loadConfig(missingValue(values))
	if err != nil {
		t.Fatal(err)
	}
	if config.port != 8080 || config.grace != 30*time.Second || config.databaseURL != values["DATABASE_URL"] || config.foundryEndpoint != values["AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT"] || config.foundryDeployment != values["AZ_AGENT_FOUNDRY_DEPLOYMENT"] {
		t.Fatalf("service config = %+v", config)
	}
	if got := strings.Join(config.retainedConfigs, ","); got != "/config/old-a.yaml,/config/old-b.yaml" {
		t.Fatalf("retained configs = %q", got)
	}
	want := chat.KeycloakConfig{
		Issuer: "https://keycloak.example/realms/product", JWKSURL: "https://keycloak.example/realms/product/protocol/openid-connect/certs",
		Audience: "agent-platform", JWTSource: chat.JWTSourceAuthorizationBearer, JWTHeader: "Authorization",
		UserClaimPath: "/preferred_username", TenantClaimPath: "/tenant", AllowedAlgorithms: []string{"RS256", "RS512"},
	}
	if !reflect.DeepEqual(config.keycloak, want) {
		t.Fatalf("keycloak config = %#v", config.keycloak)
	}
	if !reflect.DeepEqual(config.telemetry, telemetry.Config{
		ServiceName: "agent-platform", ServiceVersion: "revision", Endpoint: "https://collector.example/v1/traces",
		Headers: map[string]string{"Authorization": "Bearer secret", "X-Tenant": "tenant"}, ShutdownTimeout: 30 * time.Second,
	}) {
		t.Fatalf("telemetry config = %#v", config.telemetry)
	}
}

func TestLoadConfigParsesCredentialHeaders(t *testing.T) {
	values := validEnvironment()
	values["AGENT_PLATFORM_CREDENTIAL_HEADERS"] = " cookie, Authorization "
	loaded, err := loadConfig(missingValue(values))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Cookie", "Authorization"}; !reflect.DeepEqual(loaded.credentialHeaders, want) {
		t.Fatalf("credential headers = %v, want %v", loaded.credentialHeaders, want)
	}
	loaded, err = loadConfig(missingValue(validEnvironment()))
	if err != nil || len(loaded.credentialHeaders) != 0 {
		t.Fatalf("unset credential headers = %v/%v", loaded.credentialHeaders, err)
	}
	for _, bad := range []string{"Host", "a,,b"} {
		values["AGENT_PLATFORM_CREDENTIAL_HEADERS"] = bad
		if _, err := loadConfig(missingValue(values)); err == nil {
			t.Errorf("invalid credential headers %q accepted", bad)
		}
	}
}

func TestLoadConfigRejectsMalformedTelemetrySecretHeaders(t *testing.T) {
	values := validEnvironment()
	values["OTEL_EXPORTER_OTLP_HEADERS"] = "not-a-header"
	if _, err := loadConfig(missingValue(values)); err == nil {
		t.Fatal("malformed telemetry header accepted")
	}
}

func TestLoadConfigRejectsInvalidTelemetryEndpoint(t *testing.T) {
	values := validEnvironment()
	values["OTEL_EXPORTER_OTLP_ENDPOINT"] = "not an endpoint"
	if _, err := loadConfig(missingValue(values)); err == nil {
		t.Fatal("invalid telemetry endpoint accepted")
	}
}

func TestLoadConfigRequiresTelemetryTracePath(t *testing.T) {
	values := validEnvironment()
	values["OTEL_EXPORTER_OTLP_ENDPOINT"] = "https://collector.example"
	if _, err := loadConfig(missingValue(values)); err == nil {
		t.Fatal("telemetry endpoint without trace path accepted")
	}
}

func TestLoadConfigParsesMCPEndpoints(t *testing.T) {
	values := validEnvironment()
	loaded, err := loadConfig(missingValue(values))
	if err != nil {
		t.Fatal(err)
	}
	ids := loaded.mcpEndpoints.IDs()
	sort.Strings(ids)
	if want := []string{"java-2026-2", "java-2026-3"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("endpoint ids = %v, want %v", ids, want)
	}
	if loaded.tenantDirectoryPath != "/config/tenants.yaml" || loaded.settingsPath != "/config/settings.yaml" {
		t.Fatalf("paths = %q/%q", loaded.tenantDirectoryPath, loaded.settingsPath)
	}
	for _, bad := range []string{
		"no-equals-sign",
		"id=ftp://x",
		"id=https://u:secret@h/",
		"id=https://a.example/mcp,id=https://b.example/mcp",
	} {
		values["AGENT_PLATFORM_MCP_ENDPOINTS"] = bad
		_, err := loadConfig(missingValue(values))
		if err == nil {
			t.Errorf("invalid endpoints %q accepted", bad)
			continue
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "ftp://x") || strings.Contains(err.Error(), "a.example") {
			t.Errorf("error for %q echoes the value: %v", bad, err)
		}
	}
	values["AGENT_PLATFORM_MCP_ENDPOINTS"] = "  "
	if _, err := loadConfig(missingValue(values)); err == nil || !strings.Contains(err.Error(), "required configuration is missing") {
		t.Errorf("blank endpoints error = %v", err)
	}
}

func TestLoadTenancy(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	directoryPath := write("tenants.yaml", "tenants:\n  tenant-a:\n    product_version: \"2026.2\"\n    endpoint: java-2026-2\n")
	settingsPath := write("settings.yaml", "default:\n  max_active_journeys: 2\n  cost:\n    profile: standard\n")
	endpoints, err := tenantdir.ParseAllowlist("java-2026-2=https://mcp-a.example/mcp")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{tenantDirectoryPath: directoryPath, settingsPath: settingsPath, mcpEndpoints: endpoints}

	directory, gotEndpoints, source, err := loadTenancy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entry, endpoint, err := tenantdir.Lookup(context.Background(), directory, gotEndpoints, "tenant-a")
	if err != nil || entry.EndpointID != "java-2026-2" || endpoint.ID() != "java-2026-2" {
		t.Fatalf("lookup = %+v/%+v/%v", entry, endpoint, err)
	}
	resolved, err := source.Resolve(context.Background(), settings.Scope{Tenant: "tenant-a", Principal: "tenant-a/alice"})
	if err != nil || resolved.MaxActiveJourneys != 2 || resolved.Cost.Profile != "standard" {
		t.Fatalf("settings = %+v/%v", resolved, err)
	}

	missing := cfg
	missing.settingsPath = filepath.Join(dir, "absent.yaml")
	if _, _, _, err := loadTenancy(missing); err == nil {
		t.Error("missing settings file accepted")
	} else if strings.Contains(err.Error(), dir) {
		t.Errorf("error echoes the path: %v", err)
	}

	invalid := cfg
	invalid.tenantDirectoryPath = write("bad-tenants.yaml", "tenants:\n  tenant-a:\n    product_version: x\n    endpoint: java-2026-2\n    unknown_key: leaked-value\n")
	if _, _, _, err := loadTenancy(invalid); err == nil {
		t.Error("tenant directory with unknown key accepted")
	} else if strings.Contains(err.Error(), "leaked-value") || strings.Contains(err.Error(), "unknown_key") {
		t.Errorf("error echoes file contents: %v", err)
	}

	badSettings := cfg
	badSettings.settingsPath = write("bad-settings.yaml", "default:\n  bogus_key: leaked-value\n")
	if _, _, _, err := loadTenancy(badSettings); err == nil {
		t.Error("settings with unknown key accepted")
	} else if strings.Contains(err.Error(), "leaked-value") {
		t.Errorf("error echoes file contents: %v", err)
	}
}

func validEnvironment() map[string]string {
	return map[string]string{
		"DATABASE_URL":          "postgres://user:secret@database.example/agent",
		"AGENT_PLATFORM_CONFIG": "/config/journeys.yaml", "AGENT_PLATFORM_PORT": "8080", "AGENT_PLATFORM_SHUTDOWN_GRACE": "30s",
		"KEYCLOAK_JWT_ISSUER": "https://keycloak.example/realms/product", "KEYCLOAK_JWKS_URL": "https://keycloak.example/realms/product/protocol/openid-connect/certs",
		"KEYCLOAK_JWT_AUDIENCE": "agent-platform", "KEYCLOAK_JWT_SOURCE": "authorization_bearer", "KEYCLOAK_JWT_HEADER": "Authorization",
		"KEYCLOAK_USER_CLAIM_PATH": "/preferred_username", "KEYCLOAK_TENANT_CLAIM_PATH": "/tenant", "KEYCLOAK_JWT_ALLOWED_ALGORITHMS": "RS256,RS512",
		"AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT": "https://foundry.example/projects/agent", "AZ_AGENT_FOUNDRY_DEPLOYMENT": "gpt-deployment",
		"OTEL_SERVICE_NAME": "agent-platform", "OTEL_SERVICE_VERSION": "revision", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example/v1/traces",
		"OTEL_EXPORTER_OTLP_HEADERS":      "Authorization=Bearer secret,X-Tenant=tenant",
		"AGENT_PLATFORM_TENANT_DIRECTORY": "/config/tenants.yaml", "AGENT_PLATFORM_SETTINGS": "/config/settings.yaml",
		"AGENT_PLATFORM_MCP_ENDPOINTS": "java-2026-2=https://mcp-a.example/mcp,java-2026-3=https://mcp-b.example/mcp",
	}
}

func missingValue(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func mapsClone(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}
