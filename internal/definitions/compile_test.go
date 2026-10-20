package definitions

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	compileSkillMD  = "---\nname: planner\ndescription: Plan time off\n---\nUse the planner.\n"
	compileMinimal  = "catalog:\n  operation_meta_key: op\njourneys:\n  - id: j\n    skill: skills/planner\n    tools:\n      - op: lookup\n"
	compileTwoOpsYA = "catalog:\n  operation_meta_key: op\n  call_id_header: x-call-id\njourneys:\n  - id: j\n    skill: skills/planner\n    tools:\n      - op: %s\n"
)

// writeCompileTree writes files (slash paths) under a fresh temp dir and
// returns the journeys.yaml path.
func writeCompileTree(t *testing.T, yaml string, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"journeys.yaml":              yaml,
		"skills/planner/SKILL.md":    compileSkillMD,
		"skills/planner/guide.md":    "Prefer direct routes.\n",
		"skills/planner/ref/more.md": "More.\n",
	}
	for name, content := range extra {
		files[name] = content
	}
	for name, content := range files {
		if content == "-" { // sentinel: omit
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "journeys.yaml")
}

func TestCompileValidConfig(t *testing.T) {
	set, err := compileFile(filepath.Join("testdata", "valid", "journeys.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := set.Catalog(), (CatalogConfig{OperationMetaKey: "com.example/operation", CallIDHeader: "X-Platform-Call-Id"}); got != want {
		t.Fatalf("catalog = %+v, want %+v", got, want)
	}
	journeys := set.Journeys()
	if len(journeys) != 2 || journeys[0].ID != "vacation-planner" || journeys[1].ID != "expenses" {
		t.Fatalf("journeys out of order: %+v", journeys)
	}
	j := journeys[0]
	if j.Description != "Plan time off" || j.Prompt != "Use the planner." || j.Skill != "skills/planner/" {
		t.Fatalf("unexpected journey metadata: %+v", j)
	}
	if j.MaxTools != 15 || journeys[1].MaxTools != 3 {
		t.Fatalf("max_tools = %d, %d", j.MaxTools, journeys[1].MaxTools)
	}
	if !reflect.DeepEqual(j.Deny, []string{"leave.delete"}) {
		t.Fatalf("deny = %v", j.Deny)
	}
	if j.ProductVersions != (VersionRange{Min: "2026.2", MaxExclusive: "2027.1"}) || !j.ProductVersions.Contains("2026.10") || j.ProductVersions.Contains("2027.1") {
		t.Fatalf("product versions = %+v", j.ProductVersions)
	}
	want := []Selector{
		{Ops: []string{"leave.balance"}, Alias: "leave_balance"},
		{Ops: []string{"leave.request.v2", "leave.request"}, Alias: "leave_request", Class: ClassIdempotent, ApprovalRequired: true},
		{Tags: []string{"leave", "self-service"}, ReadOnlyHint: true},
	}
	if !reflect.DeepEqual(j.Tools, want) {
		t.Fatalf("tools = %+v\nwant   %+v", j.Tools, want)
	}
	if got := journeys[1].Tools; !reflect.DeepEqual(got, []Selector{{Ops: []string{"expense.submit"}, Alias: "expense_submit", Class: ClassEffectful}}) {
		t.Fatalf("expense tools = %+v", got)
	}
	if j.Catalog != set.Catalog() || len(j.Skills) != 1 || j.Skills[0].Name != "planner" || !j.Skills[0].AllowSupportingFiles {
		t.Fatalf("skill/catalog not attached: %+v", j.Skills)
	}
	supporting := map[string]bool{}
	for name := range j.Skills[0].Supporting {
		supporting[name] = true
	}
	if !reflect.DeepEqual(supporting, map[string]bool{"guide.md": true, "reference/nested.md": true}) {
		t.Fatalf("supporting files = %v", supporting)
	}
	if journeys[1].Skills[0].AllowSupportingFiles || len(journeys[1].Skills[0].Supporting) != 0 {
		t.Fatalf("expenses bundle should have no supporting files")
	}
	if len(j.Digest) != 64 || len(set.Digest()) != 64 || j.Digest == journeys[1].Digest {
		t.Fatalf("digests = %q %q %q", j.Digest, journeys[1].Digest, set.Digest())
	}

	// Clones are deep.
	journeys[0].Tools[0].Ops[0] = "mutated"
	journeys[0].Deny[0] = "mutated"
	again, _ := set.Journey("vacation-planner")
	if again.Tools[0].Ops[0] != "leave.balance" || again.Deny[0] != "leave.delete" {
		t.Fatal("caller mutation changed the set")
	}
}

func TestCompileSkipsDotfilesAndReadsOpScalarVsList(t *testing.T) {
	yaml := "catalog:\n  operation_meta_key: op\njourneys:\n  - id: j\n    skill: skills/planner\n    tools:\n      - op: a\n      - op: [b, c]\n        alias: bc\n"
	path := writeCompileTree(t, yaml, map[string]string{
		"skills/planner/.DS_Store":      "junk",
		"skills/planner/.hidden/x.md":   "hidden",
		"skills/planner/ref/.secret.md": "hidden",
	})
	set, err := compileFile(path)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := set.Journey("j")
	for name := range j.Skills[0].Supporting {
		if strings.Contains(name, ".") && strings.HasPrefix(filepath.Base(name), ".") || strings.HasPrefix(name, ".hidden") {
			t.Fatalf("dotfile %q was enumerated", name)
		}
	}
	if len(j.Skills[0].Supporting) != 2 {
		t.Fatalf("supporting = %v", j.Skills[0].Supporting)
	}
	if !reflect.DeepEqual(j.Tools[0].Ops, []string{"a"}) || !reflect.DeepEqual(j.Tools[1].Ops, []string{"b", "c"}) {
		t.Fatalf("ops = %+v", j.Tools)
	}
}

func TestCompileRejects(t *testing.T) {
	journey := func(body string) string {
		return "catalog:\n  operation_meta_key: op\n  call_id_header: x-call-id\njourneys:\n  - id: j\n    skill: skills/planner\n" + body
	}
	tools := func(t string) string { return journey("    tools:\n" + t) }
	cases := []struct {
		name  string
		yaml  string
		extra map[string]string
	}{
		{name: "unknown top-level", yaml: compileMinimal + "extra: 1\n"},
		{name: "unknown journey field", yaml: journey("    bogus: 1\n    tools:\n      - op: a\n")},
		{name: "unknown selector field", yaml: tools("      - op: a\n        bogus: 1\n")},
		{name: "unknown catalog field", yaml: "catalog:\n  operation_meta_key: op\n  bogus: 1\njourneys:\n  - id: j\n    skill: skills/planner\n    tools:\n      - op: a\n"},
		{name: "trailing document", yaml: compileMinimal + "---\ncatalog: {}\n"},
		{name: "empty file", yaml: ""},
		{name: "missing operation_meta_key", yaml: "catalog:\n  call_id_header: x\njourneys:\n  - id: j\n    skill: skills/planner\n    tools:\n      - op: a\n"},
		{name: "no journeys", yaml: "catalog:\n  operation_meta_key: op\njourneys: []\n"},
		{name: "duplicate journey id", yaml: compileMinimal + "  - id: j\n    skill: skills/planner\n    tools:\n      - op: b\n"},
		{name: "whitespace id", yaml: strings.Replace(compileMinimal, "id: j", "id: \"a b\"", 1)},
		{name: "long id", yaml: strings.Replace(compileMinimal, "id: j", "id: "+strings.Repeat("x", 65), 1)},
		{name: "missing skill", yaml: strings.Replace(compileMinimal, "    skill: skills/planner\n", "", 1)},
		{name: "absolute skill", yaml: strings.Replace(compileMinimal, "skills/planner", "/etc", 1)},
		{name: "escaping skill", yaml: strings.Replace(compileMinimal, "skills/planner", "../skills/planner", 1)},
		{name: "nonexistent skill", yaml: strings.Replace(compileMinimal, "skills/planner", "skills/nope", 1)},
		{name: "skill is config dir", yaml: strings.Replace(compileMinimal, "skills/planner", ".", 1)},
		{name: "missing SKILL.md", yaml: compileMinimal, extra: map[string]string{"skills/planner/SKILL.md": "-"}},
		{name: "bad frontmatter", yaml: compileMinimal, extra: map[string]string{"skills/planner/SKILL.md": "no frontmatter\n"}},
		{name: "empty tools", yaml: journey("    tools: []\n")},
		{name: "no tools", yaml: journey("")},
		{name: "op and tags", yaml: tools("      - op: a\n        tags: [x]\n")},
		{name: "neither op nor tags", yaml: tools("      - class: read_only\n")},
		{name: "class on tag selector", yaml: tools("      - tags: [x]\n        class: read_only\n")},
		{name: "empty tags", yaml: tools("      - tags: []\n")},
		{name: "duplicate tags", yaml: tools("      - tags: [x, x]\n")},
		{name: "empty op list", yaml: tools("      - op: []\n")},
		{name: "empty op alternative", yaml: tools("      - op: [a, \"\"]\n")},
		{name: "empty op scalar", yaml: tools("      - op: \"\"\n")},
		{name: "op mapping", yaml: tools("      - op: {a: b}\n")},
		{name: "duplicate alternative", yaml: tools("      - op: [a, a]\n        alias: x\n")},
		{name: "invalid class", yaml: tools("      - op: a\n        class: deduplicated\n")},
		{name: "invalid approval", yaml: tools("      - op: a\n        approval: always\n")},
		{name: "idempotent without header", yaml: "catalog:\n  operation_meta_key: op\njourneys:\n  - id: j\n    skill: skills/planner\n    tools:\n      - op: a\n        class: idempotent\n"},
		{name: "min >= max", yaml: journey("    product_versions: {min: \"2027.1\", max_exclusive: \"2026.2\"}\n    tools:\n      - op: a\n")},
		{name: "min == max", yaml: journey("    product_versions: {min: \"1\", max_exclusive: \"1\"}\n    tools:\n      - op: a\n")},
		{name: "op key in two selectors", yaml: tools("      - op: a\n      - op: [b, a]\n        alias: x\n")},
		{name: "denied key in op selector", yaml: journey("    deny: [a]\n    tools:\n      - op: [b, a]\n        alias: x\n")},
		{name: "empty deny", yaml: journey("    deny: [\"\"]\n    tools:\n      - op: a\n")},
		{name: "max_tools 0", yaml: journey("    max_tools: 0\n    tools:\n      - op: a\n")},
		{name: "max_tools 65", yaml: journey("    max_tools: 65\n    tools:\n      - op: a\n")},
		{name: "reserved request_user_input", yaml: tools("      - op: request_user_input\n")},
		{name: "reserved activate_journey", yaml: tools("      - op: [x, y]\n        alias: activate_journey\n")},
		{name: "alias reserved via punctuation", yaml: tools("      - op: activate.journey\n")},
		{name: "invalid alias digit", yaml: tools("      - op: 9lives\n")},
		{name: "invalid alias length", yaml: tools("      - op: " + strings.Repeat("a", 70) + "\n")},
		{name: "alias collision", yaml: tools("      - op: a.b\n      - op: a_b\n")},
		{name: "multi-alternative without alias", yaml: tools("      - op: [a, b]\n")},
		{name: "alias on tag selector", yaml: tools("      - tags: [x]\n        alias: x\n")},
		{name: "invalid explicit alias", yaml: tools("      - op: a\n        alias: \"bad-alias\"\n")},
		{name: "alias starting with digit", yaml: tools("      - op: a\n        alias: 9x\n")},
		{name: "alias too long", yaml: tools("      - op: a\n        alias: " + strings.Repeat("a", 65) + "\n")},
		{name: "reserved explicit alias", yaml: tools("      - op: a\n        alias: read_skill_resource\n")},
		{name: "duplicate explicit alias", yaml: tools("      - op: a\n        alias: x\n      - op: [b, c]\n        alias: x\n")},
		{name: "explicit alias equals derived alias", yaml: tools("      - op: a.b\n      - op: c\n        alias: a_b\n")},
		{name: "alias with different alternatives across journeys", yaml: compileMinimal + "  - id: k\n    skill: skills/planner\n    tools:\n      - op: b\n        alias: lookup\n"},
		{name: "invalid call id header", yaml: "catalog:\n  operation_meta_key: op\n  call_id_header: \"bad header\"\njourneys:\n  - id: j\n    skill: skills/planner\n    tools:\n      - op: a\n"},
		{name: "removed key system_prompt", yaml: strings.Replace(compileMinimal, "    skill: skills/planner\n", "    skill: skills/planner\n    system_prompt: prompts/p.md\n", 1), extra: map[string]string{"prompts/p.md": "SECRET-PROMPT-BODY\n"}},
		{name: "removed key routing", yaml: strings.Replace(compileMinimal, "    skill: skills/planner\n", "    skill: skills/planner\n    routing: {default: true}\n", 1)},
		{name: "scalar not mapping", yaml: "just a string\n"},
		{name: "syntax error", yaml: "catalog: [\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, err := compileFile(writeCompileTree(t, tc.yaml, tc.extra))
			if err == nil || set != nil {
				t.Fatalf("Compile accepted the configuration (err=%v)", err)
			}
			t.Logf("rejected: %v", err)
			for _, secret := range []string{"SECRET-PROMPT-BODY", "Use the planner", "Prefer direct routes"} {
				if strings.Contains(err.Error(), strings.TrimSpace(secret)) {
					t.Fatalf("error echoes file contents: %v", err)
				}
			}
		})
	}
}

