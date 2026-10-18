package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func mustFail(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err == nil {
		t.Fatalf("expected failure: %s", sql)
	}
}

func TestMigration000007(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	if err := migrateTo(ctx, pool, 6); err != nil {
		t.Fatal(err)
	}
	old := `{"ThreadID":"t1","RunID":"r1","CommunicationID":"c1","Principal":"p","Text":"hi","TargetJourney":"","InteractionID":"","ReplyKind":"","ReplyJSON":""}`
	// commands and runs reference each other through a deferred FK: one transaction.
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		stmts := []string{"INSERT INTO agent_conversations(id,principal) VALUES('t1','p')"}
		for _, r := range []string{"r1", "r2", "r3"} {
			payload := `{"ThreadID":"t1","RunID":"` + r + `","CommunicationID":"c-` + r + `","Principal":"p","Text":"hi"}`
			if r == "r1" {
				payload = old
			}
			stmts = append(stmts, "INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('t1','c-"+r+"','"+r+"','"+payload+"','"+r+"')")
		}
		stmts = append(stmts,
			"INSERT INTO agent_runs(id,thread_id,state) VALUES('r1','t1','completed'),('r2','t1','failed'),('r3','t1','pending')",
			"UPDATE agent_commands SET terminal_reason='failed' WHERE run_id='r2'",
			"INSERT INTO agent_events(thread_id,sequence,run_id,kind) VALUES('t1',1,'r1','run.completed')")
		for _, q := range stmts {
			if _, err := tx.Exec(ctx, q); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var states, reasons string
	if err := pool.QueryRow(ctx, "SELECT string_agg(id||':'||state, ',' ORDER BY id) FROM agent_runs").Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != "r1:completed,r2:failed,r3:pending" {
		t.Fatalf("states = %s", states)
	}
	if err := pool.QueryRow(ctx, "SELECT string_agg(run_id||':'||terminal_reason, ',' ORDER BY run_id) FROM agent_commands").Scan(&reasons); err != nil {
		t.Fatal(err)
	}
	if reasons != "r1:,r2:failed,r3:" {
		t.Fatalf("reasons = %s", reasons)
	}
	var nulls int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_runs WHERE product_version IS NULL AND endpoint_id IS NULL AND settings IS NULL AND server_info IS NULL").Scan(&nulls); err != nil || nulls != 3 {
		t.Fatalf("new columns not NULL: %d %v", nulls, err)
	}
	var events int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_events").Scan(&events); err != nil || events != 1 {
		t.Fatalf("events = %d %v", events, err)
	}

	mustExec(t, pool, "UPDATE agent_runs SET state='dependency_unavailable' WHERE id='r3'")
	mustExec(t, pool, "UPDATE agent_commands SET terminal_reason='dependency_unavailable' WHERE run_id='r3'")
	mustFail(t, pool, "UPDATE agent_runs SET state='bogus' WHERE id='r1'")
	mustFail(t, pool, "UPDATE agent_commands SET terminal_reason='bogus' WHERE run_id='r1'")
	mustFail(t, pool, "UPDATE agent_runs SET product_version='v1' WHERE id='r1'")
	mustFail(t, pool, "UPDATE agent_runs SET endpoint_id='e1' WHERE id='r1'")
	mustFail(t, pool, "UPDATE agent_runs SET product_version='',endpoint_id='' WHERE id='r1'")
	mustFail(t, pool, "UPDATE agent_runs SET settings='[]' WHERE id='r1'")
	mustFail(t, pool, "UPDATE agent_runs SET server_info='[]' WHERE id='r1'")
	mustExec(t, pool, "UPDATE agent_runs SET product_version='v1',endpoint_id='e1',settings='{}',server_info='{}' WHERE id='r1'")

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_schema_migrations").Scan(&count); err != nil || count != 7 {
		t.Fatalf("migrations = %d %v", count, err)
	}

	var c Command
	if err := json.Unmarshal([]byte(old), &c); err != nil || c.Tenant != "" || c.Text != "hi" {
		t.Fatalf("old payload: %+v %v", c, err)
	}
}

