package catalog

import (
	"math/rand"
	"reflect"
	"strconv"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
)

const testMetaKey = "operation_key"

func desc(name, key string, tags ...string) Descriptor {
	meta := map[string]any{}
	if key != "" {
		meta[testMetaKey] = key
	}
	if tags != nil {
		list := make([]any, len(tags))
		for i, tag := range tags {
			list[i] = tag
		}
		meta["tags"] = list
	}
	return Descriptor{Name: name, Description: "desc " + name, InputSchema: []byte(`{"type":"object"}`), Meta: meta}
}

func hinted(d Descriptor) Descriptor { d.ReadOnlyHint = true; return d }

func ops(keys ...string) definitions.Selector { return definitions.Selector{Ops: keys} }

func tagSel(tags ...string) definitions.Selector { return definitions.Selector{Tags: tags} }

type wantTool struct {
	alias, key, remote string
	class              definitions.Class
	approval           bool
}

func TestResolve(t *testing.T) {
	roClass := func(s definitions.Selector) definitions.Selector { s.Class = definitions.ClassReadOnly; return s }
	cases := []struct {
		name    string
		journey definitions.Journey
		tools   []Descriptor
		version string
		want    []wantTool
		unavail []Unavailable
	}{
		{
			name:    "single op with slash remote name",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("travel.lookup")}},
			tools:   []Descriptor{desc("travel/lookup", "travel.lookup"), desc("other", "other")},
			want:    []wantTool{{"travel_lookup", "travel.lookup", "travel/lookup", definitions.ClassEffectful, true}},
		},
		{
			name:    "single op with space remote name",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("travel.lookup")}},
			tools:   []Descriptor{desc("travel lookup", "travel.lookup")},
			want:    []wantTool{{"travel_lookup", "travel.lookup", "travel lookup", definitions.ClassEffectful, true}},
		},
		{
			name:    "alternatives prefer first present v2",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("lookup.v2", "lookup.v1")}},
			tools:   []Descriptor{desc("old", "lookup.v1"), desc("new", "lookup.v2")},
			want:    []wantTool{{"lookup_v2", "lookup.v2", "new", definitions.ClassEffectful, true}},
		},
		{
			name:    "alternatives fall back to v1",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("lookup.v2", "lookup.v1")}},
			tools:   []Descriptor{desc("old", "lookup.v1")},
			want:    []wantTool{{"lookup_v1", "lookup.v1", "old", definitions.ClassEffectful, true}},
		},
		{
			name:    "missing across all alternatives reports first",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("lookup.v2", "lookup.v1")}},
			tools:   []Descriptor{desc("x", "other")},
			unavail: []Unavailable{{ReasonMissing, "lookup.v2"}},
		},
		{
			name:    "ambiguous when two tools share a key",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("lookup")}},
			tools:   []Descriptor{desc("a", "lookup"), desc("b", "lookup")},
			unavail: []Unavailable{{ReasonAmbiguous, "lookup"}},
		},
		{
			name:    "tags select all tools carrying all tags",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{tagSel("leave", "hr")}},
			tools: []Descriptor{
				desc("c", "leave.c", "leave", "hr"), desc("a", "leave.a", "leave", "hr", "x"),
				desc("b", "leave.b", "leave"), desc("n", "", "leave", "hr"),
			},
			want: []wantTool{
				{"leave_a", "leave.a", "a", definitions.ClassEffectful, true},
				{"leave_c", "leave.c", "c", definitions.ClassEffectful, true},
			},
		},
		{
			name:    "tags ignore non-string elements",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{tagSel("leave")}},
			tools: []Descriptor{
				{Name: "mixed", Meta: map[string]any{testMetaKey: "m", "tags": []any{1.0, "leave", nil}}},
				{Name: "notarray", Meta: map[string]any{testMetaKey: "n", "tags": "leave"}},
			},
			want: []wantTool{{"m", "m", "mixed", definitions.ClassEffectful, true}},
		},
		{
			name: "tags with read-only hint narrow",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{
				{Tags: []string{"leave"}, ReadOnlyHint: true},
			}},
			tools: []Descriptor{hinted(desc("get", "leave.get", "leave")), desc("del", "leave.del", "leave")},
			want:  []wantTool{{"leave_get", "leave.get", "get", definitions.ClassEffectful, true}},
		},
		{
			name:    "tags matching nothing keep journey available",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("a"), tagSel("nothing")}},
			tools:   []Descriptor{desc("a", "a")},
			want:    []wantTool{{"a", "a", "a", definitions.ClassEffectful, true}},
		},
		{
			name: "same tool via op and tags merges overlays",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{
				{Ops: []string{"leave.get"}, Class: definitions.ClassReadOnly},
				{Tags: []string{"leave"}, Class: definitions.ClassIdempotent, ApprovalRequired: true},
			}},
			tools: []Descriptor{desc("get", "leave.get", "leave")},
			want:  []wantTool{{"leave_get", "leave.get", "get", definitions.ClassIdempotent, true}},
		},
		{
			name: "merge with unclassified selection is effectful",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{
				roClass(ops("leave.get")), tagSel("leave"),
			}},
			tools: []Descriptor{desc("get", "leave.get", "leave")},
			want:  []wantTool{{"leave_get", "leave.get", "get", definitions.ClassEffectful, true}},
		},
		{
			name: "tags selecting a different tool for a chosen key is ambiguous",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{
				{Ops: []string{"leave.get"}, ReadOnlyHint: true}, tagSel("leave"),
			}},
			tools:   []Descriptor{hinted(desc("a", "leave.get")), desc("b", "leave.get", "leave")},
			unavail: []Unavailable{{ReasonAmbiguous, "leave.get"}},
		},
		{
			name:    "deny removes tag-selected keys",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{tagSel("leave")}, Deny: []string{"leave.delete"}},
			tools:   []Descriptor{desc("get", "leave.get", "leave"), desc("del", "leave.delete", "leave")},
			want:    []wantTool{{"leave_get", "leave.get", "get", definitions.ClassEffectful, true}},
		},
		{
			name:    "denied required key is missing",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("leave.delete")}, Deny: []string{"leave.delete"}},
			tools:   []Descriptor{desc("del", "leave.delete")},
			unavail: []Unavailable{{ReasonMissing, "leave.delete"}},
		},
		{
			name:    "deny resolves ambiguity by removing the key entirely",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("a")}, Deny: []string{"b"}},
			tools:   []Descriptor{desc("a", "a"), desc("b1", "b"), desc("b2", "b")},
			want:    []wantTool{{"a", "a", "a", definitions.ClassEffectful, true}},
		},
		{
			name:    "alias collision between dotted and underscored keys",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{tagSel("t")}},
			tools:   []Descriptor{desc("x", "a.b", "t"), desc("y", "a_b", "t")},
			unavail: []Unavailable{{ReasonCollision, "a_b"}},
		},
		{
			name:    "reserved alias via tags",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{tagSel("t")}},
			tools:   []Descriptor{desc("x", "request_user_input", "t")},
			unavail: []Unavailable{{ReasonCollision, "request_user_input"}},
		},
		{
			name:    "invalid alias via tags",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{tagSel("t")}},
			tools:   []Descriptor{desc("x", "9lives", "t")},
			unavail: []Unavailable{{ReasonInvalidAlias, "9lives"}},
		},
		{
			name:    "count above max tools",
			journey: definitions.Journey{ID: "j", MaxTools: 2, Tools: []definitions.Selector{tagSel("t")}},
			tools:   []Descriptor{desc("a", "a", "t"), desc("b", "b", "t"), desc("c", "c", "t")},
			unavail: []Unavailable{{ReasonMaxTools, ""}},
		},
		{
			name:    "count equal to max tools is allowed",
			journey: definitions.Journey{ID: "j", MaxTools: 2, Tools: []definitions.Selector{tagSel("t")}},
			tools:   []Descriptor{desc("a", "a", "t"), desc("b", "b", "t")},
			want: []wantTool{
				{"a", "a", "a", definitions.ClassEffectful, true},
				{"b", "b", "b", definitions.ClassEffectful, true},
			},
		},
		{
			name:    "zero max tools defaults to fifteen",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{tagSel("t")}},
			tools:   manyTools(16),
			unavail: []Unavailable{{ReasonMaxTools, ""}},
		},
		{
			name: "product version below range",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("a")},
				ProductVersions: definitions.VersionRange{Min: "2026.2", MaxExclusive: "2027.1"}},
			tools:   []Descriptor{desc("a", "a")},
			version: "2026.1",
			unavail: []Unavailable{{ReasonVersion, ""}},
		},
		{
			name: "product version at exclusive max",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("a")},
				ProductVersions: definitions.VersionRange{Min: "2026.2", MaxExclusive: "2027.1"}},
			tools:   []Descriptor{desc("a", "a")},
			version: "2027.1",
			unavail: []Unavailable{{ReasonVersion, ""}},
		},
		{
			name: "product version inside range",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("a")},
				ProductVersions: definitions.VersionRange{Min: "2026.2", MaxExclusive: "2027.1"}},
			tools:   []Descriptor{desc("a", "a")},
			version: "2026.10",
			want:    []wantTool{{"a", "a", "a", definitions.ClassEffectful, true}},
		},
		{
			name:    "tools without an op key are never selected",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("a"), tagSel("t")}},
			tools: []Descriptor{
				desc("keyless", "", "t"),
				{Name: "nonstring", Meta: map[string]any{testMetaKey: 7.0, "tags": []any{"t"}}},
				{Name: "empty", Meta: map[string]any{testMetaKey: "", "tags": []any{"t"}}},
				{Name: "nometa"},
			},
			unavail: []Unavailable{{ReasonMissing, "a"}},
		},
		{
			name:    "default class is effectful and requires approval",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("a")}},
			tools:   []Descriptor{desc("a", "a")},
			want:    []wantTool{{"a", "a", "a", definitions.ClassEffectful, true}},
		},
		{
			name:    "read-only class needs no approval",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{roClass(ops("a"))}},
			tools:   []Descriptor{desc("a", "a")},
			want:    []wantTool{{"a", "a", "a", definitions.ClassReadOnly, false}},
		},
		{
			name: "read-only class with approval required needs approval",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{
				{Ops: []string{"a"}, Class: definitions.ClassReadOnly, ApprovalRequired: true},
			}},
			tools: []Descriptor{desc("a", "a")},
			want:  []wantTool{{"a", "a", "a", definitions.ClassReadOnly, true}},
		},
		{
			name: "idempotent class needs approval",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{
				{Ops: []string{"a"}, Class: definitions.ClassIdempotent},
			}},
			tools: []Descriptor{desc("a", "a")},
			want:  []wantTool{{"a", "a", "a", definitions.ClassIdempotent, true}},
		},
		{
			name:    "read-only hint alone never grants read_only",
			journey: definitions.Journey{ID: "j", Tools: []definitions.Selector{ops("a")}},
			tools:   []Descriptor{hinted(desc("a", "a"))},
			want:    []wantTool{{"a", "a", "a", definitions.ClassEffectful, true}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			version := tc.version
			if version == "" {
				version = "2026.5"
			}
			res := Resolve(tc.journey, Snapshot{Digest: "d", ProductVersion: version, Tools: tc.tools}, testMetaKey)
			if res.JourneyID != "j" {
				t.Fatalf("JourneyID = %q", res.JourneyID)
			}
			if !reflect.DeepEqual(res.Unavailable, tc.unavail) && !(len(res.Unavailable) == 0 && len(tc.unavail) == 0) {
				t.Fatalf("Unavailable = %+v, want %+v", res.Unavailable, tc.unavail)
			}
			if res.Available() != (len(tc.unavail) == 0) {
				t.Fatalf("Available = %v", res.Available())
			}
			if len(tc.unavail) > 0 {
				if res.Tools != nil {
					t.Fatalf("Tools = %+v, want nil", res.Tools)
				}
				return
			}
			var got []wantTool
			for _, tool := range res.Tools {
				got = append(got, wantTool{tool.Alias, tool.OperationKey, tool.RemoteName, tool.Class, tool.RequiresApproval})
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Tools = %+v, want %+v", got, tc.want)
			}
			for _, tool := range res.Tools {
				var src Descriptor
				for _, d := range tc.tools {
					if d.Name == tool.RemoteName {
						src = d
					}
				}
				if tool.Description != src.Description || string(tool.InputSchema) != string(src.InputSchema) {
					t.Fatalf("tool %s description/schema not from descriptor: %+v", tool.Alias, tool)
				}
			}
		})
	}
}