func TestCompileRejectsSymlinks(t *testing.T) {
	t.Run("symlinked skill dir", func(t *testing.T) {
		path := writeCompileTree(t, compileMinimal, nil)
		root := filepath.Dir(path)
		if err := os.Rename(filepath.Join(root, "skills", "planner"), filepath.Join(root, "real")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "skills", "planner")); err != nil {
			t.Fatal(err)
		}
		if _, err := compileFile(path); err == nil {
			t.Fatal("accepted a symlinked skill directory")
		}
	})
	t.Run("symlink inside bundle", func(t *testing.T) {
		path := writeCompileTree(t, compileMinimal, nil)
		root := filepath.Dir(path)
		if err := os.Symlink(filepath.Join(root, "skills", "planner", "guide.md"), filepath.Join(root, "skills", "planner", "link.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := compileFile(path); err == nil {
			t.Fatal("accepted a symlink inside the bundle")
		}
	})
	t.Run("symlinked directory inside bundle", func(t *testing.T) {
		path := writeCompileTree(t, compileMinimal, nil)
		root := filepath.Dir(path)
		if err := os.Symlink(root, filepath.Join(root, "skills", "planner", "loop")); err != nil {
			t.Fatal(err)
		}
		if _, err := compileFile(path); err == nil {
			t.Fatal("accepted a symlinked directory inside the bundle")
		}
	})
}

