package journal

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func baseBinding() CallBinding {
	return CallBinding{
		JourneyID:      "journey",
		OperationKey:   "op.key",
		NameDigest:     NameDigest("remote/name"),
		SchemaDigest:   mustSchemaDigest(`{"type":"object"}`),
		Arguments:      json.RawMessage(`{"a":1,"b":"x"}`),
		ProductVersion: "v1",
		Principal:      "principal",
		Conversation:   "thread",
		DefinitionSet:  strings.Repeat("a", 64),
	}
}

func mustSchemaDigest(s string) string {
	d, err := SchemaDigest(json.RawMessage(s))
	if err != nil {
		panic(err)
	}
	return d
}

func mustBind(t *testing.T, b CallBinding) string {
	t.Helper()
	d, err := BindCall(b)
	if err != nil {
		t.Fatalf("BindCall: %v", err)
	}
	return d
}

func TestBindCallNumberFidelity(t *testing.T) {
	a, b := baseBinding(), baseBinding()
	a.Arguments = json.RawMessage(`{"n":9007199254740992}`)
	b.Arguments = json.RawMessage(`{"n":9007199254740993}`)
	if mustBind(t, a) == mustBind(t, b) {
		t.Fatal("distinct large integers collided")
	}
	c, d := baseBinding(), baseBinding()
	c.Arguments = json.RawMessage(`{"x":1,"y":[true,null]}`)
	d.Arguments = json.RawMessage(" {\n \"y\" : [ true , null ] ,\t\"x\" : 1 } ")
	if mustBind(t, c) != mustBind(t, d) {
		t.Fatal("key order or whitespace changed the digest")
	}
	if len(mustBind(t, c)) != 64 {
		t.Fatal("digest is not hex sha256")
	}
}

func TestBindCallRejects(t *testing.T) {
	cases := map[string]func(*CallBinding){
		"trailing data":    func(b *CallBinding) { b.Arguments = json.RawMessage(`{"a":1} {}`) },
		"array":            func(b *CallBinding) { b.Arguments = json.RawMessage(`[]`) },
		"null":             func(b *CallBinding) { b.Arguments = json.RawMessage(`null`) },
		"invalid json":     func(b *CallBinding) { b.Arguments = json.RawMessage(`{`) },
		"empty arguments":  func(b *CallBinding) { b.Arguments = nil },
		"operation key":    func(b *CallBinding) { b.OperationKey = "" },
		"name digest":      func(b *CallBinding) { b.NameDigest = "" },
		"schema digest":    func(b *CallBinding) { b.SchemaDigest = "" },
		"product version":  func(b *CallBinding) { b.ProductVersion = "" },
		"principal":        func(b *CallBinding) { b.Principal = "" },
		"conversation":     func(b *CallBinding) { b.Conversation = "" },
		"definition short": func(b *CallBinding) { b.DefinitionSet = strings.Repeat("a", 63) },
		"definition long":  func(b *CallBinding) { b.DefinitionSet = strings.Repeat("a", 65) },
		"definition empty": func(b *CallBinding) { b.DefinitionSet = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := baseBinding()
			mutate(&b)
			if _, err := BindCall(b); !errors.Is(err, ErrState) {
				t.Fatalf("err = %v, want ErrState", err)
			}
		})
	}
	b := baseBinding()
	b.JourneyID = ""
	if _, err := BindCall(b); err != nil {
		t.Fatalf("empty JourneyID must be allowed: %v", err)
	}
}

func TestBindCallEveryFieldChangesDigest(t *testing.T) {
	base := mustBind(t, baseBinding())
	cases := map[string]func(*CallBinding){
		"JourneyID":      func(b *CallBinding) { b.JourneyID = "other" },
		"OperationKey":   func(b *CallBinding) { b.OperationKey = "other" },
		"NameDigest":     func(b *CallBinding) { b.NameDigest = NameDigest("other") },
		"SchemaDigest":   func(b *CallBinding) { b.SchemaDigest = mustSchemaDigest(`{"type":"string"}`) },
		"Arguments":      func(b *CallBinding) { b.Arguments = json.RawMessage(`{"a":2,"b":"x"}`) },
		"ProductVersion": func(b *CallBinding) { b.ProductVersion = "v2" },
		"Principal":      func(b *CallBinding) { b.Principal = "other" },
		"Conversation":   func(b *CallBinding) { b.Conversation = "other" },
		"DefinitionSet":  func(b *CallBinding) { b.DefinitionSet = strings.Repeat("b", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := baseBinding()
			mutate(&b)
			if mustBind(t, b) == base {
				t.Fatalf("changing %s did not change the digest", name)
			}
		})
	}
}

func TestBindCallFieldBoundaries(t *testing.T) {
	a, b := baseBinding(), baseBinding()
	a.JourneyID, a.OperationKey = "ab", "c"
	b.JourneyID, b.OperationKey = "a", "bc"
	if mustBind(t, a) == mustBind(t, b) {
		t.Fatal("characters moved between adjacent fields collided")
	}
	c, d := baseBinding(), baseBinding()
	c.Principal, c.Conversation = "p\x00q", "r"
	d.Principal, d.Conversation = "p", "q\x00r"
	if mustBind(t, c) == mustBind(t, d) {
		t.Fatal("NUL separator trick collided")
	}
}

func TestNameDigest(t *testing.T) {
	if NameDigest("a/b c") != NameDigest("a/b c") {
		t.Fatal("not deterministic")
	}
	seen := map[string]string{}
	for _, n := range []string{"a", "b", "a/b", "a b", "a/b c", ""} {
		d := NameDigest(n)
		if len(d) != 64 {
			t.Fatalf("digest of %q has length %d", n, len(d))
		}
		if prev, ok := seen[d]; ok {
			t.Fatalf("%q and %q collide", n, prev)
		}
		seen[d] = n
	}
}

func TestSchemaDigest(t *testing.T) {
	a := mustSchemaDigest(`{"type":"object","properties":{"x":{"type":"integer"},"y":{}}}`)
	b := mustSchemaDigest(` { "properties" : {"y":{},"x":{"type":"integer"}}, "type":"object" } `)
	if a != b {
		t.Fatal("key order or whitespace changed the digest")
	}
	if mustSchemaDigest("") != mustSchemaDigest("null") {
		t.Fatal("empty must equal null")
	}
	if mustSchemaDigest("null") == mustSchemaDigest(`{}`) {
		t.Fatal("null and {} collided")
	}
	for _, bad := range []string{`{`, `{} {}`, `nope`} {
		if _, err := SchemaDigest(json.RawMessage(bad)); !errors.Is(err, ErrState) {
			t.Fatalf("%q: err = %v, want ErrState", bad, err)
		}
	}
	if mustSchemaDigest(`{"maximum":9007199254740992}`) == mustSchemaDigest(`{"maximum":9007199254740993}`) {
		t.Fatal("number fidelity lost")
	}
}
