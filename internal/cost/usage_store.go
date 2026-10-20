package cost

import (
	"context"
	"time"

	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
)

// Scope selects usage rows. Tenant is required; the others are filters when set.
type Scope struct{ Tenant, Principal, Conversation, Journey string }

// Window is the half-open interval [From, To); a zero bound is unbounded.
type Window struct{ From, To time.Time }

type ModelUsage struct {
	Model                        string
	Calls, Input, Output, Cached int64
}

// Report is the usage in a scope and window, per model and in total.
type Report struct {
	Scope  Scope
	Window Window
	Total  ModelUsage   // Model ""
	Models []ModelUsage // ordered by model
}

// Usage reads the journal's usage rows into a Report.
func Usage(ctx context.Context, store *journal.Store, scope Scope, window Window) (Report, error) {
	totals, err := store.UsageTotals(ctx, journal.UsageQuery{
		Tenant: scope.Tenant, Principal: scope.Principal, Conversation: scope.Conversation, Journey: scope.Journey,
		From: window.From, To: window.To,
	})
	if err != nil {
		return Report{}, err
	}
	r := Report{Scope: scope, Window: window, Models: make([]ModelUsage, 0, len(totals))}
	for _, t := range totals {
		m := ModelUsage(t)
		r.Models = append(r.Models, m)
		r.Total.Calls += m.Calls
		r.Total.Input += m.Input
		r.Total.Output += m.Output
		r.Total.Cached += m.Cached
	}
	return r, nil
}
