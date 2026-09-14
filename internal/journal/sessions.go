package journal

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

func (s *Store) Start(ctx context.Context, o Owner, journey, digest string) (json.RawMessage, error) {
	c := o.Command
	var history json.RawMessage
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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
		if _, err := tx.Exec(ctx, "INSERT INTO agent_sessions(thread_id,journey_id,definition_digest) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", c.ThreadID, journey, digest); err != nil {
			return err
		}
		var pinned string
		if err := tx.QueryRow(ctx, "SELECT definition_digest,history FROM agent_sessions WHERE thread_id=$1 AND journey_id=$2", c.ThreadID, journey).Scan(&pinned, &history); err != nil {
			return err
		}
		if pinned != digest {
			return ErrDefinition
		}
		result, err := tx.Exec(ctx, "UPDATE agent_runs SET journey_id=$3,definition_digest=$4 WHERE thread_id=$1 AND id=$2 AND (journey_id IS NULL OR journey_id=$3 AND definition_digest=$4)", c.ThreadID, c.RunID, journey, digest)
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
	c := o.Command
	if state != RunCompleted && state != RunFailed && state != RunAuthRequired {
		return ErrState
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		var journey string
		if err := tx.QueryRow(ctx, "SELECT COALESCE(journey_id,'') FROM agent_runs WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID).Scan(&journey); err != nil {
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
			if _, err := tx.Exec(ctx, "UPDATE agent_attempts a SET outcome='outcome_unknown' FROM agent_operations o WHERE a.call_id=o.call_id AND o.thread_id=$1 AND o.run_id=$2 AND a.outcome='dispatching'", c.ThreadID, c.RunID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE agent_operations SET outcome='outcome_unknown' WHERE thread_id=$1 AND run_id=$2 AND outcome='dispatching'", c.ThreadID, c.RunID); err != nil {
				return err
			}
			if err := disposePending(ctx, tx, c.ThreadID, c.RunID, state); err != nil {
				return err
			}
		}
		if len(history) > 0 {
			if _, err := tx.Exec(ctx, "UPDATE agent_sessions SET history=$3 WHERE thread_id=$1 AND journey_id=$2", c.ThreadID, journey, history); err != nil {
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
		return appendEvent(ctx, tx, c.ThreadID, Event{RunID: c.RunID, Type: kind})
	})
}
