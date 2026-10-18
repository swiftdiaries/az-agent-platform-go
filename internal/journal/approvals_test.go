package journal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	testProductVersion = "2026.1"
	testApprovalArgs   = `{"x":1}`
	testApprovalKey    = "book.reserve"
)

var (
	testNameDigest      = NameDigest("book reserve")
	testSchemaDigest    = mustSchemaDigest(`{"type":"object","properties":{"x":{"type":"number"}}}`)
	clarifyNameDigest   = NameDigest("request_user_input")
	clarifySchemaDigest = mustSchemaDigest(`{"type":"object","properties":{"questions":{"type":"array"}}}`)
)

// startScopedWith pins the run to digest on journey and records its product version.
func startScopedWith(t *testing.T, s *Store, o Owner, journey, digest, version string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Start(ctx, o, journey, digest); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.RecordRunScope(ctx, o, RunScope{ProductVersion: version, EndpointID: "e1", Settings: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("RecordRunScope: %v", err)
	}
}

func startScoped(t *testing.T, s *Store, o Owner, journey string) {
	t.Helper()
	startScopedWith(t, s, o, journey, activeDigest, testProductVersion)
}

// bindingFor computes the v2 binding from the run's recorded database state.
func bindingFor(t *testing.T, s *Store, o Owner, journey, key, nameDigest, schemaDigest, args string) string {
	t.Helper()
	c := o.Command
	var version, digest, principal string
	if err := s.pool.QueryRow(context.Background(), "SELECT COALESCE(r.product_version,''),COALESCE(r.definition_digest,''),c.principal FROM agent_runs r JOIN agent_conversations c ON c.id=r.thread_id WHERE r.thread_id=$1 AND r.id=$2", c.ThreadID, c.RunID).Scan(&version, &digest, &principal); err != nil {
		t.Fatal(err)
	}
	b, err := BindCall(CallBinding{JourneyID: journey, OperationKey: key, NameDigest: nameDigest, SchemaDigest: schemaDigest,
		Arguments: json.RawMessage(args), ProductVersion: version, Principal: principal, Conversation: c.ThreadID, DefinitionSet: digest})
	if err != nil {
		t.Fatalf("BindCall: %v", err)
	}
	return b
}

// boundApproval is an approval whose binding matches the owner's run state.
func boundApproval(t *testing.T, s *Store, o Owner, id, callID, journey string) Interaction {
	t.Helper()
	return Interaction{ID: id, Kind: "approval", JourneyID: journey, OperationKey: testApprovalKey, NameDigest: testNameDigest, SchemaDigest: testSchemaDigest,
		Call: ProposedCall{CallID: callID, ProviderCallID: "prov-" + callID, Name: "book", Arguments: json.RawMessage(testApprovalArgs),
			Binding: bindingFor(t, s, o, journey, testApprovalKey, testNameDigest, testSchemaDigest, testApprovalArgs)}}
}

func boundClarification(t *testing.T, s *Store, o Owner, id, callID, journey string) Interaction {
	t.Helper()
	in := seedClarifyInteraction()
	in.ID, in.JourneyID = id, journey
	in.OperationKey, in.NameDigest, in.SchemaDigest = "request_user_input", clarifyNameDigest, clarifySchemaDigest
	in.Call.CallID, in.Call.ProviderCallID = callID, "prov-"+callID
	in.Call.Binding = bindingFor(t, s, o, journey, in.OperationKey, in.NameDigest, in.SchemaDigest, seedClarifyArguments)
	return in
}

// rebind recomputes op.Binding from op's fields and the owner's run state.
func rebind(t *testing.T, s *Store, o Owner, op Operation) Operation {
	t.Helper()
	op.Binding = bindingFor(t, s, o, op.JourneyID, op.OperationKey, op.NameDigest, op.SchemaDigest, op.Arguments)
	return op
}

// approvedOp is the operation matching boundApproval.
func approvedOp(t *testing.T, s *Store, o Owner, callID, journey string) Operation {
	t.Helper()
	return rebind(t, s, o, Operation{CallID: callID, Name: "book", OperationKey: testApprovalKey, NameDigest: testNameDigest, SchemaDigest: testSchemaDigest,
		Arguments: testApprovalArgs, Policy: "effectful", RequiresApproval: true, JourneyID: journey})
}

func wait(t *testing.T, s *Store, o Owner, in Interaction) error {
	t.Helper()
	return s.Wait(context.Background(), o, in, json.RawMessage(`{}`), json.RawMessage(`[]`))
}

// parkApproval runs r1 on journey A and parks it on approval i1 (call-1).
func parkApproval(t *testing.T, s *Store) (Owner, Interaction) {
	t.Helper()
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	startScoped(t, s, o, "A")
	drain(t, s, o)
	in := boundApproval(t, s, o, "i1", "call-1", "A")
	if err := wait(t, s, o, in); err != nil {
		t.Fatal(err)
	}
	return o, in
}

func replyCommand(in Interaction, run, decision string) Command {
	return Command{ThreadID: "t1", RunID: run, CommunicationID: "comm-" + run, Principal: "p", Tenant: "tenant-a", InteractionID: in.ID, ReplyKind: "approval",
		ReplyJSON: `{"decision":"` + decision + `","binding":"` + in.Call.Binding + `"}`}
}

func markerOf(t *testing.T, s *Store, id string) string {
	t.Helper()
	var m *string
	if err := s.pool.QueryRow(context.Background(), "SELECT marker FROM agent_interactions WHERE id=$1", id).Scan(&m); err != nil {
		t.Fatal(err)
	}
	if m == nil {
		return "<null>"
	}
	return *m
}

func operationRows(t *testing.T, s *Store, call string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM agent_operations WHERE call_id=$1", call).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func supersededEvents(t *testing.T, s *Store, thread string) []Event {
	t.Helper()
	events, err := s.Events(context.Background(), thread, "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range events {
		if e.Type == "interaction.superseded" {
			out = append(out, e)
		}
	}
	return out
}

func TestApprovalMarkersRequestedAtWait(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	startScoped(t, s, o, "A")
	if err := s.SetActiveJourneys(ctx, o, []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	catalog, err := s.CommitSnapshot(ctx, o, sampleSnapshot(testProductVersion, descsA))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRunCatalog(ctx, o, catalog); err != nil {
		t.Fatal(err)
	}
	drain(t, s, o)
	in := boundApproval(t, s, o, "i1", "call-1", "A")
	if err := wait(t, s, o, in); err != nil {
		t.Fatal(err)
	}
	var marker, key, name, schema, version, cat, active string
	if err := s.pool.QueryRow(ctx, "SELECT marker,operation_key,name_digest,schema_digest,product_version,catalog_digest,active_journeys::text FROM agent_interactions WHERE id='i1'").Scan(&marker, &key, &name, &schema, &version, &cat, &active); err != nil {
		t.Fatal(err)
	}
	if marker != "requested" || key != testApprovalKey || name != testNameDigest || schema != testSchemaDigest || version != testProductVersion || cat != catalog {
		t.Fatalf("columns = %q %q %q %q %q %q", marker, key, name, schema, version, cat)
	}
	sameJSON(t, active, `["A","B"]`)
	var artifact string
	if err := s.pool.QueryRow(ctx, "SELECT artifact::text FROM agent_interactions WHERE id='i1'").Scan(&artifact); err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{testApprovalKey, testNameDigest, testSchemaDigest} {
		if strings.Contains(artifact, leaked) {
			t.Fatalf("artifact leaks %q: %s", leaked, artifact)
		}
	}
}

func TestApprovalMarkersClarificationHasNoMarker(t *testing.T) {
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	startScoped(t, s, o, "A")
	drain(t, s, o)
	if err := wait(t, s, o, boundClarification(t, s, o, "i1", "call-1", "")); err != nil {
		t.Fatal(err)
	}
	if m := markerOf(t, s, "i1"); m != "<null>" {
		t.Fatalf("clarification marker = %q", m)
	}
	reply := Command{ThreadID: "t1", RunID: "r2", CommunicationID: "c2", Principal: "p", Tenant: "tenant-a", InteractionID: "i1", ReplyKind: "clarification",
		ReplyJSON: `{"answers":{"q1":{"option":"Alpha"}}}`}
	o2 := admitClaim(t, s, reply)
	if m := markerOf(t, s, "i1"); m != "<null>" {
		t.Fatalf("answered clarification marker = %q", m)
	}
	startScoped(t, s, o2, "A")
	cont, err := s.Continuation(context.Background(), o2)
	if err != nil || cont == nil || cont.Marker != "" || cont.Interaction.OperationKey != "request_user_input" || cont.Interaction.NameDigest != clarifyNameDigest {
		t.Fatalf("Continuation = %+v %v", cont, err)
	}
}

func TestApprovalMarkersDecidedThenDispatched(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	_, in := parkApproval(t, s)
	if m := markerOf(t, s, "i1"); m != "requested" {
		t.Fatalf("marker at Wait = %q", m)
	}
	o2 := admitClaim(t, s, replyCommand(in, "r2", "approve"))
	if m := markerOf(t, s, "i1"); m != "decided" {
		t.Fatalf("marker after approve = %q", m)
	}
	startScoped(t, s, o2, "A")
	cont, err := s.Continuation(ctx, o2)
	if err != nil || cont == nil || cont.Marker != "decided" || cont.Interaction.JourneyID != "A" || cont.Interaction.OperationKey != testApprovalKey ||
		cont.Interaction.NameDigest != testNameDigest || cont.Interaction.SchemaDigest != testSchemaDigest {
		t.Fatalf("Continuation = %+v %v", cont, err)
	}
	_, pinned := activeSet(t, s, o2)
	expectStrings(t, "pinned while decided", pinned, "A")

	op := approvedOp(t, s, o2, "call-1", "A")
	if op.Binding != in.Call.Binding {
		t.Fatal("test setup: operation and approval bindings differ")
	}
	if err := s.BeginAttempt(ctx, o2, op, 1, "i1"); err != nil {
		t.Fatalf("BeginAttempt: %v", err)
	}
	if m := markerOf(t, s, "i1"); m != "dispatched" || operationRows(t, s, "call-1") != 1 {
		t.Fatalf("marker = %q, operation rows = %d", m, operationRows(t, s, "call-1"))
	}
	_, pinned = activeSet(t, s, o2)
	expectStrings(t, "pinned after dispatch", pinned)
	// a dispatched approval cannot authorize again
	if err := s.BeginAttempt(ctx, o2, op, 1, "i1"); !errors.Is(err, ErrState) {
		t.Fatalf("second attempt-1: %v", err)
	}
	if err := s.EndAttempt(ctx, o2, op, 1, "completed"); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalMarkersRequestedIsNotDispatchable(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	_, in := parkApproval(t, s)
	o2 := admitClaim(t, s, replyCommand(in, "r2", "approve"))
	startScoped(t, s, o2, "A")
	// forge a consumed approval whose marker was never advanced past requested
	mustExec(t, s.pool, "UPDATE agent_interactions SET marker='requested' WHERE id='i1'")
	op := approvedOp(t, s, o2, "call-1", "A")
	if err := s.BeginAttempt(ctx, o2, op, 1, "i1"); !errors.Is(err, ErrState) {
		t.Fatalf("requested marker: %v", err)
	}
	if operationRows(t, s, "call-1") != 0 || markerOf(t, s, "i1") != "requested" {
		t.Fatal("rejected attempt changed state")
	}
}

func TestApprovalMarkersDenied(t *testing.T) {
	s := scopedStore(t)
	_, in := parkApproval(t, s)
	o2 := admitClaim(t, s, replyCommand(in, "r2", "deny"))
	if m := markerOf(t, s, "i1"); m != "denied" {
		t.Fatalf("marker after deny = %q", m)
	}
	_, pinned := activeSet(t, s, o2)
	expectStrings(t, "pinned by deny", pinned)
}

func TestApprovalMarkersDeniedOnExpiry(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	_, in := parkApproval(t, s)
	mustExec(t, s.pool, "UPDATE agent_interactions SET expires_at=clock_timestamp()-interval '1 second' WHERE id='i1'")
	if _, _, err := s.Admit(ctx, replyCommand(in, "r2", "approve")); !errors.Is(err, ErrConflict) {
		t.Fatalf("reply to expired approval: %v", err)
	}
	var state string
	if err := s.pool.QueryRow(ctx, "SELECT state FROM agent_interactions WHERE id='i1'").Scan(&state); err != nil || state != "expired" {
		t.Fatalf("state = %q %v", state, err)
	}
	if m := markerOf(t, s, "i1"); m != "denied" {
		t.Fatalf("marker after expiry = %q", m)
	}
}

func TestApprovalMarkersSupersededOnDrift(t *testing.T) {
	d2 := strings.Repeat("f", 64)
	cases := []struct {
		name    string
		digest  string // definition set the continuation run is started under
		version string
		repin   bool
		journey string // operation journey
		mutate  func(*Operation)
	}{
		{name: "definition set re-pinned", digest: d2, version: testProductVersion, repin: true, journey: "A"},
		{name: "product version", digest: activeDigest, version: "2026.2", journey: "A"},
		{name: "journey", digest: activeDigest, version: testProductVersion, journey: "B"},
		{name: "name digest", digest: activeDigest, version: testProductVersion, journey: "A", mutate: func(op *Operation) { op.NameDigest = NameDigest("book other") }},
		{name: "schema digest", digest: activeDigest, version: testProductVersion, journey: "A", mutate: func(op *Operation) { op.SchemaDigest = mustSchemaDigest(`{"type":"string"}`) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := scopedStore(t)
			_, in := parkApproval(t, s)
			o2 := admitClaim(t, s, replyCommand(in, "r2", "approve"))
			if tc.repin {
				mustExec(t, s.pool, "UPDATE agent_conversations SET definition_digest=$1 WHERE id='t1'", tc.digest)
			}
			startScopedWith(t, s, o2, "A", tc.digest, tc.version)
			op := approvedOp(t, s, o2, "call-1", tc.journey)
			if tc.mutate != nil {
				tc.mutate(&op)
				op = rebind(t, s, o2, op)
			}
			if err := s.BeginAttempt(ctx, o2, op, 1, "i1"); !errors.Is(err, ErrSuperseded) {
				t.Fatalf("BeginAttempt = %v, want ErrSuperseded", err)
			}
			if m := markerOf(t, s, "i1"); m != "superseded" {
				t.Fatalf("marker = %q (the superseded marker must commit)", m)
			}
			if operationRows(t, s, "call-1") != 0 {
				t.Fatal("a superseded approval wrote an operation")
			}
			var attempts int
			if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM agent_attempts WHERE call_id='call-1'").Scan(&attempts); err != nil || attempts != 0 {
				t.Fatalf("attempts = %d %v", attempts, err)
			}
			events := supersededEvents(t, s, "t1")
			if len(events) != 1 || events[0].CallID != "call-1" || events[0].RunID != "r2" {
				t.Fatalf("superseded events = %+v", events)
			}
			if _, pinned := activeSet(t, s, o2); len(pinned) != 0 {
				t.Fatalf("a superseded approval pins %v", pinned)
			}
			// it cannot be retried
			if err := s.BeginAttempt(ctx, o2, op, 1, "i1"); !errors.Is(err, ErrState) {
				t.Fatalf("retry after supersede: %v", err)
			}
		})
	}
}

func TestApprovalMarkersWaitRejectsBindingMismatch(t *testing.T) {
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	startScoped(t, s, o, "A")
	drain(t, s, o)
	good := boundApproval(t, s, o, "i1", "call-1", "A")

	mutations := map[string]func(*Interaction){
		"journey":       func(i *Interaction) { i.JourneyID = "B" },
		"operation key": func(i *Interaction) { i.OperationKey = "other.key" },
		"name digest":   func(i *Interaction) { i.NameDigest = NameDigest("x") },
		"schema digest": func(i *Interaction) { i.SchemaDigest = mustSchemaDigest(`{}`) },
		"arguments":     func(i *Interaction) { i.Call.Arguments = json.RawMessage(`{"x":2}`) },
		"binding":       func(i *Interaction) { i.Call.Binding = strings.Repeat("0", 64) },
		"v1 binding":    func(i *Interaction) { i.Call.Binding = mustBinding("book", json.RawMessage(testApprovalArgs)) },
	}
	for name, mutate := range mutations {
		in := good
		mutate(&in)
		if err := wait(t, s, o, in); !errors.Is(err, ErrState) {
			t.Fatalf("%s: Wait = %v, want ErrState", name, err)
		}
	}
	var n int
	if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM agent_interactions").Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejected Waits left %d interactions (%v)", n, err)
	}
	// the run is still owned and can wait with the correct binding
	if err := wait(t, s, o, good); err != nil {
		t.Fatalf("matching Wait: %v", err)
	}
}

