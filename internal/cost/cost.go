// Package cost is the cost-enforcement seam. A Gate admits a model call and
// supplies an opaque request decoration (attribution headers) for the
// provider transport; limit rejections are classified into *LimitError.
//
// This is a leaf package: it imports only the standard library and
// internal/settings (plus internal/journal in usage_store.go only).
package cost

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
)

// Attribution holds opaque IDs only: never credentials or content.
type Attribution struct{ Tenant, Principal, Conversation, Run, Journey string }

// LimitKind is the kind of enforcement limit that was hit.
type LimitKind string

const (
	LimitBudget LimitKind = "budget"
	LimitRate   LimitKind = "rate"
)

// ErrLimitExceeded is the sentinel; *LimitError unwraps to it.
var ErrLimitExceeded = errors.New("limit exceeded")

// LimitError is a terminal enforcement rejection. Error() never contains
// header values or bodies.
type LimitError struct {
	Kind       LimitKind
	RetryAfter time.Duration // 0 = not given
}

func (e *LimitError) Error() string {
	s := "limit exceeded: " + string(e.Kind)
	if e.RetryAfter > 0 {
		s += fmt.Sprintf(" (retry after %ds)", int64(e.RetryAfter/time.Second))
	}
	return s
}

// Unwrap returns ErrLimitExceeded.
func (e *LimitError) Unwrap() error { return ErrLimitExceeded }

// Decision is Admit's verdict.
type Decision struct {
	Allow      bool
	Kind       LimitKind // when denied ("" -> LimitBudget)
	RetryAfter time.Duration
}

// Allow returns an allowing decision.
func Allow() Decision { return Decision{Allow: true} }

// Deny returns a denying decision.
func Deny(kind LimitKind, retryAfter time.Duration) Decision {
	return Decision{Kind: kind, RetryAfter: retryAfter}
}

// Err is nil when allowed, else a *LimitError.
func (d Decision) Err() error {
	if d.Allow {
		return nil
	}
	kind := d.Kind
	if kind == "" {
		kind = LimitBudget
	}
	return &LimitError{Kind: kind, RetryAfter: d.RetryAfter}
}

// Gate is the cost-enforcement port.
type Gate interface {
	Admit(ctx context.Context, a Attribution, s settings.Cost) (Decision, error)
	Access(ctx context.Context, a Attribution, s settings.Cost) (Access, error)
}

// Open admits everything and decorates nothing.
type Open struct{}

var _ Gate = Open{}

func (Open) Admit(context.Context, Attribution, settings.Cost) (Decision, error) {
	return Allow(), nil
}

func (Open) Access(context.Context, Attribution, settings.Cost) (Access, error) {
	return Access{}, nil
}

var errUnknownGate = errors.New("cost: unknown gate kind")

// New selects the adapter for AGENT_PLATFORM_COST_GATE: "" or "open" yields
// Open; "headers" yields Headers configured by ParseHeaderNames(headerSpec).
// Errors never echo the inputs.
func New(kind, headerSpec string) (Gate, error) {
	switch kind {
	case "", "open":
		return Open{}, nil
	case "headers":
		names, err := ParseHeaderNames(headerSpec)
		if err != nil {
			return nil, err
		}
		return NewHeaders(names)
	}
	return nil, errUnknownGate
}

// Tokens is one model call's usage, returned by providers in
// runtime.ModelResponse.Usage.
type Tokens struct {
	Model                 string
	Input, Output, Cached int64
}
