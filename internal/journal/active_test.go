package journal

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

var activeDigest = strings.Repeat("d", 64)

// approvalArtifact is an approval with a placeholder binding, for rows inserted directly.
func approvalArtifact(id, callID, journey string) Interaction {
	return Interaction{ID: id, Kind: "approval", JourneyID: journey, Call: ProposedCall{
		CallID: callID, ProviderCallID: "prov-" + callID, Name: "book", Arguments: json.RawMessage(`{"x":1}`), Binding: "placeholder-binding",
	}}
}

func clarificationArtifact(id, callID, journey string) Interaction {
	in := seedClarifyInteraction()
	in.ID, in.JourneyID = id, journey
	in.Call.CallID, in.Call.ProviderCallID = callID, "prov-"+callID
	return in
}

// rawInteraction inserts an interaction row directly; journey "" stores NULL.
func rawInteraction(t *testing.T, s *Store, o Owner, in Interaction, journey, state, continuation, reply string) {
	t.Helper()
	artifact, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var j, cont, rep any
	if journey != "" {
		j = journey
	}
	if continuation != "" {
		cont = continuation
	}
	if reply != "" {
		rep = reply
	}
	var marker any
	if in.Kind == "approval" {
		switch {
		case state == "pending":
			marker = "requested"
		case strings.Contains(reply, `"approve"`):
			marker = "decided"
		default:
			marker = "denied"
		}
	}
	mustExec(t, s.pool, "INSERT INTO agent_interactions(id,thread_id,run_id,journey_id,definition_digest,kind,artifact,checkpoint,history,expires_at,state,continuation_run_id,reply,marker) VALUES($1,$2,$3,$4,$5,$6,$7,'{}','[]',clock_timestamp()+interval '1 hour',$8,$9,$10,$11)",
		in.ID, o.Command.ThreadID, o.Command.RunID, j, activeDigest, in.Kind, artifact, state, cont, rep, marker)
}

func activeSet(t *testing.T, s *Store, o Owner) (active, pinned []string) {
	t.Helper()
	active, pinned, err := s.ActiveJourneys(context.Background(), o)
	if err != nil {
		t.Fatalf("ActiveJourneys: %v", err)
	}
	return active, pinned
}

func expectStrings(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func TestActiveJourneysRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))

	active, pinned := activeSet(t, s, o)
	expectStrings(t, "fresh active", active)
	expectStrings(t, "fresh pinned", pinned)

	if err := s.SetActiveJourneys(ctx, o, []string{"B", "A", "C"}); err != nil {
		t.Fatal(err)
	}
	active, _ = activeSet(t, s, o)
	expectStrings(t, "active", active, "B", "A", "C")

	snap, err := s.Snapshot(ctx, "t1", "p")
	if err != nil {
		t.Fatal(err)
	}
	expectStrings(t, "snapshot active", snap.ActiveJourneys, "B", "A", "C")

	if err := s.SetActiveJourneys(ctx, o, []string{"A"}); err != nil {
		t.Fatal(err)
	}
	active, _ = activeSet(t, s, o)
	expectStrings(t, "replaced active", active, "A")

	if err := s.SetActiveJourneys(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.pool.QueryRow(ctx, "SELECT active_journeys::text FROM agent_conversations WHERE id='t1'").Scan(&stored); err != nil || stored != "[]" {
		t.Fatalf("nil stored as %q %v, want []", stored, err)
	}
	active, _ = activeSet(t, s, o)
	expectStrings(t, "cleared active", active)
}

func TestActiveJourneysInvalidSetWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	if err := s.SetActiveJourneys(ctx, o, []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]string{
		"duplicate": {"A", "C", "A"},
		"empty":     {"A", ""},
		"only":      {""},
	} {
		if err := s.SetActiveJourneys(ctx, o, bad); !errors.Is(err, ErrState) {
			t.Fatalf("%s: err = %v, want ErrState", name, err)
		}
		active, _ := activeSet(t, s, o)
		expectStrings(t, name+" left active", active, "A", "B")
	}
}