func TestCompileDigestIsContentAddressed(t *testing.T) {
	yaml := fmt.Sprintf(compileTwoOpsYA, "lookup")
	a, err := compileFile(writeCompileTree(t, yaml, nil))
	if err != nil {
		t.Fatal(err)
	}
	b, err := compileFile(writeCompileTree(t, yaml, nil))
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := a.Journey("j")
	jb, _ := b.Journey("j")
	if a.Digest() != b.Digest() || ja.Digest != jb.Digest {
		t.Fatalf("identical content in two directories differs: %s/%s %s/%s", a.Digest(), b.Digest(), ja.Digest, jb.Digest)
	}
	for name, c := range map[string]struct {
		yaml  string
		extra map[string]string
	}{
		"prompt":     {yaml, map[string]string{"skills/planner/SKILL.md": "---\nname: planner\ndescription: Plan time off\n---\nchanged prompt\n"}},
		"selector":   {fmt.Sprintf(compileTwoOpsYA, "other"), nil},
		"supporting": {yaml, map[string]string{"skills/planner/guide.md": "changed\n"}},
		"catalog":    {strings.Replace(yaml, "op\n", "op2\n", 1), nil},
	} {
		changed, err := compileFile(writeCompileTree(t, c.yaml, c.extra))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		jc, _ := changed.Journey("j")
		if changed.Digest() == a.Digest() || jc.Digest == ja.Digest {
			t.Fatalf("%s change did not change the digests", name)
		}
	}
}

