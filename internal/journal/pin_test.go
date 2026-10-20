package journal

import (
	"context"
	"errors"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/journeys"
)

// pinOp inserts an operation row directly with the columns BeginAttempt writes (BeginAttempt itself refuses a
// second operation while one is unresolved). journey "" stores NULL.
func pinOp(t *testing.T, s *Store, o Owner, call, journey, outcome string) {
	t.Helper()
	var j any
	if journey != "" {
		j = journey
	}
	mustExec(t, s.pool, "INSERT INTO agent_operations(call_id,thread_id,run_id,alias,operation_key,arguments,binding,policy,requires_approval,outcome,journey_id) VALUES($1,$2,$3,'book','op','{}','placeholder-binding','read_only',false,$4,$5)",
		call, o.Command.ThreadID, o.Command.RunID, outcome, j)
}

func pinOwner(t *testing.T) (*Store, Owner) {
	t.Helper()
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	startScoped(t, s, o, "")
	return s, o
}

func TestPinUnresolvedOperationOutcomeUnknown(t *testing.T) {
	s, o := pinOwner(t)
	pinOp(t, s, o, "op1", "J", "outcome_unknown")
	_, pinned := activeSet(t, s, o)
	expectStrings(t, "pinned", pinned, "J")
}

func TestPinUnresolvedOperationDispatching(t *testing.T) {
	s, o := pinOwner(t)
	pinOp(t, s, o, "op1", "J", "dispatching")
	_, pinned := activeSet(t, s, o)
	expectStrings(t, "pinned", pinned, "J")
}

func TestPinResolvedOperationDoesNotPin(t *testing.T) {
	s, o := pinOwner(t)
	pinOp(t, s, o, "op1", "A", "completed")
	pinOp(t, s, o, "op2", "B", "rejected")
	pinOp(t, s, o, "op3", "C", "auth_required")
	_, pinned := activeSet(t, s, o)
	expectStrings(t, "pinned", pinned)
}

func TestPinUnresolvedOperationWithoutJourney(t *testing.T) {
	s, o := pinOwner(t)
	pinOp(t, s, o, "op1", "", "outcome_unknown")
	pinOp(t, s, o, "op2", "", "dispatching")
	mustExec(t, s.pool, "UPDATE agent_operations SET journey_id='' WHERE call_id='op2'")
	_, pinned := activeSet(t, s, o)
	expectStrings(t, "pinned", pinned)
}

func TestPinMergesApprovalAndOperationSortedDistinct(t *testing.T) {
	s, o := pinOwner(t)
	rawInteraction(t, s, o, approvalArtifact("i1", "c1", "B"), "B", "pending", "", "")
	pinOp(t, s, o, "op1", "B", "outcome_unknown")
	pinOp(t, s, o, "op2", "A", "dispatching")
	pinOp(t, s, o, "op3", "A", "outcome_unknown")
	_, pinned := activeSet(t, s, o)
	expectStrings(t, "pinned", pinned, "A", "B")
}

func TestPinOtherThreadDoesNotPin(t *testing.T) {
	s, o := pinOwner(t)
	o2 := admitClaim(t, s, threadCommand("t2", "r2", "c2", "hi"))
	startScoped(t, s, o2, "")
	pinOp(t, s, o2, "op1", "J", "outcome_unknown")
	_, pinned := activeSet(t, s, o)
	expectStrings(t, "pinned", pinned)
	_, pinned2 := activeSet(t, s, o2)
	expectStrings(t, "other thread pinned", pinned2, "J")
	stale := o
	stale.Epoch++
	if _, _, err := s.ActiveJourneys(context.Background(), stale); !errors.Is(err, ErrOwnership) {
		t.Fatalf("stale ActiveJourneys: %v", err)
	}
}

func TestPinSurvivesLoweredLimitAndEvictionPressure(t *testing.T) {
	s, o := pinOwner(t)
	if err := s.SetActiveJourneys(context.Background(), o, []string{"J", "K", "L"}); err != nil {
		t.Fatal(err)
	}
	pinOp(t, s, o, "op1", "J", "outcome_unknown")
	active, pinned := activeSet(t, s, o)
	expectStrings(t, "active", active, "J", "K", "L")
	expectStrings(t, "pinned", pinned, "J")

	set := journeys.NewActiveSet(active)
	evicted, err := set.Fit(1, pinned)
	if err != nil {
		t.Fatal(err)
	}
	expectStrings(t, "Fit evicted", evicted, "K", "L")
	expectStrings(t, "active after Fit", set.IDs(), "J")
	if _, err := set.Activate("M", 1, pinned); !errors.Is(err, journeys.ErrRefused) {
		t.Fatalf("Activate with only pinned remaining at the limit: %v, want ErrRefused", err)
	}
	expectStrings(t, "active after refusal", set.IDs(), "J")
}