func TestActiveJourneysFenced(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	if err := s.SetActiveJourneys(ctx, o, []string{"A"}); err != nil {
		t.Fatal(err)
	}
	stale := o
	stale.Epoch++
	if _, _, err := s.ActiveJourneys(ctx, stale); !errors.Is(err, ErrOwnership) {
		t.Fatalf("stale ActiveJourneys: %v", err)
	}
	if err := s.SetActiveJourneys(ctx, stale, []string{"Z"}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("stale SetActiveJourneys: %v", err)
	}
	wrongID := o
	wrongID.ID = "other"
	if err := s.SetActiveJourneys(ctx, wrongID, []string{"Z"}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("wrong owner SetActiveJourneys: %v", err)
	}
	active, _ := activeSet(t, s, o)
	expectStrings(t, "active after rejected writes", active, "A")

	if _, err := s.Start(ctx, o, "", activeDigest); err != nil {
		t.Fatal(err)
	}
	drain(t, s, o)
	if err := s.Finish(ctx, o, RunCompleted, nil, "done", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ActiveJourneys(ctx, o); !errors.Is(err, ErrOwnership) {
		t.Fatalf("finished-run ActiveJourneys: %v", err)
	}
	if err := s.SetActiveJourneys(ctx, o, []string{"Z"}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("finished-run SetActiveJourneys: %v", err)
	}
}

func TestActiveJourneysPinnedByPendingApproval(t *testing.T) {
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	rawInteraction(t, s, o, approvalArtifact("i1", "call-1", ""), "A", "pending", "", "")
	_, pinned := activeSet(t, s, o)
	expectStrings(t, "pinned", pinned, "A")
}

func TestActiveJourneysNotPinnedByClarificationOrJourneyless(t *testing.T) {
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	rawInteraction(t, s, o, clarificationArtifact("i1", "call-1", ""), "A", "pending", "", "")
	_, pinned := activeSet(t, s, o)
	expectStrings(t, "pinned by clarification", pinned)

	mustExec(t, s.pool, "UPDATE agent_interactions SET state='expired' WHERE id='i1'")
	rawInteraction(t, s, o, approvalArtifact("i2", "call-2", ""), "", "pending", "", "")
	_, pinned = activeSet(t, s, o)
	expectStrings(t, "pinned by journeyless approval", pinned)
}

func TestActiveJourneysPinnedSortedDistinct(t *testing.T) {
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	reply := `{"decision":"approve","binding":"x"}`
	rawInteraction(t, s, o, approvalArtifact("i1", "call-1", ""), "B", "consumed", "r1", reply)
	rawInteraction(t, s, o, approvalArtifact("i2", "call-2", ""), "A", "consumed", "r1", reply)
	rawInteraction(t, s, o, approvalArtifact("i3", "call-3", ""), "B", "consumed", "r1", reply)
	rawInteraction(t, s, o, approvalArtifact("i4", "call-4", ""), "C", "pending", "", "")
	_, pinned := activeSet(t, s, o)
	expectStrings(t, "pinned", pinned, "A", "B", "C")
}

func TestActiveJourneysPinnedByConsumedApproveUntilDispatched(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o1 := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	startScoped(t, s, o1, "A")
	drain(t, s, o1)
	in := boundApproval(t, s, o1, "i1", "call-1", "A")
	if err := s.Wait(ctx, o1, in, json.RawMessage(`{}`), json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}
	reply := Command{ThreadID: "t1", RunID: "r2", CommunicationID: "c2", Principal: "p", Tenant: "tenant-a", InteractionID: "i1", ReplyKind: "approval",
		ReplyJSON: `{"decision":"approve","binding":"` + in.Call.Binding + `"}`}
	o2 := admitClaim(t, s, reply)
	startScoped(t, s, o2, "A")

	_, pinned := activeSet(t, s, o2)
	expectStrings(t, "pinned before dispatch", pinned, "A")

	if err := s.BeginAttempt(ctx, o2, approvedOp(t, s, o2, "call-1", "A"), 1, "i1"); err != nil {
		t.Fatal(err)
	}
	_, pinned = activeSet(t, s, o2)
	expectStrings(t, "pinned after BeginAttempt", pinned)
}

func TestActiveJourneysConsumedPinIsPerRun(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o1 := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	startScoped(t, s, o1, "A")
	drain(t, s, o1)
	in := boundApproval(t, s, o1, "i1", "call-1", "A")
	if err := s.Wait(ctx, o1, in, json.RawMessage(`{}`), json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}
	reply := Command{ThreadID: "t1", RunID: "r2", CommunicationID: "c2", Principal: "p", Tenant: "tenant-a", InteractionID: "i1", ReplyKind: "approval",
		ReplyJSON: `{"decision":"approve","binding":"` + in.Call.Binding + `"}`}
	o2 := admitClaim(t, s, reply)
	if err := s.Finish(ctx, o2, RunCompleted, nil, "done", "", ""); err != nil {
		t.Fatal(err)
	}
	// An unrelated later run: the consumed approval of r2 is not its concern.
	o3 := admitClaim(t, s, threadCommand("t1", "r3", "c3", "again"))
	_, pinned := activeSet(t, s, o3)
	expectStrings(t, "pinned for another run", pinned)
}

func TestActiveJourneysNotPinnedByDenyOrClarification(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o1 := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	startScoped(t, s, o1, "A")
	drain(t, s, o1)
	in := boundApproval(t, s, o1, "i1", "call-1", "A")
	if err := s.Wait(ctx, o1, in, json.RawMessage(`{}`), json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}
	deny := Command{ThreadID: "t1", RunID: "r2", CommunicationID: "c2", Principal: "p", Tenant: "tenant-a", InteractionID: "i1", ReplyKind: "approval",
		ReplyJSON: `{"decision":"deny","binding":"` + in.Call.Binding + `"}`}
	o2 := admitClaim(t, s, deny)
	_, pinned := activeSet(t, s, o2)
	expectStrings(t, "pinned by deny", pinned)
	drain(t, s, o2)
	if err := s.Finish(ctx, o2, RunCompleted, nil, "done", "", ""); err != nil {
		t.Fatal(err)
	}

	o3 := admitClaim(t, s, threadCommand("t1", "r3", "c3", "more"))
	startScoped(t, s, o3, "A")
	drain(t, s, o3)
	clar := boundClarification(t, s, o3, "i2", "call-2", "A")
	if err := s.Wait(ctx, o3, clar, json.RawMessage(`{}`), json.RawMessage(`[]`)); err != nil {
		t.Fatal(err)
	}
	answer := Command{ThreadID: "t1", RunID: "r4", CommunicationID: "c4", Principal: "p", Tenant: "tenant-a", InteractionID: "i2", ReplyKind: "clarification",
		ReplyJSON: `{"answers":{"q1":{"option":"Alpha"}}}`}
	o4 := admitClaim(t, s, answer)
	_, pinned = activeSet(t, s, o4)
	expectStrings(t, "pinned by clarification", pinned)
}

func TestStartWithoutJourney(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	history, err := s.Start(ctx, o, "", activeDigest)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, string(history), `[]`)
	var journey *string
	var digest string
	if err := s.pool.QueryRow(ctx, "SELECT journey_id,definition_digest FROM agent_runs WHERE id='r1'").Scan(&journey, &digest); err != nil {
		t.Fatal(err)
	}
	if journey != nil || digest != activeDigest {
		t.Fatalf("run journey=%v digest=%q, want NULL and the digest", journey, digest)
	}
	if pin, ok := conversationPin(t, s, "t1"); !ok || pin != activeDigest {
		t.Fatalf("conversation pin = %q %v", pin, ok)
	}

	// Finish writes history for a run with a definition pin and no journey.
	drain(t, s, o)
	const h = `[{"role":"user","contents":[{"type":"text","text":"H"}]}]`
	if err := s.Finish(ctx, o, RunCompleted, json.RawMessage(h), "done", "", ""); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, conversationHistory(t, s, "t1"), h)
	if got := runByID(t, s, "r1"); got.JourneyID != "" || got.State != RunCompleted {
		t.Fatalf("run = %+v", got)
	}
}

