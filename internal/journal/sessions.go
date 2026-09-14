package journal

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

func (s *Store) Start(ctx context.Context, c Command, journey, digest string) (json.RawMessage, error) {
	var history json.RawMessage
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockConversation(ctx, tx, c.ThreadID, c.Principal); err != nil {
			return err
		}
		var state RunState
		if err := tx.QueryRow(ctx, "SELECT state FROM agent_runs WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID).Scan(&state); err != nil {
			return err
		}
		if state != RunPending {
			return ErrState
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
		if _, err := tx.Exec(ctx, "UPDATE agent_runs SET state='running',journey_id=$3,definition_digest=$4 WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID, journey, digest); err != nil {
			return err
		}
		return appendEvent(ctx, tx, c.ThreadID, Event{RunID: c.RunID, Type: "run.started"})
	})
	return history, err
}

// Finish atomically stores provider history and terminal lifecycle events. External
// calls never occur in this transaction. Task 3 adds epoch and lease fencing.
func (s *Store) Finish(ctx context.Context, c Command, state RunState, history json.RawMessage, answer, callID, tool string) error {
	if state != RunCompleted && state != RunFailed && state != RunAuthRequired {
		return ErrState
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockConversation(ctx, tx, c.ThreadID, c.Principal); err != nil {
			return err
		}
		var current RunState
		var journey string
		if err := tx.QueryRow(ctx, "SELECT state,COALESCE(journey_id,'') FROM agent_runs WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID).Scan(&current, &journey); err != nil {
			return err
		}
		if current != RunRunning && !(current == RunPending && state != RunCompleted) {
			return ErrState
		}
		if state == RunCompleted {
			if _, err := tx.Exec(ctx, "UPDATE agent_sessions SET history=$3 WHERE thread_id=$1 AND journey_id=$2", c.ThreadID, journey, history); err != nil {
				return err
			}
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
