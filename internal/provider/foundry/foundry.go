// Package foundry adapts the configured Azure AI Foundry project deployment to
// the product-owned runtime Model boundary.
package foundry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/agent/harness/toolautocall"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/provider/foundryprovider"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

const (
	// EnvProjectEndpoint names the ConfigMap value holding the Azure AI Foundry
	// project endpoint, for example https://example.projects.ai.azure.com/projects/x.
	EnvProjectEndpoint = "AZ_AGENT_FOUNDRY_PROJECT_ENDPOINT"
	// EnvDeployment names the ConfigMap value holding the Foundry model deployment.
	EnvDeployment = "AZ_AGENT_FOUNDRY_DEPLOYMENT"
)

// Config supports Azure AI Foundry project endpoints authenticated with an AAD
// TokenCredential. Credential construction belongs to process composition, so
// workload identity and Secret reference policy do not enter this package.
type Config struct {
	ProjectEndpoint string
	Deployment      string
	Credential      azcore.TokenCredential
	// HTTPClient is optional and primarily makes the wire contract testable.
	HTTPClient *http.Client
}

// ConfigFromEnv reads this adapter's explicitly new, nonsecret ConfigMap keys.
func ConfigFromEnv(lookup func(string) string, credential azcore.TokenCredential) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("Foundry environment lookup is required")
	}
	return validate(Config{
		ProjectEndpoint: lookup(EnvProjectEndpoint),
		Deployment:      lookup(EnvDeployment),
		Credential:      credential,
	})
}

// Adapter makes one Foundry Responses API request per runtime provider turn.
// It never invokes an exposed tool or retains a provider-side conversation.
type Adapter struct{ config Config }

func New(config Config) (*Adapter, error) {
	config, err := validate(config)
	if err != nil {
		return nil, err
	}
	return &Adapter{config: config}, nil
}

func validate(config Config) (Config, error) {
	config.ProjectEndpoint = strings.TrimRight(strings.TrimSpace(config.ProjectEndpoint), "/")
	config.Deployment = strings.TrimSpace(config.Deployment)
	if config.ProjectEndpoint == "" {
		return Config{}, fmt.Errorf("%s is required", EnvProjectEndpoint)
	}
	endpoint, err := url.ParseRequestURI(config.ProjectEndpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return Config{}, errors.New("Foundry project endpoint must be an absolute URL")
	}
	if config.Deployment == "" {
		return Config{}, fmt.Errorf("%s is required", EnvDeployment)
	}
	if config.Credential == nil {
		return Config{}, errors.New("Foundry AAD credential is required")
	}
	return config, nil
}

func (a *Adapter) Complete(ctx context.Context, request runtime.ModelRequest) (runtime.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return runtime.ModelResponse{}, err
	}
	if a == nil {
		return runtime.ModelResponse{}, errors.New("Foundry model is not configured")
	}
	maxIterations := 0
	client := a.config.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	clientCopy := *client
	clientCopy.Transport = &dispatchTransport{base: client.Transport, onDispatch: request.OnDispatch}
	model := foundryprovider.NewAgent(
		a.config.ProjectEndpoint,
		a.config.Credential,
		foundryprovider.ModelDeployment(a.config.Deployment),
		foundryprovider.AgentConfig{
			Config:             agent.Config{Tools: tools(request.Tools)},
			Instructions:       request.Instructions,
			DisableStoreOutput: true,
			ToolAutoCall: &toolautocall.Config{
				MaximumIterationsPerRequest: &maxIterations,
			},
			OpenAIOptions: []option.RequestOption{
				option.WithHTTPClient(&clientCopy),
				option.WithMaxRetries(0),
			},
		},
	)
	response, err := model.Run(ctx, request.ProviderMessages).Collect()
	if err != nil {
		return runtime.ModelResponse{}, sanitize(err)
	}
	result := runtime.ModelResponse{Text: response.String()}
	for content := range response.Contents() {
		if call, ok := content.(*message.FunctionCallContent); ok {
			result.ToolCalls = append(result.ToolCalls, runtime.ToolCall{
				CallID: call.CallID, Name: call.Name, Arguments: []byte(call.Arguments),
			})
		}
	}
	return result, nil
}

func sanitize(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var apiError *openai.Error
	if errors.As(err, &apiError) && (apiError.StatusCode == http.StatusUnauthorized || apiError.StatusCode == http.StatusForbidden) {
		return fmt.Errorf("Foundry authentication failed: %w", platformmcp.ErrAuthRequired)
	}
	return errors.New("Foundry model request failed")
}

type dispatchTransport struct {
	base       http.RoundTripper
	onDispatch func(context.Context) error
	once       sync.Once
	err        error
}

func (t *dispatchTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.onDispatch != nil {
		t.once.Do(func() { t.err = t.onDispatch(request.Context()) })
		if t.err != nil {
			return nil, t.err
		}
	}
	if t.base != nil {
		return t.base.RoundTrip(request)
	}
	return http.DefaultTransport.RoundTrip(request)
}

type schemaTool struct{ model runtime.ModelTool }

func (t schemaTool) Name() string        { return t.model.Name }
func (t schemaTool) Description() string { return t.model.Description }
func (t schemaTool) Schema() any         { return t.model.Schema }
func (schemaTool) ReturnSchema() any     { return nil }
func (t schemaTool) Call(context.Context, string) (any, error) {
	return nil, fmt.Errorf("%s is dispatched by the product runtime", t.model.Name)
}

func tools(modelTools []runtime.ModelTool) []tool.Tool {
	tools := make([]tool.Tool, len(modelTools))
	for i, modelTool := range modelTools {
		tools[i] = schemaTool{model: modelTool}
	}
	return tools
}