func TestStartWithoutJourneyKeepsExistingJourney(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	mustExec(t, s.pool, "UPDATE agent_runs SET journey_id='J',definition_digest=$1 WHERE id='r1'", activeDigest)
	if _, err := s.Start(ctx, o, "", activeDigest); err != nil {
		t.Fatal(err)
	}
	var journey string
	if err := s.pool.QueryRow(ctx, "SELECT journey_id FROM agent_runs WHERE id='r1'").Scan(&journey); err != nil || journey != "J" {
		t.Fatalf("journey = %q %v, want J", journey, err)
	}
	if _, err := s.Start(ctx, o, "K", activeDigest); !errors.Is(err, ErrDefinition) {
		t.Fatalf("conflicting journey: %v", err)
	}
}

func TestFinishWithoutDefinitionPinKeepsHistory(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	drain(t, s, o)
	if err := s.Finish(ctx, o, RunFailed, json.RawMessage(`[{"role":"user","contents":[]}]`), "", "", ""); err != nil {
		t.Fatal(err)
	}
	sameJSON(t, conversationHistory(t, s, "t1"), `[]`)
}

func interactionJourney(t *testing.T, s *Store, id string) (journey *string) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(), "SELECT journey_id FROM agent_interactions WHERE id=$1", id).Scan(&journey); err != nil {
		t.Fatal(err)
	}
	return journey
}

