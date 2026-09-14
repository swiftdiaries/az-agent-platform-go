package runtime

import (
	"context"
	"errors"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	"time"
)

// Two dispatches share a deadline, exact intent, and platform call ID. No provider
// retries are performed here: Model.Complete is a complete, buffered turn boundary.
func guardedCall(ctx context.Context, in RunInput, bound *platformmcp.Bound, op journal.Operation, approval string) (any, error) {
	ownerContext := ctx
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for attempt := 1; attempt <= 2; attempt++ {
		if err := in.Store.BeginAttempt(ctx, in.Owner, op, attempt, approval); err != nil {
			return nil, err
		}
		result, err := bound.Call(ctx, op.CallID, op.Name, []byte(op.Arguments))
		outcome := "completed"
		switch {
		case errors.Is(err, platformmcp.ErrAuthRequired):
			outcome = "auth_required"
		case errors.Is(err, platformmcp.ErrBusinessRejected):
			outcome = "rejected"
		case err != nil:
			outcome = "outcome_unknown"
		}
		if saveErr := in.Store.EndAttempt(ownerContext, in.Owner, op, attempt, outcome); saveErr != nil {
			return nil, saveErr
		}
		if err == nil {
			return result, nil
		}
		if outcome == "auth_required" {
			return nil, err
		}
		// A business rejection is definite; repeat unchanged once before offering one
		// argument repair. Transport uncertainty requires reviewed replay safety.
		safe := outcome == "rejected" || op.Policy == "read_only" || op.Policy == "deduplicated"
		if !safe || attempt == 2 {
			if outcome == "outcome_unknown" {
				return nil, journal.ErrOutcomeUnknown
			}
			return map[string]any{"error": "business_rejected"}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return nil, journal.ErrOutcomeUnknown
}
