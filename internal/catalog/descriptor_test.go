package catalog

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseDescriptors(t *testing.T) {
	input := json.RawMessage(`[
 {"name":"travel/lookup","description":"Look up","inputSchema":{"type":"object"},
  "_meta":{"operation_key":"lookup","rank":3,"tags":["a","b"]},"annotations":{"readOnlyHint":true}},
 {"name":"plain","annotations":{"readOnlyHint":"true"}},
 {"name":"other","annotations":{"readOnlyHint":false}}
]`)
	got, err := ParseDescriptors(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	d := got[0]
	if d.Name != "travel/lookup" || d.Description != "Look up" || string(d.InputSchema) != `{"type":"object"}` {
		t.Fatalf("descriptor = %+v", d)
	}
	if v, ok := d.Meta["rank"].(float64); !ok || v != 3 {
		t.Fatalf("rank = %#v", d.Meta["rank"])
	}
	if !reflect.DeepEqual(d.Meta["tags"], []any{"a", "b"}) {
		t.Fatalf("tags = %#v", d.Meta["tags"])
	}
	if !d.ReadOnlyHint || got[1].ReadOnlyHint || got[2].ReadOnlyHint {
		t.Fatalf("hints = %v %v %v", d.ReadOnlyHint, got[1].ReadOnlyHint, got[2].ReadOnlyHint)
	}
	var first map[string]any
	if err := json.Unmarshal(d.Raw, &first); err != nil || first["name"] != "travel/lookup" {
		t.Fatalf("raw = %s (%v)", d.Raw, err)
	}
	if got[1].Meta != nil || got[1].InputSchema != nil {
		t.Fatalf("absent fields = %+v", got[1])
	}
	if empty, err := ParseDescriptors(json.RawMessage(`[]`)); err != nil || len(empty) != 0 {
		t.Fatalf("empty = %v, %v", empty, err)
	}
	for name, bad := range map[string]string{
		"object":   `{"name":"x"}`,
		"null":     `null`,
		"string":   `"x"`,
		"nameless": `[{"name":"x"},{"description":"no name"}]`,
		"empty":    `[{"name":""}]`,
		"element":  `["x"]`,
		"trailing": `[] []`,
	} {
		if _, err := ParseDescriptors(json.RawMessage(bad)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
