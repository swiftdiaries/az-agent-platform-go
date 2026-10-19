package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/swiftdiaries/az-agent-platform-go/internal/cost"
	"github.com/swiftdiaries/az-agent-platform-go/internal/credential"
	"github.com/swiftdiaries/az-agent-platform-go/internal/defstore"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/provider/foundry"
	"github.com/swiftdiaries/az-agent-platform-go/internal/provider/litellm"
	"github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"github.com/swiftdiaries/az-agent-platform-go/internal/service"
	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
	"github.com/swiftdiaries/az-agent-platform-go/internal/telemetry"
	"github.com/swiftdiaries/az-agent-platform-go/internal/tenantdir"
)

const (
	envDatabaseURL   = "DATABASE_URL"
	envPort          = "AGENT_PLATFORM_PORT"
	envShutdownGrace = "AGENT_PLATFORM_SHUTDOWN_GRACE"
	envModelProvider = "AZ_AGENT_MODEL_PROVIDER"
	// envCredentialHeaders optionally names the inbound headers forwarded to MCP.
	envCredentialHeaders = "AGENT_PLATFORM_CREDENTIAL_HEADERS"
	// envTenantDirectory is the path of the tenant directory YAML file.
	envTenantDirectory = "AGENT_PLATFORM_TENANT_DIRECTORY"
	// envMCPEndpoints is the MCP endpoint allowlist, as id=url,id2=url2.
	envMCPEndpoints = "AGENT_PLATFORM_MCP_ENDPOINTS"
	// envSettings is the path of the settings YAML file.
	envSettings = "AGENT_PLATFORM_SETTINGS"
	// envCostGate selects the cost enforcement adapter: open (default) or headers.
	envCostGate = "AGENT_PLATFORM_COST_GATE"
	// envCostHeaders names the attribution headers the headers gate sends.
	envCostHeaders = "AGENT_PLATFORM_COST_HEADERS"
	// envHistoryBudget is the per-model history budget, as model=tokens,model2=tokens.
	envHistoryBudget = "AGENT_PLATFORM_HISTORY_BUDGET"
)

