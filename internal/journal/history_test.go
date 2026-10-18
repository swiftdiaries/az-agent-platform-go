package journal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func historyEvents(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), "SELECT run_id||' '||COALESCE(detail::text,'') FROM agent_events WHERE kind='history_truncated' ORDER BY sequence")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	return out
}

func TestHistoryTruncatedWritesEvent(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	if err := s.HistoryTruncated(ctx, o, HistoryTruncation{TurnsDropped: 2, MessagesDropped: 5, EstimateBefore: 900, EstimateAfter: 400, Budget: 500}); err != nil {
		t.Fatal(err)
	}
	var runID string
	var detail []byte
	if err := s.pool.QueryRow(ctx, "SELECT run_id,detail FROM agent_events WHERE kind='history_truncated'").Scan(&runID, &detail); err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	if err := json.Unmarshal(detail, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"turns_dropped": 2, "messages_dropped": 5, "estimate_before": 900, "estimate_after": 400, "budget": 500}
	if runID != "r1" || len(got) != len(want) {
		t.Fatalf("run %q detail %v", runID, got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %d, want %d", k, got[k], v)
		}
	}
}

func TestHistoryTruncatedFenced(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	stale := o
	stale.Epoch++
	if err := s.HistoryTruncated(ctx, stale, HistoryTruncation{TurnsDropped: 1}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("stale owner: %v", err)
	}
	if got := historyEvents(t, s); len(got) != 0 {
		t.Fatalf("events = %v", got)
	}
}

func TestHistoryTruncatedRejectsNegative(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	for _, bad := range []HistoryTruncation{{TurnsDropped: -1}, {MessagesDropped: -1}, {EstimateBefore: -1}, {EstimateAfter: -1}, {Budget: -1}} {
		if err := s.HistoryTruncated(ctx, o, bad); !errors.Is(err, ErrState) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if got := historyEvents(t, s); len(got) != 0 {
		t.Fatalf("events = %v", got)
	}
}