func TestCompileHelpers(t *testing.T) {
	for in, want := range map[string]struct {
		alias string
		valid bool
	}{
		"lookup_destination":    {"lookup_destination", true},
		"leave.request.v2":      {"leave_request_v2", true},
		"travel/lookup":         {"travel_lookup", true},
		"com.example/op-1":      {"com_example_op_1", true},
		"_x":                    {"_x", true},
		"9lives":                {"9lives", false},
		"":                      {"", false},
		strings.Repeat("a", 64): {strings.Repeat("a", 64), true},
		strings.Repeat("a", 65): {strings.Repeat("a", 65), false},
		"é":                     {"__", true},
	} {
		alias, ok := AliasFor(in)
		if alias != want.alias || ok != want.valid {
			t.Errorf("AliasFor(%q) = %q, %v; want %q, %v", in, alias, ok, want.alias, want.valid)
		}
	}
	for _, name := range []string{"request_user_input", "read_skill_resource", "activate_journey"} {
		if !IsReservedToolName(name) {
			t.Errorf("%s not reserved", name)
		}
	}
	if IsReservedToolName("lookup") || IsReservedToolName("load_skill") || IsReservedToolName("Activate_Journey") {
		t.Error("ordinary name reserved")
	}

	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"2026.2", "2026.10", -1}, {"2026.10", "2026.2", 1}, {"v1", "v2", -1}, {"1", "1.0", -1},
		{"1.0", "1", 1}, {"1.0", "1.0", 0}, {"1.2", "1.02", 0}, {"1", "a", -1}, {"a", "1", 1},
		{"1.rc1", "1.rc2", -1}, {"99999999999999999999999", "100000000000000000000000", -1},
	} {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}

	r := VersionRange{Min: "2026.2", MaxExclusive: "2027.1"}
	for v, want := range map[string]bool{"2026.1": false, "2026.2": true, "2026.10": true, "2027.0": true, "2027.1": false, "2028": false} {
		if r.Contains(v) != want {
			t.Errorf("Contains(%q) = %v, want %v", v, !want, want)
		}
	}
	if !(VersionRange{}).IsZero() || r.IsZero() || !(VersionRange{}).Contains("anything") {
		t.Error("IsZero/Contains on zero range")
	}
	if !(VersionRange{Min: "2"}).Contains("3") || (VersionRange{Min: "2"}).Contains("1") {
		t.Error("min-only range")
	}
	if !(VersionRange{MaxExclusive: "2"}).Contains("1") || (VersionRange{MaxExclusive: "2"}).Contains("2") {
		t.Error("max-only range")
	}
}

