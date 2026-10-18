package catalog

import (
	"encoding/json"
	"sort"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
)

const defaultMaxTools = 15

// Snapshot is the set of tools an integration endpoint advertised for one
// product version. It is evidence, never authorization.
type Snapshot struct {
	Digest, ProductVersion, EndpointID, ServerVersion string
	Tools                                             []Descriptor
}

// Reason says why a journey cannot be served from a snapshot.
type Reason string

const (
	ReasonMissing      Reason = "missing_operation"
	ReasonAmbiguous    Reason = "ambiguous_operation"
	ReasonCollision    Reason = "alias_collision"
	ReasonInvalidAlias Reason = "invalid_alias"
	ReasonMaxTools     Reason = "max_tools_exceeded"
	ReasonVersion      Reason = "product_version_out_of_range"
)

// Unavailable is one reason a journey cannot be served. Operation is an
// operation key, or an alias for collisions; it is never a remote tool name.
type Unavailable struct {
	Reason    Reason
	Operation string
}

// Tool is one resolved operation as the model sees it.
type Tool struct {
	Alias, OperationKey, RemoteName string
	Description                     string
	InputSchema                     json.RawMessage
	Class                           definitions.Class // never empty: unclassified resolves to effectful
	RequiresApproval                bool              // Class != read_only or the overlay requires approval
}

// Resolution is the outcome of resolving one journey against a snapshot.
type Resolution struct {
	JourneyID   string
	Tools       []Tool        // sorted by alias; nil when unavailable
	Unavailable []Unavailable // empty means available
}

// Available reports whether the journey can be served.
func (r Resolution) Available() bool { return len(r.Unavailable) == 0 }

type selection struct {
	class    definitions.Class
	approval bool
}

// Resolve matches the journey's reviewed selectors against the snapshot. It is
// pure and deterministic: the result does not depend on the order of
// snapshot.Tools.
func Resolve(journey definitions.Journey, snapshot Snapshot, metaKey string) Resolution {
	res := Resolution{JourneyID: journey.ID}
	if !journey.ProductVersions.IsZero() && !journey.ProductVersions.Contains(snapshot.ProductVersion) {
		res.Unavailable = []Unavailable{{ReasonVersion, ""}}
		return res
	}

	denied := make(map[string]bool, len(journey.Deny))
	for _, key := range journey.Deny {
		denied[key] = true
	}
	// byKey indexes selectable tools (non-empty string key, not denied).
	keys := make([]string, len(snapshot.Tools))
	byKey := map[string][]int{}
	for i, d := range snapshot.Tools {
		if key, ok := d.Meta[metaKey].(string); ok && key != "" && !denied[key] {
			keys[i] = key
			byKey[key] = append(byKey[key], i)
		}
	}

	var unavailable []Unavailable
	seen := map[Unavailable]bool{}
	add := func(reason Reason, operation string) {
		u := Unavailable{reason, operation}
		if !seen[u] {
			seen[u] = true
			unavailable = append(unavailable, u)
		}
	}

	selected := map[int]selection{}
	keyTools := map[string]map[int]bool{}
	choose := func(index int, s definitions.Selector) {
		next := selection{class: s.Class, approval: s.ApprovalRequired}
		if next.class == "" {
			next.class = definitions.ClassEffectful
		}
		if prev, ok := selected[index]; ok {
			next.approval = next.approval || prev.approval
			if classRank(prev.class) > classRank(next.class) {
				next.class = prev.class
			}
		}
		selected[index] = next
		if keyTools[keys[index]] == nil {
			keyTools[keys[index]] = map[int]bool{}
		}
		keyTools[keys[index]][index] = true
	}

	// Operation selectors: the first alternative with a candidate wins.
	for _, s := range journey.Tools {
		if len(s.Ops) == 0 {
			continue
		}
		found := false
		for _, op := range s.Ops {
			var candidates []int
			for _, index := range byKey[op] {
				if !s.ReadOnlyHint || snapshot.Tools[index].ReadOnlyHint {
					candidates = append(candidates, index)
				}
			}
			if len(candidates) == 0 {
				continue
			}
			found = true
			if len(candidates) > 1 {
				add(ReasonAmbiguous, op)
			} else {
				choose(candidates[0], s)
			}
			break
		}
		if !found {
			add(ReasonMissing, s.Ops[0])
		}
	}

	// Tag selectors are optional: they add every matching keyed tool.
	for _, s := range journey.Tools {
		if len(s.Ops) != 0 || len(s.Tags) == 0 {
			continue
		}
		for index, d := range snapshot.Tools {
			if keys[index] != "" && (!s.ReadOnlyHint || d.ReadOnlyHint) && hasAllTags(d.Meta, s.Tags) {
				choose(index, s)
			}
		}
	}

	selectedKeys := make([]string, 0, len(keyTools))
	for key := range keyTools {
		selectedKeys = append(selectedKeys, key)
	}
	sort.Strings(selectedKeys)

	// Two different remote tools for one operation key are ambiguous.
	for _, key := range selectedKeys {
		if len(keyTools[key]) > 1 {
			add(ReasonAmbiguous, key)
		}
	}

	// Aliases must be valid, unreserved and unique within the journey.
	aliasOwner := map[string]string{}
	for _, key := range selectedKeys {
		alias, valid := definitions.AliasFor(key)
		switch {
		case !valid:
			add(ReasonInvalidAlias, key)
		case definitions.IsReservedToolName(alias):
			add(ReasonCollision, alias)
		default:
			if owner, ok := aliasOwner[alias]; ok && owner != key {
				add(ReasonCollision, alias)
			}
			aliasOwner[alias] = key
		}
	}

	maxTools := journey.MaxTools
	if maxTools <= 0 {
		maxTools = defaultMaxTools
	}
	if len(selected) > maxTools {
		add(ReasonMaxTools, "")
	}

	if len(unavailable) > 0 {
		res.Unavailable = unavailable
		return res
	}
	tools := make([]Tool, 0, len(selected))
	for index, sel := range selected {
		d := snapshot.Tools[index]
		alias, _ := definitions.AliasFor(keys[index])
		tools = append(tools, Tool{
			Alias: alias, OperationKey: keys[index], RemoteName: d.Name,
			Description: d.Description, InputSchema: d.InputSchema,
			Class:            sel.class,
			RequiresApproval: sel.class != definitions.ClassReadOnly || sel.approval,
		})
	}
	sort.Slice(tools, func(a, b int) bool { return tools[a].Alias < tools[b].Alias })
	if len(tools) > 0 {
		res.Tools = tools
	}
	return res
}

// classRank orders classes from least to most restrictive; unknown values
// rank as effectful.
func classRank(c definitions.Class) int {
	switch c {
	case definitions.ClassReadOnly:
		return 0
	case definitions.ClassIdempotent:
		return 1
	}
	return 2
}

func hasAllTags(meta map[string]any, want []string) bool {
	list, _ := meta["tags"].([]any)
	have := make(map[string]bool, len(list))
	for _, v := range list {
		if tag, ok := v.(string); ok {
			have[tag] = true
		}
	}
	for _, tag := range want {
		if !have[tag] {
			return false
		}
	}
	return true
}