// admitClaim drives the real Admit then Claim flow for one fresh command.
func admitClaim(t *testing.T, s *Store, c Command) Owner {
	t.Helper()
	ctx := context.Background()
	_, fresh, err := s.Admit(ctx, c)
	if err != nil || !fresh {
		t.Fatalf("Admit: fresh=%v err=%v", fresh, err)
	}
	o, err := s.Claim(ctx, c, "o1", LeaseDuration)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return o
}

func scopedStore(t *testing.T) *Store {
	t.Helper()
	pool := pgtest.Database(t)
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return New(pool)
}

func testCommand() Command {
	return Command{ThreadID: "t1", RunID: "r1", CommunicationID: "c1", Principal: "p", Tenant: "tenant-a", Text: "hello"}
}

func onlyRun(t *testing.T, s *Store) Run {
	t.Helper()
	snap, err := s.Snapshot(context.Background(), "t1", "p")
	if err != nil || len(snap.Runs) == 0 {
		t.Fatalf("Snapshot: %v %d", err, len(snap.Runs))
	}
	return snap.Runs[0]
}

func TestRecordRunScope(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	c := testCommand()
	o := admitClaim(t, s, c)

	var payload string
	if err := s.pool.QueryRow(ctx, "SELECT payload::text FROM agent_commands WHERE run_id='r1'").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var stored Command
	if err := json.Unmarshal([]byte(payload), &stored); err != nil || stored.Tenant != "tenant-a" {
		t.Fatalf("payload tenant: %s %v", payload, err)
	}

	scope := RunScope{ProductVersion: "2.1", EndpointID: "ep-1", Settings: json.RawMessage(`{"digest":"abc","max_active_journeys":3}`)}
	if err := s.RecordRunScope(ctx, o, scope); err != nil {
		t.Fatal(err)
	}
	run := onlyRun(t, s)
	if run.ProductVersion != "2.1" || run.EndpointID != "ep-1" {
		t.Fatalf("scope = %+v", run)
	}
	var got map[string]any
	if err := json.Unmarshal(run.Settings, &got); err != nil || got["digest"] != "abc" || got["max_active_journeys"] != float64(3) {
		t.Fatalf("settings = %s %v", run.Settings, err)
	}
	if run.ServerInfo != nil {
		t.Fatalf("server info = %s", run.ServerInfo)
	}

	// identical (whitespace and key order are irrelevant under jsonb) is a no-op
	same := RunScope{ProductVersion: "2.1", EndpointID: "ep-1", Settings: json.RawMessage(`{ "max_active_journeys": 3, "digest": "abc" }`)}
	if err := s.RecordRunScope(ctx, o, same); err != nil {
		t.Fatalf("identical re-record: %v", err)
	}
	for name, diff := range map[string]RunScope{
		"version":  {ProductVersion: "2.2", EndpointID: "ep-1", Settings: scope.Settings},
		"endpoint": {ProductVersion: "2.1", EndpointID: "ep-2", Settings: scope.Settings},
		"settings": {ProductVersion: "2.1", EndpointID: "ep-1", Settings: json.RawMessage(`{"digest":"zzz"}`)},
	} {
		if err := s.RecordRunScope(ctx, o, diff); !errors.Is(err, ErrState) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	for name, bad := range map[string]RunScope{
		"no version":   {EndpointID: "e", Settings: json.RawMessage(`{}`)},
		"no endpoint":  {ProductVersion: "v", Settings: json.RawMessage(`{}`)},
		"nil settings": {ProductVersion: "v", EndpointID: "e"},
		"array":        {ProductVersion: "v", EndpointID: "e", Settings: json.RawMessage(`[]`)},
		"string":       {ProductVersion: "v", EndpointID: "e", Settings: json.RawMessage(`"x"`)},
		"null":         {ProductVersion: "v", EndpointID: "e", Settings: json.RawMessage(`null`)},
		"invalid json": {ProductVersion: "v", EndpointID: "e", Settings: json.RawMessage(`{`)},
	} {
		if err := s.RecordRunScope(ctx, o, bad); !errors.Is(err, ErrState) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	run = onlyRun(t, s)
	if run.ProductVersion != "2.1" || run.EndpointID != "ep-1" {
		t.Fatalf("scope changed: %+v", run)
	}

	// a stale owner is fenced out
	for name, stale := range map[string]Owner{
		"epoch": {Command: o.Command, ID: o.ID, Epoch: o.Epoch + 1},
		"id":    {Command: o.Command, ID: "other", Epoch: o.Epoch},
	} {
		if err := s.RecordRunScope(ctx, stale, scope); !errors.Is(err, ErrOwnership) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestRecordRunScopeStaleWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	stale := Owner{Command: o.Command, ID: o.ID, Epoch: o.Epoch + 1}
	scope := RunScope{ProductVersion: "2.1", EndpointID: "ep-1", Settings: json.RawMessage(`{}`)}
	if err := s.RecordRunScope(ctx, stale, scope); !errors.Is(err, ErrOwnership) {
		t.Fatalf("err = %v", err)
	}
	run := onlyRun(t, s)
	if run.ProductVersion != "" || run.EndpointID != "" || run.Settings != nil {
		t.Fatalf("stale write landed: %+v", run)
	}
}

func TestRecordServerInfo(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	if err := s.RecordServerInfo(ctx, o, json.RawMessage(`{"name":"mcp","version":"1.0"}`)); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(onlyRun(t, s).ServerInfo, &got); err != nil || got["version"] != "1.0" {
		t.Fatalf("server info = %v %v", got, err)
	}
	if err := s.RecordServerInfo(ctx, o, json.RawMessage(`{"name":"mcp","version":"1.1"}`)); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if err := json.Unmarshal(onlyRun(t, s).ServerInfo, &got); err != nil || got["version"] != "1.1" {
		t.Fatalf("server info = %v %v", got, err)
	}
	for _, bad := range []string{``, `[]`, `"x"`, `null`, `{`} {
		if err := s.RecordServerInfo(ctx, o, json.RawMessage(bad)); !errors.Is(err, ErrState) {
			t.Fatalf("%q: err = %v", bad, err)
		}
	}
	stale := Owner{Command: o.Command, ID: o.ID, Epoch: o.Epoch + 1}
	if err := s.RecordServerInfo(ctx, stale, json.RawMessage(`{"name":"x"}`)); !errors.Is(err, ErrOwnership) {
		t.Fatalf("stale: %v", err)
	}
	if err := json.Unmarshal(onlyRun(t, s).ServerInfo, &got); err != nil || got["version"] != "1.1" {
		t.Fatalf("server info changed: %v %v", got, err)
	}
}

func TestFinishDependencyUnavailable(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	c := testCommand()
	o := admitClaim(t, s, c)
	steer := Command{ThreadID: "t1", RunID: "r2", CommunicationID: "c2", Principal: "p", Tenant: "tenant-a", Text: "more"}
	receipt, fresh, err := s.Admit(ctx, steer)
	if err != nil || fresh || receipt.ExecutionRunID != "r1" {
		t.Fatalf("steer: %+v fresh=%v err=%v", receipt, fresh, err)
	}
	if err := s.Finish(ctx, o, RunDependencyUnavailable, nil, "", "", ""); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(ctx, "t1", "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Runs) != 1 || snap.Runs[0].State != RunDependencyUnavailable {
		t.Fatalf("runs = %+v", snap.Runs)
	}
	outcomes := map[string]CommandOutcome{}
	for _, oc := range snap.Runs[0].CommandOutcomes {
		outcomes[oc.CommunicationID] = oc
	}
	if oc := outcomes["c2"]; oc.State != "rejected" || oc.Reason != "dependency_unavailable" {
		t.Fatalf("c2 outcome = %+v", oc)
	}
	var sawRun, sawRejected bool
	for _, e := range snap.Events {
		if e.Type == "run.dependency_unavailable" && e.RunID == "r1" {
			sawRun = true
		}
		if e.Type == "command.rejected" && e.CommunicationID == "c2" && e.Reason == "dependency_unavailable" {
			sawRejected = true
		}
	}
	if !sawRun || !sawRejected {
		t.Fatalf("events = %+v", snap.Events)
	}
	// the terminal run no longer has a live owner
	if err := s.Check(ctx, o); !errors.Is(err, ErrOwnership) {
		t.Fatalf("Check after finish: %v", err)
	}
}
