package journal

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

const LeaseDuration = 15 * time.Second

type Owner struct {
	Command Command
	ID      string
	Epoch   int64
}

// Claim never searches for work. Only the ingress holding fresh credentials may
// claim its explicitly admitted pending run; a running run cannot be transferred.
func (s *Store) Claim(ctx context.Context, c Command, id string, duration time.Duration) (Owner, error) {
	o := Owner{Command: c, ID: id}
	if id == "" || duration <= 0 {
		return o, ErrOwnership
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockConversation(ctx, tx, c.ThreadID, c.Principal); err != nil {
			if err == ErrForbidden {
				return ErrOwnership
			}
			return err
		}
		var payload []byte
		if err := tx.QueryRow(ctx, "SELECT payload FROM agent_commands WHERE thread_id=$1 AND run_id=$2 AND execution_run_id=$2", c.ThreadID, c.RunID).Scan(&payload); err != nil {
			if err == pgx.ErrNoRows {
				return ErrOwnership
			}
			return err
		}
		var accepted Command
		if err := json.Unmarshal(payload, &accepted); err != nil {
			return err
		}
		if accepted != c {
			return ErrOwnership
		}
		var state RunState
		var epoch int64
		var valid bool
		if err := tx.QueryRow(ctx, "SELECT state,owner_epoch,lease_until>clock_timestamp() FROM agent_runs WHERE thread_id=$1 AND id=$2 FOR UPDATE", c.ThreadID, c.RunID).Scan(&state, &epoch, &valid); err != nil {
			if err == pgx.ErrNoRows {
				return ErrOwnership
			}
			return err
		}
		// Re-evaluate after the row lock; SELECT expressions can run before waiting.
		if err := tx.QueryRow(ctx, "SELECT lease_until>clock_timestamp() FROM agent_runs WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID).Scan(&valid); err != nil {
			return err
		}
		recoverable := false
		if state == RunPending && epoch == 0 && !valid && c.InteractionID != "" {
			if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM agent_interactions WHERE thread_id=$1 AND id=$2 AND continuation_run_id=$3 AND state='consumed')", c.ThreadID, c.InteractionID, c.RunID).Scan(&recoverable); err != nil {
				return err
			}
		}
		if state != RunPending || !valid && !recoverable {
			return ErrOwnership
		}
		if err := tx.QueryRow(ctx, "UPDATE agent_conversations SET owner_epoch=owner_epoch+1 WHERE id=$1 RETURNING owner_epoch", c.ThreadID).Scan(&o.Epoch); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE agent_runs SET state='running',owner_id=$3,owner_epoch=$4,lease_until=clock_timestamp()+$5*interval '1 millisecond' WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID, id, o.Epoch, duration.Milliseconds()); err != nil {
			return err
		}
		return appendEvent(ctx, tx, c.ThreadID, Event{RunID: c.RunID, Type: "run.started"})
	})
	return o, err
}

// All owned operations lock conversation then run, and only then read the live
// database clock. A transaction-start timestamp could resurrect an expired owner.
func lockOwner(ctx context.Context, tx pgx.Tx, o Owner) error {
	c := o.Command
	if err := lockConversation(ctx, tx, c.ThreadID, c.Principal); err != nil {
		return err
	}
	var id string
	if err := tx.QueryRow(ctx, "SELECT id FROM agent_runs WHERE thread_id=$1 AND id=$2 FOR UPDATE", c.ThreadID, c.RunID).Scan(&id); err != nil {
		if err == pgx.ErrNoRows {
			return ErrOwnership
		}
		return err
	}
	var valid bool
	if err := tx.QueryRow(ctx, "SELECT state='running' AND owner_id=$3 AND owner_epoch=$4 AND lease_until>clock_timestamp() FROM agent_runs WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID, o.ID, o.Epoch).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrOwnership
	}
	return nil
}

func (s *Store) Check(ctx context.Context, o Owner) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return lockOwner(ctx, tx, o) })
}
func (s *Store) Renew(ctx context.Context, o Owner, duration time.Duration) error {
	if duration <= 0 {
		return ErrOwnership
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE agent_runs SET lease_until=clock_timestamp()+$3*interval '1 millisecond' WHERE thread_id=$1 AND id=$2", o.Command.ThreadID, o.Command.RunID, duration.Milliseconds())
		return err
	})
}

// Reap records interruption only. It cannot start a worker or restore credentials.
func (s *Store) Reap(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, "SELECT thread_id,id FROM agent_runs WHERE state IN ('pending','running') AND lease_until<=clock_timestamp() UNION SELECT thread_id,run_id FROM agent_interactions WHERE state='pending' AND expires_at<=clock_timestamp()")
	if err != nil {
		return err
	}
	type expired struct{ thread, run string }
	var runs []expired
	for rows.Next() {
		var r expired
		if err := rows.Scan(&r.thread, &r.run); err != nil {
			rows.Close()
			return err
		}
		runs = append(runs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range runs {
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			var id string
			if err := tx.QueryRow(ctx, "SELECT id FROM agent_conversations WHERE id=$1 FOR UPDATE", r.thread).Scan(&id); err != nil {
				return err
			}
			if err := interruptExpired(ctx, tx, r.thread, r.run); err != nil {
				return err
			}
			return expireInteractions(ctx, tx, r.thread)

		})
		if err != nil {
			return err
		}
	}
	return nil
}

// The caller holds the conversation lock. Lock the candidate run before
// deciding from its current identity, owner epoch, continuation, and time.
func interruptExpired(ctx context.Context, tx pgx.Tx, thread, candidate string) error {
	var run string
	var state RunState
	var epoch int64
	err := tx.QueryRow(ctx, "SELECT id,state,owner_epoch FROM agent_runs WHERE thread_id=$1 AND state IN ('pending','running') AND ($2='' OR id=$2) FOR UPDATE", thread, candidate).Scan(&run, &state, &epoch)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var expired, continuation bool
	if err := tx.QueryRow(ctx, "SELECT lease_until<=clock_timestamp(),EXISTS(SELECT 1 FROM agent_interactions WHERE thread_id=$1 AND continuation_run_id=$2 AND state='consumed') FROM agent_runs WHERE thread_id=$1 AND id=$2", thread, run).Scan(&expired, &continuation); err != nil {
		return err
	}
	if !expired || state == RunPending && epoch == 0 && continuation {
		return nil
	}
	result, err := tx.Exec(ctx, "UPDATE agent_runs SET state='interrupted' WHERE thread_id=$1 AND id=$2 AND state IN ('pending','running')", thread, run)
	if err != nil || result.RowsAffected() != 1 {
		return err
	}
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
	if err := disposePending(ctx, tx, thread, run, RunInterrupted); err != nil {
		return err
	}
	return appendEvent(ctx, tx, thread, Event{RunID: run, Type: "run.interrupted"})
}