type config struct {
	databaseURL       string
	port              int
	grace             time.Duration
	keycloak          chat.KeycloakConfig
	foundryEndpoint   string
	foundryDeployment string
	modelProvider     string
	litellm           litellm.Config
	telemetry         telemetry.Config
	credentialHeaders []string
	costGate          cost.Gate
	historyBudget     int

	tenantDirectoryPath string
	settingsPath        string
	mcpEndpoints        tenantdir.Allowlist
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if len(os.Args) > 1 && os.Args[1] == "activate" {
		code := runActivate(ctx, os.Args[2:], os.Getenv, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}
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
	directory, endpoints, settingsSource, err := loadTenancy(config)
	if err != nil {
		return err
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
		Pool: pool, Authenticator: authenticator, Model: model,
		CredentialHeaders: config.credentialHeaders, CostGate: config.costGate,
		Directory: directory, Endpoints: endpoints, Settings: settingsSource,
		HistoryBudget: config.historyBudget,
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

// loadTenancy loads the tenant directory and settings files named by config.
// Errors are fixed messages: file contents and paths are never echoed.
func loadTenancy(config config) (tenantdir.Directory, tenantdir.Allowlist, settings.Source, error) {
	directory, err := tenantdir.LoadStatic(config.tenantDirectoryPath)
	if err != nil {
		return nil, tenantdir.Allowlist{}, nil, errors.New("invalid tenant directory")
	}
	source, err := settings.LoadStatic(config.settingsPath)
	if err != nil {
		return nil, tenantdir.Allowlist{}, nil, errors.New("invalid settings")
	}
	return directory, config.mcpEndpoints, source, nil
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
	credentialHeaders, err := credential.ParseHeaderNames(lookup(envCredentialHeaders))
	if err != nil {
		return config{}, errors.New("invalid credential header configuration")
	}
	costGate, err := cost.New(strings.TrimSpace(lookup(envCostGate)), lookup(envCostHeaders))
	if err != nil {
		return config{}, errors.New("invalid cost gate configuration")
	}
	tenantDirectoryPath, err := require(envTenantDirectory)
	if err != nil {
		return config{}, err
	}
	endpointSpec, err := require(envMCPEndpoints)
	if err != nil {
		return config{}, err
	}
	mcpEndpoints, err := tenantdir.ParseAllowlist(endpointSpec)
	if err != nil || len(mcpEndpoints.IDs()) == 0 {
		return config{}, errors.New("invalid MCP endpoint configuration")
	}
	settingsPath, err := require(envSettings)
	if err != nil {
		return config{}, err
	}
	configuredModel := foundryDeployment
	if modelProvider == "litellm" {
		configuredModel = litellmConfig.Model
	}
	historyBudget, err := parseHistoryBudget(lookup(envHistoryBudget), configuredModel)
	if err != nil {
		return config{}, err
	}
	return config{
		tenantDirectoryPath: tenantDirectoryPath, mcpEndpoints: mcpEndpoints, settingsPath: settingsPath,
		databaseURL: databaseURL, port: port, grace: grace, keycloak: keycloak,
		foundryEndpoint: foundryEndpoint, foundryDeployment: foundryDeployment,
		modelProvider: modelProvider, litellm: litellmConfig,
		telemetry: telemetryConfig, credentialHeaders: credentialHeaders, costGate: costGate,
		historyBudget: historyBudget,
	}, nil
}

// parseHistoryBudget parses model=tokens pairs and returns the entry for the
// configured model. Errors are fixed messages and never echo the value.
func parseHistoryBudget(value, model string) (int, error) {
	budgets := map[string]int{}
	if strings.TrimSpace(value) == "" {
		return 0, errors.New("history budget is missing")
	}
	for _, pair := range strings.Split(value, ",") {
		name, tokens, ok := strings.Cut(pair, "=")
		name, tokens = strings.TrimSpace(name), strings.TrimSpace(tokens)
		count, err := strconv.Atoi(tokens)
		if _, duplicate := budgets[name]; !ok || name == "" || err != nil || count < 1 || duplicate {
			return 0, errors.New("invalid history budget configuration")
		}
		budgets[name] = count
	}
	budget, ok := budgets[model]
	if !ok {
		return 0, errors.New("history budget is missing for the configured model")
	}
	return budget, nil
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

// runActivate implements `agent-platform activate [--rollback] --bundle <dir>`. It is safe to run repeatedly and concurrently
// (it is the init container of every Agent pod). Failures print one fixed line naming a category; bundle
// content and the database URL are never printed.
func runActivate(ctx context.Context, args []string, lookup func(string) string, stdout, stderr io.Writer) int {
	fail := func(category string) int {
		fmt.Fprintf(stderr, "agent platform activation failed: %s\n", category)
		return 1
	}
	flags := flag.NewFlagSet("activate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	bundle := flags.String("bundle", "", "directory holding the definition bundle")
	rollback := flags.Bool("rollback", false, "move current back to a set activated before")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*bundle) == "" {
		return fail("usage")
	}
	databaseURL := strings.TrimSpace(lookup(envDatabaseURL))
	if databaseURL == "" {
		return fail("configuration")
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fail("database")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fail("database")
	}
	res, err := defstore.Activate(ctx, pool, *bundle, defstore.Options{Rollback: *rollback})
	if err != nil {
		return fail(activateCategory(err))
	}
	switch res.Outcome {
	case defstore.AlreadyCurrent:
		fmt.Fprintf(stdout, "already current %s\n", res.Digest)
	case defstore.Superseded:
		fmt.Fprintf(stdout, "not activated %s: activated before (use --rollback); current %s\n", res.Digest, res.Current)
	default:
		fmt.Fprintf(stdout, "activated %s\n", res.Digest)
	}
	return 0
}

// activateCategory names the one fixed stderr category for an Activate failure.
func activateCategory(err error) string {
	switch {
	case errors.Is(err, defstore.ErrDatabase):
		return "database"
	case errors.Is(err, defstore.ErrBundle):
		return "bundle"
	case errors.Is(err, journal.ErrDefinition):
		return "unavailable definition"
	}
	return "other"
}
