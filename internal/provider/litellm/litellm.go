// Package litellm adapts a LiteLLM proxy to the product-owned runtime Model boundary.
package litellm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/microsoft/agent-framework-go/message"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

const (
	EnvBaseURL = "AZ_AGENT_LITELLM_BASE_URL"
	EnvAPIKey  = "AZ_AGENT_LITELLM_API_KEY"
	EnvModel   = "AZ_AGENT_LITELLM_MODEL"
)

type Config struct {
	BaseURL string
	APIKey  string
	Model   string
	// HTTPClient is optional and permits a local wire-contract test.
	HTTPClient *http.Client
}

func ConfigFromEnv(lookup func(string) string) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("LiteLLM environment lookup required")
	}
	return validate(Config{BaseURL: lookup(EnvBaseURL), APIKey: lookup(EnvAPIKey), Model: lookup(EnvModel)})
}

func validate(config Config) (Config, error) {
	config.BaseURL = strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.Model = strings.TrimSpace(config.Model)
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return Config{}, errors.New("invalid LiteLLM base URL")
	}
	if config.APIKey == "" || config.Model == "" {
		return Config{}, errors.New("LiteLLM API key and model are required")
	}
	return config, nil
}

type Adapter struct{ config Config }

func New(config Config) (*Adapter, error) {
	validated, err := validate(config)
	if err != nil {
		return nil, err
	}
	return &Adapter{config: validated}, nil
}

func (a *Adapter) Complete(ctx context.Context, request runtime.ModelRequest) (runtime.ModelResponse, error) {
	if a == nil {
		return runtime.ModelResponse{}, errors.New("LiteLLM adapter unavailable")
	}
	if err := ctx.Err(); err != nil {
		return runtime.ModelResponse{}, err
	}
	input, err := inputItems(request.ProviderMessages)
	if err != nil {
		return runtime.ModelResponse{}, errors.New("LiteLLM model input failed")
	}
	params := responses.ResponseNewParams{
		Model:        a.config.Model,
		Input:        responses.ResponseNewParamsInputUnion{OfInputItemList: input},
		Instructions: openai.String(request.Instructions),
		Store:        openai.Bool(false),
	}
	for _, item := range request.Tools {
		parameters, err := schema(item.Schema)
		if err != nil {
			return runtime.ModelResponse{}, errors.New("LiteLLM model input failed")
		}
		params.Tools = append(params.Tools, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
			Name: item.Name, Description: openai.String(item.Description), Parameters: parameters,
		}})
	}
	client := a.config.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dispatch := &dispatchTransport{base: client.Transport, onDispatch: request.OnDispatch}
	clientCopy.Transport = dispatch
	sdk := openai.NewClient(
		option.WithBaseURL(a.config.BaseURL), option.WithAPIKey(a.config.APIKey),
		option.WithHTTPClient(&clientCopy), option.WithMaxRetries(0),
	)
	response, err := sdk.Responses.New(ctx, params)
	if dispatch.err != nil {
		return runtime.ModelResponse{}, dispatch.err
	}
	if err != nil {
		return runtime.ModelResponse{}, sanitize(err)
	}
	if response == nil || response.Status != responses.ResponseStatusCompleted {
		return runtime.ModelResponse{}, errors.New("LiteLLM model request failed")
	}
	result := runtime.ModelResponse{Text: response.OutputText()}
	for _, item := range response.Output {
		if item.Type == "function_call" {
			call := item.AsFunctionCall()
			result.ToolCalls = append(result.ToolCalls, runtime.ToolCall{
				CallID: call.CallID, Name: call.Name, Arguments: []byte(call.Arguments),
			})
		}
	}
	return result, nil
}

func inputItems(history []*message.Message) (responses.ResponseInputParam, error) {
	var input responses.ResponseInputParam
	for _, item := range history {
		if item == nil || len(item.Contents) == 0 {
			return nil, errors.New("invalid message")
		}
		for _, content := range item.Contents {
			switch value := content.(type) {
			case *message.TextContent:
				if value == nil || (item.Role != message.RoleUser && item.Role != message.RoleAssistant && item.Role != message.RoleSystem) {
					return nil, errors.New("invalid text")
				}
				input = append(input, responses.ResponseInputItemUnionParam{OfMessage: &responses.EasyInputMessageParam{
					Role:    responses.EasyInputMessageRole(item.Role),
					Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(value.Text)},
				}})
			case *message.FunctionCallContent:
				if value == nil || item.Role != message.RoleAssistant || value.CallID == "" || value.Name == "" {
					return nil, errors.New("invalid function call")
				}
				input = append(input, responses.ResponseInputItemUnionParam{OfFunctionCall: &responses.ResponseFunctionToolCallParam{
					CallID: value.CallID, Name: value.Name, Arguments: value.Arguments,
				}})
			case *message.FunctionResultContent:
				if value == nil || item.Role != message.RoleTool || value.CallID == "" {
					return nil, errors.New("invalid function result")
				}
				output, err := resultString(value.Result)
				if err != nil {
					return nil, err
				}
				input = append(input, responses.ResponseInputItemUnionParam{OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: openai.String(value.CallID),
					Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: openai.String(output)},
				}})
			default:
				return nil, errors.New("unsupported message content")
			}
		}
	}
	if len(input) == 0 {
		return nil, errors.New("empty model input")
	}
	return input, nil
}

func resultString(value any) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	data, err := json.Marshal(value)
	return string(data), err
}

func schema(value any) (map[string]any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errors.New("invalid tool schema")
	}
	return object, nil
}

func sanitize(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		return fmt.Errorf("LiteLLM authentication failed: %w", platformmcp.ErrAuthRequired)
	}
	return errors.New("LiteLLM model request failed")
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
