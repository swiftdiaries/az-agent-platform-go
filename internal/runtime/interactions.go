package runtime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/message"

	"github.com/swiftdiaries/az-agent-platform-go/internal/catalog"
	"github.com/swiftdiaries/az-agent-platform-go/internal/cost"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journeys"
	platformskills "github.com/swiftdiaries/az-agent-platform-go/internal/skills"
)

type questionTool struct{}

func (questionTool) Name() string { return toolRequestUserInput }
func (questionTool) Description() string {
	return "Ask one to three structured clarification questions. An answer never approves a business action."
}
func (questionTool) ReturnSchema() any { return nil }
func (questionTool) Schema() any {
	var schema any
	_ = json.Unmarshal([]byte(`{"type":"object","additionalProperties":false,"required":["questions"],"properties":{"questions":{"type":"array","minItems":1,"maxItems":3,"items":{"type":"object","additionalProperties":false,"required":["id","header","question","options"],"properties":{"id":{"type":"string"},"header":{"type":"string","maxLength":12},"question":{"type":"string"},"options":{"type":"array","minItems":2,"maxItems":3,"items":{"type":"object","additionalProperties":false,"required":["label","description"],"properties":{"label":{"type":"string"},"description":{"type":"string"}}}}}}}}}`), &schema)
	return schema
}

// conversation is the state of one run's conversation agent: the pinned set, the
// catalog, the journeys available to this user (configuration order), and the active
// set, which activation changes between rounds.
type conversation struct {
	set       *definitions.Set
	cat       toolCatalog
	available []definitions.Journey
	active    *journeys.ActiveSet
	limit     int
}

func (c *conversation) journey(id string) (definitions.Journey, bool) {
	for _, journey := range c.available {
		if journey.ID == id {
			return journey, true
		}
	}
	return definitions.Journey{}, false
}

func (c *conversation) isAvailable(id string) bool {
	_, ok := c.journey(id)
	return ok
}

func (c *conversation) availableIDs() []string {
	ids := make([]string, len(c.available))
	for i, journey := range c.available {
		ids[i] = journey.ID
	}
	return ids
}

// activeJourneys returns the active journeys, least recently used first.
func (c *conversation) activeJourneys() []definitions.Journey {
	var out []definitions.Journey
	for _, id := range c.active.IDs() {
		if journey, ok := c.journey(id); ok {
			out = append(out, journey)
		}
	}
	return out
}

// owner resolves an alias to the active journey that owns it, most recently used first.
func (c *conversation) owner(alias string) (string, bool) {
	return owningJourney(c.active.IDs(), func(journeyID, alias string) bool {
		_, ok := c.cat.Tool(journeyID, alias)
		return ok
	}, alias)
}

// callBinding is the v2 binding of one proposed call and the fields it was computed from.
type callBinding struct {
	journeyID, operationKey, nameDigest, schemaDigest, binding string
}

// bindCall computes the binding of a call. An alias call passes its owning journey and
// resolved tool; a clarification passes "" and the question tool's operation.
func (c *conversation) bindCall(in RunInput, journeyID, operationKey, remoteName string, schema json.RawMessage, arguments string) (callBinding, error) {
	schemaDigest, err := journal.SchemaDigest(schema)
	if err != nil {
		return callBinding{}, err
	}
	b := callBinding{journeyID: journeyID, operationKey: operationKey, nameDigest: journal.NameDigest(remoteName), schemaDigest: schemaDigest}
	b.binding, err = journal.BindCall(journal.CallBinding{
		JourneyID: journeyID, OperationKey: operationKey, NameDigest: b.nameDigest, SchemaDigest: schemaDigest,
		Arguments: json.RawMessage(arguments), ProductVersion: in.ProductVersion, Principal: in.Principal,
		Conversation: in.ThreadID, DefinitionSet: c.set.Digest(),
	})
	return b, err
}

func (c *conversation) bindTool(in RunInput, journeyID string, t catalog.Tool, arguments string) (callBinding, error) {
	return c.bindCall(in, journeyID, t.OperationKey, t.RemoteName, t.InputSchema, arguments)
}

func (c *conversation) bindClarification(in RunInput, arguments string) (callBinding, error) {
	schema, err := json.Marshal(questionTool{}.Schema())
	if err != nil {
		return callBinding{}, err
	}
	return c.bindCall(in, "", toolRequestUserInput, toolRequestUserInput, schema, arguments)
}

