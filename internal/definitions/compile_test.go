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
	compileMinimal  = "catalog:\n  operation_meta_key: op\njourneys:\n  - id: j\n    skill: skills/planner\n    system_prompt: prompts/p.md\n    tools:\n      - op: lookup\n"
	compileTwoOpsYA = "catalog:\n  operation_meta_key: op\n  call_id_header: x-call-id\njourneys:\n  - id: j\n    skill: skills/planner\n    system_prompt: prompts/p.md\n    tools:\n      - op: %s\n"
)

// writeCompileTree writes files (slash paths) under a fresh temp dir and
// returns the journeys.yaml path.
func writeCompileTree(t *testing.T, yaml string, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"journeys.yaml":              yaml,
		"prompts/p.md":               "SECRET-PROMPT-BODY\n",
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
	set, err := Compile(filepath.Join("testdata", "valid", "journeys.yaml"))
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
	if j.Description != "Plan time off" || j.Prompt != "You plan vacations.\n" || j.SystemPrompt != "prompts/planner.md" || j.Skill != "skills/planner/" {
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
	if !reflect.DeepEqual(j.Routing, Routing{Default: true, Keywords: []string{"vacation", "leave"}, Priority: 10}) {
		t.Fatalf("routing = %+v", j.Routing)
	}
	want := []Selector{
		{Ops: []string{"leave.balance"}},
		{Ops: []string{"leave.request.v2", "leave.request"}, Class: ClassIdempotent, ApprovalRequired: true},
		{Tags: []string{"leave", "self-service"}, ReadOnlyHint: true},
	}
	if !reflect.DeepEqual(j.Tools, want) {
		t.Fatalf("tools = %+v\nwant   %+v", j.Tools, want)
	}
	if got := journeys[1].Tools; !reflect.DeepEqual(got, []Selector{{Ops: []string{"expense.submit"}, Class: ClassEffectful}}) {
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
	journeys[0].Routing.Keywords[0] = "mutated"
	again, _ := set.Journey("vacation-planner")
	if again.Tools[0].Ops[0] != "leave.balance" || again.Deny[0] != "leave.delete" || again.Routing.Keywords[0] != "vacation" {
		t.Fatal("caller mutation changed the set")
	}
	if def, ok := set.DefaultJourney(); !ok || def.ID != "vacation-planner" {
		t.Fatalf("default journey = %q %v", def.ID, ok)
	}
}

func TestCompileSkipsDotfilesAndReadsOpScalarVsList(t *testing.T) {
	yaml := "catalog:\n  operation_meta_key: op\njourneys:\n  - id: j\n    skill: skills/planner\n    system_prompt: prompts/p.md\n    tools:\n      - op: a\n      - op: [b, c]\n"
	path := writeCompileTree(t, yaml, map[string]string{
		"skills/planner/.DS_Store":      "junk",
		"skills/planner/.hidden/x.md":   "hidden",
		"skills/planner/ref/.secret.md": "hidden",
	})
	set, err := Compile(path)
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
		return "catalog:\n  operation_meta_key: op\n  call_id_header: x-call-id\njourneys:\n  - id: j\n    skill: skills/planner\n    system_prompt: prompts/p.md\n" + body
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
		{name: "unknown catalog field", yaml: "catalog:\n  operation_meta_key: op\n  bogus: 1\njourneys:\n  - id: j\n    skill: skills/planner\n    system_prompt: prompts/p.md\n    tools:\n      - op: a\n"},
		{name: "trailing document", yaml: compileMinimal + "---\ncatalog: {}\n"},
		{name: "empty file", yaml: ""},
		{name: "missing operation_meta_key", yaml: "catalog:\n  call_id_header: x\njourneys:\n  - id: j\n    skill: skills/planner\n    system_prompt: prompts/p.md\n    tools:\n      - op: a\n"},
		{name: "no journeys", yaml: "catalog:\n  operation_meta_key: op\njourneys: []\n"},
		{name: "duplicate journey id", yaml: compileMinimal + "  - id: j\n    skill: skills/planner\n    system_prompt: prompts/p.md\n    tools:\n      - op: b\n"},
		{name: "whitespace id", yaml: strings.Replace(compileMinimal, "id: j", "id: \"a b\"", 1)},
		{name: "long id", yaml: strings.Replace(compileMinimal, "id: j", "id: "+strings.Repeat("x", 65), 1)},
		{name: "missing skill", yaml: strings.Replace(compileMinimal, "    skill: skills/planner\n", "", 1)},
		{name: "absolute skill", yaml: strings.Replace(compileMinimal, "skills/planner", "/etc", 1)},
		{name: "escaping skill", yaml: strings.Replace(compileMinimal, "skills/planner", "../skills/planner", 1)},
		{name: "nonexistent skill", yaml: strings.Replace(compileMinimal, "skills/planner", "skills/nope", 1)},
		{name: "skill is config dir", yaml: strings.Replace(compileMinimal, "skills/planner", ".", 1)},
		{name: "missing prompt", yaml: strings.Replace(compileMinimal, "    system_prompt: prompts/p.md\n", "", 1)},
		{name: "absolute prompt", yaml: strings.Replace(compileMinimal, "prompts/p.md", "/etc/passwd", 1)},
		{name: "escaping prompt", yaml: strings.Replace(compileMinimal, "prompts/p.md", "prompts/../../p.md", 1)},
		{name: "nonexistent prompt", yaml: strings.Replace(compileMinimal, "prompts/p.md", "prompts/none.md", 1)},
		{name: "missing SKILL.md", yaml: compileMinimal, extra: map[string]string{"skills/planner/SKILL.md": "-"}},
		{name: "bad frontmatter", yaml: compileMinimal, extra: map[string]string{"skills/planner/SKILL.md": "no frontmatter\n"}},
		{name: "empty tools", yaml: journey("    tools: []\n")},
		{name: "no tools", yaml: journey("")},
		{name: "op and tags", yaml: tools("      - op: a\n        tags: [x]\n")},
		{name: "neither op nor tags", yaml: tools("      - class: read_only\n")},
		{name: "empty tags", yaml: tools("      - tags: []\n")},
		{name: "duplicate tags", yaml: tools("      - tags: [x, x]\n")},
		{name: "empty op list", yaml: tools("      - op: []\n")},
		{name: "empty op alternative", yaml: tools("      - op: [a, \"\"]\n")},
		{name: "empty op scalar", yaml: tools("      - op: \"\"\n")},
		{name: "op mapping", yaml: tools("      - op: {a: b}\n")},
		{name: "duplicate alternative", yaml: tools("      - op: [a, a]\n")},
		{name: "invalid class", yaml: tools("      - op: a\n        class: deduplicated\n")},
		{name: "invalid approval", yaml: tools("      - op: a\n        approval: always\n")},
		{name: "idempotent without header", yaml: "catalog:\n  operation_meta_key: op\njourneys:\n  - id: j\n    skill: skills/planner\n    system_prompt: prompts/p.md\n    tools:\n      - op: a\n        class: idempotent\n"},
		{name: "min >= max", yaml: journey("    product_versions: {min: \"2027.1\", max_exclusive: \"2026.2\"}\n    tools:\n      - op: a\n")},
		{name: "min == max", yaml: journey("    product_versions: {min: \"1\", max_exclusive: \"1\"}\n    tools:\n      - op: a\n")},
		{name: "op key in two selectors", yaml: tools("      - op: a\n      - op: [b, a]\n")},
		{name: "denied key in op selector", yaml: journey("    deny: [a]\n    tools:\n      - op: [b, a]\n")},
		{name: "empty deny", yaml: journey("    deny: [\"\"]\n    tools:\n      - op: a\n")},
		{name: "max_tools 0", yaml: journey("    max_tools: 0\n    tools:\n      - op: a\n")},
		{name: "max_tools 65", yaml: journey("    max_tools: 65\n    tools:\n      - op: a\n")},
		{name: "reserved request_user_input", yaml: tools("      - op: request_user_input\n")},
		{name: "reserved activate_journey", yaml: tools("      - op: [x, activate_journey]\n")},
		{name: "alias reserved via punctuation", yaml: tools("      - op: load.skill\n")},
		{name: "invalid alias digit", yaml: tools("      - op: 9lives\n")},
		{name: "invalid alias length", yaml: tools("      - op: " + strings.Repeat("a", 70) + "\n")},
		{name: "alias collision", yaml: tools("      - op: a.b\n      - op: a_b\n")},
		{name: "invalid call id header", yaml: "catalog:\n  operation_meta_key: op\n  call_id_header: \"bad header\"\njourneys:\n  - id: j\n    skill: skills/planner\n    system_prompt: prompts/p.md\n    tools:\n      - op: a\n"},
		{name: "duplicate keyword", yaml: journey("    routing: {keywords: [A, a]}\n    tools:\n      - op: a\n")},
		{name: "scalar not mapping", yaml: "just a string\n"},
		{name: "syntax error", yaml: "catalog: [\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, err := Compile(writeCompileTree(t, tc.yaml, tc.extra))
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
		if _, err := Compile(path); err == nil {
			t.Fatal("accepted a symlinked skill directory")
		}
	})
	t.Run("symlink inside bundle", func(t *testing.T) {
		path := writeCompileTree(t, compileMinimal, nil)
		root := filepath.Dir(path)
		if err := os.Symlink(filepath.Join(root, "prompts", "p.md"), filepath.Join(root, "skills", "planner", "link.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := Compile(path); err == nil {
			t.Fatal("accepted a symlink inside the bundle")
		}
	})
	t.Run("symlinked directory inside bundle", func(t *testing.T) {
		path := writeCompileTree(t, compileMinimal, nil)
		root := filepath.Dir(path)
		if err := os.Symlink(root, filepath.Join(root, "skills", "planner", "loop")); err != nil {
			t.Fatal(err)
		}
		if _, err := Compile(path); err == nil {
			t.Fatal("accepted a symlinked directory inside the bundle")
		}
	})
	t.Run("symlinked prompt", func(t *testing.T) {
		path := writeCompileTree(t, compileMinimal, nil)
		root := filepath.Dir(path)
		if err := os.Rename(filepath.Join(root, "prompts", "p.md"), filepath.Join(root, "real.md")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "real.md"), filepath.Join(root, "prompts", "p.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := Compile(path); err == nil {
			t.Fatal("accepted a symlinked prompt")
		}
	})
}

func TestCompileDigestIsContentAddressed(t *testing.T) {
	yaml := fmt.Sprintf(compileTwoOpsYA, "lookup")
	a, err := Compile(writeCompileTree(t, yaml, nil))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Compile(writeCompileTree(t, yaml, nil))
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
		"prompt":     {yaml, map[string]string{"prompts/p.md": "changed prompt\n"}},
		"selector":   {fmt.Sprintf(compileTwoOpsYA, "other"), nil},
		"supporting": {yaml, map[string]string{"skills/planner/guide.md": "changed\n"}},
		"catalog":    {strings.Replace(yaml, "op\n", "op2\n", 1), nil},
	} {
		changed, err := Compile(writeCompileTree(t, c.yaml, c.extra))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		jc, _ := changed.Journey("j")
		if changed.Digest() == a.Digest() || jc.Digest == ja.Digest {
			t.Fatalf("%s change did not change the digests", name)
		}
	}
}

func compileRetainTree(t *testing.T, id, prompt string) *Set {
	t.Helper()
	yaml := strings.Replace(fmt.Sprintf(compileTwoOpsYA, "lookup"), "id: j", "id: "+id, 1)
	set, err := Compile(writeCompileTree(t, yaml, map[string]string{"prompts/p.md": prompt}))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestCompileRetain(t *testing.T) {
	current := compileRetainTree(t, "j", "v2 prompt\n")
	old := compileRetainTree(t, "j", "v1 prompt\n")
	gone := compileRetainTree(t, "other", "v1 prompt\n")

	retained, err := Retain(current, old, gone)
	if err != nil {
		t.Fatal(err)
	}
	cj, _ := current.Journey("j")
	oj, _ := old.Journey("j")
	gj, _ := gone.Journey("other")
	for _, digest := range []string{cj.Digest, oj.Digest, gj.Digest} {
		if !retained.HasDigest(digest) {
			t.Fatalf("HasDigest(%s) = false", digest)
		}
	}
	if id, ok := retained.JourneyIDForDigest(oj.Digest); !ok || id != "j" {
		t.Fatalf("JourneyIDForDigest = %q %v", id, ok)
	}
	if id, ok := retained.JourneyIDForDigest(gj.Digest); !ok || id != "other" {
		t.Fatalf("JourneyIDForDigest = %q %v", id, ok)
	}
	if _, ok := retained.JourneyIDForDigest("nope"); ok || retained.HasDigest("nope") {
		t.Fatal("unknown digest accepted")
	}
	if got := retained.Journeys(); len(got) != 1 || got[0].ID != "j" || got[0].Digest != cj.Digest {
		t.Fatalf("Journeys() must only list current journeys: %+v", got)
	}
	if _, ok := retained.Journey("other"); ok {
		t.Fatal("retained-only journey is served as current")
	}
	if retained.Digest() != current.Digest() {
		t.Fatal("Retain changed the set digest")
	}

	pj, owner, ok := retained.Pinned(oj.Digest)
	if !ok || owner != old || pj.Prompt != "v1 prompt\n" {
		t.Fatalf("retained Pinned = %q owner=%p want %p ok=%v", pj.Prompt, owner, old, ok)
	}
	pj, owner, ok = retained.Pinned(cj.Digest)
	if !ok || owner != retained || pj.Prompt != "v2 prompt\n" {
		t.Fatalf("current Pinned owner = %p want %p ok=%v", owner, retained, ok)
	}
	if _, _, ok := retained.Pinned("nope"); ok {
		t.Fatal("unknown digest pinned")
	}
	if current.HasDigest(oj.Digest) {
		t.Fatal("Retain mutated the current set")
	}
	// Retaining a retained copy keeps the original owner.
	again, err := Retain(retained, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, owner, _ := again.Pinned(oj.Digest); owner != old {
		t.Fatal("owner lost across Retain")
	}
	chained, err := Retain(current, retained)
	if err != nil {
		t.Fatal(err)
	}
	if _, owner, _ := chained.Pinned(oj.Digest); owner != old {
		t.Fatal("chained retained owner is not the original set")
	}
	if _, err := Retain(nil); err == nil {
		t.Fatal("Retain(nil) succeeded")
	}
}

func TestCompileRetainCollisionAndLimits(t *testing.T) {
	current := compileRetainTree(t, "j", "v2 prompt\n")
	old := compileRetainTree(t, "j", "v1 prompt\n")
	oj, _ := old.Journey("j")

	forged := *current
	forged.versions = map[string]Journey{oj.Digest: oj}
	forged.canonical = map[string][]byte{oj.Digest: []byte("different")}
	forged.origin = map[string]*Set{}
	if _, err := Retain(old, &forged); err == nil || !strings.Contains(err.Error(), "digest collision") {
		t.Fatalf("collision not detected: %v", err)
	}

	// Aggregate file budget: each set accounts 2 files (SKILL.md + guide.md + ref/more.md = 3).
	perSet := old.skillFiles
	if perSet == 0 {
		t.Fatal("skill files not counted")
	}
	fits := MaxCompiledSkillFiles / perSet
	sets := make([]*Set, fits+1)
	for i := range sets {
		sets[i] = compileRetainTree(t, fmt.Sprintf("j%03d", i), "p\n")
	}
	if _, err := Retain(sets[0], sets[1:fits]...); err != nil {
		t.Fatalf("rejected retained bundles within limit: %v", err)
	}
	if _, err := Retain(sets[0], sets[1:]...); err == nil || !strings.Contains(err.Error(), "retained skill bundles exceed aggregate limits") {
		t.Fatalf("aggregate limit not enforced: %v", err)
	}
	// Re-retaining an already retained digest consumes no budget.
	if _, err := Retain(sets[0], sets[1], sets[1], sets[0]); err != nil {
		t.Fatal(err)
	}
}

func TestCompileInferRouting(t *testing.T) {
	yaml := `catalog:
  operation_meta_key: op
journeys:
  - id: general
    skill: skills/planner
    system_prompt: prompts/p.md
    routing: {default: true}
    tools: [{op: a}]
  - id: travel
    skill: skills/planner
    system_prompt: prompts/p.md
    routing: {keywords: [Flight, hotel], priority: 5}
    tools: [{op: b}]
  - id: premium
    skill: skills/planner
    system_prompt: prompts/p.md
    routing: {keywords: [flight], priority: 9}
    tools: [{op: c}]
  - id: tie
    skill: skills/planner
    system_prompt: prompts/p.md
    routing: {keywords: [hotel], priority: 5}
    tools: [{op: d}]
`
	set, err := Compile(writeCompileTree(t, yaml, nil))
	if err != nil {
		t.Fatal(err)
	}
	for text, want := range map[string]string{
		"book a FLIGHT please": "premium",
		"need a hotel":         "travel", // equal priority: first wins
		"hello there":          "general",
		"  ":                   "",
	} {
		got, ok := set.Infer(text)
		if want == "" {
			if ok {
				t.Fatalf("Infer(%q) = %q, want none", text, got.ID)
			}
			continue
		}
		if !ok || got.ID != want {
			t.Fatalf("Infer(%q) = %q %v, want %q", text, got.ID, ok, want)
		}
	}

	// Without a default, the first journey is the fallback.
	noDefault := strings.Replace(yaml, "routing: {default: true}", "routing: {}", 1)
	set, err = Compile(writeCompileTree(t, noDefault, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := set.Infer("hello"); !ok || got.ID != "general" {
		t.Fatalf("fallback = %q %v", got.ID, ok)
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
	for _, name := range []string{"request_user_input", "load_skill", "read_skill_resource", "activate_journey"} {
		if !IsReservedToolName(name) {
			t.Errorf("%s not reserved", name)
		}
	}
	if IsReservedToolName("lookup") || IsReservedToolName("Load_Skill") {
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
			if _, err := compileSkill(root, tc.declaration); err == nil || !strings.Contains(err.Error(), "parent components are not allowed") {
				t.Fatalf("expected raw parent component rejection, got %v", err)
			}
		})
	}
}

func TestCompileRepositoryConfig(t *testing.T) {
	set, err := Compile(filepath.Join("..", "..", "configs", "journeys.yaml"))
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
	if !journeys[0].Routing.Default || len(journeys[0].Skills) != 1 || journeys[0].Skills[0].Name != "planner" {
		t.Fatalf("vacation-planner = %+v", journeys[0])
	}
	if got, want := journeys[0].Tools, []Selector{{Ops: []string{"lookup_destination"}, Class: ClassReadOnly}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("vacation-planner tools = %+v", got)
	}
	swap := journeys[1]
	if swap.Skills[0].Name != "shift-swap" || swap.Routing.Priority != 10 || !reflect.DeepEqual(swap.Routing.Keywords, []string{"shift", "swap"}) {
		t.Fatalf("shift-swap = %+v", swap)
	}
	want := []Selector{
		{Ops: []string{"lookup_shift"}, Class: ClassReadOnly},
		{Ops: []string{"request_shift_swap"}}, // unclassified: effectful, approval required
	}
	if !reflect.DeepEqual(swap.Tools, want) {
		t.Fatalf("shift-swap tools = %+v, want %+v", swap.Tools, want)
	}
	if swap.Tools[1].Class != "" || swap.Tools[1].ApprovalRequired {
		t.Fatalf("request_shift_swap must stay unclassified, got %+v", swap.Tools[1])
	}
}