func TestApprovalMarkersWaitRequiresRecordedScope(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	if _, err := s.Start(ctx, o, "A", activeDigest); err != nil {
		t.Fatal(err)
	}
	drain(t, s, o)
	in := Interaction{ID: "i1", Kind: "approval", JourneyID: "A", OperationKey: testApprovalKey, NameDigest: testNameDigest, SchemaDigest: testSchemaDigest,
		Call: ProposedCall{CallID: "call-1", ProviderCallID: "p1", Name: "book", Arguments: json.RawMessage(testApprovalArgs), Binding: strings.Repeat("a", 64)}}
	if err := wait(t, s, o, in); !errors.Is(err, ErrState) {
		t.Fatalf("Wait without recorded scope = %v, want ErrState", err)
	}
	// a clarification needs the scope too
	clar := seedClarifyInteraction()
	if err := wait(t, s, o, clar); !errors.Is(err, ErrState) {
		t.Fatalf("clarification Wait without scope = %v", err)
	}
}

func TestApprovalMarkersWaitRequiresJourneyAndKey(t *testing.T) {
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	startScoped(t, s, o, "A")
	drain(t, s, o)
	noJourney := boundApproval(t, s, o, "i1", "call-1", "")
	if err := wait(t, s, o, noJourney); !errors.Is(err, ErrState) {
		t.Fatalf("approval without journey = %v", err)
	}
	noKey := boundApproval(t, s, o, "i1", "call-1", "A")
	noKey.OperationKey = ""
	if err := wait(t, s, o, noKey); !errors.Is(err, ErrState) {
		t.Fatalf("approval without operation key = %v", err)
	}
}

