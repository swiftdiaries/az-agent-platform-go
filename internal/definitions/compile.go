package definitions

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

const (
	defaultMaxTools = 15
	maxMaxTools     = 64
	maxJourneyID    = 64
)

type rawConfig struct {
	Catalog            rawCatalog   `yaml:"catalog"`
	ConversationPrompt string       `yaml:"conversation_prompt"`
	Journeys           []rawJourney `yaml:"journeys"`
}

type rawCatalog struct {
	OperationMetaKey string `yaml:"operation_meta_key"`
	CallIDHeader     string `yaml:"call_id_header"`
}

type rawJourney struct {
	ID              string      `yaml:"id"`
	Skill           string      `yaml:"skill"`
	ProductVersions rawVersions `yaml:"product_versions"`
	Tools           []rawTool   `yaml:"tools"`
	Deny            []string    `yaml:"deny"`
	MaxTools        *int        `yaml:"max_tools"`
}

type rawVersions struct {
	Min          string `yaml:"min"`
	MaxExclusive string `yaml:"max_exclusive"`
}

type rawTool struct {
	Op       opList   `yaml:"op"`
	Tags     []string `yaml:"tags"`
	ReadOnly bool     `yaml:"read_only"`
	Class    string   `yaml:"class"`
	Approval string   `yaml:"approval"`
}

// opList accepts a scalar or a sequence of scalars.
type opList struct {
	values  []string
	present bool
}

type shapeError string

func (e shapeError) Error() string { return string(e) }

func (o *opList) UnmarshalYAML(n *yaml.Node) error {
	scalar := func(c *yaml.Node) (string, error) {
		if c.Kind != yaml.ScalarNode || c.ShortTag() == "!!null" {
			return "", shapeError("op must be a string or a list of strings")
		}
		return c.Value, nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		v, err := scalar(n)
		if err != nil {
			return err
		}
		o.values = []string{v}
	case yaml.SequenceNode:
		o.values = make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := scalar(c)
			if err != nil {
				return err
			}
			o.values = append(o.values, v)
		}
	default:
		return shapeError("op must be a string or a list of strings")
	}
	o.present = true
	return nil
}

var lineRe = regexp.MustCompile(`line (\d+)`)

// sanitizeDecodeError reports where decoding failed without echoing any value.
func sanitizeDecodeError(err error) error {
	var shape shapeError
	if errors.As(err, &shape) {
		return fmt.Errorf("decode journey config: %s", string(shape))
	}
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		var msgs []string
		for _, e := range typeErr.Errors {
			kind := "invalid value"
			switch {
			case strings.Contains(e, "not found in type"):
				kind = "unknown field"
			case strings.Contains(e, "already defined"):
				kind = "duplicate key"
			}
			if m := lineRe.FindStringSubmatch(e); m != nil {
				kind = "line " + m[1] + ": " + kind
			}
			msgs = append(msgs, kind)
			if len(msgs) == 5 {
				break
			}
		}
		return fmt.Errorf("decode journey config: %s", strings.Join(msgs, "; "))
	}
	if m := lineRe.FindStringSubmatch(err.Error()); m != nil {
		return fmt.Errorf("decode journey config: YAML syntax error near line %s", m[1])
	}
	return errors.New("decode journey config: invalid YAML")
}

