package runtime

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"

	"github.com/swiftdiaries/az-agent-platform-go/internal/catalog"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journeys"
)

// Names of the built-in tools of the conversation agent. Catalog aliases can never
// use these names (the selector compiler reserves them).
const (
	toolRequestUserInput  = "request_user_input"
	toolActivateJourney   = "activate_journey"
	toolReadSkillResource = "read_skill_resource"
)

// toolCatalog is what the conversation loop needs from a catalog binding.
type toolCatalog interface {
	Resolution(journeyID string) (catalog.Resolution, bool)
	Tool(journeyID, alias string) (catalog.Tool, bool)
	Tools(journeyIDs ...string) []tool.Tool
	Call(ctx context.Context, callID, alias string, arguments []byte) (any, error)
}

var _ toolCatalog = (*catalog.Binding)(nil)

// jsonSchema normalizes a schema literal to the generic JSON shapes the other
// tools use (map[string]any, []any), whatever Go types built it.
func jsonSchema(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}

type activateTool struct{ available []string }

func (activateTool) Name() string { return toolActivateJourney }
func (activateTool) Description() string {
	return "Activate one available journey so that its tools can be used. Call it with the journey ID before using that journey's tools."
}
func (activateTool) ReturnSchema() any { return nil }
func (t activateTool) Schema() any {
	journey := map[string]any{"type": "string"}
	if len(t.available) > 0 {
		journey["enum"] = slices.Clone(t.available)
	}
	return jsonSchema(map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"journey"},
		"properties": map[string]any{"journey": journey},
	})
}

type readSkillResourceTool struct{}

func (readSkillResourceTool) Name() string { return toolReadSkillResource }
func (readSkillResourceTool) Description() string {
	return "Read one declared supporting file of an active journey."
}
func (readSkillResourceTool) ReturnSchema() any { return nil }
func (readSkillResourceTool) Schema() any {
	return jsonSchema(map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"journey", "resource"},
		"properties": map[string]any{"journey": map[string]any{"type": "string"}, "resource": map[string]any{"type": "string"}},
	})
}

// offersResources reports whether any active journey has a skill whose supporting
// files may be read.
func offersResources(active []definitions.Journey) bool {
	for _, journey := range active {
		if _, ok := resourceSkill(journey); ok {
			return true
		}
	}
	return false
}

// resourceSkill returns the name of the journey's first skill with readable supporting files.
func resourceSkill(journey definitions.Journey) (string, bool) {
	for _, skill := range journey.Skills {
		if !skill.Disabled && skill.AllowSupportingFiles {
			return skill.Name, true
		}
	}
	return "", false
}

// roundTools is the tool list of one provider call: the active journeys' alias tools,
// then request_user_input, activate_journey (enumerating the available journey IDs),
// and read_skill_resource only when an active journey has supporting files.
func roundTools(aliasTools []tool.Tool, available []string, active []definitions.Journey) []tool.Tool {
	tools := append([]tool.Tool(nil), aliasTools...)
	tools = append(tools, tool.Tool(questionTool{}), tool.Tool(activateTool{available: slices.Clone(available)}))
	if offersResources(active) {
		tools = append(tools, tool.Tool(readSkillResourceTool{}))
	}
	return tools
}

// journeyMetadata is the model-visible metadata of journeys: the ID and the skill's
// frontmatter description.
func journeyMetadata(list []definitions.Journey) []journeys.Metadata {
	out := make([]journeys.Metadata, len(list))
	for i, journey := range list {
		out[i] = journeys.Metadata{ID: journey.ID, Description: journey.Description}
	}
	return out
}

// composeInstructions renders the instructions of one round: the base prompt, the
// available journeys, and the body of each active journey (least recently used first).
func composeInstructions(base string, available, active []definitions.Journey) string {
	bodies := make([]journeys.Instructions, len(active))
	for i, journey := range active {
		bodies[i] = journeys.Instructions{ID: journey.ID, Body: journey.Prompt}
	}
	return journeys.Compose(base, journeyMetadata(available), bodies)
}

type callKind int

const (
	// callInactive is a call to a tool that is not offered this round. It is never dispatched.
	callInactive callKind = iota
	// callActivate is an activate_journey call.
	callActivate
	// callNotExecuted is a call that shares a response with an activation.
	callNotExecuted
	// callAsk is request_user_input.
	callAsk
	// callReadResource is read_skill_resource.
	callReadResource
	// callAlias is a call to the alias tool of an active journey.
	callAlias
)

type plannedCall struct {
	Call *message.FunctionCallContent
	Kind callKind
	// Journey owns the alias for callAlias.
	Journey string
}

// owningJourney returns the first active journey, most recently used first, that
// has the alias. active is least recently used first.
func owningJourney(active []string, has func(journeyID, alias string) bool, alias string) (string, bool) {
	for i := len(active) - 1; i >= 0; i-- {
		if has(active[i], alias) {
			return active[i], true
		}
	}
	return "", false
}

// planCalls classifies the calls of one model response; each call gets one step in
// order. When any call is activate_journey, every activation is processed and every
// other call is not executed, so the history stays well formed. owner resolves an
// alias to the active journey that owns it; resources reports whether
// read_skill_resource is offered this round.
func planCalls(calls []*message.FunctionCallContent, resources bool, owner func(alias string) (string, bool)) []plannedCall {
	if len(calls) == 0 {
		return nil
	}
	activating := slices.ContainsFunc(calls, func(c *message.FunctionCallContent) bool { return c.Name == toolActivateJourney })
	plan := make([]plannedCall, len(calls))
	for i, c := range calls {
		step := plannedCall{Call: c}
		switch {
		case c.Name == toolActivateJourney:
			step.Kind = callActivate
		case activating:
			step.Kind = callNotExecuted
		case c.Name == toolRequestUserInput:
			step.Kind = callAsk
		case c.Name == toolReadSkillResource && resources:
			step.Kind = callReadResource
		default:
			if journey, ok := owner(c.Name); ok {
				step.Kind, step.Journey = callAlias, journey
			}
		}
		plan[i] = step
	}
	return plan
}
