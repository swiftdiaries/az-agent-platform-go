package journal

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
)

var ErrOutcomeUnknown = errors.New("tool outcome unknown")

type Operation struct{ CallID, Name, Arguments, Binding, Policy string }

// BeginAttempt commits the exact intent before dispatch; an unclosed intent is
// uncertain even if the process died in the gap before reaching the transport.
func (s *Store) BeginAttempt(ctx context.Context, o Owner, op Operation, attempt int, approval string) error {
	binding, err := ActionBinding(op.Name, []byte(op.Arguments))
	if err != nil || binding != op.Binding || op.CallID == "" || attempt < 1 || attempt > 2 || (op.Policy != "read_only" && op.Policy != "effectful" && op.Policy != "deduplicated") {
		return ErrState
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		c := o.Command
		var unknown bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM agent_operations WHERE thread_id=$1 AND outcome IN ('dispatching','outcome_unknown') AND call_id<>$2)", c.ThreadID, op.CallID).Scan(&unknown); err != nil {
			return err
		}
		if unknown {
			return ErrOutcomeUnknown
		}
		if attempt == 1 {
			if op.Policy != "read_only" {
				var artifact, reply []byte
				err := tx.QueryRow(ctx, "SELECT artifact,reply FROM agent_interactions WHERE id=$1 AND thread_id=$2 AND continuation_run_id=$3 AND kind='approval' AND state='consumed' AND expires_at>clock_timestamp()", approval, c.ThreadID, c.RunID).Scan(&artifact, &reply)
				if err != nil {
					return ErrState
				}
				var i Interaction
				var answer Reply
				if json.Unmarshal(artifact, &i) != nil || json.Unmarshal(reply, &answer) != nil || answer.Decision != "approve" || i.Call.CallID != op.CallID || i.Call.Binding != op.Binding || i.Call.Name != op.Name {
					return ErrState
				}
			}
			if _, err := tx.Exec(ctx, "INSERT INTO agent_operations(call_id,thread_id,run_id,name,arguments,binding,policy,outcome) VALUES($1,$2,$3,$4,$5,$6,$7,'dispatching')", op.CallID, c.ThreadID, c.RunID, op.Name, op.Arguments, op.Binding, op.Policy); err != nil {
				return err
			}
		} else {
			var previous, policy, binding string
			if err := tx.QueryRow(ctx, "SELECT outcome,policy,binding FROM agent_operations WHERE call_id=$1 AND thread_id=$2 AND run_id=$3", op.CallID, c.ThreadID, c.RunID).Scan(&previous, &policy, &binding); err != nil {
				return err
			}
			if binding != op.Binding || policy != op.Policy || previous != "rejected" && (previous != "outcome_unknown" || policy == "effectful") {
				return ErrState
			}
			if _, err := tx.Exec(ctx, "UPDATE agent_operations SET outcome='dispatching' WHERE call_id=$1", op.CallID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "INSERT INTO agent_attempts(call_id,attempt,outcome) VALUES($1,$2,'dispatching')", op.CallID, attempt); err != nil {
			return err
		}
		return appendEvent(ctx, tx, c.ThreadID, Event{RunID: c.RunID, Type: "tool.started", CallID: op.CallID, ToolName: op.Name})
	})
}
func (s *Store) EndAttempt(ctx context.Context, o Owner, op Operation, attempt int, outcome string) error {
	switch outcome {
	case "completed", "rejected", "auth_required", "outcome_unknown":
	default:
		return ErrState
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, "UPDATE agent_operations SET outcome=$4 WHERE call_id=$1 AND thread_id=$2 AND run_id=$3 AND outcome='dispatching'", op.CallID, o.Command.ThreadID, o.Command.RunID, outcome)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrState
		}
		result, err = tx.Exec(ctx, "UPDATE agent_attempts SET outcome=$3 WHERE call_id=$1 AND attempt=$2 AND outcome='dispatching'", op.CallID, attempt, outcome)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrState
		}
		return appendEvent(ctx, tx, o.Command.ThreadID, Event{RunID: o.Command.RunID, Type: "tool." + outcome, CallID: op.CallID, ToolName: op.Name})
	})
}

func markOperationsUnknown(ctx context.Context, tx pgx.Tx, thread, run string) error {
	if _, err := tx.Exec(ctx, "UPDATE agent_attempts a SET outcome='outcome_unknown' FROM agent_operations o WHERE a.call_id=o.call_id AND o.thread_id=$1 AND o.run_id=$2 AND a.outcome='dispatching'", thread, run); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, "UPDATE agent_operations SET outcome='outcome_unknown' WHERE thread_id=$1 AND run_id=$2 AND outcome='dispatching' RETURNING call_id,name", thread, run)
	if err != nil {
		return err
	}
	var operations []Operation
	for rows.Next() {
		var op Operation
		if err := rows.Scan(&op.CallID, &op.Name); err != nil {
			rows.Close()
			return err
		}
		operations = append(operations, op)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, op := range operations {
		if err := appendEvent(ctx, tx, thread, Event{RunID: run, Type: "tool.outcome_unknown", CallID: op.CallID, ToolName: op.Name}); err != nil {
			return err
		}
	}
	return nil
}