// Compile reads a YAML journey configuration and its skill bundles from the
// OS filesystem and compiles them into a content-addressed Set.
func Compile(path string) (*Set, error) {
	data, err := readLimited(path, MaxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read journey config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("journey config is empty")
		}
		return nil, sanitizeDecodeError(err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("journey config must contain exactly one YAML document")
	}

	catalog, err := compileCatalog(raw.Catalog)
	if err != nil {
		return nil, err
	}
	if len(raw.Journeys) == 0 {
		return nil, errors.New("at least one journey is required")
	}
	root, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}

	var conversationPrompt string
	if raw.ConversationPrompt != "" {
		promptPath, err := confinedPath(root, raw.ConversationPrompt)
		if err != nil {
			return nil, fmt.Errorf("conversation_prompt: %v", err)
		}
		data, err := readLimited(promptPath, MaxPromptBytes)
		if err != nil {
			return nil, fmt.Errorf("conversation_prompt: %v", err)
		}
		conversationPrompt = string(data)
	}

	set := &Set{
		conversationPrompt: conversationPrompt,
		catalog:            catalog, journeys: make(map[string]Journey), canonical: make(map[string][]byte),
	}
	skills := make(map[string]SkillPackage)
	var digests []string
	for i, rj := range raw.Journeys {
		n := i + 1
		journey, err := compileJourney(root, catalog, n, rj, skills, set)
		if err != nil {
			return nil, err
		}
		if _, dup := set.journeys[journey.ID]; dup {
			return nil, fmt.Errorf("journey %d: duplicate journey id", n)
		}
		canonical, err := canonicalBytes(journey)
		if err != nil {
			return nil, err
		}
		journey.Digest = hexDigest(canonical)
		set.journeys[journey.ID] = journey
		set.canonical[journey.Digest] = canonical
		set.order = append(set.order, journey.ID)
		digests = append(digests, journey.Digest)
	}
	if err := checkPolicyConflicts(set); err != nil {
		return nil, err
	}
	if set.digest, err = setDigest(catalog, conversationPrompt, digests); err != nil {
		return nil, err
	}
	return set, nil
}

func compileCatalog(raw rawCatalog) (CatalogConfig, error) {
	if raw.OperationMetaKey == "" || strings.TrimSpace(raw.OperationMetaKey) == "" {
		return CatalogConfig{}, errors.New("catalog.operation_meta_key is required")
	}
	catalog := CatalogConfig{OperationMetaKey: raw.OperationMetaKey}
	if raw.CallIDHeader != "" {
		if !validHeaderToken(raw.CallIDHeader) {
			return CatalogConfig{}, errors.New("catalog.call_id_header is not a valid header name")
		}
		catalog.CallIDHeader = textproto.CanonicalMIMEHeaderKey(raw.CallIDHeader)
	}
	return catalog, nil
}

func validHeaderToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
		if !ok {
			return false
		}
	}
	return s != ""
}

func compileJourney(root string, catalog CatalogConfig, n int, rj rawJourney, skills map[string]SkillPackage, set *Set) (Journey, error) {
	fail := func(format string, args ...any) (Journey, error) {
		return Journey{}, fmt.Errorf("journey %d: %s", n, fmt.Sprintf(format, args...))
	}
	if rj.ID == "" || len(rj.ID) > maxJourneyID || strings.IndexFunc(rj.ID, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return fail("id is required, at most %d bytes, without whitespace", maxJourneyID)
	}
	versions := VersionRange{Min: rj.ProductVersions.Min, MaxExclusive: rj.ProductVersions.MaxExclusive}
	if err := validateVersionRange(versions); err != nil {
		return fail("%v", err)
	}
	maxTools := defaultMaxTools
	if rj.MaxTools != nil {
		maxTools = *rj.MaxTools
		if maxTools < 1 || maxTools > maxMaxTools {
			return fail("max_tools must be between 1 and %d", maxMaxTools)
		}
	}
	tools, deny, err := compileTools(catalog, rj)
	if err != nil {
		return fail("%v", err)
	}

	if rj.Skill == "" {
		return fail("skill is required")
	}
	key := filepath.Clean(rj.Skill)
	pkg, ok := skills[key]
	if !ok {
		if pkg, err = compileBundle(root, rj.Skill); err != nil {
			return fail("skill: %v", err)
		}
		bytesUsed, filesUsed := skillFootprint(pkg)
		if set.skillBytes > MaxCompiledSkillBytes-bytesUsed || set.skillFiles > MaxCompiledSkillFiles-filesUsed {
			return fail("compiled skill bundles exceed aggregate limits")
		}
		set.skillBytes += bytesUsed
		set.skillFiles += filesUsed
		skills[key] = pkg
	}

	return Journey{
		ID: rj.ID, Description: pkg.Description, Prompt: skillBody(pkg.Instructions.Data),
		Skills: []SkillPackage{pkg}, Tools: tools, Deny: deny, MaxTools: maxTools,
		ProductVersions: versions, Catalog: catalog, Skill: rj.Skill,
	}, nil
}

