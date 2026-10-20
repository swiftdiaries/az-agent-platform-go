package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Descriptor is one raw MCP tool descriptor as returned by tools/list.
type Descriptor struct {
	Name, Description string
	InputSchema       json.RawMessage
	Meta              map[string]any // _meta: numbers decode as float64, arrays as []any
	ReadOnlyHint      bool           // true only when annotations.readOnlyHint is literally true
	Raw               json.RawMessage
}

type rawDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Meta        map[string]any  `json:"_meta"`
	Annotations *struct {
		ReadOnlyHint any `json:"readOnlyHint"`
	} `json:"annotations"`
}

// ParseDescriptors decodes a JSON array of MCP tool descriptors, keeping each
// element's bytes in Descriptor.Raw. Input that is not an array, and elements
// that are not objects or have no name, are rejected.
func ParseDescriptors(raw json.RawMessage) ([]Descriptor, error) {
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil || elements == nil {
		return nil, errors.New("catalog descriptors must be a JSON array")
	}
	out := make([]Descriptor, 0, len(elements))
	for i, element := range elements {
		var d rawDescriptor
		if err := json.Unmarshal(element, &d); err != nil {
			return nil, fmt.Errorf("catalog descriptor %d is malformed", i)
		}
		if d.Name == "" {
			return nil, fmt.Errorf("catalog descriptor %d has no name", i)
		}
		hint := false
		if d.Annotations != nil {
			hint = d.Annotations.ReadOnlyHint == true
		}
		var schema json.RawMessage
		if len(d.InputSchema) > 0 {
			schema = append(json.RawMessage(nil), d.InputSchema...)
		}
		out = append(out, Descriptor{
			Name: d.Name, Description: d.Description, InputSchema: schema,
			Meta: d.Meta, ReadOnlyHint: hint,
			Raw: append(json.RawMessage(nil), element...),
		})
	}
	return out, nil
}
