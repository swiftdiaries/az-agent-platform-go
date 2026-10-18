package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/swiftdiaries/az-agent-platform-go/internal/catalog"
	"github.com/swiftdiaries/az-agent-platform-go/internal/credential"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
	platformskills "github.com/swiftdiaries/az-agent-platform-go/internal/skills"
	"github.com/swiftdiaries/az-agent-platform-go/internal/tenantdir"
)

type ModelTool struct {
	Name        string
	Description string
	Schema      any
}

type ToolResult struct {
	CallID string
	Name   string
	Value  any
}

type Handoff struct{ JourneyID, Text string }

type ModelRequest struct {
	Handoff         Handoff
	History         json.RawMessage
	PendingCommands []journal.Input
	Instructions    string
	Messages        []string
	// ProviderMessages is the exact MAF history for this turn. It is transient:
	// providers must not replace the product-owned durable history with a
	// provider session.
	ProviderMessages []*message.Message
	Tools            []ModelTool
	ToolResults      []ToolResult
	DefinitionDigest string
	SkillCatalog     []SkillMetadata
	SkillMaterial    []platformskills.Material
	Provenance       RequestProvenance
	// OnDispatch commits materialization at the final provider request boundary.
	// A provider must call it at most once, after constructing the exact request
	// and immediately before its transport sends it.
	OnDispatch func(context.Context) error
}

// SkillMetadata is the metadata-only skill catalog exposed to the provider.
type SkillMetadata struct{ Name, Description string }

type RequestProvenance struct {
	Definition, System, Handoff, Catalog, History MaterialProvenance
	Pending, IncludedInput, Skills                []MaterialProvenance
}

type MaterialProvenance struct{ ID, Digest string }

type ToolCall struct {
	CallID    string
	Name      string
	Arguments json.RawMessage
}

type ModelResponse struct {
	Text      string
	ToolCalls []ToolCall
	ToolCall  *ToolCall
}

type Model interface {
	Complete(context.Context, ModelRequest) (ModelResponse, error)
}

type Runner struct {
	set     *definitions.Set
	model   Model
	tenancy Tenancy
}

// Tenancy is what a Runner needs to resolve a tenant at run start.
type Tenancy struct {
	Directory tenantdir.Directory
	Endpoints tenantdir.Allowlist
	Settings  settings.Source
}

// ErrDependencyUnavailable marks a tenant directory, allowlist, settings or MCP
// endpoint failure. The run ends dependency_unavailable and no request that
// could carry a credential has been sent.
var ErrDependencyUnavailable = errors.New("dependency unavailable")

// ErrJourneyUnavailable marks a journey whose tools do not resolve against the
// conversation's catalog snapshot. The run ends failed and no model call is made.
var ErrJourneyUnavailable = errors.New("journey unavailable")

// RunScope is the resolved, recorded scope of one run.
type RunScope struct {
	Entry    tenantdir.Entry
	Endpoint tenantdir.Endpoint
	Settings settings.Settings
}

type RunInput struct {
	Store            *journal.Store
	Owner            journal.Owner
	Iteration        int
	History          json.RawMessage
	ThreadID         string
	RunID            string
	Principal        string
	Tenant           string
	Text             string
	TargetJourney    string
	DefinitionDigest string
	Credential       credential.Credential
	// ProductVersion, Endpoint and Settings are filled from Scope before Run.
	ProductVersion string
	Endpoint       tenantdir.Endpoint
	Settings       settings.Settings
}

type RunOutput struct {
	Waiting   bool
	History   json.RawMessage
	JourneyID string
	Answer    string
	CallID    string
	ToolName  string
}

func NewRunner(set *definitions.Set, model Model, tenancy Tenancy) *Runner {
	return &Runner{set: set, model: model, tenancy: tenancy}
}

