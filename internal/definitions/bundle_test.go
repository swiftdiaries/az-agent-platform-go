package definitions

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const bundleYAML = "catalog:\n  operation_meta_key: op\nconversation_prompt: prompts/conv.md\njourneys:\n  - id: j\n    skill: skills/planner\n    tools:\n      - op: lookup\n"

// writeBundleDir writes files in the given order (slash paths) under root.
func writeBundleDir(t *testing.T, root string, order []string, files map[string]string) {
	t.Helper()
	for _, name := range order {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(files[name]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func bundleFiles() map[string]string {
	return map[string]string{
		"journeys.yaml":              bundleYAML,
		"prompts/conv.md":            "You are the conversation agent.\n",
		"skills/planner/SKILL.md":    compileSkillMD,
		"skills/planner/guide.md":    "Prefer direct routes.\n",
		"skills/planner/ref/more.md": "More.\n",
		"README.md":                  "unreferenced\n",
		"skills/unused/SKILL.md":     "---\nname: unused\ndescription: Unused\n---\nBody\n",
	}
}

func bundleDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := bundleFiles()
	order := make([]string, 0, len(files))
	for name := range files {
		order = append(order, name)
	}
	writeBundleDir(t, root, order, files)
	return root
}

func TestBundleRoundTripDigest(t *testing.T) {
	for name, dir := range map[string]string{"configs": filepath.Join("..", "..", "configs"), "temp": bundleDir(t)} {
		t.Run(name, func(t *testing.T) {
			b, err := ReadBundle(os.DirFS(dir))
			if err != nil {
				t.Fatal(err)
			}
			mem, err := CompileBundle(b)
			if err != nil {
				t.Fatal(err)
			}
			again, err := CompileBundle(mem.Bundle())
			if err != nil {
				t.Fatal(err)
			}
			if mem.Digest() == "" || mem.Digest() != again.Digest() {
				t.Fatalf("digests differ: %s %s", mem.Digest(), again.Digest())
			}
			if !reflect.DeepEqual(mem.Bundle(), again.Bundle()) {
				t.Fatal("Bundle() is not stable across a round trip")
			}
			if mem.ConversationPrompt() != again.ConversationPrompt() {
				t.Fatal("conversation prompt differs")
			}
		})
	}
}

func TestBundleOrderIndependent(t *testing.T) {
	files := bundleFiles()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sortStrings := func(s []string, reverse bool) []string {
		out := append([]string(nil), s...)
		for i := range out {
			for j := i + 1; j < len(out); j++ {
				if (out[j] < out[i]) != reverse {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
		return out
	}
	a, b := t.TempDir(), t.TempDir()
	writeBundleDir(t, a, sortStrings(names, false), files)
	writeBundleDir(t, b, sortStrings(names, true), files)
	setA, err := compileFile(filepath.Join(a, BundleConfig))
	if err != nil {
		t.Fatal(err)
	}
	setB, err := compileFile(filepath.Join(b, BundleConfig))
	if err != nil {
		t.Fatal(err)
	}
	fwd, rev := Bundle{}, Bundle{}
	for _, name := range sortStrings(names, false) {
		fwd[name] = []byte(files[name])
	}
	for _, name := range sortStrings(names, true) {
		rev[name] = []byte(files[name])
	}
	setC, err := CompileBundle(fwd)
	if err != nil {
		t.Fatal(err)
	}
	setD, err := CompileBundle(rev)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Set{setB, setC, setD} {
		if s.Digest() != setA.Digest() || !reflect.DeepEqual(s.Bundle(), setA.Bundle()) {
			t.Fatal("set depends on file creation or insertion order")
		}
	}
}

func TestBundleExcludesUnreferencedFiles(t *testing.T) {
	root := bundleDir(t)
	set, err := compileFile(filepath.Join(root, BundleConfig))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for name := range set.Bundle() {
		got = append(got, name)
	}
	want := map[string]bool{"journeys.yaml": true, "prompts/conv.md": true, "skills/planner/SKILL.md": true, "skills/planner/guide.md": true, "skills/planner/ref/more.md": true}
	if len(got) != len(want) {
		t.Fatalf("bundle files = %v", got)
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("unexpected bundle file %q", name)
		}
	}
	if !bytes.Equal(set.Bundle()[BundleConfig], []byte(bundleYAML)) {
		t.Fatal("config bytes not stored under BundleConfig")
	}
}

func TestBundleSkipsDotFiles(t *testing.T) {
	root := bundleDir(t)
	writeBundleDir(t, root, []string{".hidden", ".git/config", "skills/planner/.DS_Store", "skills/planner/.cache/x.md"}, map[string]string{
		".hidden": "h", ".git/config": "c", "skills/planner/.DS_Store": "d", "skills/planner/.cache/x.md": "x",
	})
	b, err := ReadBundle(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	for name := range b {
		for _, part := range strings.Split(name, "/") {
			if strings.HasPrefix(part, ".") {
				t.Fatalf("dot path %q in bundle", name)
			}
		}
	}
	if len(b) != len(bundleFiles()) {
		t.Fatalf("bundle has %d files", len(b))
	}
}

func TestBundleRejectsSymlink(t *testing.T) {
	root := bundleDir(t)
	if err := os.Symlink(filepath.Join(root, "README.md"), filepath.Join(root, "skills", "planner", "link.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := ReadBundle(os.DirFS(root)); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("ReadBundle err = %v", err)
	}
	// A symlinked directory is rejected too.
	root2 := bundleDir(t)
	if err := os.Symlink(filepath.Join(root2, "skills"), filepath.Join(root2, "linked")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := ReadBundle(os.DirFS(root2)); err == nil {
		t.Fatal("symlinked directory accepted")
	}
}

func TestBundleLimits(t *testing.T) {
	root := bundleDir(t)
	big := t.TempDir()
	if err := os.WriteFile(filepath.Join(big, BundleConfig), []byte(bundleYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= MaxBundleFiles; i++ {
		if err := os.WriteFile(filepath.Join(big, "f"+strings.Repeat("x", 3)+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676))), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadBundle(os.DirFS(big)); err == nil || !strings.Contains(err.Error(), "too many files") {
		t.Fatalf("file count err = %v", err)
	}
	huge := t.TempDir()
	if err := os.WriteFile(filepath.Join(huge, BundleConfig), []byte(bundleYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(huge, "blob"), make([]byte, MaxBundleFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBundle(os.DirFS(huge)); err == nil {
		t.Fatal("oversized file accepted")
	}
	many := t.TempDir()
	if err := os.WriteFile(filepath.Join(many, BundleConfig), []byte(bundleYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, MaxBundleFileBytes)
	for i := 0; i <= MaxBundleBytes/MaxBundleFileBytes; i++ {
		if err := os.WriteFile(filepath.Join(many, "c"+string(rune('a'+i))), chunk, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadBundle(os.DirFS(many)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("total size err = %v", err)
	}
	// CompileBundle enforces the same bounds on in-memory input.
	b := Bundle{BundleConfig: []byte(bundleYAML)}
	for i := 0; i <= MaxBundleFiles; i++ {
		b[string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676))] = []byte("x")
	}
	if _, err := CompileBundle(b); err == nil {
		t.Fatal("CompileBundle accepted too many files")
	}
	if _, err := ReadBundle(os.DirFS(root)); err != nil {
		t.Fatal(err)
	}
}

func TestBundleMissingConfig(t *testing.T) {
	root := bundleDir(t)
	if err := os.Remove(filepath.Join(root, BundleConfig)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBundle(os.DirFS(root)); err == nil || !strings.Contains(err.Error(), BundleConfig) {
		t.Fatalf("ReadBundle err = %v", err)
	}
	if _, err := CompileBundle(Bundle{"skills/planner/SKILL.md": []byte(compileSkillMD)}); err == nil || !strings.Contains(err.Error(), BundleConfig) {
		t.Fatalf("CompileBundle err = %v", err)
	}
	if _, err := CompileBundle(nil); err == nil {
		t.Fatal("nil bundle accepted")
	}
}

func TestCompileBundleConfinement(t *testing.T) {
	skill := func(p string) string {
		return "catalog:\n  operation_meta_key: op\njourneys:\n  - id: j\n    skill: " + p + "\n    tools:\n      - op: lookup\n"
	}
	prompt := func(p string) string {
		return "catalog:\n  operation_meta_key: op\nconversation_prompt: " + p + "\njourneys:\n  - id: j\n    skill: skills/planner\n    tools:\n      - op: lookup\n"
	}
	cases := map[string]string{
		"skill dotdot":       skill("../skills/planner"),
		"skill inner dotdot": skill("skills/../skills/planner"),
		"skill absolute":     skill("/skills/planner"),
		"prompt dotdot":      prompt("../conv.md"),
		"prompt absolute":    prompt("/prompts/conv.md"),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			b := Bundle{
				BundleConfig:              []byte(cfg),
				"prompts/conv.md":         []byte("p"),
				"conv.md":                 []byte("p"),
				"skills/planner/SKILL.md": []byte(compileSkillMD),
			}
			if set, err := CompileBundle(b); err == nil || set != nil {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	if _, err := CompileBundle(Bundle{BundleConfig: []byte(bundleYAML), "../escape": []byte("x")}); err == nil {
		t.Fatal("invalid bundle key accepted")
	}
}

func TestCompileBundleRejectsInvalidConfigs(t *testing.T) {
	good := map[string]string{
		"skills/planner/SKILL.md": compileSkillMD,
		"skills/planner/guide.md": "g",
	}
	for name, cfg := range map[string]string{
		"syntax":         "catalog: [\n",
		"empty":          "",
		"unknown field":  "bogus: 1\n",
		"scalar":         "just a string\n",
		"no journeys":    "catalog:\n  operation_meta_key: op\n",
		"missing skill":  strings.Replace(compileMinimal, "skills/planner", "skills/nothere", 1),
		"skill dotdot":   strings.Replace(compileMinimal, "skills/planner", "../planner", 1),
		"skill absolute": strings.Replace(compileMinimal, "skills/planner", "/skills/planner", 1),
		"skill dot":      strings.Replace(compileMinimal, "skills/planner", ".", 1),
		"bad alias":      compileMinimal + "      - op: lookup2\n        alias: Bad Alias\n",
		"two documents":  compileMinimal + "---\n" + compileMinimal,
	} {
		t.Run(name, func(t *testing.T) {
			b := Bundle{BundleConfig: []byte(cfg)}
			for n, c := range good {
				b[n] = []byte(c)
			}
			if set, err := CompileBundle(b); err == nil || set != nil {
				t.Fatalf("invalid config accepted: set=%v err=%v", set, err)
			}
		})
	}
}

func TestCompileBundleIsolatedFromInput(t *testing.T) {
	in := Bundle{
		BundleConfig:              []byte(bundleYAML),
		"prompts/conv.md":         []byte("prompt"),
		"skills/planner/SKILL.md": []byte(compileSkillMD),
		"skills/planner/guide.md": []byte("guide"),
	}
	set, err := CompileBundle(in)
	if err != nil {
		t.Fatal(err)
	}
	before := set.Bundle()
	digest := set.Digest()
	for _, data := range in {
		for i := range data {
			data[i] = 'Z'
		}
	}
	in["extra.md"] = []byte("x")
	out := set.Bundle()
	for _, data := range out {
		for i := range data {
			data[i] = 'Q'
		}
	}
	delete(out, "prompts/conv.md")
	if !reflect.DeepEqual(set.Bundle(), before) || set.Digest() != digest {
		t.Fatal("set changed after mutating input or output bundle")
	}
	j, _ := set.Journey("j")
	if !bytes.Equal(j.Skills[0].Supporting["guide.md"].Data, []byte("guide")) {
		t.Fatal("compiled skill data aliases the input")
	}
	if _, err := CompileBundle(set.Bundle()); err != nil {
		t.Fatal(err)
	}
}

func TestReadBundleErrorsDoNotEchoContent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, BundleConfig), []byte("SECRET: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := ReadBundle(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	_, err = CompileBundle(b)
	if err == nil || strings.Contains(err.Error(), "SECRET") || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}