func TestConfinedPathRejectsRawParentComponents(t *testing.T) {
	root := t.TempDir()
	writeSkillBudgetFiles(t, root)

	for _, tc := range []struct {
		name        string
		declaration skillDeclaration
	}{
		{name: "package root", declaration: skillDeclaration{Name: "planner", Root: "skills/../skills/planner"}},
		{name: "supporting file", declaration: skillDeclaration{Name: "planner", Root: "skills/planner", SupportingFiles: []string{"nested/../guide.md"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := (&source{fsys: os.DirFS(root)}).compileSkill(tc.declaration); err == nil || !strings.Contains(err.Error(), "parent components are not allowed") {
				t.Fatalf("expected raw parent component rejection, got %v", err)
			}
		})
	}
}

func TestCompileRepositoryConfig(t *testing.T) {
	set, err := compileFile(filepath.Join("..", "..", "configs", "journeys.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := set.Catalog(), (CatalogConfig{OperationMetaKey: "operation_key", CallIDHeader: "X-Platform-Call-Id"}); got != want {
		t.Fatalf("catalog = %+v, want %+v", got, want)
	}
	journeys := set.Journeys()
	if len(journeys) != 2 || journeys[0].ID != "vacation-planner" || journeys[1].ID != "shift-swap" {
		t.Fatalf("journeys = %+v", journeys)
	}
	if len(journeys[0].Skills) != 1 || journeys[0].Skills[0].Name != "planner" {
		t.Fatalf("vacation-planner = %+v", journeys[0])
	}
	if got, want := journeys[0].Tools, []Selector{{Ops: []string{"lookup_destination"}, Alias: "lookup_destination", Class: ClassReadOnly}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("vacation-planner tools = %+v", got)
	}
	swap := journeys[1]
	if swap.Skills[0].Name != "shift-swap" {
		t.Fatalf("shift-swap = %+v", swap)
	}
	want := []Selector{
		{Ops: []string{"lookup_shift"}, Alias: "lookup_shift", Class: ClassReadOnly},
		{Ops: []string{"request_shift_swap"}, Alias: "request_shift_swap"}, // unclassified: effectful, approval required
	}
	if !reflect.DeepEqual(swap.Tools, want) {
		t.Fatalf("shift-swap tools = %+v, want %+v", swap.Tools, want)
	}
	if swap.Tools[1].Class != "" || swap.Tools[1].ApprovalRequired {
		t.Fatalf("request_shift_swap must stay unclassified, got %+v", swap.Tools[1])
	}
}

func TestCompileConversationPrompt(t *testing.T) {
	compile := func(t *testing.T, yaml string, extra map[string]string) (*Set, error) {
		t.Helper()
		return compileFile(writeCompileTree(t, "conversation_prompt: conv.md\n"+yaml, extra))
	}
	t.Run("present", func(t *testing.T) {
		set, err := compile(t, compileMinimal, map[string]string{"conv.md": "You are the conversation agent.\n"})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := set.ConversationPrompt(), "You are the conversation agent.\n"; got != want {
			t.Fatalf("ConversationPrompt() = %q, want %q", got, want)
		}
		other, err := compile(t, compileMinimal, map[string]string{"conv.md": "A different base prompt.\n"})
		if err != nil {
			t.Fatal(err)
		}
		absent, err := compileFile(writeCompileTree(t, compileMinimal, nil))
		if err != nil {
			t.Fatal(err)
		}
		if set.Digest() == other.Digest() || set.Digest() == absent.Digest() {
			t.Fatalf("conversation prompt does not change the set digest: %s %s %s", set.Digest(), other.Digest(), absent.Digest())
		}
		again, err := compile(t, compileMinimal, map[string]string{"conv.md": "You are the conversation agent.\n"})
		if err != nil || again.Digest() != set.Digest() {
			t.Fatalf("digest is not deterministic: %v", err)
		}
		for id, j := range set.journeys {
			if j.Digest != absent.journeys[id].Digest {
				t.Fatalf("journey digest must not depend on the conversation prompt")
			}
		}
	})
	t.Run("absent", func(t *testing.T) {
		set, err := compileFile(writeCompileTree(t, compileMinimal, nil))
		if err != nil {
			t.Fatal(err)
		}
		if got := set.ConversationPrompt(); got != "" {
			t.Fatalf("ConversationPrompt() = %q, want empty", got)
		}
	})
	t.Run("rejects", func(t *testing.T) {
		big := strings.Repeat("x", MaxPromptBytes+1)
		cases := []struct {
			name, key string
			extra     map[string]string
		}{
			{name: "absolute", key: "/etc/passwd"},
			{name: "escaping", key: "../conv.md"},
			{name: "escaping nested", key: "prompts/../../conv.md"},
			{name: "missing file", key: "none.md"},
			{name: "oversize", key: "conv.md", extra: map[string]string{"conv.md": big}},
			{name: "directory", key: "skills"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				yaml := "conversation_prompt: " + tc.key + "\n" + compileMinimal
				set, err := compileFile(writeCompileTree(t, yaml, tc.extra))
				if err == nil || set != nil {
					t.Fatalf("Compile accepted the configuration (err=%v)", err)
				}
				if strings.Contains(err.Error(), "SECRET-PROMPT-BODY") {
					t.Fatalf("error echoes file contents: %v", err)
				}
			})
		}
		t.Run("symlink", func(t *testing.T) {
			path := writeCompileTree(t, "conversation_prompt: link.md\n"+compileMinimal, nil)
			dir := filepath.Dir(path)
			if err := os.Symlink(filepath.Join(dir, "skills", "planner", "guide.md"), filepath.Join(dir, "link.md")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if set, err := compileFile(path); err == nil || set != nil {
				t.Fatalf("Compile accepted a symlinked conversation prompt (err=%v)", err)
			}
		})
		t.Run("empty value", func(t *testing.T) {
			// An explicit empty path is the same as an absent key.
			set, err := compileFile(writeCompileTree(t, "conversation_prompt: \"\"\n"+compileMinimal, nil))
			if err != nil || set.ConversationPrompt() != "" {
				t.Fatalf("err=%v", err)
			}
		})
	})
}

func TestCompileSkillBodyPrompt(t *testing.T) {
	t.Run("prompt is the SKILL.md body", func(t *testing.T) {
		skill := "---\nname: planner\ndescription: Plan time off\n---\n\n  Use the planner.\n\nSecond paragraph.\n\n"
		set, err := compileFile(writeCompileTree(t, compileMinimal, map[string]string{"skills/planner/SKILL.md": skill}))
		if err != nil {
			t.Fatal(err)
		}
		j, _ := set.Journey("j")
		if want := "Use the planner.\n\nSecond paragraph."; j.Prompt != want {
			t.Fatalf("Prompt = %q, want %q", j.Prompt, want)
		}
	})
	t.Run("body changes the journey digest", func(t *testing.T) {
		build := func(body string) Journey {
			skill := "---\nname: planner\ndescription: Plan time off\n---\n" + body
			set, err := compileFile(writeCompileTree(t, compileMinimal, map[string]string{"skills/planner/SKILL.md": skill}))
			if err != nil {
				t.Fatal(err)
			}
			j, _ := set.Journey("j")
			return j
		}
		if a, b := build("one\n"), build("two\n"); a.Digest == b.Digest || a.Prompt == b.Prompt {
			t.Fatal("body change did not change the journey")
		}
	})
	t.Run("empty body", func(t *testing.T) {
		skill := "---\nname: planner\ndescription: Plan time off\n---\n"
		set, err := compileFile(writeCompileTree(t, compileMinimal, map[string]string{"skills/planner/SKILL.md": skill}))
		if err != nil {
			t.Fatal(err)
		}
		if j, _ := set.Journey("j"); j.Prompt != "" {
			t.Fatalf("Prompt = %q, want empty", j.Prompt)
		}
	})
}

// The runtime no longer routes and prompts come from SKILL.md bodies, so the
// per-journey routing and system_prompt keys are unknown fields.
func TestCompileRejectsRemovedKeys(t *testing.T) {
	for key, line := range map[string]string{
		"routing":       "    routing: {default: true}\n",
		"system_prompt": "    system_prompt: prompts/p.md\n",
	} {
		t.Run(key, func(t *testing.T) {
			yaml := strings.Replace(compileMinimal, "    skill: skills/planner\n", "    skill: skills/planner\n"+line, 1)
			set, err := compileFile(writeCompileTree(t, yaml, map[string]string{"prompts/p.md": "p\n"}))
			if err == nil || set != nil {
				t.Fatalf("Compile accepted %s (err=%v)", key, err)
			}
			if !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("%s was not rejected as an unknown field: %v", key, err)
			}
		})
	}
}

func policyConflictYAML(first, second string) string {
	return "catalog:\n  operation_meta_key: op\njourneys:\n" +
		"  - id: zeta\n    skill: skills/planner\n    tools:\n" + first +
		"  - id: alpha\n    skill: skills/planner\n    tools:\n" + second
}

func TestCompilePolicyConflict(t *testing.T) {
	cases := []struct {
		name, first, second, want string
	}{
		{
			name:   "key conflict by class",
			first:  "      - op: lookup\n        class: read_only\n",
			second: "      - op: lookup\n",
			want:   `definitions: operation "lookup" has conflicting policies in journeys alpha, zeta`,
		},
		{
			name:   "key conflict by approval required",
			first:  "      - op: lookup\n        class: read_only\n",
			second: "      - op: lookup\n        class: read_only\n        approval: required\n",
			want:   `definitions: operation "lookup" has conflicting policies in journeys alpha, zeta`,
		},
		{
			name:   "key conflict through op alternatives",
			first:  "      - op: [other, lookup]\n        alias: pick\n        class: read_only\n",
			second: "      - op: lookup\n",
			want:   `definitions: operation "lookup" has conflicting policies in journeys alpha, zeta`,
		},
		{
			name:   "alias conflict between two keys",
			first:  "      - op: [a, b]\n        alias: pick\n        class: read_only\n",
			second: "      - op: [a, b]\n        alias: pick\n",
			want:   `definitions: operation "a" has conflicting policies in journeys alpha, zeta`,
		},
		{
			name:   "same alias and ops, different class",
			first:  "      - op: [a, b]\n        alias: pick\n        class: read_only\n",
			second: "      - op: [a, b]\n        alias: pick\n",
			want:   `definitions: operation "a" has conflicting policies in journeys alpha, zeta`,
		},
		{
			name:   "same alias and ops, different approval",
			first:  "      - op: [a, b]\n        alias: pick\n        class: read_only\n",
			second: "      - op: [a, b]\n        alias: pick\n        class: read_only\n        approval: required\n",
			want:   `definitions: operation "a" has conflicting policies in journeys alpha, zeta`,
		},
		{
			name:   "one alias, different alternative lists",
			first:  "      - op: a.b\n        alias: pick\n",
			second: "      - op: a_b\n        alias: pick\n",
			want:   `definitions: alias "pick" names different operations in journeys alpha, zeta`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeCompileTree(t, policyConflictYAML(tc.first, tc.second), nil)
			_, err := compileFile(path)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Compile error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCompilePolicyConflictAcceptsEqualPolicies(t *testing.T) {
	cases := []struct{ name, first, second string }{
		{"same policy", "      - op: lookup\n        class: read_only\n", "      - op: lookup\n        class: read_only\n"},
		{"unset class equals effectful", "      - op: lookup\n", "      - op: lookup\n        class: effectful\n"},
		{"alias with equal ops and policies", "      - op: [a, b]\n        alias: pick\n", "      - op: [a, b]\n        alias: pick\n        class: effectful\n"},
		{"tag selectors are not checked", "      - tags: [x]\n", "      - tags: [x]\n        approval: required\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeCompileTree(t, policyConflictYAML(tc.first, tc.second), nil)
			if _, err := compileFile(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCompileSelectorAlias(t *testing.T) {
	yaml := "catalog:\n  operation_meta_key: op\njourneys:\n  - id: j\n    skill: skills/planner\n    tools:\n" +
		"      - op: a.b\n      - op: c\n        alias: renamed\n      - op: [d, e]\n        alias: pick\n      - tags: [t]\n"
	set, err := compileFile(writeCompileTree(t, yaml, nil))
	if err != nil {
		t.Fatal(err)
	}
	j, _ := set.Journey("j")
	var got []string
	for _, s := range j.Tools {
		got = append(got, s.Alias)
	}
	if want := []string{"a_b", "renamed", "pick", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("aliases = %q, want %q", got, want)
	}
}

func TestValidAlias(t *testing.T) {
	for s, want := range map[string]bool{"a": true, "_x1": true, "A_b": true, "": false, "1a": false, "a-b": false, "a.b": false, strings.Repeat("a", 64): true, strings.Repeat("a", 65): false} {
		if ValidAlias(s) != want {
			t.Errorf("ValidAlias(%q) = %v", s, !want)
		}
	}
}
