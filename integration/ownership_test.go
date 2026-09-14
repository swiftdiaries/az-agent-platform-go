package integration_test

import (
	"context"
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
	if before.RunState != journal.RunInterrupted || before.Runs[0].PendingCommands != 2 || calls.Load() != 1 {
		t.Fatalf("loss: %+v calls %d", before, calls.Load())
	}
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