// Scope resolves directory, then allowlist, then settings, and records them on
// the run (fenced) before any dispatch. A resolution failure is
// ErrDependencyUnavailable and nothing has been sent anywhere.
func (r *Runner) Scope(ctx context.Context, in RunInput) (RunScope, error) {
	entry, endpoint, err := tenantdir.Lookup(ctx, r.tenancy.Directory, r.tenancy.Endpoints, in.Tenant)
	if err != nil {
		return RunScope{}, unavailable(ctx, err)
	}
	if r.tenancy.Settings == nil {
		return RunScope{}, unavailable(ctx, settings.ErrUnavailable)
	}
	resolved, err := r.tenancy.Settings.Resolve(ctx, settings.Scope{Tenant: in.Tenant, Principal: in.Principal})
	if err != nil {
		return RunScope{}, unavailable(ctx, err)
	}
	if in.Store == nil {
		return RunScope{}, journal.ErrDefinition
	}
	if err := in.Store.RecordRunScope(ctx, in.Owner, journal.RunScope{ProductVersion: entry.ProductVersion, EndpointID: entry.EndpointID, Settings: resolved.Record()}); err != nil {
		return RunScope{}, err
	}
	return RunScope{Entry: entry, Endpoint: endpoint, Settings: resolved}, nil
}

// unavailable fails closed, except that the caller's own cancellation stays a
// cancellation rather than a dependency verdict.
func unavailable(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	return fmt.Errorf("%w: %w", ErrDependencyUnavailable, err)
}

// Ready verifies only durable definition availability. It deliberately does
// not contact a provider or MCP server.
func (r *Runner) Ready(ctx context.Context, store *journal.Store) (bool, error) {
	if r == nil || r.set == nil || store == nil {
		return false, journal.ErrDefinition
	}
	return store.DefinitionsReady(ctx, r.set.Serves)
}

// Binding resolves the conversation's pinned definition set (or the database
// current pointer for a new one) and routes the request within that set.
func (r *Runner) Binding(ctx context.Context, in RunInput) (string, string, error) {
	if r == nil || r.set == nil || in.Store == nil {
		return "", "", journal.ErrDefinition
	}
	digest, err := in.Store.ResolveDefinition(ctx, in.Owner, r.set.Digest(), r.set.Serves)
	if err != nil {
		return "", "", err
	}
	set, ok := r.set.Version(digest)
	if !ok {
		return "", "", journal.ErrDefinition
	}
	journey, err := r.route(ctx, set, in.TargetJourney, in.Text)
	if err != nil {
		return "", "", err
	}
	return journey.ID, digest, nil
}

func (r *Runner) Run(ctx context.Context, input RunInput) (RunOutput, error) {
	if !input.Endpoint.Valid() {
		return RunOutput{}, fmt.Errorf("%w: no allowlisted endpoint", ErrDependencyUnavailable)
	}
	set, ok := r.set.Version(input.DefinitionDigest)
	if !ok {
		return RunOutput{}, journal.ErrDefinition
	}
	journey, ok := set.Journey(input.TargetJourney)
	if !ok {
		return RunOutput{}, journal.ErrDefinition
	}
	ctx, span := otel.Tracer("az-agent-platform/runtime").Start(ctx, "journey.run")
	span.SetAttributes(
		attribute.String("thread.id", input.ThreadID), attribute.String("run.id", input.RunID),
		attribute.String("journey.id", journey.ID),
	)
	defer span.End()
	if err := input.Store.Check(ctx, input.Owner); err != nil {
		return RunOutput{}, err
	}
	binding, err := catalog.Open(ctx, catalog.Scope{ProductVersion: input.ProductVersion, Endpoint: input.Endpoint}, input.Credential, input.Store, input.Owner, set, catalog.Options{})
	if err != nil {
		if errors.Is(err, catalog.ErrEndpointUnavailable) {
			return RunOutput{}, fmt.Errorf("%w: %w", ErrDependencyUnavailable, err)
		}
		return RunOutput{}, err
	}
	defer binding.Close()
	resolution, ok := binding.Resolution(journey.ID)
	if !ok || !resolution.Available() {
		return RunOutput{}, unavailableJourney(resolution)
	}

	if err := input.Store.Check(ctx, input.Owner); err != nil {
		return RunOutput{}, err
	}
	return r.runHarness(ctx, input, journey, binding)
}