func TestApprovalMarkersSupersede(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	_, in := parkApproval(t, s)

	// pending interactions cannot be superseded (the owner is r1's continuation only)
	o2 := admitClaim(t, s, replyCommand(in, "r2", "approve"))
	startScoped(t, s, o2, "A")

	stale := o2
	stale.Epoch++
	if err := s.Supersede(ctx, stale, "i1"); !errors.Is(err, ErrOwnership) {
		t.Fatalf("stale owner: %v", err)
	}
	if m := markerOf(t, s, "i1"); m != "decided" {
		t.Fatalf("stale supersede changed the marker to %q", m)
	}
	if err := s.Supersede(ctx, o2, "missing"); !errors.Is(err, ErrState) {
		t.Fatalf("unknown interaction: %v", err)
	}
	if err := s.Supersede(ctx, o2, "i1"); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if m := markerOf(t, s, "i1"); m != "superseded" {
		t.Fatalf("marker = %q", m)
	}
	if err := s.Supersede(ctx, o2, "i1"); err != nil {
		t.Fatalf("second Supersede: %v", err)
	}
	events := supersededEvents(t, s, "t1")
	if len(events) != 1 || events[0].CallID != "call-1" {
		t.Fatalf("events = %+v", events)
	}
	if _, pinned := activeSet(t, s, o2); len(pinned) != 0 {
		t.Fatalf("superseded approval pins %v", pinned)
	}
	if operationRows(t, s, "call-1") != 0 {
		t.Fatal("Supersede wrote an operation")
	}
}

