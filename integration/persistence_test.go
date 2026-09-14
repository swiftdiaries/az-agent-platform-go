package integration_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
)

// Removing admission serialization or payload comparison must break this test.
func TestAdmissionConcurrentDuplicates(t *testing.T) {
	pool := database(t)
	store := journal.New(pool)
	ctx := context.Background()
	command := journal.Command{ThreadID: "thread", RunID: "run", CommunicationID: "comm", Principal: "alice", Text: "remember Kyoto"}
	var wg sync.WaitGroup
	receipts := make(chan journal.Receipt, 12)
	for range 12 {
		wg.Go(func() {
			receipt, _, err := store.Admit(ctx, command)
			if err != nil {
				t.Error(err)
				return
			}
			receipts <- receipt
		})
	}
	wg.Wait()
	close(receipts)
	for receipt := range receipts {
		if receipt.RunID != "run" || receipt.State != journal.RunPending {
			t.Errorf("receipt: %+v", receipt)
		}
	}
	command.Text = "changed"
	if _, _, err := store.Admit(ctx, command); !errors.Is(err, journal.ErrConflict) {
		t.Fatalf("changed duplicate: %v", err)
	}
	snapshot, err := store.Snapshot(ctx, "thread", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Events) != 1 || snapshot.Watermark != 1 {
		t.Fatalf("duplicate events: %+v", snapshot)
	}
	command.CommunicationID = "second"
	command.RunID = "second"
	if receipt, fresh, err := store.Admit(ctx, command); err != nil || fresh || receipt.ExecutionRunID != "run" {
		t.Fatalf("steering receipt: %+v %v %v", receipt, fresh, err)
	}
}

func TestPersistenceAtomicCompletion(t *testing.T) {
	ctx := context.Background()
	pool := database(t)
	store := journal.New(pool)
	c := journal.Command{ThreadID: "t", RunID: "r", CommunicationID: "c", Principal: "alice", Text: "hello"}
	if _, _, err := store.Admit(ctx, c); err != nil {
		t.Fatal(err)
	}
	owner := claimForTest(t, store, c, true)
	digest := strings.Repeat("a", 64)
	if history, err := store.Start(ctx, owner, "planner", digest); err != nil || string(history) != "[]" {
		t.Fatalf("start history %s: %v", history, err)
	}
	before, _ := store.Snapshot(ctx, "t", "alice")
	fingerprint := func() string {
		t.Helper()
		var result string
		err := pool.QueryRow(ctx, `SELECT md5(jsonb_build_array(
 (SELECT jsonb_agg(c ORDER BY id) FROM agent_conversations c),
 (SELECT jsonb_agg(c ORDER BY run_id) FROM agent_commands c),
 (SELECT jsonb_agg(s ORDER BY journey_id) FROM agent_sessions s),
 (SELECT jsonb_agg(r ORDER BY id) FROM agent_runs r),
 (SELECT jsonb_agg(e ORDER BY sequence) FROM agent_events e))::text)`).Scan(&result)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	beforeFingerprint := fingerprint()
	if err := store.Finish(ctx, owner, journal.RunCompleted, []byte(`{}`), "answer", "", ""); err == nil {
		t.Fatal("non-array history committed")
	}
	after, _ := store.Snapshot(ctx, "t", "alice")
	if beforeFingerprint != fingerprint() {
		t.Fatal("failed completion changed database fingerprint")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed completion changed authoritative graph/events")
	}
	// Fail after history and completion events have been written inside Finish.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected completion failure'; END $$;
 CREATE TRIGGER reject_completion BEFORE UPDATE ON agent_runs FOR EACH ROW EXECUTE FUNCTION reject_completion()`); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, owner, journal.RunCompleted, []byte(`[{"role":"assistant"}]`), "answer", "call", "tool"); err == nil {
		t.Fatal("injected completion failure accepted")
	}
	if beforeFingerprint != fingerprint() {
		t.Fatal("late completion failure changed history/state/events")
	}
	if _, err := pool.Exec(ctx, "DROP TRIGGER reject_completion ON agent_runs; DROP FUNCTION reject_completion()"); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(ctx, owner, journal.RunCompleted, []byte(`[{"role":"assistant"}]`), "answer", "call", "tool"); err != nil {
		t.Fatal(err)
	}
	after, _ = store.Snapshot(ctx, "t", "alice")
	if after.RunState != journal.RunCompleted || after.Answer != "answer" || after.Watermark != 6 {
		t.Fatalf("completion: %+v", after)
	}
	receipt, fresh, err := store.Admit(ctx, c)
	if err != nil || fresh || receipt.State != journal.RunPending {
		t.Fatalf("original receipt: %+v %v %v", receipt, fresh, err)
	}
	c.RunID = "r2"
	c.CommunicationID = "c2"
	if _, _, err := store.Admit(ctx, c); err != nil {
		t.Fatal(err)
	}
	owner = claimForTest(t, store, c, true)
	if _, err := store.Start(ctx, owner, "planner", strings.Repeat("b", 64)); !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("changed definition bound: %v", err)
	}
	if history, err := store.Start(ctx, owner, "planner", digest); err != nil || !bytes.Contains(history, []byte("assistant")) {
		t.Fatalf("restored history: %s %v", history, err)
	}
}
