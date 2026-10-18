package journal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

func TestMigration000012(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	seedPre000011(t, ctx, pool)
	if err := migrateTo(ctx, pool, 11); err != nil {
		t.Fatal(err)
	}
	// Runs and commands in every old state, written at v11.
	old := []string{"auth_required", "interrupted", "awaiting_input", "dependency_unavailable"}
	// A run and its command reference each other, so they go in one transaction.
	addRun := func(id, state string) error {
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('m',$1,$1,$2::jsonb,$1)", id, seedPayload("m", id, id)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO agent_runs(id,thread_id,state) VALUES($1,'m',$2)", id, state)
			return err
		})
	}
	for _, st := range old {
		if err := addRun("old-"+st, st); err != nil {
			t.Fatal(err)
		}
	}
	for i, reason := range []string{"", "failed", "auth_required", "interrupted", "consumed", "dependency_unavailable"} {
		id := "oc" + string(rune('a'+i))
		mustExec(t, pool, "INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id,terminal_reason) VALUES('m',$1,$1,$2::jsonb,'m-r4',$3)", id, seedPayload("m", id, id), reason)
	}
	if addRun("lim-pre", "limit_exceeded") == nil {
		t.Fatal("limit_exceeded run accepted at v11")
	}
	mustFail(t, pool, "UPDATE agent_commands SET terminal_reason='limit_exceeded' WHERE communication_id='oca'")

	if err := migrateTo(ctx, pool, 12); err != nil {
		t.Fatal(err)
	}
	if len(migrationNames) < 12 {
		t.Fatalf("migrationNames = %d", len(migrationNames))
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_runs WHERE thread_id='m'").Scan(&n); err != nil || n != 5+len(old) {
		t.Fatalf("runs = %d %v", n, err)
	}
	if err := addRun("lim-post", "limit_exceeded"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, "UPDATE agent_commands SET terminal_reason='limit_exceeded' WHERE communication_id='oca'")
	mustFail(t, pool, "UPDATE agent_commands SET terminal_reason='bogus' WHERE communication_id='ocb'")

	ins := "INSERT INTO agent_usage(thread_id,run_id,seq,input_tokens,output_tokens,cached_tokens,tenant,principal,journey_id) VALUES('m','m-r1',$1,$2,$3,$4,$5,'p',$6)"
	mustExec(t, pool, ins, 1, 1, 2, 3, "t", nil)
	mustFail(t, pool, ins, 2, -1, 0, 0, "t", nil)
	mustFail(t, pool, ins, 2, 0, -1, 0, "t", nil)
	mustFail(t, pool, ins, 2, 0, 0, -1, "t", nil)
	mustFail(t, pool, ins, 2, 0, 0, 0, "", nil)
	mustFail(t, pool, ins, 2, 0, 0, 0, "t", "")
	mustFail(t, pool, ins, 0, 0, 0, 0, "t", nil)
	mustFail(t, pool, ins, 1, 0, 0, 0, "t", nil) // duplicate seq
	mustFail(t, pool, "INSERT INTO agent_usage(thread_id,run_id,seq,input_tokens,output_tokens,cached_tokens,tenant,principal) VALUES('m','nope',1,0,0,0,'t','p')")
}

