package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/provider/foundry"
	"github.com/swiftdiaries/az-agent-platform-go/internal/provider/litellm"
	"github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"github.com/swiftdiaries/az-agent-platform-go/internal/service"
	"github.com/swiftdiaries/az-agent-platform-go/internal/telemetry"
)

const (
	envDatabaseURL   = "DATABASE_URL"
	envConfig        = "AGENT_PLATFORM_CONFIG"
	envRetained      = "AGENT_PLATFORM_RETAINED_CONFIGS"
	envPort          = "AGENT_PLATFORM_PORT"
	envShutdownGrace = "AGENT_PLATFORM_SHUTDOWN_GRACE"
	envModelProvider = "AZ_AGENT_MODEL_PROVIDER"
)

type config struct {
	databaseURL       string
	definitionPath    string
	retainedConfigs   []string
	port              int
	grace             time.Duration
	keycloak          chat.KeycloakConfig
	foundryEndpoint   string
	foundryDeployment string
	modelProvider     string
	litellm           litellm.Config
	telemetry         telemetry.Config
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv); err != nil {
		// Startup errors intentionally do not expose ConfigMap or Secret values.
		fmt.Fprintln(os.Stderr, "agent platform startup failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, lookup func(string) string) error {
	config, err := loadConfig(lookup)
	if err != nil {
		return err
	}
	provider, err := telemetry.New(config.telemetry)
	if err != nil {
		return errors.New("invalid telemetry configuration")
	}
	telemetryStopped := false
	shutdownTelemetry := func(ctx context.Context) error {
		telemetryStopped = true
		return provider.Shutdown(ctx)
	}
	defer func() {
		if !telemetryStopped {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), config.grace)
			defer cancel()
			_ = shutdownTelemetry(shutdownCtx)
		}
	}()
	registry, err := definitions.Load(config.definitionPath, config.retainedConfigs...)
	if err != nil {
		return errors.New("invalid definitions")
	}
	pool, err := pgxpool.New(ctx, config.databaseURL)
	if err != nil {
		return errors.New("database unavailable")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return errors.New("database unavailable")
	}
	if err := journal.Migrate(ctx, pool); err != nil {
		return errors.New("database migration failed")
	}
	authenticator, err := chat.NewKeycloakAuthenticator(config.keycloak)
	if err != nil {
		return errors.New("invalid identity configuration")
	}
	model, err := newModel(config)
	if err != nil {
		return errors.New("invalid model configuration")
	}
	app, err := service.New(service.Dependencies{
		Pool: pool, Registry: registry, Authenticator: authenticator, Model: model,
	})
	if err != nil {
		return errors.New("service initialization failed")
	}
	defer app.Close()
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(config.port)))
	if err != nil {
		return errors.New("listener unavailable")
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- app.Serve(listener) }()
	select {
	case <-ctx.Done():
		graceCtx, cancel := context.WithTimeout(context.Background(), config.grace)
		defer cancel()
		serviceErr := app.Shutdown(graceCtx)
		telemetryErr := shutdownTelemetry(graceCtx)
		if serviceErr != nil {
			return errors.New("service shutdown failed")
		}
		if telemetryErr != nil {
			return errors.New("telemetry shutdown failed")
		}
		return nil
	case <-serveErr:
		return errors.New("service stopped")
	}
}

