package runtime

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/microsoft/agent-framework-go/message"
	"github.com/microsoft/agent-framework-go/tool"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
)

type namedTool string

func (n namedTool) Name() string        { return string(n) }
func (n namedTool) Description() string { return "" }

func toolNames(tools []tool.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name()
	}
	return names
}

func journeyWithSkill(id string, skill definitions.SkillPackage) definitions.Journey {
	return definitions.Journey{ID: id, Skills: []definitions.SkillPackage{skill}}
}

func TestRoundToolsOrderAndBuiltins(t *testing.T) {
	got := roundTools([]tool.Tool{namedTool("lookup"), namedTool("book")}, []string{"a", "b"}, nil)
	want := []string{"lookup", "book", "request_user_input", "activate_journey"}
	if !slices.Equal(toolNames(got), want) {
		t.Fatalf("tools = %v, want %v", toolNames(got), want)
	}
}

func TestRoundToolsNoActiveJourneys(t *testing.T) {
	got := roundTools(nil, []string{"a"}, nil)
	if want := []string{"request_user_input", "activate_journey"}; !slices.Equal(toolNames(got), want) {
		t.Fatalf("tools = %v, want %v", toolNames(got), want)
	}
}

func TestRoundToolsReadSkillResourceOnlyWithSupportingFiles(t *testing.T) {
	plain := journeyWithSkill("plain", definitions.SkillPackage{Name: "plain"})
	disabled := journeyWithSkill("off", definitions.SkillPackage{Name: "off", Disabled: true, AllowSupportingFiles: true})
	rich := journeyWithSkill("rich", definitions.SkillPackage{Name: "rich", AllowSupportingFiles: true})
	cases := []struct {
		name   string
		active []definitions.Journey
		want   bool
	}{
		{"none active", nil, false},
		{"no supporting files", []definitions.Journey{plain}, false},
		{"disabled skill", []definitions.Journey{disabled}, false},
		{"supporting files", []definitions.Journey{rich}, true},
		{"one of two", []definitions.Journey{plain, rich}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if offered := offersResources(c.active); offered != c.want {
				t.Fatalf("offersResources = %v, want %v", offered, c.want)
			}
			names := toolNames(roundTools(nil, []string{"plain", "off", "rich"}, c.active))
			if slices.Contains(names, "read_skill_resource") != c.want {
				t.Fatalf("tools = %v, offered want %v", names, c.want)
			}
			if c.want && names[len(names)-1] != "read_skill_resource" {
				t.Fatalf("read_skill_resource not last: %v", names)
			}
		})
	}
}

