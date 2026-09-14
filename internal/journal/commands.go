package journal

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Admit commits an immutable receipt before execution. Busy conversations reject new input in
// this development slice; Task 3 adds the owner inbox and admission/finish steering protocol.
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
		err := tx.QueryRow(ctx, "SELECT payload FROM agent_commands WHERE thread_id=$1 AND communication_id=$2", c.ThreadID, c.CommunicationID).Scan(&previous)
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
		var busy bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM agent_runs WHERE thread_id=$1 AND state IN ('pending','running'))", c.ThreadID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return ErrBusy
		}
		if _, err = tx.Exec(ctx, "INSERT INTO agent_commands VALUES($1,$2,$3,$4)", c.ThreadID, c.CommunicationID, c.RunID, payload); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO agent_runs(id,thread_id,state) VALUES($1,$2,'pending')", c.RunID, c.ThreadID); err != nil {
			return err
		}
		if err = appendEvent(ctx, tx, c.ThreadID, Event{RunID: c.RunID, Type: "command.accepted"}); err != nil {
			return err
		}
		fresh = true
		return nil
	})
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		err = ErrConflict
	}
	return receipt, fresh, err
}