func TestUsageRecordFenced(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	c := testCommand()
	o := admitClaim(t, s, c)

	for i, u := range []Usage{{Model: "m1", Input: 10, Output: 5, Cached: 2}, {Model: "m2", JourneyID: "J"}, {}} {
		seq, err := s.RecordUsage(ctx, o, u)
		if err != nil || seq != i+1 {
			t.Fatalf("RecordUsage %d: seq=%d err=%v", i, seq, err)
		}
	}
	var tenant, principal string
	var journey *string
	if err := s.pool.QueryRow(ctx, "SELECT tenant,principal,journey_id FROM agent_usage WHERE seq=2").Scan(&tenant, &principal, &journey); err != nil {
		t.Fatal(err)
	}
	if tenant != c.Tenant || principal != c.Principal || journey == nil || *journey != "J" {
		t.Fatalf("row 2 = %q %q %v", tenant, principal, journey)
	}
	if err := s.pool.QueryRow(ctx, "SELECT journey_id FROM agent_usage WHERE seq=3").Scan(&journey); err != nil || journey != nil {
		t.Fatalf("empty journey = %v %v", journey, err)
	}

	for _, u := range []Usage{{Input: -1}, {Output: -1}, {Cached: -1}} {
		if _, err := s.RecordUsage(ctx, o, u); !errors.Is(err, ErrState) {
			t.Fatalf("negative %+v: %v", u, err)
		}
	}
	count := func() int {
		var n int
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM agent_usage").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 3 {
		t.Fatalf("rows = %d", count())
	}

	stale := o
	stale.Epoch++
	if _, err := s.RecordUsage(ctx, stale, Usage{Model: "x"}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("stale epoch: %v", err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id='r1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordUsage(ctx, o, Usage{Model: "x"}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("expired lease: %v", err)
	}
	if count() != 3 {
		t.Fatalf("rows after fenced = %d", count())
	}
}

func TestUsageTotals(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	rec := func(u Usage) {
		t.Helper()
		if _, err := s.RecordUsage(ctx, o, u); err != nil {
			t.Fatal(err)
		}
	}
	rec(Usage{Model: "b", Input: 1, Output: 2, Cached: 3, JourneyID: "J1"})
	rec(Usage{Model: "a", Input: 10, Output: 20, Cached: 30, JourneyID: "J2"})
	rec(Usage{Model: "b", Input: 100, Output: 200, Cached: 300})
	// A second tenant and principal in another conversation.
	c2 := Command{ThreadID: "t2", RunID: "r2", CommunicationID: "c2", Principal: "q", Tenant: "tenant-b", Text: "hi"}
	o2 := admitClaim(t, s, c2)
	if _, err := s.RecordUsage(ctx, o2, Usage{Model: "a", Input: 7}); err != nil {
		t.Fatal(err)
	}

	get := func(q UsageQuery) []UsageTotal {
		t.Helper()
		got, err := s.UsageTotals(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	eq := func(name string, got, want []UsageTotal) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %+v want %+v", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: %+v want %+v", name, got, want)
			}
		}
	}
	eq("tenant", get(UsageQuery{Tenant: "tenant-a"}), []UsageTotal{{"a", 1, 10, 20, 30}, {"b", 2, 101, 202, 303}})
	eq("other tenant", get(UsageQuery{Tenant: "tenant-b"}), []UsageTotal{{"a", 1, 7, 0, 0}})
	eq("principal", get(UsageQuery{Tenant: "tenant-a", Principal: "p"}), []UsageTotal{{"a", 1, 10, 20, 30}, {"b", 2, 101, 202, 303}})
	eq("principal none", get(UsageQuery{Tenant: "tenant-a", Principal: "q"}), []UsageTotal{})
	eq("conversation", get(UsageQuery{Tenant: "tenant-a", Conversation: "t1"}), []UsageTotal{{"a", 1, 10, 20, 30}, {"b", 2, 101, 202, 303}})
	eq("conversation other", get(UsageQuery{Tenant: "tenant-a", Conversation: "t2"}), []UsageTotal{})
	eq("journey", get(UsageQuery{Tenant: "tenant-a", Journey: "J1"}), []UsageTotal{{"b", 1, 1, 2, 3}})

	var first, last time.Time
	if err := s.pool.QueryRow(ctx, "SELECT min(recorded_at),max(recorded_at) FROM agent_usage WHERE tenant='tenant-a'").Scan(&first, &last); err != nil {
		t.Fatal(err)
	}
	eq("From inclusive", get(UsageQuery{Tenant: "tenant-a", From: first}), []UsageTotal{{"a", 1, 10, 20, 30}, {"b", 2, 101, 202, 303}})
	eq("To exclusive", get(UsageQuery{Tenant: "tenant-a", To: first}), []UsageTotal{})
	eq("From after", get(UsageQuery{Tenant: "tenant-a", From: last.Add(time.Millisecond)}), []UsageTotal{})
	eq("window", get(UsageQuery{Tenant: "tenant-a", From: first, To: last}), []UsageTotal{{"a", 1, 10, 20, 30}, {"b", 1, 1, 2, 3}})
	if _, err := s.UsageTotals(ctx, UsageQuery{}); !errors.Is(err, ErrState) {
		t.Fatalf("empty tenant: %v", err)
	}
}

