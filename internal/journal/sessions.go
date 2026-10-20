package journal

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

func (s *Store) Start(ctx context.Context, o Owner, journey, digest string) (json.RawMessage, error) {
	c := o.Command
	var history json.RawMessage
	if len(digest) != 64 {
		return nil, ErrDefinition
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// ponytail: one definition-rollout lock; split per journey only if startup contention matters.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(789134628)"); err != nil {
			return err
		}
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		var unresolved bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM agent_operations WHERE thread_id=$1 AND outcome IN ('dispatching','outcome_unknown'))", c.ThreadID).Scan(&unresolved); err != nil {
			return err
		}
		if unresolved {
			return ErrOutcomeUnknown
		}
		var pin *string
		if err := tx.QueryRow(ctx, "SELECT definition_digest FROM agent_conversations WHERE id=$1", c.ThreadID).Scan(&pin); err != nil {
			return err
		}
		if pin == nil {
			// An unpinned conversation (new, or migrated) pins to current; current is
			// bootstrapped to digest when absent.
			var current string
			err := tx.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current").Scan(&current)
			if err == pgx.ErrNoRows {
				if _, err = tx.Exec(ctx, "INSERT INTO agent_definition_current(definition_digest) VALUES($1)", digest); err != nil {
					return err
				}
				current = digest
			} else if err != nil {
				return err
			}
			if current != digest {
				return ErrDefinition
			}
			if _, err = tx.Exec(ctx, "UPDATE agent_conversations SET definition_digest=$2 WHERE id=$1", c.ThreadID, digest); err != nil {
				return err
			}
		} else if *pin != digest {
			return ErrDefinition
		}
		if err := tx.QueryRow(ctx, "SELECT history FROM agent_conversations WHERE id=$1", c.ThreadID).Scan(&history); err != nil {
			return err
		}
		// A reply continuation run copies the interaction's journey and digest; the
		// digest of a pre-000009 interaction is an old per-journey digest, so overwrite it.
		// An empty journey (no explicit target) pins the definition and leaves journey_id as is.
		result, err := tx.Exec(ctx, "UPDATE agent_runs SET journey_id=COALESCE(NULLIF($3,''),journey_id),definition_digest=$4 WHERE thread_id=$1 AND id=$2 AND ($3='' OR journey_id IS NULL OR journey_id=$3)", c.ThreadID, c.RunID, journey, digest)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrDefinition
		}
		return nil
	})
	return history, err
}

// Finish atomically stores provider history and terminal lifecycle events. External
// calls never occur in this transaction. Admission shares this conversation lock.
func (s *Store) Finish(ctx context.Context, o Owner, state RunState, history json.RawMessage, answer, callID, tool string) error {
	if state != RunCompleted && state != RunFailed && state != RunAuthRequired && state != RunDependencyUnavailable {
		return ErrState
	}
	return s.finish(ctx, o, state, history, answer, callID, tool, nil)
}

// FinishFailed finishes a run as failed with a typed client-caused reason
// (journey_unavailable, policy_conflict or activation_refused) recorded on the
// terminal run.failed event. Everything else matches Finish(RunFailed):
// operations are marked unknown and pending commands are disposed failed.
func (s *Store) FinishFailed(ctx context.Context, o Owner, history json.RawMessage, reason string) error {
	switch reason {
	case ReasonJourneyUnavailable, ReasonPolicyConflict, ReasonActivationRefused:
	default:
		return ErrState
	}
	return s.finish(ctx, o, RunFailed, history, "", "", "", &Event{Type: "run.failed", Reason: reason})
}

// finish is the shared terminal transaction; terminal, when set, replaces the
// run's default terminal event.
func (s *Store) finish(ctx context.Context, o Owner, state RunState, history json.RawMessage, answer, callID, tool string, terminal *Event) error {
	c := o.Command
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		var pinned bool
		if err := tx.QueryRow(ctx, "SELECT definition_digest IS NOT NULL FROM agent_runs WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID).Scan(&pinned); err != nil {
			return err
		}
		if state == RunCompleted {
			var pending bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM agent_commands WHERE thread_id=$1 AND execution_run_id=$2 AND NOT included AND terminal_reason='')", c.ThreadID, c.RunID).Scan(&pending); err != nil {
				return err
			}
			if pending {
				return ErrPending
			}
		}
		if state != RunCompleted {
			if err := markOperationsUnknown(ctx, tx, c.ThreadID, c.RunID); err != nil {
				return err
			}
			if err := disposePending(ctx, tx, c.ThreadID, c.RunID, state); err != nil {
				return err
			}
		}
		if len(history) > 0 && pinned {
			if _, err := tx.Exec(ctx, "UPDATE agent_conversations SET history=$2 WHERE id=$1", c.ThreadID, history); err != nil {
				return err
			}
		}
		if state == RunCompleted {
			if callID != "" {
				if err := appendEvent(ctx, tx, c.ThreadID, Event{RunID: c.RunID, Type: "tool.completed", CallID: callID, ToolName: tool}); err != nil {
					return err
				}
			}
			if err := appendEvent(ctx, tx, c.ThreadID, Event{RunID: c.RunID, Type: "message.completed", Answer: answer}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE agent_runs SET state=$3,answer=$4 WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID, state, answer); err != nil {
			return err
		}
		kind := "run." + string(state)
		if state == RunAuthRequired {
			kind = "auth_required"
		}
		e := Event{RunID: c.RunID, Type: kind}
		if terminal != nil {
			e = *terminal
			e.RunID = c.RunID
		}
		return appendEvent(ctx, tx, c.ThreadID, e)
	})
}