func schemaOf(t *testing.T, value tool.Tool) map[string]any {
	t.Helper()
	data, err := json.Marshal(value.(tool.SchemaTool).Schema())
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRoundToolsActivateSchemaEnumeratesAvailable(t *testing.T) {
	var activate tool.Tool
	for _, candidate := range roundTools(nil, []string{"billing", "travel"}, nil) {
		if candidate.Name() == "activate_journey" {
			activate = candidate
		}
	}
	if activate == nil {
		t.Fatal("activate_journey missing")
	}
	schema := schemaOf(t, activate)
	properties, _ := schema["properties"].(map[string]any)
	journey, _ := properties["journey"].(map[string]any)
	if schema["additionalProperties"] != false || !reflect.DeepEqual(schema["required"], []any{"journey"}) || journey["type"] != "string" || !reflect.DeepEqual(journey["enum"], []any{"billing", "travel"}) {
		t.Fatalf("schema = %v", schema)
	}
}

func TestRoundToolsActivateSchemaOmitsEmptyEnum(t *testing.T) {
	for _, candidate := range roundTools(nil, nil, nil) {
		if candidate.Name() != "activate_journey" {
			continue
		}
		properties := schemaOf(t, candidate)["properties"].(map[string]any)
		if _, ok := properties["journey"].(map[string]any)["enum"]; ok {
			t.Fatalf("empty enum offered: %v", properties)
		}
		return
	}
	t.Fatal("activate_journey missing")
}

func call(id, name, arguments string) *message.FunctionCallContent {
	return &message.FunctionCallContent{CallID: id, Name: name, Arguments: arguments}
}

func owners(table map[string]string) func(string) (string, bool) {
	return func(alias string) (string, bool) {
		journey, ok := table[alias]
		return journey, ok
	}
}

type planned struct {
	ID      string
	Kind    callKind
	Journey string
}

func summarize(plan []plannedCall) []planned {
	out := make([]planned, len(plan))
	for i, p := range plan {
		out[i] = planned{p.Call.CallID, p.Kind, p.Journey}
	}
	return out
}

func TestDispatchPlanClassifiesEachCall(t *testing.T) {
	plan := planCalls([]*message.FunctionCallContent{
		call("1", "lookup", "{}"),
		call("2", "request_user_input", "{}"),
		call("3", "read_skill_resource", "{}"),
		call("4", "evicted_alias", "{}"),
		call("5", "mystery", "{}"),
	}, true, owners(map[string]string{"lookup": "travel"}))
	want := []planned{
		{"1", callAlias, "travel"},
		{"2", callAsk, ""},
		{"3", callReadResource, ""},
		{"4", callInactive, ""},
		{"5", callInactive, ""},
	}
	if got := summarize(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %+v, want %+v", got, want)
	}
}

func TestDispatchPlanReadSkillResourceNotOfferedIsInactive(t *testing.T) {
	plan := planCalls([]*message.FunctionCallContent{call("1", "read_skill_resource", "{}")}, false, owners(nil))
	if got := summarize(plan); !reflect.DeepEqual(got, []planned{{"1", callInactive, ""}}) {
		t.Fatalf("plan = %+v", got)
	}
}

func TestDispatchPlanActivationMakesEverySiblingNotExecuted(t *testing.T) {
	plan := planCalls([]*message.FunctionCallContent{
		call("1", "lookup", "{}"),
		call("2", "activate_journey", `{"journey":"a"}`),
		call("3", "request_user_input", "{}"),
		call("4", "activate_journey", `{"journey":"b"}`),
		call("5", "read_skill_resource", "{}"),
		call("6", "mystery", "{}"),
	}, true, owners(map[string]string{"lookup": "travel"}))
	want := []planned{
		{"1", callNotExecuted, ""},
		{"2", callActivate, ""},
		{"3", callNotExecuted, ""},
		{"4", callActivate, ""},
		{"5", callNotExecuted, ""},
		{"6", callNotExecuted, ""},
	}
	if got := summarize(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %+v, want %+v", got, want)
	}
}

func TestDispatchPlanGivesEveryCallExactlyOneStepInOrder(t *testing.T) {
	calls := []*message.FunctionCallContent{call("a", "x", "{}"), call("b", "activate_journey", "{}"), call("c", "y", "{}")}
	plan := planCalls(calls, false, owners(nil))
	if len(plan) != len(calls) {
		t.Fatalf("plan length = %d", len(plan))
	}
	for i := range calls {
		if plan[i].Call != calls[i] {
			t.Fatalf("step %d is %q, want %q", i, plan[i].Call.CallID, calls[i].CallID)
		}
	}
	if planCalls(nil, true, owners(nil)) != nil {
		t.Fatal("no calls must plan nothing")
	}
}

func TestOwningJourneyPrefersMostRecentlyUsed(t *testing.T) {
	has := func(journey, alias string) bool {
		return alias == "shared" || (journey == "a" && alias == "only_a")
	}
	active := []string{"a", "b"} // LRU first
	if journey, ok := owningJourney(active, has, "shared"); !ok || journey != "b" {
		t.Fatalf("shared -> %q, %v; want the MRU journey b", journey, ok)
	}
	if journey, ok := owningJourney(active, has, "only_a"); !ok || journey != "a" {
		t.Fatalf("only_a -> %q, %v", journey, ok)
	}
	if journey, ok := owningJourney(active, has, "gone"); ok || journey != "" {
		t.Fatalf("gone -> %q, %v", journey, ok)
	}
	if _, ok := owningJourney(nil, has, "shared"); ok {
		t.Fatal("empty active set owns nothing")
	}
}

func TestComposeInstructionsListsAvailableAndActiveBodies(t *testing.T) {
	available := []definitions.Journey{
		{ID: "billing", Description: "Handle invoices", Prompt: "bill body"},
		{ID: "travel", Description: "Book trips", Prompt: "travel body"},
	}
	active := []definitions.Journey{available[1], available[0]} // LRU first
	got := composeInstructions("  Be helpful.\n", available, active)
	want := "Be helpful.\n\n" +
		"## Available journeys\nCall activate_journey with a journey ID before using that journey's tools.\n" +
		"- billing: Handle invoices\n- travel: Book trips\n\n" +
		"## Active journey: travel\ntravel body\n\n" +
		"## Active journey: billing\nbill body"
	if got != want {
		t.Fatalf("instructions\n got: %q\nwant: %q", got, want)
	}
}

func TestComposeInstructionsEmpty(t *testing.T) {
	if got := composeInstructions("", nil, nil); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestJourneyMetadataKeepsOrderAndUsesDescription(t *testing.T) {
	got := journeyMetadata([]definitions.Journey{{ID: "b", Description: "B desc", Prompt: "hidden"}, {ID: "a", Description: "A desc"}})
	if len(got) != 2 || got[0].ID != "b" || got[0].Description != "B desc" || got[1].ID != "a" {
		t.Fatalf("metadata = %+v", got)
	}
}
