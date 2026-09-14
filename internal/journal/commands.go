package journal

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Admit commits an immutable receipt and maps input to the live owner inbox,
// or schedules a new pending run if finish already committed.
func (s *Store) Admit(ctx context.Context, c Command) (Receipt, bool, error) {
	receipt := Receipt{ThreadID: c.ThreadID, RunID: c.RunID, CommunicationID: c.CommunicationID, State: RunPending}
	fresh := false
	if c.ThreadID == "" || c.RunID == "" || c.CommunicationID == "" || c.Principal == "" || c.Text == "" {
		return Receipt{}, false, ErrState
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return Receipt{}, false, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO agent_conversations(id,principal) VALUES($1,$2) ON CONFLICT DO NOTHING", c.ThreadID, c.Principal); err != nil {
			return err
		}
		if err := lockConversation(ctx, tx, c.ThreadID, c.Principal); err != nil {
			return err
		}
		var previous []byte
		err := tx.QueryRow(ctx, "SELECT payload,execution_run_id FROM agent_commands WHERE thread_id=$1 AND communication_id=$2", c.ThreadID, c.CommunicationID).Scan(&previous, &receipt.ExecutionRunID)
		if err == nil {
			var original Command
			if err = json.Unmarshal(previous, &original); err != nil {
				return err
			}
			if original != c {
				return ErrConflict
			}
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		if err := interruptExpired(ctx, tx, c.ThreadID); err != nil {
			return err
		}
		var execution, journey, originalTarget string
		err = tx.QueryRow(ctx, "SELECT r.id,COALESCE(r.journey_id,''),c.payload->>'TargetJourney' FROM agent_runs r JOIN agent_commands c ON c.run_id=r.id WHERE r.thread_id=$1 AND r.state IN ('pending','running')", c.ThreadID).Scan(&execution, &journey, &originalTarget)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		fresh = err == pgx.ErrNoRows
		if fresh {
			execution = c.RunID
		} else if c.TargetJourney != "" && c.TargetJourney != journey && c.TargetJourney != originalTarget {
			return ErrBusy
		}
		receipt.ExecutionRunID = execution
		if _, err = tx.Exec(ctx, "INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES($1,$2,$3,$4,$5)", c.ThreadID, c.CommunicationID, c.RunID, payload, execution); err != nil {
			return err
		}
		if fresh {
			if _, err = tx.Exec(ctx, "INSERT INTO agent_runs(id,thread_id,state) VALUES($1,$2,'pending')", c.RunID, c.ThreadID); err != nil {
				return err
			}
		}
		if err = appendEvent(ctx, tx, c.ThreadID, Event{RunID: execution, Type: "command.accepted", CommunicationID: c.CommunicationID}); err != nil {
			return err
		}
		return nil
	})
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		err = ErrConflict
	}
	return receipt, fresh, err
}
