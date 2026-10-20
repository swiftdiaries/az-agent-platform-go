package cost

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

func usageStore(t *testing.T) *journal.Store {
	t.Helper()
	pool := pgtest.Database(t)
	if err := journal.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return journal.New(pool)
}

func ownRun(t *testing.T, s *journal.Store, c journal.Command) journal.Owner {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.Admit(ctx, c); err != nil {
		t.Fatal(err)
	}
	o, err := s.Claim(ctx, c, "o1", journal.LeaseDuration)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestUsageReport(t *testing.T) {
	ctx := context.Background()
	s := usageStore(t)
	o := ownRun(t, s, journal.Command{ThreadID: "t1", RunID: "r1", CommunicationID: "c1", Principal: "p", Tenant: "ta", Text: "hi"})
	for _, u := range []journal.Usage{
		{Model: "b", Input: 1, Output: 2, Cached: 3, JourneyID: "J"},
		{Model: "a", Input: 10, Output: 20, Cached: 30},
		{Model: "b", Input: 100, Output: 200, Cached: 300},
	} {
		if _, err := s.RecordUsage(ctx, o, u); err != nil {
			t.Fatal(err)
		}
	}
	o2 := ownRun(t, s, journal.Command{ThreadID: "t2", RunID: "r2", CommunicationID: "c2", Principal: "q", Tenant: "tb", Text: "hi"})
	if _, err := s.RecordUsage(ctx, o2, journal.Usage{Model: "a", Input: 5}); err != nil {
		t.Fatal(err)
	}

	r, err := Usage(ctx, s, Scope{Tenant: "ta"}, Window{})
	if err != nil {
		t.Fatal(err)
	}
	wantModels := []ModelUsage{{"a", 1, 10, 20, 30}, {"b", 2, 101, 202, 303}}
	if len(r.Models) != 2 || r.Models[0] != wantModels[0] || r.Models[1] != wantModels[1] {
		t.Fatalf("models = %+v", r.Models)
	}
	if want := (ModelUsage{"", 3, 111, 222, 333}); r.Total != want {
		t.Fatalf("total = %+v want %+v", r.Total, want)
	}
	if r.Scope.Tenant != "ta" {
		t.Fatalf("scope = %+v", r.Scope)
	}

	j, err := Usage(ctx, s, Scope{Tenant: "ta", Journey: "J", Principal: "p", Conversation: "t1"}, Window{})
	if err != nil || j.Total != (ModelUsage{"", 1, 1, 2, 3}) || len(j.Models) != 1 {
		t.Fatalf("journey = %+v %v", j, err)
	}
	future, err := Usage(ctx, s, Scope{Tenant: "ta"}, Window{From: time.Now().Add(time.Hour)})
	if err != nil || future.Total != (ModelUsage{}) || future.Models == nil || len(future.Models) != 0 {
		t.Fatalf("future = %+v %v", future, err)
	}
	if _, err := Usage(ctx, s, Scope{}, Window{}); !errors.Is(err, journal.ErrState) {
		t.Fatalf("empty tenant: %v", err)
	}
}
