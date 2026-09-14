package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
)

func TestOwnershipExplicitClaimAndExpiry(t *testing.T) {
	ctx := context.Background()
	pool := database(t)
	store := journal.New(pool)
	c := journal.Command{ThreadID: "owner-thread", RunID: "owner-run", CommunicationID: "owner-command", Principal: "alice", Text: "hello"}
	if _, err := store.Claim(ctx, c, "A", time.Second); !errors.Is(err, journal.ErrOwnership) {
		t.Fatalf("unadmitted claim: %v", err)
	}
	if _, _, err := store.Admit(ctx, c); err != nil {
		t.Fatal(err)
	}
	owner, err := store.Claim(ctx, c, "A", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	wrongEpoch := owner
	wrongEpoch.Epoch++
	if err := store.Check(ctx, wrongEpoch); !errors.Is(err, journal.ErrOwnership) {
		t.Fatalf("wrong epoch accepted: %v", err)
	}
	if _, err := store.Claim(ctx, c, "B", time.Second); !errors.Is(err, journal.ErrOwnership) {
		t.Fatalf("takeover: %v", err)
	}
	if _, err := store.Start(ctx, owner, "planner", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if err := store.Renew(ctx, owner, time.Second); !errors.Is(err, journal.ErrOwnership) {
		t.Fatalf("expired renewal: %v", err)
	}
	if err := store.Finish(ctx, owner, journal.RunCompleted, []byte(`[]`), "stale", "", ""); !errors.Is(err, journal.ErrOwnership) {
		t.Fatalf("stale finish: %v", err)
	}
	if err := store.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(ctx, c.ThreadID, c.Principal)
	if err != nil || snapshot.RunState != journal.RunInterrupted || snapshot.Answer != "" {
		t.Fatalf("reaped snapshot %+v: %v", snapshot, err)
	}
	if _, err := store.Claim(ctx, c, "C", time.Second); !errors.Is(err, journal.ErrOwnership) {
		t.Fatalf("interrupted replay: %v", err)
	}
}

func claimForTest(t *testing.T, store *journal.Store, c journal.Command, include bool) journal.Owner {
	t.Helper()
	owner, err := store.Claim(context.Background(), c, "test-owner", journal.LeaseDuration)
	if err != nil {
		t.Fatal(err)
	}
	if include {
		inbox, err := store.Pending(context.Background(), owner)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Included(context.Background(), owner, inbox.Commands); err != nil {
			t.Fatal(err)
		}
	}
	return owner
}

func TestOwnershipLeaseExpiresDuringRowLock(t *testing.T) {
	ctx := context.Background()
	pool := database(t)
	store := journal.New(pool)
	operations := map[string]func(journal.Owner) error{
		"check": func(o journal.Owner) error { return store.Check(ctx, o) },
		"renew": func(o journal.Owner) error { return store.Renew(ctx, o, time.Second) },
		"start": func(o journal.Owner) error {
			_, err := store.Start(ctx, o, "planner", strings.Repeat("a", 64))
			return err
		},
		"included": func(o journal.Owner) error {
			return store.Included(ctx, o, []journal.Input{{CommunicationID: o.Command.CommunicationID}})
		},
		"finish": func(o journal.Owner) error { return store.Finish(ctx, o, journal.RunFailed, nil, "", "", "") },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			c := journal.Command{ThreadID: name, RunID: name, CommunicationID: name, Principal: "alice", Text: "hello"}
			if _, _, err := store.Admit(ctx, c); err != nil {
				t.Fatal(err)
			}
			o, err := store.Claim(ctx, c, "A", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			gate, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Rollback(ctx)
			if _, err := gate.Exec(ctx, "UPDATE agent_runs SET lease_until=clock_timestamp()+interval '200 milliseconds' WHERE id=$1", c.RunID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- operation(o) }()
			waitDatabase(t, pool, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE NOT granted AND locktype='transactionid')")
			if _, err := gate.Exec(ctx, "SELECT pg_sleep(0.25)"); err != nil {
				t.Fatal(err)
			}
			if err := gate.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, journal.ErrOwnership) {
				t.Fatalf("expired %s admitted: %v", name, err)
			}
		})
	}
}