func TestApprovalMarkersSupersedeStates(t *testing.T) {
	ctx := context.Background()
	t.Run("pending", func(t *testing.T) {
		s := scopedStore(t)
		_, in := parkApproval(t, s)
		o2 := admitClaim(t, s, replyCommand(in, "r2", "approve"))
		// put the approval back to pending: a pending approval cannot be superseded
		mustExec(t, s.pool, "UPDATE agent_interactions SET state='pending',marker='requested',continuation_run_id=NULL WHERE id='i1'")
		if err := s.Supersede(ctx, o2, "i1"); !errors.Is(err, ErrState) {
			t.Fatalf("foreign/pending: %v", err)
		}
		if m := markerOf(t, s, "i1"); m != "requested" {
			t.Fatalf("marker = %q", m)
		}
	})
	t.Run("dispatched", func(t *testing.T) {
		s := scopedStore(t)
		_, in := parkApproval(t, s)
		o2 := admitClaim(t, s, replyCommand(in, "r2", "approve"))
		startScoped(t, s, o2, "A")
		if err := s.BeginAttempt(ctx, o2, approvedOp(t, s, o2, "call-1", "A"), 1, "i1"); err != nil {
			t.Fatal(err)
		}
		if err := s.Supersede(ctx, o2, "i1"); !errors.Is(err, ErrState) {
			t.Fatalf("dispatched: %v", err)
		}
		if m := markerOf(t, s, "i1"); m != "dispatched" {
			t.Fatalf("marker = %q", m)
		}
	})
	t.Run("denied", func(t *testing.T) {
		s := scopedStore(t)
		_, in := parkApproval(t, s)
		o2 := admitClaim(t, s, replyCommand(in, "r2", "deny"))
		if err := s.Supersede(ctx, o2, "i1"); !errors.Is(err, ErrState) {
			t.Fatalf("denied: %v", err)
		}
	})
	t.Run("another run's approval", func(t *testing.T) {
		s := scopedStore(t)
		_, in := parkApproval(t, s)
		o2 := admitClaim(t, s, replyCommand(in, "r2", "approve"))
		if err := s.Finish(ctx, o2, RunFailed, nil, "", "", ""); err != nil {
			t.Fatal(err)
		}
		o3 := admitClaim(t, s, threadCommand("t1", "r3", "c3", "again"))
		if err := s.Supersede(ctx, o3, "i1"); !errors.Is(err, ErrState) {
			t.Fatalf("approval consumed by another run: %v", err)
		}
	})
}