// unavailableJourney describes why a journey's tools did not resolve. It names
// only reasons and operation keys or aliases, never remote tool names.
func unavailableJourney(resolution catalog.Resolution) error {
	if len(resolution.Unavailable) == 0 {
		return ErrJourneyUnavailable
	}
	reasons := make([]string, 0, len(resolution.Unavailable))
	for _, u := range resolution.Unavailable {
		reasons = append(reasons, string(u.Reason)+" ("+u.Operation+")")
	}
	return fmt.Errorf("%w: %s", ErrJourneyUnavailable, strings.Join(reasons, ", "))
}

func modelRun(parent context.Context, model Model, callBase string, before func(context.Context, *ModelRequest) error, observed func(context.Context) error) agent.RunFunc {
	return func(ctx context.Context, messages []*message.Message, options ...agent.Option) iter.Seq2[*agent.ResponseUpdate, error] {
		return func(yield func(*agent.ResponseUpdate, error) bool) {
			ctx = trace.ContextWithSpanContext(ctx, trace.SpanContextFromContext(parent))
			request := ModelRequest{}
			for instructions := range agent.AllOptions(options, agent.WithInstructions) {
				request.Instructions = instructions
			}
			for candidate := range agent.AllOptions(options, agent.WithTool) {
				schema, _ := candidate.(tool.SchemaTool)
				request.Tools = append(request.Tools, ModelTool{Name: candidate.Name(), Description: candidate.Description(), Schema: schemaValue(schema)})
			}
			request.ProviderMessages = messages
			callNames := map[string]string{}
			for _, msg := range messages {
				for _, content := range msg.Contents {
					if call, ok := content.(*message.FunctionCallContent); ok {
						callNames[call.CallID] = call.Name
					}
				}
			}
			for _, msg := range messages {
				if text := msg.String(); text != "" {
					request.Messages = append(request.Messages, text)
				}
				for _, content := range msg.Contents {
					if result, ok := content.(*message.FunctionResultContent); ok {
						request.ToolResults = append(request.ToolResults, ToolResult{CallID: result.CallID, Name: callNames[result.CallID], Value: result.Result})
					}
				}
			}
			if err := before(ctx, &request); err != nil {
				yield(nil, err)
				return
			}
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			observedOnce := false
			request.OnDispatch = func(context.Context) error {
				if observedOnce {
					return nil
				}
				if err := observed(ctx); err != nil {
					return err
				}
				observedOnce = true
				return nil
			}
			modelCtx, span := otel.Tracer("az-agent-platform/runtime").Start(ctx, "model.call")
			span.SetAttributes(attribute.String("call.id", callBase))
			response, err := model.Complete(modelCtx, request)
			span.End()
			if err != nil {
				yield(nil, err)
				return
			}
			// Test and local models have no HTTP transport callback. Their successful
			// completion is the observable request boundary.
			if err := request.OnDispatch(ctx); err != nil {
				yield(nil, err)
				return
			}
			update := &agent.ResponseUpdate{Role: message.RoleAssistant}
			calls := response.ToolCalls
			if response.ToolCall != nil {
				calls = append(calls, *response.ToolCall)
			}
			if len(calls) > 0 {
				update.FinishReason = "tool_calls"
				for index, call := range calls {
					id := call.CallID
					if id == "" {
						id = fmt.Sprintf("%s-%d-%s", callBase, index, rand.Text())
					}
					update.Contents = append(update.Contents, &message.FunctionCallContent{CallID: id, Name: call.Name, Arguments: string(call.Arguments)})
				}
			} else {
				update.FinishReason = "stop"
				update.Contents = message.Contents{&message.TextContent{Text: response.Text}}
			}

			yield(update, nil)
		}
	}
}

func schemaValue(schema tool.SchemaTool) any {
	if schema == nil {
		return nil
	}
	return schema.Schema()
}

func stableCallID(threadID, runID string) string {
	sum := sha256.Sum256([]byte(threadID + "\x00" + runID + "\x00tool-1"))
	return "call_" + hex.EncodeToString(sum[:8])
}

func digestValue(value any) string {
	data, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func cloneSkillMaterial(in []platformskills.Material) []platformskills.Material {
	out := make([]platformskills.Material, len(in))
	for i, material := range in {
		out[i] = material
		out[i].Body = append([]byte(nil), material.Body...)
	}
	return out
}