func newModel(config config) (runtime.Model, error) {
	if config.modelProvider == "litellm" {
		return litellm.New(config.litellm)
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	return foundry.New(foundry.Config{
		ProjectEndpoint: config.foundryEndpoint, Deployment: config.foundryDeployment, Credential: credential,
	})
}

func loadConfig(lookup func(string) string) (config, error) {
	if lookup == nil {
		return config{}, errors.New("configuration unavailable")
	}
	require := func(name string) (string, error) {
		value := strings.TrimSpace(lookup(name))
		if value == "" {
			return "", errors.New("required configuration is missing")
		}
		return value, nil
	}
	databaseURL, err := require(envDatabaseURL)
	if err != nil {
		return config{}, err
	}
	definitionPath, err := require(envConfig)
	if err != nil {
		return config{}, err
	}
	portText, err := require(envPort)
	if err != nil {
		return config{}, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return config{}, errors.New("invalid listener configuration")
	}
	graceText, err := require(envShutdownGrace)
	if err != nil {
		return config{}, err
	}
	grace, err := time.ParseDuration(graceText)
	if err != nil || grace <= 0 {
		return config{}, errors.New("invalid shutdown configuration")
	}
	keycloak, err := keycloakConfig(require)
	if err != nil {
		return config{}, err
	}
	modelProvider := strings.TrimSpace(lookup(envModelProvider))
	if modelProvider == "" {
		modelProvider = "foundry"
	}
	var foundryEndpoint, foundryDeployment string
	var litellmConfig litellm.Config
	switch modelProvider {
	case "foundry":
		foundryEndpoint, err = require(foundry.EnvProjectEndpoint)
		if err != nil {
			return config{}, err
		}
		foundryDeployment, err = require(foundry.EnvDeployment)
		if err != nil {
			return config{}, err
		}
	case "litellm":
		litellmConfig, err = litellm.ConfigFromEnv(lookup)
		if err != nil {
			return config{}, err
		}
	default:
		return config{}, errors.New("invalid model provider")
	}
	telemetryConfig, err := telemetryConfig(require, lookup, grace)
	if err != nil {
		return config{}, err
	}
	return config{
		databaseURL: databaseURL, definitionPath: definitionPath,
		retainedConfigs: splitValues(lookup(envRetained)), port: port, grace: grace, keycloak: keycloak,
		foundryEndpoint: foundryEndpoint, foundryDeployment: foundryDeployment,
		modelProvider: modelProvider, litellm: litellmConfig,
		telemetry: telemetryConfig,
	}, nil
}

func keycloakConfig(require func(string) (string, error)) (chat.KeycloakConfig, error) {
	issuer, err := require("KEYCLOAK_JWT_ISSUER")
	if err != nil {
		return chat.KeycloakConfig{}, err
	}
	jwksURL, err := require("KEYCLOAK_JWKS_URL")
	if err != nil {
		return chat.KeycloakConfig{}, err
	}
	audience, err := require("KEYCLOAK_JWT_AUDIENCE")
	if err != nil {
		return chat.KeycloakConfig{}, err
	}
	source, err := require("KEYCLOAK_JWT_SOURCE")
	if err != nil {
		return chat.KeycloakConfig{}, err
	}
	header, err := require("KEYCLOAK_JWT_HEADER")
	if err != nil {
		return chat.KeycloakConfig{}, err
	}
	userPath, err := require("KEYCLOAK_USER_CLAIM_PATH")
	if err != nil {
		return chat.KeycloakConfig{}, err
	}
	tenantPath, err := require("KEYCLOAK_TENANT_CLAIM_PATH")
	if err != nil {
		return chat.KeycloakConfig{}, err
	}
	algorithms, err := require("KEYCLOAK_JWT_ALLOWED_ALGORITHMS")
	if err != nil {
		return chat.KeycloakConfig{}, err
	}
	return chat.KeycloakConfig{
		Issuer: issuer, JWKSURL: jwksURL, Audience: audience, JWTSource: chat.JWTSource(source), JWTHeader: header,
		UserClaimPath: userPath, TenantClaimPath: tenantPath, AllowedAlgorithms: splitValues(algorithms),
	}, nil
}

func splitValues(value string) []string {
	var result []string
	for _, value := range strings.Split(value, ",") {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func telemetryConfig(require func(string) (string, error), lookup func(string) string, shutdownTimeout time.Duration) (telemetry.Config, error) {
	serviceName, err := require("OTEL_SERVICE_NAME")
	if err != nil {
		return telemetry.Config{}, err
	}
	serviceVersion, err := require("OTEL_SERVICE_VERSION")
	if err != nil {
		return telemetry.Config{}, err
	}
	headers, err := telemetryHeaders(lookup("OTEL_EXPORTER_OTLP_HEADERS"))
	if err != nil {
		return telemetry.Config{}, err
	}
	endpoint := strings.TrimSpace(lookup("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint != "" {
		parsed, err := url.ParseRequestURI(endpoint)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path == "" || parsed.Path == "/" {
			return telemetry.Config{}, errors.New("invalid telemetry endpoint configuration")
		}
	}
	return telemetry.Config{
		ServiceName: serviceName, ServiceVersion: serviceVersion,
		Endpoint: endpoint, Headers: headers,
		ShutdownTimeout: shutdownTimeout,
	}, nil
}

func telemetryHeaders(value string) (map[string]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	headers := make(map[string]string)
	for _, item := range strings.Split(value, ",") {
		name, headerValue, ok := strings.Cut(item, "=")
		name = textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
		headerValue = strings.TrimSpace(headerValue)
		if !ok || name == "" || headerValue == "" || strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(headerValue, "\r\n") {
			return nil, errors.New("invalid telemetry header configuration")
		}
		if _, exists := headers[name]; exists {
			return nil, errors.New("invalid telemetry header configuration")
		}
		headers[name] = headerValue
	}
	return headers, nil
}