// interaction builds the interaction that carries a call and its binding.
func (b callBinding) interaction(kind, callID string, call *message.FunctionCallContent) journal.Interaction {
	return journal.Interaction{
		ID: "interaction_" + rand.Text(), Kind: kind, JourneyID: b.journeyID, OperationKey: b.operationKey,
		NameDigest: b.nameDigest, SchemaDigest: b.schemaDigest,
		Call: journal.ProposedCall{CallID: callID, ProviderCallID: call.CallID, Name: call.Name, Arguments: json.RawMessage(call.Arguments), Binding: b.binding},
	}
}

func (b callBinding) operation(callID, name, arguments string, t catalog.Tool) journal.Operation {
	return journal.Operation{CallID: callID, Name: name, OperationKey: b.operationKey, NameDigest: b.nameDigest, SchemaDigest: b.schemaDigest, Arguments: arguments, Binding: b.binding, Policy: string(t.Class), RequiresApproval: t.RequiresApproval, JourneyID: b.journeyID}
}

// activate processes one activate_journey call. Only a failure of the fenced journal
// ends the run; every refusal is a result for the model.
func (c *conversation) activate(ctx context.Context, in RunInput, arguments string) (any, error) {
	var args struct {
		Journey string `json:"journey"`
	}
	if journal.DecodeStrict([]byte(arguments), &args) != nil || args.Journey == "" {
		return map[string]string{"error": "invalid_arguments"}, nil
	}
	if !c.isAvailable(args.Journey) {
		return map[string]string{"error": "journey_unavailable"}, nil
	}
	// The persisted set equals the in-memory one (every change is written); the fenced
	// read also supplies the journeys pinned now.
	persisted, pinned, err := in.Store.ActiveJourneys(ctx, in.Owner)
	if err != nil {
		return nil, err
	}
	next := journeys.NewActiveSet(persisted)
	evicted, err := next.Activate(args.Journey, c.limit, pinned)
	if errors.Is(err, journeys.ErrRefused) {
		return map[string]string{"error": "activation_refused", "message": "all active journeys are pinned"}, nil
	}
	if err != nil {
		return nil, err
	}
	if !slices.Equal(persisted, next.IDs()) {
		if err := in.Store.SetActiveJourneys(ctx, in.Owner, next.IDs()); err != nil {
			return nil, err
		}
	}
	c.active = next
	result := map[string]any{"activated": args.Journey}
	if len(evicted) > 0 {
		result["evicted"] = evicted
	}
	return result, nil
}

