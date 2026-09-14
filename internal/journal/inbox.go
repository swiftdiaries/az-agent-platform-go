package journal

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type Input struct {
	CommunicationID, Text, Digest string
	Payload                       json.RawMessage
	Materialized                  bool
}
type Inbox struct {
	Commands      []Input
	Context       []string
	ContextInputs []Input
}

// Pending materializes input, but does not claim provider inclusion.
func (s *Store) Pending(ctx context.Context, o Owner) (Inbox, error) {
	var inbox Inbox
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT communication_id,payload FROM agent_commands WHERE thread_id=$1 AND execution_run_id=$2 AND NOT included AND terminal_reason='' ORDER BY ordinal", o.Command.ThreadID, o.Command.RunID)
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
			if c.Text != "" {
				in.Payload, err = json.Marshal(c.Text)
				in.Materialized = true
			} else if c.ReplyKind == "clarification" {
				var reply Reply
				if err = DecodeStrict([]byte(c.ReplyJSON), &reply); err == nil {
					in.Payload, err = json.Marshal(reply)
				}
				in.Materialized = err == nil
			} else if c.ReplyKind == "approval" {
				var reply Reply
				if err = DecodeStrict([]byte(c.ReplyJSON), &reply); err == nil && reply.Decision == "deny" {
					in.Payload, err = json.Marshal(map[string]string{"error": "approval_denied"})
					in.Materialized = err == nil
				}
			}
			if err != nil {
				rows.Close()
				return err
			}
			if in.Materialized {
				in.Digest = fmt.Sprintf("%x", sha256.Sum256(in.Payload))
			}
			inbox.Commands = append(inbox.Commands, in)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// Selected conversation context: every provider-observed input from the
		// latest completed turn outside this journey, followed by its final answer.
		var contextRun, answer string
		err = tx.QueryRow(ctx, `SELECT r.id,r.answer FROM agent_runs r JOIN agent_events e ON e.thread_id=r.thread_id AND e.run_id=r.id AND e.kind='run.completed' WHERE r.thread_id=$1 AND r.state='completed' AND r.journey_id IS DISTINCT FROM (SELECT journey_id FROM agent_runs WHERE id=$2) ORDER BY e.sequence DESC LIMIT 1`, o.Command.ThreadID, o.Command.RunID).Scan(&contextRun, &answer)
		if err == pgx.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, "SELECT communication_id,payload FROM agent_commands WHERE thread_id=$1 AND execution_run_id=$2 AND included ORDER BY ordinal", o.Command.ThreadID, contextRun)
		if err != nil {
			return err
		}
		for rows.Next() {
			var input Input
			var payload []byte
			if err := rows.Scan(&input.CommunicationID, &payload); err != nil {
				rows.Close()
				return err
			}
			var command Command
			if err := json.Unmarshal(payload, &command); err != nil {
				rows.Close()
				return err
			}
			if command.Text == "" {
				continue
			}
			input.Text = command.Text
			input.Payload, err = json.Marshal(command.Text)
			if err != nil {
				rows.Close()
				return err
			}
			input.Materialized = true
			input.Digest = fmt.Sprintf("%x", sha256.Sum256(input.Payload))
			inbox.Context = append(inbox.Context, input.Text)
			inbox.ContextInputs = append(inbox.ContextInputs, input)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		inbox.Context = append(inbox.Context, answer)
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
			result, err := tx.Exec(ctx, "UPDATE agent_commands SET included=true WHERE thread_id=$1 AND execution_run_id=$2 AND communication_id=$3 AND NOT included AND terminal_reason=''", o.Command.ThreadID, o.Command.RunID, c.CommunicationID)
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

// The caller holds the conversation/run locks. Terminal runs cannot drain again:
// dispose their unincluded commands in the same transaction as the run outcome.
func disposePending(ctx context.Context, tx pgx.Tx, thread, run string, reason RunState) error {
	rows, err := tx.Query(ctx, `WITH disposed AS (
 UPDATE agent_commands SET terminal_reason=$3
 WHERE thread_id=$1 AND execution_run_id=$2 AND NOT included AND terminal_reason=''
 RETURNING communication_id,ordinal)
 SELECT communication_id FROM disposed ORDER BY ordinal`, thread, run, reason)
	if err != nil {
		return err
	}
	var commands []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		commands = append(commands, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	kind := "command.rejected"
	if reason == RunInterrupted {
		kind = "command.interrupted"
	}
	for _, id := range commands {
		if err := appendEvent(ctx, tx, thread, Event{RunID: run, CommunicationID: id, Type: kind, Reason: string(reason)}); err != nil {
			return err
		}
	}
	return nil
}
