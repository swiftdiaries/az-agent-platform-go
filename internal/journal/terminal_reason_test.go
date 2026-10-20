package journal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCodeFinishFailedRecordsReason(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{ReasonJourneyUnavailable, ReasonPolicyConflict, ReasonActivationRefused} {
		t.Run(reason, func(t *testing.T) {
			s := scopedStore(t)
			o := admitClaim(t, s, testCommand())
			if err := s.FinishFailed(ctx, o, nil, reason); err != nil {
				t.Fatal(err)
			}
			snap, err := s.Snapshot(ctx, "t1", "p")
			if err != nil {
				t.Fatal(err)
			}
			run := snap.Runs[0]
			if run.State != RunFailed || run.TerminalReason != reason {
				t.Fatalf("run = %+v", run)
			}
			n := 0
			for _, e := range snap.Events {
				if e.Type == "run.failed" {
					n++
					if e.Reason != reason {
						t.Fatalf("event = %+v", e)
					}
				}
			}
			if n != 1 {
				t.Fatalf("run.failed events = %d", n)
			}
		})
	}
	for _, bad := range []string{ReasonInteractionExpired, "bogus", ""} {
		s := scopedStore(t)
		o := admitClaim(t, s, testCommand())
		if err := s.FinishFailed(ctx, o, nil, bad); !errors.Is(err, ErrState) {
			t.Fatalf("FinishFailed(%q) = %v", bad, err)
		}
		if run := onlyRun(t, s); run.State == RunFailed {
			t.Fatalf("rejected reason %q still failed the run", bad)
		}
	}
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	if err := s.Finish(ctx, o, RunFailed, nil, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if run := onlyRun(t, s); run.State != RunFailed || run.TerminalReason != "" {
		t.Fatalf("plain failure run = %+v", run)
	}
}

func TestCodeSnapshotLimitDetail(t *testing.T) {
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	if err := s.FinishLimitExceeded(context.Background(), o, nil, "budget", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	run := onlyRun(t, s)
	var detail struct {
		Kind              string `json:"kind"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	if err := json.Unmarshal(run.TerminalDetail, &detail); err != nil || run.TerminalReason != "budget" || detail.Kind != "budget" || detail.RetryAfterSeconds != 30 {
		t.Fatalf("run = %+v detail=%s %v", run, run.TerminalDetail, err)
	}
}

func TestExpiredInteractionReason(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	_, in := parkApproval(t, s)
	mustExec(t, s.pool, "UPDATE agent_interactions SET expires_at=clock_timestamp()-interval '1 second' WHERE id='i1'")
	if _, _, err := s.Admit(ctx, replyCommand(in, "r2", "approve")); err == nil {
		t.Fatal("reply after expiry succeeded")
	}
	snap, err := s.Snapshot(ctx, "t1", "p")
	if err != nil {
		t.Fatal(err)
	}
	var original *Run
	for i := range snap.Runs {
		if snap.Runs[i].RunID == "r1" {
			original = &snap.Runs[i]
		}
	}
	if original == nil || original.State != RunFailed || original.TerminalReason != ReasonInteractionExpired {
		t.Fatalf("original run = %+v", original)
	}
	n := 0
	for _, e := range snap.Events {
		if e.Type == "run.failed" && e.RunID == "r1" && e.Reason == ReasonInteractionExpired {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("run.failed events = %d", n)
	}
	if m := markerOf(t, s, "i1"); m != "denied" {
		t.Fatalf("marker = %q", m)
	}
}

func TestExpiredReplyReturnsErrExpired(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	_, in := parkApproval(t, s)
	mustExec(t, s.pool, "UPDATE agent_interactions SET expires_at=clock_timestamp()-interval '1 second' WHERE id='i1'")
	_, _, err := s.Admit(ctx, replyCommand(in, "r2", "approve"))
	if !errors.Is(err, ErrExpired) || !errors.Is(err, ErrConflict) {
		t.Fatalf("first reply after expiry: %v", err)
	}
	_, _, err = s.Admit(ctx, replyCommand(in, "r3", "approve"))
	if !errors.Is(err, ErrExpired) || !errors.Is(err, ErrConflict) {
		t.Fatalf("reply to expired interaction: %v", err)
	}

	s = scopedStore(t)
	_, in = parkApproval(t, s)
	admitClaim(t, s, replyCommand(in, "r2", "deny"))
	_, _, err = s.Admit(ctx, replyCommand(in, "r3", "approve"))
	if !errors.Is(err, ErrConflict) || errors.Is(err, ErrExpired) {
		t.Fatalf("reply to consumed interaction: %v", err)
	}
}