// readResource processes one read_skill_resource call. It returns the file body and
// its provenance. skills.ErrChanged (and any read failure other than an undeclared
// file) ends the run.
func (c *conversation) readResource(arguments string) (any, *MaterialProvenance, error) {
	var args struct {
		Journey  string `json:"journey"`
		Resource string `json:"resource"`
	}
	if journal.DecodeStrict([]byte(arguments), &args) != nil || args.Journey == "" || args.Resource == "" {
		return map[string]string{"error": "invalid_arguments"}, nil, nil
	}
	unavailable := map[string]string{"error": "resource_unavailable"}
	journey, ok := c.journey(args.Journey)
	if !ok || !c.active.Contains(args.Journey) || args.Resource == "SKILL.md" {
		return unavailable, nil, nil
	}
	name, ok := resourceSkill(journey)
	if !ok {
		return unavailable, nil, nil
	}
	material, err := platformskills.New(journey).Read(name, args.Resource)
	if errors.Is(err, platformskills.ErrUnavailable) {
		return unavailable, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return string(material.Body), &MaterialProvenance{ID: args.Journey + "/" + material.Resource, Digest: material.FileDigest}, nil
}

func (r *Runner) runHarness(ctx context.Context, in RunInput, conv *conversation) (output RunOutput, runErr error) {
	var history []*message.Message
	if err := json.Unmarshal(in.History, &history); err != nil {
		return RunOutput{}, err
	}
	// Preserve completed results on a terminal failure and close any unexecuted
	// proposal before a later user turn. This records no claim about remote cancellation.
	defer func() {
		if runErr == nil || len(history) == 0 {
			return
		}
		open := map[string]bool{}
		var order []string
		for _, m := range history {
			for _, c := range m.Contents {
				switch content := c.(type) {
				case *message.FunctionCallContent:
					if !open[content.CallID] {
						order = append(order, content.CallID)
					}
					open[content.CallID] = true
				case *message.FunctionResultContent:
					delete(open, content.CallID)
				}
			}
		}
		for _, id := range order {
			if open[id] {
				history = append(history, resultMessage(id, map[string]string{"error": "run_stopped"}))
				delete(open, id)
			}
		}
		output.History, _ = json.Marshal(history)
	}()
	// pending and requestHistory are shared by every round's agent. invoke drains the
	// inbox into pending before it builds the agent; the agent's before hook copies it
	// onto the request and its observed callback records its inclusion.
	var pending []journal.Input
	var requestHistory json.RawMessage
	var skills []MaterialProvenance
	// buildAgent makes a fresh single-provider-call agent from the current active set.
	// It is never reused across rounds: activation changes its tools and instructions.
	buildAgent := func() *agent.Agent {
		active := conv.activeJourneys()
		activeIDs := make([]string, len(active))
		for i, journey := range active {
			activeIDs[i] = journey.ID
		}
		metadata := journeyMetadata(conv.available)
		instructions := composeInstructions(conv.set.ConversationPrompt(), conv.available, active)
		tools := roundTools(conv.cat.Tools(activeIDs...), conv.availableIDs(), active)
		before := func(ctx context.Context, req *ModelRequest) error {
			req.PendingCommands = pending
			req.History = requestHistory
			req.DefinitionDigest = conv.set.Digest()
			req.ActiveJourneys = slices.Clone(activeIDs)
			req.Provenance = RequestProvenance{
				Definition: MaterialProvenance{ID: "set", Digest: conv.set.Digest()},
				System:     MaterialProvenance{ID: "instructions", Digest: digestValue(req.Instructions)},
				Catalog:    MaterialProvenance{ID: "journeys", Digest: digestValue(metadata)},
				History:    MaterialProvenance{ID: in.ThreadID, Digest: digestValue(req.History)},
				Skills:     slices.Clone(skills),
			}
			for _, journey := range active {
				req.Provenance.Active = append(req.Provenance.Active, MaterialProvenance{ID: journey.ID, Digest: journey.Digest})
			}
			for _, command := range req.PendingCommands {
				if !command.Materialized {
					continue
				}
				item := MaterialProvenance{ID: command.CommunicationID, Digest: command.Digest}
				req.Provenance.Pending = append(req.Provenance.Pending, item)
				req.Provenance.IncludedInput = append(req.Provenance.IncludedInput, item)
			}
			return in.Store.Check(ctx, in.Owner)
		}
		observed := func(ctx context.Context) error { return in.Store.Included(ctx, in.Owner, pending) }
		recordUsage := func(ctx context.Context, t cost.Tokens) error {
			_, err := in.Store.RecordUsage(ctx, in.Owner, journal.Usage{Model: t.Model, Input: t.Input, Output: t.Output, Cached: t.Cached, JourneyID: in.Attribution.Journey})
			return err
		}
		return agent.New(
			agent.ProviderConfig{ProviderName: "configured-model", Run: modelRun(ctx, r.model, stableCallID(in.ThreadID, in.RunID), before, observed, in.Access, recordUsage)},
			agent.Config{ID: "conversation", Name: "conversation", Description: "Conversation agent", Tools: tools, RunOptions: []agent.Option{agent.WithInstructions(instructions)}},
		)
	}
	continuation, err := in.Store.Continuation(ctx, in.Owner)
	if err != nil {
		return RunOutput{}, err
	}
	repairBudget := 1
	var approved *journal.Interaction
	if continuation != nil && in.Iteration == 0 {
		var checkpoint struct {
			Mode    string `json:"mode"`
			Repairs int    `json:"repairs"`
		}
		if journal.DecodeStrict(continuation.Checkpoint, &checkpoint) != nil || checkpoint.Mode != "product_resume_in_place" {
			return RunOutput{}, journal.ErrState
		}
		repairBudget = checkpoint.Repairs
		if err = json.Unmarshal(continuation.History, &history); err != nil {
			return RunOutput{}, err
		}
		i := continuation.Interaction
		if i.Kind == "approval" && continuation.Reply.Decision == "approve" {
			approved = &i
		} else {
			var result any = continuation.Reply
			if i.Kind == "approval" {
				result = map[string]string{"error": "approval_denied"}
			}
			m := resultMessage(i.Call.ProviderCallID, result)
			history = append(history, m)
		}
	}
	invoke := func() (*agent.Response, error) {
		inbox, err := in.Store.Pending(ctx, in.Owner)
		if err != nil {
			return nil, err
		}
		pending = inbox.Commands
		requestHistory, err = json.Marshal(history)
		if err != nil {
			return nil, err
		}
		beforeInput := len(history)
		for _, c := range pending {
			if c.Text != "" {
				m := &message.Message{Role: message.RoleUser, Contents: message.Contents{&message.TextContent{Text: c.Text}}}
				history = append(history, m)
			}
		}
		// Function results precede newly admitted user messages in materialized history.
		response, err := buildAgent().Run(ctx, history).Collect()
		if err == nil {
			history = append(history, response.Messages...)
		} else {
			history = history[:beforeInput]
		}
		return response, err
	}
	callNumber := 0
	nextCallID := func() string {
		callNumber++
		return stableCallID(in.ThreadID, fmt.Sprintf("%s/%d/%d", in.RunID, in.Iteration, callNumber))
	}
	// wait checkpoints one interaction with the history so far and ends the run waiting.
	// journal.ErrPending (steering arrived) is returned unchanged.
	wait := func(i journal.Interaction) (RunOutput, error) {
		checkpoint, err := json.Marshal(struct {
			Mode    string `json:"mode"`
			Repairs int    `json:"repairs"`
		}{"product_resume_in_place", repairBudget})
		if err != nil {
			return RunOutput{}, err
		}
		serialized, err := json.Marshal(history)
		if err != nil {
			return RunOutput{}, err
		}
		if err := in.Store.Wait(ctx, in.Owner, i, checkpoint, serialized); err != nil {
			return RunOutput{}, err
		}
		return RunOutput{JourneyID: in.TargetJourney, History: serialized, Waiting: true}, nil
	}
	// settle records a dispatch result, spending the repair budget on a business rejection.
	settle := func(providerCallID string, result any) error {
		if failure, ok := result.(map[string]any); ok && failure["error"] == "business_rejected" {
			if repairBudget == 0 {
				return fmt.Errorf("business repair budget exhausted")
			}
			repairBudget--
		}
		history = append(history, resultMessage(providerCallID, result))
		return nil
	}
	if approved != nil {
		call := &message.FunctionCallContent{CallID: approved.Call.ProviderCallID, Name: approved.Call.Name, Arguments: string(approved.Call.Arguments)}
		// supersededWait opens a new approval for the same action, bound to the same
		// journey and the current definitions; the provider call stays open.
		supersededWait := func(t catalog.Tool) (RunOutput, bool, error) {
			b, err := conv.bindTool(in, approved.JourneyID, t, call.Arguments)
			if err != nil {
				return RunOutput{}, false, err
			}
			out, err := wait(b.interaction("approval", nextCallID(), call))
			if errors.Is(err, journal.ErrPending) {
				history = append(history, resultMessage(call.CallID, map[string]string{"error": "superseded_by_steering"}))
				return RunOutput{}, false, nil
			}
			return out, err == nil, err
		}
		// The approval is only valid for the journey it was granted under, resolved now.
		tool, resolves := catalog.Tool{}, false
		if conv.isAvailable(approved.JourneyID) {
			tool, resolves = conv.cat.Tool(approved.JourneyID, call.Name)
		}
		var out RunOutput
		var waiting bool
		switch {
		case !resolves:
			if err := in.Store.Supersede(ctx, in.Owner, approved.ID); err != nil {
				return RunOutput{}, err
			}
			history = append(history, resultMessage(call.CallID, map[string]string{"error": "approval_superseded", "message": "the approved action is no longer available"}))
		default:
			b, err := conv.bindTool(in, approved.JourneyID, tool, call.Arguments)
			if err != nil {
				return RunOutput{}, err
			}
			if b.binding != approved.Call.Binding {
				if err := in.Store.Supersede(ctx, in.Owner, approved.ID); err != nil {
					return RunOutput{}, err
				}
				if out, waiting, err = supersededWait(tool); err != nil || waiting {
					return out, err
				}
				break
			}
			callNumber++
			op := b.operation(approved.Call.CallID, call.Name, call.Arguments, tool)
			// The approval is consumed (and marked dispatched) even if the policy no longer requires one.
			op.RequiresApproval = true
			result, err := guardedCall(ctx, in, conv.cat, op, approved.ID)
			if errors.Is(err, journal.ErrSuperseded) {
				if out, waiting, err = supersededWait(tool); err != nil || waiting {
					return out, err
				}
				break
			}
			if err != nil {
				return RunOutput{}, err
			}
			if err := settle(call.CallID, result); err != nil {
				return RunOutput{}, err
			}
		}
		approved = nil
	}
	var calls []*message.FunctionCallContent
	for round := 0; round < 16; round++ {
		if len(calls) == 0 {
			response, err := invoke()
			if err != nil {
				return RunOutput{}, fmt.Errorf("model request failed: %w", err)
			}
			for c := range response.Contents() {
				if call, ok := c.(*message.FunctionCallContent); ok && !call.InformationalOnly {
					calls = append(calls, call)
				}
			}
			if len(calls) == 0 {
				if response.String() == "" {
					return RunOutput{}, fmt.Errorf("model returned no answer")
				}
				serialized, err := json.Marshal(history)
				return RunOutput{JourneyID: in.TargetJourney, Answer: response.String(), History: serialized}, err
			}
		}
		plan := planCalls(calls, offersResources(conv.activeJourneys()), conv.owner)
		for index, step := range plan {
			call := step.Call
			callNumber++
			switch step.Kind {
			case callNotExecuted:
				history = append(history, resultMessage(call.CallID, map[string]string{"error": "not_executed", "message": "not executed; call again after activation"}))
				continue
			case callActivate:
				result, err := conv.activate(ctx, in, call.Arguments)
				if err != nil {
					return RunOutput{}, err
				}
				history = append(history, resultMessage(call.CallID, result))
				continue
			case callReadResource:
				result, provenance, err := conv.readResource(call.Arguments)
				if err != nil {
					return RunOutput{}, err
				}
				if provenance != nil {
					skills = append(skills, *provenance)
				}
				history = append(history, resultMessage(call.CallID, result))
				continue
			case callInactive:
				// Never dispatched, whatever the model asks for: the gate fails closed.
				history = append(history, resultMessage(call.CallID, map[string]string{"error": "tool_not_active"}))
				continue
			}
			callID := nextCallID()
			var resolved catalog.Tool
			var b callBinding
			var err error
			if step.Kind == callAlias {
				var allowed bool
				resolved, allowed = conv.cat.Tool(step.Journey, call.Name)
				if !allowed {
					return RunOutput{}, fmt.Errorf("tool is not allowed")
				}
				b, err = conv.bindTool(in, step.Journey, resolved, call.Arguments)
			} else if step.Kind == callAsk {
				b, err = conv.bindClarification(in, call.Arguments)
			}
			if err != nil {
				return RunOutput{}, err
			}
			if step.Kind == callAsk || resolved.RequiresApproval {
				// Close siblings explicitly before checkpointing one externally answerable
				// request; they may be proposed again after this wait, never auto-dispatched.
				for _, sibling := range plan[index+1:] {
					m := resultMessage(sibling.Call.CallID, map[string]string{"error": "deferred_for_human_input"})
					history = append(history, m)
				}

				// A clarification is not owned by a journey; an approval belongs to the
				// journey that owns the tool.
				i := b.interaction("approval", callID, call)
				if step.Kind == callAsk {
					i.Kind = "clarification"
					var args struct {
						Questions []journal.Question `json:"questions"`
					}
					if err := journal.DecodeStrict([]byte(call.Arguments), &args); err != nil {
						return RunOutput{}, err
					}
					i.Questions = args.Questions
				}
				out, err := wait(i)
				if errors.Is(err, journal.ErrPending) {
					m := resultMessage(call.CallID, map[string]string{"error": "superseded_by_steering"})
					history = append(history, m)
					break
				}
				return out, err
			}
			result, err := guardedCall(ctx, in, conv.cat, b.operation(callID, call.Name, call.Arguments, resolved), "")
			if err != nil {
				return RunOutput{}, err
			}
			if err := settle(call.CallID, result); err != nil {
				return RunOutput{}, err
			}
		}
		calls = nil
	}
	return RunOutput{}, fmt.Errorf("provider tool round budget exhausted")
}

func resultMessage(id string, result any) *message.Message {
	return &message.Message{Role: message.RoleTool, Contents: message.Contents{&message.FunctionResultContent{CallID: id, Result: result}}}
}