func validateVersionRange(r VersionRange) error {
	for _, v := range []string{r.Min, r.MaxExclusive} {
		if v == "" {
			continue
		}
		if strings.IndexFunc(v, unicode.IsSpace) >= 0 {
			return errors.New("product_versions bound contains whitespace")
		}
		for _, seg := range strings.Split(v, ".") {
			if seg == "" {
				return errors.New("product_versions bound has an empty segment")
			}
		}
	}
	if r.Min != "" && r.MaxExclusive != "" && CompareVersions(r.Min, r.MaxExclusive) >= 0 {
		return errors.New("product_versions min must be lower than max_exclusive")
	}
	return nil
}

func compileTools(catalog CatalogConfig, rj rawJourney) ([]Selector, []string, error) {
	if len(rj.Tools) == 0 {
		return nil, nil, errors.New("tools must not be empty")
	}
	denied := make(map[string]bool, len(rj.Deny))
	var deny []string
	for _, key := range rj.Deny {
		if key == "" || denied[key] {
			return nil, nil, errors.New("deny has an empty or duplicate operation key")
		}
		denied[key] = true
		deny = append(deny, key)
	}
	seenOps := map[string]bool{}
	aliases := map[string]bool{}
	tools := make([]Selector, 0, len(rj.Tools))
	for i, rt := range rj.Tools {
		fail := func(format string, args ...any) ([]Selector, []string, error) {
			return nil, nil, fmt.Errorf("tool %d: %s", i+1, fmt.Sprintf(format, args...))
		}
		hasOp, hasTags := rt.Op.present, rt.Tags != nil
		if hasOp == hasTags {
			return fail("exactly one of op and tags is required")
		}
		sel := Selector{ReadOnlyHint: rt.ReadOnly}
		switch Class(rt.Class) {
		case "", ClassReadOnly, ClassIdempotent, ClassEffectful:
			sel.Class = Class(rt.Class)
		default:
			return fail("invalid class")
		}
		if sel.Class == ClassIdempotent && catalog.CallIDHeader == "" {
			return fail("class idempotent requires catalog.call_id_header")
		}
		switch rt.Approval {
		case "":
		case "required":
			sel.ApprovalRequired = true
		default:
			return fail("approval must be \"required\"")
		}
		if hasOp {
			if len(rt.Op.values) == 0 {
				return fail("op must not be empty")
			}
			for _, key := range rt.Op.values {
				if key == "" {
					return fail("op has an empty alternative")
				}
				if seenOps[key] {
					return fail("operation key is selected more than once")
				}
				if denied[key] {
					return fail("operation key is also denied")
				}
				seenOps[key] = true
				alias, ok := AliasFor(key)
				if !ok {
					return fail("operation key has no valid tool alias")
				}
				if IsReservedToolName(alias) || aliases[alias] {
					return fail("operation key alias is reserved or collides with another operation")
				}
				aliases[alias] = true
			}
			sel.Ops = append([]string(nil), rt.Op.values...)
		} else {
			if len(rt.Tags) == 0 {
				return fail("tags must not be empty")
			}
			seen := map[string]bool{}
			for _, tag := range rt.Tags {
				if tag == "" || seen[tag] {
					return fail("tags has an empty or duplicate entry")
				}
				seen[tag] = true
			}
			sel.Tags = append([]string(nil), rt.Tags...)
		}
		tools = append(tools, sel)
	}
	return tools, deny, nil
}