func TestFinishLimitExceeded(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	pending := Command{ThreadID: "t1", RunID: "r2", CommunicationID: "c2", Principal: "p", Tenant: "tenant-a", Text: "more"}
	if _, _, err := s.Admit(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishLimitExceeded(ctx, o, nil, "budget", 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(ctx, "t1", "p")
	if err != nil {
		t.Fatal(err)
	}
	run := snap.Runs[0]
	if run.State != RunLimitExceeded || run.PendingCommands != 0 {
		t.Fatalf("run = %+v", run)
	}
	var disposed bool
	for _, co := range run.CommandOutcomes {
		if co.CommunicationID == "c2" {
			disposed = true
			if co.State != "rejected" || co.Reason != "limit_exceeded" {
				t.Fatalf("outcome = %+v", co)
			}
		}
	}
	if !disposed {
		t.Fatalf("pending command outcome missing: %+v", run.CommandOutcomes)
	}
	var limit *Event
	for i, e := range snap.Events {
		if e.Type == "run.limit_exceeded" {
			limit = &snap.Events[i]
		}
	}
	if limit == nil || limit.Reason != "budget" || limit.RunID != "r1" {
		t.Fatalf("limit event = %+v", limit)
	}
	var detail struct {
		Kind              string `json:"kind"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	if err := json.Unmarshal(limit.Detail, &detail); err != nil || detail.Kind != "budget" || detail.RetryAfterSeconds != 2 {
		t.Fatalf("detail = %s %v", limit.Detail, err)
	}
	var unknown int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM agent_operations WHERE outcome='outcome_unknown'").Scan(&unknown); err != nil || unknown != 0 {
		t.Fatalf("outcome_unknown = %d %v", unknown, err)
	}
}

func TestFinishLimitExceededRateUnknownRetry(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	if err := s.FinishLimitExceeded(ctx, o, nil, "rate", 0); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, "t1", "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == "run.limit_exceeded" {
			if e.Reason != "rate" || string(e.Detail) != `{"kind": "rate", "retry_after_seconds": 0}` {
				t.Fatalf("event = %+v %s", e, e.Detail)
			}
			return
		}
	}
	t.Fatal("no run.limit_exceeded event")
}

func TestFinishLimitExceededRejects(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	if err := s.Finish(ctx, o, RunLimitExceeded, nil, "", "", ""); !errors.Is(err, ErrState) {
		t.Fatalf("Finish(limit_exceeded): %v", err)
	}
	if err := s.FinishLimitExceeded(ctx, o, nil, "bogus", 0); !errors.Is(err, ErrState) {
		t.Fatalf("bad kind: %v", err)
	}
	if got := onlyRun(t, s).State; got != RunRunning {
		t.Fatalf("state = %s", got)
	}
	stale := o
	stale.Epoch++
	if err := s.FinishLimitExceeded(ctx, stale, nil, "budget", 0); !errors.Is(err, ErrOwnership) {
		t.Fatalf("stale: %v", err)
	}
	if got := onlyRun(t, s).State; got != RunRunning {
		t.Fatalf("state after stale = %s", got)
	}
	if err := s.FinishLimitExceeded(ctx, o, nil, "budget", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishLimitExceeded(ctx, o, nil, "budget", 0); !errors.Is(err, ErrOwnership) {
		t.Fatalf("second finish: %v", err)
	}
}
