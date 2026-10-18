package journal

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// ActiveJourneys returns the conversation's active set (least recently used first) and the journeys pinned
// now. Fenced (lockOwner). pinned = DISTINCT non-empty journey_id of approval interactions of this thread that
// are state='pending', or state='consumed' AND continuation_run_id=this run AND marker='decided' (not yet
// dispatched, denied or superseded). Sorted ascending.
func (s *Store) ActiveJourneys(ctx context.Context, o Owner) (active, pinned []string, err error) {
	c := o.Command
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(ctx, "SELECT active_journeys FROM agent_conversations WHERE id=$1", c.ThreadID).Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &active); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT i.journey_id FROM agent_interactions i
WHERE i.thread_id=$1 AND i.kind='approval' AND COALESCE(i.journey_id,'')<>'' AND (
  i.state='pending' OR (i.state='consumed' AND i.continuation_run_id=$2 AND i.marker='decided'))
GROUP BY i.journey_id ORDER BY i.journey_id COLLATE "C"`, c.ThreadID, c.RunID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var journey string
			if err := rows.Scan(&journey); err != nil {
				return err
			}
			pinned = append(pinned, journey)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}
	return active, pinned, nil
}

// SetActiveJourneys replaces the conversation's active set. Fenced. Every ID must be non-empty and unique
// (else ErrState, nothing written). nil is stored as [].
func (s *Store) SetActiveJourneys(ctx context.Context, o Owner, active []string) error {
	seen := make(map[string]bool, len(active))
	for _, id := range active {
		if id == "" || seen[id] {
			return ErrState
		}
		seen[id] = true
	}
	if active == nil {
		active = []string{}
	}
	data, err := json.Marshal(active)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE agent_conversations SET active_journeys=$2 WHERE id=$1", o.Command.ThreadID, data)
		return err
	})
}