// compileBundle compiles the skill bundle rooted at the config-relative path.
func compileBundle(root, relative string) (SkillPackage, error) {
	if clean := filepath.Clean(relative); clean == "." {
		return SkillPackage{}, errors.New("path must name a bundle directory")
	}
	packageRoot, err := confinedPath(root, relative)
	if err != nil {
		return SkillPackage{}, err
	}
	info, err := os.Stat(packageRoot)
	if err != nil || !info.IsDir() {
		return SkillPackage{}, errors.New("package root is not a directory")
	}
	instructionPath, err := confinedPath(packageRoot, "SKILL.md")
	if err != nil {
		return SkillPackage{}, err
	}
	instructions, err := readLimited(instructionPath, MaxSkillBytes)
	if err != nil {
		return SkillPackage{}, err
	}
	name, _, err := skillMetadata(instructions)
	if err != nil {
		return SkillPackage{}, err
	}
	var supporting []string
	if err := enumerateBundle(packageRoot, "", &supporting); err != nil {
		return SkillPackage{}, err
	}
	sort.Strings(supporting)
	return compileSkill(root, skillDeclaration{Name: name, Root: relative, SupportingFiles: supporting, AllowSupportingFiles: len(supporting) > 0})
}

// enumerateBundle lists regular files below dir (slash-relative), skipping
// dot-prefixed entries and the top-level SKILL.md, and rejecting symlinks.
func enumerateBundle(dir, rel string, files *[]string) error {
	entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		relName := path.Join(rel, name)
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(relName)))
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symbolic links are not allowed in a skill bundle")
		}
		if strings.HasPrefix(name, ".") {
			continue
		}
		switch {
		case info.IsDir():
			if err := enumerateBundle(dir, relName, files); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if relName == "SKILL.md" {
				continue
			}
			if len(*files) >= MaxCompiledSkillFiles {
				return errors.New("skill bundle has too many files")
			}
			*files = append(*files, relName)
		default:
			return errors.New("skill bundle contains an unsupported file type")
		}
	}
	return nil
}

// effectivePolicy is the static (class, requires-approval) pair of an
// operation key within one journey.
type effectivePolicy struct {
	class    Class
	approval bool
}

func policyClassRank(c Class) int {
	switch c {
	case ClassReadOnly:
		return 0
	case ClassIdempotent:
		return 1
	}
	return 2
}

// journeyPolicies merges the op selectors of a journey per key the way
// catalog resolution does: strictest class, approval OR-ed.
func journeyPolicies(j Journey) map[string]effectivePolicy {
	out := map[string]effectivePolicy{}
	for _, s := range j.Tools {
		for _, key := range s.Ops {
			next := effectivePolicy{class: s.Class, approval: s.ApprovalRequired}
			if next.class == "" {
				next.class = ClassEffectful
			}
			if prev, ok := out[key]; ok {
				next.approval = next.approval || prev.approval
				if policyClassRank(prev.class) > policyClassRank(next.class) {
					next.class = prev.class
				}
			}
			out[key] = next
		}
	}
	for key, p := range out {
		p.approval = p.approval || p.class != ClassReadOnly
		out[key] = p
	}
	return out
}

// checkPolicyConflicts rejects one operation key, or one alias, carrying
// different effective policies in different journeys. Tag selectors are
// checked at bind time.
func checkPolicyConflicts(set *Set) error {
	ids := append([]string(nil), set.order...)
	sort.Strings(ids)
	byKey := map[string]map[string]effectivePolicy{}
	byAlias := map[string]map[string]effectivePolicy{}
	for _, id := range ids {
		for key, p := range journeyPolicies(set.journeys[id]) {
			if byKey[key] == nil {
				byKey[key] = map[string]effectivePolicy{}
			}
			byKey[key][id] = p
			if alias, ok := AliasFor(key); ok {
				if byAlias[alias] == nil {
					byAlias[alias] = map[string]effectivePolicy{}
				}
				byAlias[alias][id] = p
			}
		}
	}
	for _, group := range []struct {
		kind string
		m    map[string]map[string]effectivePolicy
	}{{"operation", byKey}, {"alias", byAlias}} {
		names := make([]string, 0, len(group.m))
		for name := range group.m {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			var first string
			for _, id := range ids {
				p, ok := group.m[name][id]
				if !ok {
					continue
				}
				if first == "" {
					first = id
				} else if p != group.m[name][first] {
					return fmt.Errorf("definitions: %s %q has conflicting policies in journeys %s, %s", group.kind, name, first, id)
				}
			}
		}
	}
	return nil
}
