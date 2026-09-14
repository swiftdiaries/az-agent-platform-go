package journal

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

type Input struct{ CommunicationID, Text string }
type Inbox struct {
	Commands []Input
	Context  []string
}

// Pending materializes input, but does not claim provider inclusion.
func (s *Store) Pending(ctx context.Context, o Owner) (Inbox, error) {
	var inbox Inbox
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT communication_id,payload FROM agent_commands WHERE thread_id=$1 AND execution_run_id=$2 AND NOT included ORDER BY ordinal", o.Command.ThreadID, o.Command.RunID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var in Input
			var payload []byte
			if err := rows.Scan(&in.CommunicationID, &payload); err != nil {
				rows.Close()
				return err
			}
			var c Command
			if err := json.Unmarshal(payload, &c); err != nil {
				rows.Close()
				return err
			}
			in.Text = c.Text
			inbox.Commands = append(inbox.Commands, in)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// Selected conversation context: the latest completed turn outside this journey.
		var text, answer string
		err = tx.QueryRow(ctx, `SELECT c.payload->>'Text',r.answer FROM agent_runs r JOIN agent_commands c ON c.run_id=r.id WHERE r.thread_id=$1 AND r.state='completed' AND r.journey_id IS DISTINCT FROM (SELECT journey_id FROM agent_runs WHERE id=$2) ORDER BY c.ordinal DESC LIMIT 1`, o.Command.ThreadID, o.Command.RunID).Scan(&text, &answer)
		if err == nil {
			inbox.Context = []string{text, answer}
		} else if err != pgx.ErrNoRows {
			return err
		}
		return nil
	})
	return inbox, err
}

// Included is called only by the actual provider adapter after observing Complete.
// A failed transaction leaves every command pending and no inclusion event.
func (s *Store) Included(ctx context.Context, o Owner, commands []Input) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		for _, c := range commands {
			result, err := tx.Exec(ctx, "UPDATE agent_commands SET included=true WHERE thread_id=$1 AND execution_run_id=$2 AND communication_id=$3 AND NOT included", o.Command.ThreadID, o.Command.RunID, c.CommunicationID)
			if err != nil {
				return err
			}
			if result.RowsAffected() != 1 {
				return ErrState
			}
			if err := appendEvent(ctx, tx, o.Command.ThreadID, Event{RunID: o.Command.RunID, CommunicationID: c.CommunicationID, Type: "command.included"}); err != nil {
				return err
			}
		}
		return nil
	})
}
