package journal

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
)

// Snapshot and watermark share one PostgreSQL repeatable-read MVCC view, even
// when completion commits between the graph and journal reads.
func (s *Store) Snapshot(ctx context.Context, thread, principal string) (Snapshot, error) {
	snapshot := Snapshot{ThreadID: thread, Principal: principal}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT sequence FROM agent_conversations WHERE id=$1 AND principal=$2", thread, principal).Scan(&snapshot.Watermark); err != nil {
			if err == pgx.ErrNoRows {
				return ErrForbidden
			}
			return err
		}
		rows, err := tx.Query(ctx, "SELECT id,state,COALESCE(journey_id,''),answer,ARRAY(SELECT run_id FROM agent_commands c WHERE c.thread_id=r.thread_id AND c.execution_run_id=r.id ORDER BY ordinal),(SELECT count(*) FROM agent_commands c WHERE c.thread_id=r.thread_id AND c.execution_run_id=r.id AND NOT included AND terminal_reason=''),(SELECT jsonb_agg(jsonb_build_object('CommunicationID',c.communication_id,'State',CASE WHEN c.included THEN 'included' WHEN c.terminal_reason='' THEN 'pending' WHEN c.terminal_reason='interrupted' THEN 'interrupted' ELSE 'rejected' END,'Reason',c.terminal_reason) ORDER BY c.ordinal) FROM agent_commands c WHERE c.thread_id=r.thread_id AND c.execution_run_id=r.id) FROM agent_runs r WHERE thread_id=$1", thread)
		if err != nil {
			return err
		}
		for rows.Next() {
			var run Run
			var outcomes []byte
			if err := rows.Scan(&run.RunID, &run.State, &run.JourneyID, &run.Answer, &run.CommandRunIDs, &run.PendingCommands, &outcomes); err != nil {
				rows.Close()
				return err
			}
			if err := json.Unmarshal(outcomes, &run.CommandOutcomes); err != nil {
				rows.Close()
				return err
			}
			snapshot.Runs = append(snapshot.Runs, run)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		snapshot.Events, err = readEvents(ctx, tx, thread, 0)
		if err != nil {
			return err
		}
		if len(snapshot.Events) > 0 {
			latest := snapshot.Events[len(snapshot.Events)-1].RunID
			for _, run := range snapshot.Runs {
				if run.RunID == latest {
					snapshot.RunID = run.RunID
					snapshot.RunState = run.State
					snapshot.JourneyID = run.JourneyID
					snapshot.Answer = run.Answer
				}
			}
		}
		return nil
	})
	return snapshot, err
}
func (s *Store) Events(ctx context.Context, thread, principal string, after int64) ([]Event, error) {
	var found bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM agent_conversations WHERE id=$1 AND principal=$2)", thread, principal).Scan(&found); err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrForbidden
	}
	return readEvents(ctx, s.pool, thread, after)
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func readEvents(ctx context.Context, q querier, thread string, after int64) ([]Event, error) {
	rows, err := q.Query(ctx, "SELECT sequence,kind,run_id,call_id,tool_name,answer,communication_id,reason FROM agent_events WHERE thread_id=$1 AND sequence>$2 ORDER BY sequence", thread, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Event{}
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.Sequence, &e.Type, &e.RunID, &e.CallID, &e.ToolName, &e.Answer, &e.CommunicationID, &e.Reason); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}