func TestWaitInteractionJourney(t *testing.T) {
	ctx := context.Background()
	wait := func(t *testing.T, s *Store, thread, runJourney string, build func(Owner) Interaction) Owner {
		t.Helper()
		o := admitClaim(t, s, threadCommand(thread, "r-"+thread, "c-"+thread, "hi"))
		startScoped(t, s, o, runJourney)
		drain(t, s, o)
		if err := s.Wait(ctx, o, build(o), json.RawMessage(`{}`), json.RawMessage(`[]`)); err != nil {
			t.Fatal(err)
		}
		return o
	}
	approval := func(s *Store, id, call, journey string) func(Owner) Interaction {
		return func(o Owner) Interaction { return boundApproval(t, s, o, id, call, journey) }
	}
	clarification := func(s *Store, id, call, journey string) func(Owner) Interaction {
		return func(o Owner) Interaction { return boundClarification(t, s, o, id, call, journey) }
	}
	s := scopedStore(t)

	t.Run("interaction journey on a journeyless run", func(t *testing.T) {
		wait(t, s, "t1", "", approval(s, "i1", "call-1", "A"))
		if j := interactionJourney(t, s, "i1"); j == nil || *j != "A" {
			t.Fatalf("journey = %v, want A", j)
		}
		var artifact string
		if err := s.pool.QueryRow(ctx, "SELECT artifact::text FROM agent_interactions WHERE id='i1'").Scan(&artifact); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(artifact), "journey") {
			t.Fatalf("artifact leaks the journey: %s", artifact)
		}
	})
	t.Run("interaction journey overrides the run journey", func(t *testing.T) {
		wait(t, s, "t2", "B", approval(s, "i2", "call-2", "A"))
		if j := interactionJourney(t, s, "i2"); j == nil || *j != "A" {
			t.Fatalf("journey = %v, want A", j)
		}
	})
	t.Run("empty clarification journey falls back to the run journey", func(t *testing.T) {
		wait(t, s, "t3", "B", clarification(s, "i3", "call-3", ""))
		if j := interactionJourney(t, s, "i3"); j == nil || *j != "B" {
			t.Fatalf("journey = %v, want B", j)
		}
	})
	t.Run("no journey anywhere stores NULL", func(t *testing.T) {
		wait(t, s, "t4", "", clarification(s, "i4", "call-4", ""))
		if j := interactionJourney(t, s, "i4"); j != nil {
			t.Fatalf("journey = %q, want NULL", *j)
		}
	})
}

