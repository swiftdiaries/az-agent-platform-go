package journal

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// HistoryTruncation counts what one provider request's history budget dropped.
type HistoryTruncation struct {
	TurnsDropped, MessagesDropped, EstimateBefore, EstimateAfter, Budget int
}

// HistoryTruncated appends a history_truncated event for the owned run, fenced by
// lockOwner. The detail carries counts only, never content.
func (s *Store) HistoryTruncated(ctx context.Context, o Owner, t HistoryTruncation) error {
	if t.TurnsDropped < 0 || t.MessagesDropped < 0 || t.EstimateBefore < 0 || t.EstimateAfter < 0 || t.Budget < 0 {
		return ErrState
	}
	detail, err := json.Marshal(map[string]int{
		"turns_dropped":    t.TurnsDropped,
		"messages_dropped": t.MessagesDropped,
		"estimate_before":  t.EstimateBefore,
		"estimate_after":   t.EstimateAfter,
		"budget":           t.Budget,
	})
	if err != nil {
		return err
	}
	c := o.Command
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		return appendEvent(ctx, tx, c.ThreadID, Event{Type: "history_truncated", RunID: c.RunID, Detail: detail})
	})
}
