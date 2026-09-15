package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/provider/litellm"
	"github.com/swiftdiaries/az-agent-platform-go/internal/telemetry"
)

func TestLoadConfigRequiresEveryProductionBoundary(t *testing.T) {
	values := validEnvironment()
	for _, name := range []string{"DATABASE_URL", "AGENT_PLATFORM_CONFIG", "AGENT_PLATFORM_PORT", "AGENT_PLATFORM_SHUTDOWN_GRACE", "KEYCLOAK_JWT_ISSUER", "KEYCLOAK_JWKS_URL", "KEYCLOAK_JWT_AUDIENCE", "KEYCLOAK_JWT_SOURCE", "KEYCLOAK_JWT_HEADER", "KEYCLOAK_USER_CLAIM_PATH", "KEYCLOAK_TENANT_CLAIM_PATH", "KEYCLOAK_JWT_ALLOWED_ALGORITHMS", "AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT", "AZ_AGENT_FOUNDRY_DEPLOYMENT", "OTEL_SERVICE_NAME", "OTEL_SERVICE_VERSION"} {
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

func validEnvironment() map[string]string {
	return map[string]string{
		"DATABASE_URL":          "postgres://user:secret@database.example/agent",
		"AGENT_PLATFORM_CONFIG": "/config/journeys.yaml", "AGENT_PLATFORM_PORT": "8080", "AGENT_PLATFORM_SHUTDOWN_GRACE": "30s",
		"KEYCLOAK_JWT_ISSUER": "https://keycloak.example/realms/product", "KEYCLOAK_JWKS_URL": "https://keycloak.example/realms/product/protocol/openid-connect/certs",
		"KEYCLOAK_JWT_AUDIENCE": "agent-platform", "KEYCLOAK_JWT_SOURCE": "authorization_bearer", "KEYCLOAK_JWT_HEADER": "Authorization",
		"KEYCLOAK_USER_CLAIM_PATH": "/preferred_username", "KEYCLOAK_TENANT_CLAIM_PATH": "/tenant", "KEYCLOAK_JWT_ALLOWED_ALGORITHMS": "RS256,RS512",
		"AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT": "https://foundry.example/projects/agent", "AZ_AGENT_FOUNDRY_DEPLOYMENT": "gpt-deployment",
		"OTEL_SERVICE_NAME": "agent-platform", "OTEL_SERVICE_VERSION": "revision", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example/v1/traces",
		"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer secret,X-Tenant=tenant",
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