func TestExpiredContinuationRecoveryNeverTakesOverOwnedWork(t *testing.T) {
	for _, state := range []string{"pending", "interrupted"} {
		t.Run(state, func(t *testing.T) {
			pool := database(t)
			store, owner, interaction := localWait(t, pool)
			if err := store.Wait(t.Context(), owner, interaction, []byte(`{}`), []byte(`[]`)); err != nil {
				t.Fatal(err)
			}
			reply := journal.Command{ThreadID: "thread", RunID: "reply", CommunicationID: "reply", Principal: "alice", InteractionID: interaction.ID, ReplyKind: "clarification", ReplyJSON: `{"answers":{"choice":{"option":"A"}}}`}
			if _, fresh, err := store.Admit(t.Context(), reply); err != nil || !fresh {
				t.Fatal(err)
			}
			if _, err := store.Claim(t.Context(), reply, "first-owner", time.Second); err != nil {
				t.Fatal(err)
			}
			if state == "interrupted" {
				if _, err := pool.Exec(t.Context(), "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", reply.RunID); err != nil {
					t.Fatal(err)
				}
				if err := store.Reap(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else if _, err := pool.Exec(t.Context(), "UPDATE agent_runs SET state='pending',lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", reply.RunID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Claim(t.Context(), reply, "second-owner", time.Second); !errors.Is(err, journal.ErrOwnership) {
				t.Fatalf("%s continuation taken over: %v", state, err)
			}
		})
	}
}

func TestReapStaleCandidateCannotInterruptNewContinuation(t *testing.T) {
	pool := database(t)
	store, owner, interaction := localWait(t, pool)
	if err := store.Wait(t.Context(), owner, interaction, []byte(`{}`), []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE agent_runs SET state='running',lease_until=clock_timestamp()-interval '1 second' WHERE id='run'"); err != nil {
		t.Fatal(err)
	}
	gate, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(t.Context())
	if _, err := gate.Exec(t.Context(), "SELECT id FROM agent_conversations WHERE id='thread' FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- store.Reap(context.Background()) }()
	waitDatabase(t, pool, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE NOT granted AND locktype='transactionid')")
	reply := journal.Command{ThreadID: "thread", RunID: "reply", CommunicationID: "reply", Principal: "alice", InteractionID: interaction.ID, ReplyKind: "clarification", ReplyJSON: `{"answers":{"choice":{"option":"A"}}}`}
	payload, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"UPDATE agent_runs SET state='completed' WHERE id='run'", nil},
		{"INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('thread','reply','reply',$1,'reply')", []any{payload}},
		{"INSERT INTO agent_runs(id,thread_id,state,journey_id,definition_digest,lease_until) SELECT 'reply',thread_id,'pending',journey_id,definition_digest,clock_timestamp()-interval '1 second' FROM agent_runs WHERE id='run'", nil},
		{"UPDATE agent_interactions SET state='consumed',continuation_run_id='reply',reply=$1 WHERE id=$2", []any{[]byte(reply.ReplyJSON), interaction.ID}},
	} {
		if _, err := gate.Exec(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := gate.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var state string
	var epoch int64
	if err := pool.QueryRow(t.Context(), "SELECT state,owner_epoch FROM agent_runs WHERE id='reply'").Scan(&state, &epoch); err != nil || state != "pending" || epoch != 0 {
		t.Fatal("stale reaper interrupted new continuation", state, epoch, err)
	}
	steering := journal.Command{ThreadID: "thread", RunID: "steering", CommunicationID: "steering", Principal: "alice", Text: "new context"}
	receipt, fresh, err := store.Admit(t.Context(), steering)
	if err != nil || fresh || receipt.ExecutionRunID != reply.RunID {
		t.Fatal("ingress did not preserve continuation", receipt, fresh, err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT state,owner_epoch FROM agent_runs WHERE id='reply'").Scan(&state, &epoch); err != nil || state != "pending" || epoch != 0 {
		t.Fatal("ingress interrupted new continuation", state, epoch, err)
	}
	if _, err := store.Claim(t.Context(), reply, "fresh-owner", time.Second); err != nil {
		t.Fatal("preserved continuation was not recoverable", err)
	}
}

func TestOwnershipLossDuringModelRejectsResultAndRequiresFreshIngress(t *testing.T) {
	ctx := context.Background()
	pool := database(t)
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	runner := fixtureRunner(t, modelFunc(func(_ context.Context, request agentruntime.ModelRequest) (agentruntime.ModelResponse, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			close(returned)
			return agentruntime.ModelResponse{Text: "stale answer"}, nil
		}
		if len(request.PendingCommands) != 1 || request.PendingCommands[0].Text != "fresh ingress" {
			return agentruntime.ModelResponse{}, errors.New("resumed old pending work")
		}
		return agentruntime.ModelResponse{Text: "fresh answer"}, nil
	}))
	a := platform.New(runner, journal.New(pool))
	defer a.Close()
	b := platform.New(runner, journal.New(pool))
	defer b.Close()
	command := platform.Command{ThreadID: "thread", RunID: "run", CommunicationID: "comm", Principal: "alice", Text: "initial"}
	if _, err := a.Submit(ctx, platform.Submission{Command: command}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	command.RunID = "steer"
	command.CommunicationID = "steer"
	command.Text = "pending correction"
	receipt, err := b.Submit(ctx, platform.Submission{Command: command})
	if err != nil || receipt.ExecutionRunID != "run" {
		t.Fatalf("steering %+v %v", receipt, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	if err := journal.New(pool).Reap(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := b.Snapshot(ctx, "thread", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if before.RunState != journal.RunInterrupted || before.Runs[0].PendingCommands != 0 || calls.Load() != 1 {
		t.Fatalf("loss: %+v calls %d", before, calls.Load())
	}
	assertCommandOutcome(t, before, "comm", "interrupted", "interrupted")
	assertCommandOutcome(t, before, "steer", "interrupted", "interrupted")
	close(release)
	<-returned
	a.Close() // Join stale local completion before checking authoritative state.
	after, err := b.Snapshot(ctx, "thread", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("stale completion changed journal before %+v after %+v", before, after)
	}
	duplicate, err := b.Submit(ctx, platform.Submission{Command: command})
	if err != nil || duplicate != receipt || calls.Load() != 1 {
		t.Fatalf("retry restarted interrupted owner: %+v %v", duplicate, err)
	}
	command.RunID = "fresh"
	command.CommunicationID = "fresh"
	command.Text = "fresh ingress"
	if _, err := b.Submit(ctx, platform.Submission{Command: command}); err != nil {
		t.Fatal(err)
	}
	waitDatabase(t, pool, "SELECT EXISTS(SELECT 1 FROM agent_runs WHERE id='fresh' AND state='completed')")
	if calls.Load() != 2 {
		t.Fatalf("automatic execution: %d", calls.Load())
	}
}
