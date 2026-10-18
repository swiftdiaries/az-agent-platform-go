package journal

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// Usage is one model completion's token counts. JourneyID "" is stored as NULL.
type Usage struct {
	Model                 string
	Input, Output, Cached int64
	JourneyID             string
}

// RecordUsage writes one agent_usage row for the owned run, fenced by lockOwner,
// and returns its seq. Tenant and principal come from the durable run command.
func (s *Store) RecordUsage(ctx context.Context, o Owner, u Usage) (int, error) {
	if u.Input < 0 || u.Output < 0 || u.Cached < 0 {
		return 0, ErrState
	}
	c := o.Command
	var seq int
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO agent_usage(thread_id,run_id,seq,model,input_tokens,output_tokens,cached_tokens,tenant,principal,journey_id)
SELECT $1,$2,COALESCE(max(seq),0)+1,$3,$4,$5,$6,$7,$8,NULLIF($9,'') FROM agent_usage WHERE thread_id=$1 AND run_id=$2
RETURNING seq`, c.ThreadID, c.RunID, u.Model, u.Input, u.Output, u.Cached, c.Tenant, c.Principal, u.JourneyID).Scan(&seq)
	})
	return seq, err
}

// FinishLimitExceeded is the only way to finish a run as limit_exceeded; Finish
// rejects that state. Pending commands are disposed with reason limit_exceeded
// and the run is never marked outcome_unknown. kind is "budget" or "rate".
func (s *Store) FinishLimitExceeded(ctx context.Context, o Owner, history json.RawMessage, kind string, retryAfter time.Duration) error {
	if kind != "budget" && kind != "rate" {
		return ErrState
	}
	seconds := int64(0)
	if retryAfter > 0 {
		seconds = int64(math.Ceil(retryAfter.Seconds()))
	}
	detail, err := json.Marshal(struct {
		Kind              string `json:"kind"`
		RetryAfterSeconds int64  `json:"retry_after_seconds"`
	}{kind, seconds})
	if err != nil {
		return err
	}
	return s.finish(ctx, o, RunLimitExceeded, history, "", "", "", &Event{Type: "run.limit_exceeded", Reason: kind, Detail: detail})
}

type UsageQuery struct {
	Tenant                           string    // required
	Principal, Conversation, Journey string    // "" = any
	From, To                         time.Time // [From, To); zero = unbounded
}

type UsageTotal struct {
	Model                        string
	Calls, Input, Output, Cached int64
}

// UsageTotals sums matching rows grouped by model, ordered by model.
func (s *Store) UsageTotals(ctx context.Context, q UsageQuery) ([]UsageTotal, error) {
	if q.Tenant == "" {
		return nil, ErrState
	}
	var from, to *time.Time
	if !q.From.IsZero() {
		from = &q.From
	}
	if !q.To.IsZero() {
		to = &q.To
	}
	rows, err := s.pool.Query(ctx, `SELECT model,count(*),sum(input_tokens)::bigint,sum(output_tokens)::bigint,sum(cached_tokens)::bigint
FROM agent_usage WHERE tenant=$1
 AND ($2='' OR principal=$2) AND ($3='' OR thread_id=$3) AND ($4='' OR journey_id=$4)
 AND ($5::timestamptz IS NULL OR recorded_at>=$5) AND ($6::timestamptz IS NULL OR recorded_at<$6)
GROUP BY model ORDER BY model`, q.Tenant, q.Principal, q.Conversation, q.Journey, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	totals := []UsageTotal{}
	for rows.Next() {
		var t UsageTotal
		if err := rows.Scan(&t.Model, &t.Calls, &t.Input, &t.Output, &t.Cached); err != nil {
			return nil, err
		}
		totals = append(totals, t)
	}
	return totals, rows.Err()
}