func TestReplyTargetJourney(t *testing.T) {
	ctx := context.Background()
	park := func(t *testing.T, s *Store, journey string) {
		t.Helper()
		o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
		startScoped(t, s, o, "")
		drain(t, s, o)
		in := boundClarification(t, s, o, "i1", "call-1", journey)
		if err := s.Wait(ctx, o, in, json.RawMessage(`{}`), json.RawMessage(`[]`)); err != nil {
			t.Fatal(err)
		}
	}
	reply := func(target string) Command {
		return Command{ThreadID: "t1", RunID: "r2", CommunicationID: "c2", Principal: "p", Tenant: "tenant-a", TargetJourney: target, InteractionID: "i1", ReplyKind: "clarification",
			ReplyJSON: `{"answers":{"q1":{"option":"Alpha"}}}`}
	}

	t.Run("both non-empty and different", func(t *testing.T) {
		s := scopedStore(t)
		park(t, s, "A")
		if _, _, err := s.Admit(ctx, reply("B")); !errors.Is(err, ErrState) {
			t.Fatalf("mismatch: %v", err)
		}
	})
	t.Run("matching target", func(t *testing.T) {
		s := scopedStore(t)
		park(t, s, "A")
		if _, fresh, err := s.Admit(ctx, reply("A")); err != nil || !fresh {
			t.Fatalf("match: fresh=%v %v", fresh, err)
		}
	})
	t.Run("interaction without journey accepts any target", func(t *testing.T) {
		s := scopedStore(t)
		park(t, s, "")
		if _, fresh, err := s.Admit(ctx, reply("B")); err != nil || !fresh {
			t.Fatalf("NULL journey with target: fresh=%v %v", fresh, err)
		}
		var journey *string
		var digest string
		if err := s.pool.QueryRow(ctx, "SELECT journey_id,definition_digest FROM agent_runs WHERE id='r2'").Scan(&journey, &digest); err != nil {
			t.Fatal(err)
		}
		if journey != nil || digest != activeDigest {
			t.Fatalf("continuation run journey=%v digest=%q", journey, digest)
		}
	})
	t.Run("empty target", func(t *testing.T) {
		s := scopedStore(t)
		park(t, s, "A")
		if _, fresh, err := s.Admit(ctx, reply("")); err != nil || !fresh {
			t.Fatalf("no target: fresh=%v %v", fresh, err)
		}
	})
}

func TestBeginAttemptOperationJourney(t *testing.T) {
	ctx := context.Background()
	opJourney := func(t *testing.T, s *Store, call string) *string {
		t.Helper()
		var j *string
		if err := s.pool.QueryRow(ctx, "SELECT journey_id FROM agent_operations WHERE call_id=$1", call).Scan(&j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	readOnly := func(t *testing.T, s *Store, o Owner, call, journey string) Operation {
		op := policyOp(t, s, o, call, "read_only")
		op.JourneyID = journey
		return rebind(t, s, o, op)
	}

	t.Run("operation journey is stored", func(t *testing.T) {
		s := scopedStore(t)
		o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
		startScoped(t, s, o, "")
		if err := s.BeginAttempt(ctx, o, readOnly(t, s, o, "op1", "J"), 1, ""); err != nil {
			t.Fatal(err)
		}
		if j := opJourney(t, s, "op1"); j == nil || *j != "J" {
			t.Fatalf("journey = %v, want J", j)
		}
	})
	t.Run("operation journey overrides the run journey", func(t *testing.T) {
		s := scopedStore(t)
		o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
		startScoped(t, s, o, "R")
		if err := s.BeginAttempt(ctx, o, readOnly(t, s, o, "op1", "J"), 1, ""); err != nil {
			t.Fatal(err)
		}
		if j := opJourney(t, s, "op1"); j == nil || *j != "J" {
			t.Fatalf("journey = %v, want J", j)
		}
	})
	t.Run("empty operation journey falls back to the run journey", func(t *testing.T) {
		s := scopedStore(t)
		o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
		startScoped(t, s, o, "R")
		if err := s.BeginAttempt(ctx, o, readOnly(t, s, o, "op1", ""), 1, ""); err != nil {
			t.Fatal(err)
		}
		if j := opJourney(t, s, "op1"); j == nil || *j != "R" {
			t.Fatalf("journey = %v, want R", j)
		}
	})
	t.Run("no journey anywhere stores NULL", func(t *testing.T) {
		s := scopedStore(t)
		o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
		startScoped(t, s, o, "")
		if err := s.BeginAttempt(ctx, o, readOnly(t, s, o, "op1", ""), 1, ""); err != nil {
			t.Fatal(err)
		}
		if j := opJourney(t, s, "op1"); j != nil {
			t.Fatalf("journey = %q, want NULL", *j)
		}
	})
}