func manyTools(n int) []Descriptor {
	out := make([]Descriptor, n)
	for i := range out {
		out[i] = desc("tool"+strconv.Itoa(i), "k"+strconv.Itoa(i), "t")
	}
	return out
}

func TestResolveDeterministic(t *testing.T) {
	journey := definitions.Journey{ID: "j", MaxTools: 3, Tools: []definitions.Selector{
		ops("a.one"), ops("missing.x"), tagSel("t"), ops("dup"),
		{Ops: []string{"b.two"}, Class: definitions.ClassReadOnly},
	}}
	tools := []Descriptor{
		desc("a", "a.one", "t"), desc("b", "b.two", "t"), desc("c", "c.three", "t"), desc("d", "d_four", "t"),
		desc("e", "d.four", "t"), desc("dup1", "dup"), desc("dup2", "dup"), desc("f", "9x", "t"),
		desc("g", "request_user_input", "t"),
	}
	ok := definitions.Journey{ID: "j", Tools: []definitions.Selector{tagSel("t")}}
	want := Resolve(journey, Snapshot{Tools: tools, ProductVersion: "1"}, testMetaKey)
	wantOK := Resolve(ok, Snapshot{Tools: manyTools(5), ProductVersion: "1"}, testMetaKey)
	if want.Available() || !wantOK.Available() {
		t.Fatalf("fixtures: %+v / %+v", want, wantOK)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		shuffled := append([]Descriptor(nil), tools...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := Resolve(journey, Snapshot{Tools: shuffled, ProductVersion: "1"}, testMetaKey); !reflect.DeepEqual(got, want) {
			t.Fatalf("shuffle %d: %+v != %+v", i, got, want)
		}
		many := manyTools(5)
		rng.Shuffle(len(many), func(a, b int) { many[a], many[b] = many[b], many[a] })
		if got := Resolve(ok, Snapshot{Tools: many, ProductVersion: "1"}, testMetaKey); !reflect.DeepEqual(got, wantOK) {
			t.Fatalf("available shuffle %d: %+v != %+v", i, got, wantOK)
		}
	}
	seen := map[Unavailable]bool{}
	for _, u := range want.Unavailable {
		if seen[u] {
			t.Fatalf("duplicate unavailable %+v", u)
		}
		seen[u] = true
	}
}
