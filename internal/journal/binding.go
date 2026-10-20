package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

const (
	bindingDomain    = "az-agent-platform/approval-binding/v2\x00"
	nameDigestDomain = "az-agent-platform/remote-name/v1\x00"
	schemaDomain     = "az-agent-platform/input-schema/v1\x00"
)

// CallBinding is everything an approval is bound to; changing any field changes the digest.
type CallBinding struct {
	JourneyID      string // owning journey; "" only for a clarification
	OperationKey   string
	NameDigest     string
	SchemaDigest   string
	Arguments      json.RawMessage // a JSON object: strict, json.Number preserved, trailing data rejected
	ProductVersion string
	Principal      string
	Conversation   string
	DefinitionSet  string // pinned definition-set digest (64 chars)
}

// canonicalBinding fixes field order; json.Marshal quotes every string, so field boundaries are unambiguous.
type canonicalBinding struct {
	JourneyID      string         `json:"journey_id"`
	OperationKey   string         `json:"operation_key"`
	NameDigest     string         `json:"name_digest"`
	SchemaDigest   string         `json:"schema_digest"`
	Arguments      map[string]any `json:"arguments"`
	ProductVersion string         `json:"product_version"`
	Principal      string         `json:"principal"`
	Conversation   string         `json:"conversation"`
	DefinitionSet  string         `json:"definition_set"`
}

// BindCall returns the hex sha256 of the domain-tagged canonical encoding of b.
func BindCall(b CallBinding) (string, error) {
	if b.OperationKey == "" || b.NameDigest == "" || b.SchemaDigest == "" || b.ProductVersion == "" ||
		b.Principal == "" || b.Conversation == "" || len(b.DefinitionSet) != 64 {
		return "", ErrState
	}
	var args map[string]any
	if err := DecodeStrict(b.Arguments, &args); err != nil || args == nil {
		return "", ErrState
	}
	data, err := json.Marshal(canonicalBinding{
		JourneyID: b.JourneyID, OperationKey: b.OperationKey, NameDigest: b.NameDigest,
		SchemaDigest: b.SchemaDigest, Arguments: args, ProductVersion: b.ProductVersion,
		Principal: b.Principal, Conversation: b.Conversation, DefinitionSet: b.DefinitionSet,
	})
	if err != nil {
		return "", ErrState
	}
	return domainDigest(bindingDomain, data), nil
}

// NameDigest is the hex sha256 of a remote tool name under its own domain tag.
func NameDigest(name string) string {
	return domainDigest(nameDigestDomain, []byte(name))
}

// SchemaDigest hashes the canonical form of a JSON schema; empty input is treated as null.
func SchemaDigest(schema json.RawMessage) (string, error) {
	if len(schema) == 0 {
		schema = json.RawMessage("null")
	}
	var value any
	if err := DecodeStrict(schema, &value); err != nil {
		return "", ErrState
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", ErrState
	}
	return domainDigest(schemaDomain, data), nil
}

func domainDigest(domain string, data []byte) string {
	sum := sha256.Sum256(append([]byte(domain), data...))
	return hex.EncodeToString(sum[:])
}
