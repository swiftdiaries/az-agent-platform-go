package journal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

// Identifiers seeded by seedPre000009. A later test drives the same data
// through the Store API, so they are package-level.
var (
	seedDigestA         = strings.Repeat("a", 64)
	seedDigestB         = strings.Repeat("b", 64)
	seedCatalogDigest   = strings.Repeat("9", 64)
	seedApprovalBinding = mustBinding("book", json.RawMessage(`{"x":1}`))
)

const (
	seedPrincipal = "p"

	seedHistoryA  = `[{"role":"user","contents":[{"type":"text","text":"from A"}]}]`
	seedHistoryB  = `[{"role":"user","contents":[{"type":"text","text":"from B"}]}]`
	seedHistoryAp = `[{"role":"user","contents":[{"type":"text","text":"from approval"}]}]`
	seedHistoryC  = `[{"role":"user","contents":[{"type":"text","text":"from clarify"}]}]`

	seedApprovalInteractionID = "interaction-approval"
	seedApprovalCallID        = "call-approve"
	seedApprovalProviderCall  = "prov-approve"
	seedApprovalCallName      = "book"
	seedApprovalArguments     = `{"x":1}`

	seedClarifyInteractionID = "interaction-clarify"
	seedClarifyCallID        = "call-clarify"
	seedClarifyProviderCall  = "prov-clarify"
	seedClarifyCallName      = "request_user_input"
	seedClarifyArguments     = `{"questions":[{"id":"q1","header":"Pick one","question":"Which option?","options":[{"label":"Alpha","description":"The first option"},{"label":"Beta","description":"The second option"}]}]}`

	seedCheckpoint = `{"mode":"product_resume_in_place","repairs":1}`
)

// mustBinding is the legacy (v1) approval binding the pre-000011 code computed: sha256(name \x00 json(arguments)),
// with json.Number preserved. ActionBinding is gone; migrated rows still carry this form.
func mustBinding(name string, arguments json.RawMessage) string {
	d := json.NewDecoder(bytes.NewReader(arguments))
	d.UseNumber()
	var value map[string]any
	if err := d.Decode(&value); err != nil {
		panic(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(append([]byte(name+"\x00"), data...))
	return hex.EncodeToString(sum[:])
}

// seedApprovalInteraction is the artifact stored for interaction-approval.
func seedApprovalInteraction() Interaction {
	return Interaction{ID: seedApprovalInteractionID, Kind: "approval", Call: ProposedCall{
		CallID: seedApprovalCallID, ProviderCallID: seedApprovalProviderCall, Name: seedApprovalCallName,
		Arguments: json.RawMessage(seedApprovalArguments), Binding: seedApprovalBinding,
	}}
}

// seedClarifyInteraction is the artifact stored for interaction-clarify.
func seedClarifyInteraction() Interaction {
	return Interaction{ID: seedClarifyInteractionID, Kind: "clarification",
		Questions: []Question{{ID: "q1", Header: "Pick one", Question: "Which option?", Options: []Option{
			{Label: "Alpha", Description: "The first option"}, {Label: "Beta", Description: "The second option"},
		}}},
		Call: ProposedCall{
			CallID: seedClarifyCallID, ProviderCallID: seedClarifyProviderCall, Name: seedClarifyCallName,
			Arguments: json.RawMessage(seedClarifyArguments), Binding: mustBinding(seedClarifyCallName, json.RawMessage(seedClarifyArguments)),
		},
	}
}

func seedPayload(thread, run, comm string) string {
	return fmt.Sprintf(`{"ThreadID":%q,"RunID":%q,"CommunicationID":%q,"Principal":%q,"Text":"hi"}`, thread, run, comm, seedPrincipal)
}

// seedPre000009 builds a populated v8 database: a multi-journey thread with a
// steering row and a journey-less run, two threads parked on an interaction, and
// an empty thread.
func seedPre000009(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if err := migrateTo(ctx, pool, 8); err != nil {
		t.Fatal(err)
	}
	for _, in := range []Interaction{seedApprovalInteraction(), seedClarifyInteraction()} {
		if err := in.Validate(); err != nil {
			t.Fatalf("seed interaction %s invalid: %v", in.ID, err)
		}
	}
	approval, err := json.Marshal(seedApprovalInteraction())
	if err != nil {
		t.Fatal(err)
	}
	clarify, err := json.Marshal(seedClarifyInteraction())
	if err != nil {
		t.Fatal(err)
	}
	type stmt struct {
		sql  string
		args []any
	}
	s := func(sql string, args ...any) stmt { return stmt{sql, args} }
	stmts := []stmt{
		s("INSERT INTO agent_conversations(id,principal) VALUES('multi',$1),('approval',$1),('clarify',$1),('empty',$1)", seedPrincipal),
		s("INSERT INTO agent_catalog_snapshots(digest,tenant,principal,product_version,endpoint_id,server_version,descriptors) VALUES($1,'t','p','1','e','s','[]')", seedCatalogDigest),
		s("INSERT INTO agent_definition_current(journey_id,definition_digest) VALUES('A',$1),('B',$2)", seedDigestA, seedDigestB),
		s("INSERT INTO agent_sessions(thread_id,journey_id,definition_digest,history) VALUES('multi','A',$1,$3::jsonb),('multi','B',$2,$4::jsonb),('approval','A',$1,$5::jsonb),('clarify','A',$1,$6::jsonb)",
			seedDigestA, seedDigestB, seedHistoryA, seedHistoryB, seedHistoryAp, seedHistoryC),
		// commands in insert order, so ordinals ascend: c1, c2, steering s2, c3, then the parked ones.
		s("INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('multi','c1','r1',$1::jsonb,'r1')", seedPayload("multi", "r1", "c1")),
		s("INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('multi','c2','r2',$1::jsonb,'r2')", seedPayload("multi", "r2", "c2")),
		s("INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id,included) VALUES('multi','s2','r2-steer',$1::jsonb,'r2',true)", seedPayload("multi", "r2-steer", "s2")),
		s("INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('multi','c3','r3',$1::jsonb,'r3')", seedPayload("multi", "r3", "c3")),
		s("INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('approval','ca','ra',$1::jsonb,'ra')", seedPayload("approval", "ra", "ca")),
		s("INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('clarify','cc','rc',$1::jsonb,'rc')", seedPayload("clarify", "rc", "cc")),
		s("INSERT INTO agent_runs(id,thread_id,state,journey_id,definition_digest) VALUES('r1','multi','completed','A',$1),('r2','multi','completed','B',$2),('ra','approval','awaiting_input','A',$1),('rc','clarify','awaiting_input','A',$1)",
			seedDigestA, seedDigestB),
		s("INSERT INTO agent_runs(id,thread_id,state) VALUES('r3','multi','dependency_unavailable')"),
		s("UPDATE agent_conversations SET catalog_digest=$1,product_version='1',catalog_validated_at=now() WHERE id='multi'", seedCatalogDigest),
		s("UPDATE agent_runs SET catalog_digest=$1 WHERE id='r1'", seedCatalogDigest),
		s("INSERT INTO agent_operations(call_id,thread_id,run_id,alias,operation_key,arguments,binding,policy,requires_approval,outcome) VALUES('op1','multi','r1','lookup','lookup','{}','b','read_only',false,'completed')"),
		s("INSERT INTO agent_interactions(id,thread_id,run_id,journey_id,definition_digest,kind,artifact,checkpoint,history,expires_at) VALUES($1,'approval','ra','A',$3,'approval',$4::jsonb,$6::jsonb,$7::jsonb,clock_timestamp()+interval '1 day'),($2,'clarify','rc','A',$3,'clarification',$5::jsonb,$6::jsonb,$8::jsonb,clock_timestamp()+interval '1 day')",
			seedApprovalInteractionID, seedClarifyInteractionID, seedDigestA, string(approval), string(clarify), seedCheckpoint, seedHistoryAp, seedHistoryC),
	}
	// commands and runs reference each other through a deferred FK: one transaction.
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, st := range stmts {
			if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
				return fmt.Errorf("%s: %w", st.sql, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigration000009(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	seedPre000009(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	count := func(t *testing.T, query string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}

	t.Run("conversation history copied from the latest executing run with a journey", func(t *testing.T) {
		want := map[string]string{"multi": seedHistoryB, "approval": seedHistoryAp, "clarify": seedHistoryC, "empty": `[]`}
		for thread, history := range want {
			var got string
			if err := pool.QueryRow(ctx, "SELECT history::text FROM agent_conversations WHERE id=$1", thread).Scan(&got); err != nil {
				t.Fatal(err)
			}
			var gotV, wantV any
			if err := json.Unmarshal([]byte(got), &gotV); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(history), &wantV); err != nil {
				t.Fatal(err)
			}
			g, _ := json.Marshal(gotV)
			w, _ := json.Marshal(wantV)
			if string(g) != string(w) {
				t.Errorf("thread %s history = %s, want %s", thread, g, w)
			}
		}
	})

	t.Run("conversations start unpinned with no active journeys", func(t *testing.T) {
		if n := count(t, "SELECT count(*) FROM agent_conversations"); n != 4 {
			t.Fatalf("conversations = %d", n)
		}
		if n := count(t, "SELECT count(*) FROM agent_conversations WHERE definition_digest IS NULL AND active_journeys = '[]'::jsonb"); n != 4 {
			t.Fatalf("conversations with NULL digest and empty active_journeys = %d, want 4", n)
		}
		if _, err := pool.Exec(ctx, "UPDATE agent_conversations SET history='{}' WHERE id='empty'"); err == nil {
			t.Fatal("non-array conversation history accepted")
		}
	})

	t.Run("000008 data is intact", func(t *testing.T) {
		if n := count(t, "SELECT count(*) FROM agent_conversations WHERE id='multi' AND catalog_digest=$1 AND product_version='1' AND catalog_validated_at IS NOT NULL", seedCatalogDigest); n != 1 {
			t.Fatalf("multi catalog pin lost: %d", n)
		}
		if n := count(t, "SELECT count(*) FROM agent_runs WHERE id='r1' AND catalog_digest=$1", seedCatalogDigest); n != 1 {
			t.Fatalf("r1 catalog_digest lost: %d", n)
		}
		if n := count(t, "SELECT count(*) FROM agent_operations WHERE call_id='op1' AND alias='lookup' AND operation_key='lookup' AND policy='read_only' AND requires_approval=false"); n != 1 {
			t.Fatalf("op1 columns changed: %d", n)
		}
		if n := count(t, "SELECT count(*) FROM agent_operations WHERE call_id='op1' AND journey_id IS NULL"); n != 1 {
			t.Fatalf("op1 journey_id not NULL: %d", n)
		}
	})

	t.Run("old session tables are archived", func(t *testing.T) {
		if n := count(t, "SELECT count(*) FROM (SELECT 1 WHERE to_regclass('agent_sessions') IS NOT NULL) x"); n != 0 {
			t.Fatal("agent_sessions still exists")
		}
		if n := count(t, "SELECT count(*) FROM agent_sessions_archived"); n != 4 {
			t.Fatalf("agent_sessions_archived rows = %d, want 4", n)
		}
		if n := count(t, "SELECT count(*) FROM agent_definition_current_archived"); n != 2 {
			t.Fatalf("agent_definition_current_archived rows = %d, want 2", n)
		}
	})

	t.Run("runs and interactions no longer reference the archived sessions", func(t *testing.T) {
		if n := count(t, "SELECT count(*) FROM pg_constraint WHERE contype='f' AND confrelid='agent_sessions_archived'::regclass AND conrelid IN ('agent_runs'::regclass,'agent_interactions'::regclass)"); n != 0 {
			t.Fatalf("foreign keys onto agent_sessions_archived = %d", n)
		}
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			payload := seedPayload("empty", "rx", "cx")
			if _, err := tx.Exec(ctx, "INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('empty','cx','rx',$1::jsonb,'rx')", payload); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO agent_runs(id,thread_id,state,journey_id,definition_digest) VALUES('rx','empty','completed','X',$1)", strings.Repeat("d", 64))
			return err
		})
		if err != nil {
			t.Fatalf("run for a journey with no session row: %v", err)
		}
	})

	t.Run("runs and interactions keep their pins and state", func(t *testing.T) {
		rows, err := pool.Query(ctx, "SELECT id, coalesce(journey_id,'<null>'), coalesce(definition_digest,'<null>'), state FROM agent_runs WHERE id<>'rx' ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id, journey, digest, state string
			if err := rows.Scan(&id, &journey, &digest, &state); err != nil {
				t.Fatal(err)
			}
			got = append(got, strings.Join([]string{id, journey, digest, state}, ":"))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		want := []string{
			"r1:A:" + seedDigestA + ":completed",
			"r2:B:" + seedDigestB + ":completed",
			"r3:<null>:<null>:dependency_unavailable",
			"ra:A:" + seedDigestA + ":awaiting_input",
			"rc:A:" + seedDigestA + ":awaiting_input",
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("runs = %v, want %v", got, want)
		}
		if n := count(t, "SELECT count(*) FROM agent_interactions WHERE state='pending' AND journey_id='A' AND definition_digest=$1", seedDigestA); n != 2 {
			t.Fatalf("pending interactions with their pins = %d, want 2", n)
		}
	})

	t.Run("agent_definition_current is a new empty singleton", func(t *testing.T) {
		if n := count(t, "SELECT count(*) FROM agent_definition_current"); n != 0 {
			t.Fatalf("rows = %d, want 0", n)
		}
		insert := "INSERT INTO agent_definition_current(definition_digest) VALUES($1)"
		mustFail(t, pool, insert, strings.Repeat("c", 63))
		mustFail(t, pool, "INSERT INTO agent_definition_current(singleton,definition_digest) VALUES(false,$1)", strings.Repeat("c", 64))
		mustExec(t, pool, insert, strings.Repeat("c", 64))
		mustFail(t, pool, insert, strings.Repeat("e", 64))
	})

	t.Run("Migrate again is a no-op", func(t *testing.T) {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatalf("second Migrate: %v", err)
		}
		if n := count(t, "SELECT count(*) FROM agent_schema_migrations"); n != len(migrationNames) {
			t.Fatalf("migrations = %d, want %d", n, len(migrationNames))
		}
	})
}

// seedPre000010 builds a v9 database with a pinned journey run, a parked
// interaction and a populated active set.
func seedPre000010(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if err := migrateTo(ctx, pool, 9); err != nil {
		t.Fatal(err)
	}
	approval, err := json.Marshal(seedApprovalInteraction())
	if err != nil {
		t.Fatal(err)
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, st := range []struct {
			sql  string
			args []any
		}{
			{"INSERT INTO agent_conversations(id,principal,definition_digest,active_journeys) VALUES('v9','p',$1,'[\"A\",\"B\"]'::jsonb),('plain','p',NULL,'[]'::jsonb)", []any{seedDigestA}},
			{"INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('v9','c1','r1',$1::jsonb,'r1')", []any{seedPayload("v9", "r1", "c1")}},
			{"INSERT INTO agent_runs(id,thread_id,state,journey_id,definition_digest) VALUES('r1','v9','awaiting_input','A',$1)", []any{seedDigestA}},
			{"INSERT INTO agent_interactions(id,thread_id,run_id,journey_id,definition_digest,kind,artifact,checkpoint,history,expires_at) VALUES($1,'v9','r1','A',$2,'approval',$3::jsonb,$4::jsonb,$5::jsonb,clock_timestamp()+interval '1 day')",
				[]any{seedApprovalInteractionID, seedDigestA, string(approval), seedCheckpoint, seedHistoryAp}},
		} {
			if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
				return fmt.Errorf("%s: %w", st.sql, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigration000010(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	seedPre000010(t, ctx, pool)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	count := func(t *testing.T, query string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}

	t.Run("seeded v9 data upgrades", func(t *testing.T) {
		if n := count(t, "SELECT count(*) FROM agent_conversations WHERE id='v9' AND active_journeys='[\"A\",\"B\"]'::jsonb AND definition_digest=$1", seedDigestA); n != 1 {
			t.Fatalf("v9 conversation changed: %d", n)
		}
		if n := count(t, "SELECT count(*) FROM agent_runs WHERE id='r1' AND journey_id='A' AND definition_digest=$1 AND state='awaiting_input'", seedDigestA); n != 1 {
			t.Fatalf("r1 changed: %d", n)
		}
		if n := count(t, "SELECT count(*) FROM agent_interactions WHERE id=$1 AND journey_id='A' AND state='pending'", seedApprovalInteractionID); n != 1 {
			t.Fatalf("interaction changed: %d", n)
		}
		if n := count(t, "SELECT count(*) FROM agent_schema_migrations"); n != len(migrationNames) {
			t.Fatalf("migrations = %d, want %d", n, len(migrationNames))
		}
	})

	t.Run("a non-array active_journeys is rejected", func(t *testing.T) {
		mustFail(t, pool, "UPDATE agent_conversations SET active_journeys='{}'::jsonb WHERE id='plain'")
		mustFail(t, pool, "UPDATE agent_conversations SET active_journeys='\"A\"'::jsonb WHERE id='plain'")
		mustFail(t, pool, "UPDATE agent_conversations SET active_journeys='null'::jsonb WHERE id='plain'")
		mustExec(t, pool, "UPDATE agent_conversations SET active_journeys='[\"A\"]'::jsonb WHERE id='plain'")
	})

	run := func(id string, journey, digest any) error {
		return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('plain',$1,$2,$3::jsonb,$2)", "cm-"+id, id, seedPayload("plain", id, "cm-"+id)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO agent_runs(id,thread_id,state,journey_id,definition_digest) VALUES($1,'plain','completed',$2,$3)", id, journey, digest)
			return err
		})
	}

	t.Run("a run with a digest and no journey is accepted", func(t *testing.T) {
		if err := run("rd", nil, seedDigestA); err != nil {
			t.Fatalf("digest without journey: %v", err)
		}
	})
	t.Run("a journey without a digest is rejected", func(t *testing.T) {
		if err := run("rj", "A", nil); err == nil {
			t.Fatal("journey without digest accepted")
		}
	})
	t.Run("a run with neither, and with both, is accepted", func(t *testing.T) {
		if err := run("rn", nil, nil); err != nil {
			t.Fatalf("neither: %v", err)
		}
		if err := run("rb", "A", seedDigestA); err != nil {
			t.Fatalf("both: %v", err)
		}
	})
	t.Run("a NULL interaction journey is accepted", func(t *testing.T) {
		mustExec(t, pool, "INSERT INTO agent_interactions(id,thread_id,run_id,journey_id,definition_digest,kind,artifact,checkpoint,history,expires_at,marker) VALUES('inull','plain','rd',NULL,$1,'approval','{}','{}','[]',clock_timestamp()+interval '1 day','requested')", seedDigestA)
		if n := count(t, "SELECT count(*) FROM agent_interactions WHERE id='inull' AND journey_id IS NULL"); n != 1 {
			t.Fatalf("NULL-journey interaction = %d", n)
		}
	})
	t.Run("the replaced constraint is gone", func(t *testing.T) {
		if n := count(t, "SELECT count(*) FROM pg_constraint WHERE conrelid='agent_runs'::regclass AND conname='agent_runs_check'"); n != 0 {
			t.Fatal("agent_runs_check still present")
		}
	})
	t.Run("Migrate again is a no-op", func(t *testing.T) {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatalf("second Migrate: %v", err)
		}
	})
}

// seedPre000011 extends a v10 database (the v8 seed migrated through 000010) with decided,
// dispatched, denied and expired approvals on thread "m", then leaves it at v10.
func seedPre000011(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	seedPre000009(t, ctx, pool)
	if err := migrateTo(ctx, pool, 10); err != nil {
		t.Fatal(err)
	}
	artifact := func(call string) string {
		in := seedApprovalInteraction()
		in.ID, in.Call.CallID, in.Call.ProviderCallID = "i-"+call, call, "prov-"+call
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	continuation := Command{ThreadID: "m", RunID: "m-c1", CommunicationID: "m-c1", Principal: seedPrincipal,
		InteractionID: "i-undispatched", ReplyKind: "approval", ReplyJSON: `{"decision":"approve","binding":"` + seedApprovalBinding + `"}`}
	payload, err := json.Marshal(continuation)
	if err != nil {
		t.Fatal(err)
	}
	approve := `{"decision":"approve","binding":"` + seedApprovalBinding + `"}`
	deny := `{"decision":"deny","binding":"` + seedApprovalBinding + `"}`
	interaction := func(call, run, state, continuationRun string, reply any) []any {
		return []any{"i-" + call, run, state, artifact(call), continuationRun, reply}
	}
	rows := [][]any{
		interaction("undispatched", "m-r1", "consumed", "m-c1", approve),
		interaction("dispatched", "m-r2", "consumed", "m-c2", approve),
		interaction("denied", "m-r3", "consumed", "m-c3", deny),
		interaction("expired", "m-r4", "expired", "", nil),
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		exec := func(sql string, args ...any) error {
			if _, err := tx.Exec(ctx, sql, args...); err != nil {
				return fmt.Errorf("%s: %w", sql, err)
			}
			return nil
		}
		if err := exec("INSERT INTO agent_conversations(id,principal) VALUES('m',$1)", seedPrincipal); err != nil {
			return err
		}
		for _, r := range []string{"m-r1", "m-r2", "m-r3", "m-r4"} {
			if err := exec("INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('m',$1,$2,$3::jsonb,$2)", "cm-"+r, r, seedPayload("m", r, "cm-"+r)); err != nil {
				return err
			}
		}
		if err := exec("INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('m','m-c1','m-c1',$1::jsonb,'m-c1')", string(payload)); err != nil {
			return err
		}
		if err := exec("INSERT INTO agent_runs(id,thread_id,state,journey_id,definition_digest) VALUES('m-r1','m','completed','A',$1),('m-r2','m','completed','A',$1),('m-r3','m','completed','A',$1),('m-r4','m','failed','A',$1),('m-c1','m','pending','A',$1)", seedDigestA); err != nil {
			return err
		}
		for _, r := range rows {
			if err := exec("INSERT INTO agent_interactions(id,thread_id,run_id,journey_id,definition_digest,kind,artifact,checkpoint,history,expires_at,state,continuation_run_id,reply) VALUES($1,'m',$2,'A',$6,'approval',$4::jsonb,$7::jsonb,$8::jsonb,CASE WHEN $3='expired' THEN clock_timestamp()-interval '1 day' ELSE clock_timestamp()+interval '1 day' END,$3,NULLIF($5,''),$9::jsonb)",
				r[0], r[1], r[2], r[3], r[4], seedDigestA, seedCheckpoint, seedHistoryAp, r[5]); err != nil {
				return err
			}
		}
		return exec("INSERT INTO agent_operations(call_id,thread_id,run_id,alias,operation_key,arguments,binding,policy,requires_approval,outcome) VALUES('dispatched','m','m-r2','book','','{}','b','effectful',true,'completed')")
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigration000011(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	seedPre000011(t, ctx, pool)
	if err := migrateTo(ctx, pool, 11); err != nil {
		t.Fatal(err)
	}
	if len(migrationNames) < 11 {
		t.Fatalf("migrationNames = %d, want at least 11", len(migrationNames))
	}

	t.Run("markers are backfilled", func(t *testing.T) {
		want := map[string]string{
			seedApprovalInteractionID: "requested", // the pre-000009 pending approval
			"i-undispatched":          "decided",
			"i-dispatched":            "dispatched",
			"i-denied":                "denied",
			"i-expired":               "denied",
			seedClarifyInteractionID:  "<null>", // a pending clarification keeps no marker
		}
		for id, marker := range want {
			var got *string
			if err := pool.QueryRow(ctx, "SELECT marker FROM agent_interactions WHERE id=$1", id).Scan(&got); err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			g := "<null>"
			if got != nil {
				g = *got
			}
			if g != marker {
				t.Errorf("marker of %s = %q, want %q", id, g, marker)
			}
		}
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_interactions WHERE operation_key IS NOT NULL OR name_digest IS NOT NULL OR schema_digest IS NOT NULL OR product_version IS NOT NULL OR catalog_digest IS NOT NULL OR active_journeys IS NOT NULL").Scan(&n); err != nil || n != 0 {
			t.Fatalf("legacy rows with v2 columns = %d %v", n, err)
		}
	})

	t.Run("constraints", func(t *testing.T) {
		mustFail(t, pool, "UPDATE agent_interactions SET marker='bogus' WHERE id='i-denied'")
		mustFail(t, pool, "UPDATE agent_interactions SET marker=NULL WHERE id='i-denied'")
		mustFail(t, pool, "UPDATE agent_interactions SET marker='requested' WHERE id=$1", seedClarifyInteractionID)
		mustExec(t, pool, "UPDATE agent_interactions SET marker='superseded' WHERE id='i-denied'")
		mustExec(t, pool, "UPDATE agent_interactions SET marker='denied' WHERE id='i-denied'")
	})

	s := New(pool)
	e := strings.Repeat("e", 64)
	continued := func(t *testing.T, c Command) Owner {
		t.Helper()
		o, err := s.Claim(ctx, c, "o1", LeaseDuration)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Start(ctx, o, "A", e); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordRunScope(ctx, o, RunScope{ProductVersion: testProductVersion, EndpointID: "e1", Settings: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		return o
	}
	superseded := func(t *testing.T, o Owner, interaction, call string) {
		t.Helper()
		op := approvedOp(t, s, o, call, "A")
		if err := s.BeginAttempt(ctx, o, op, 1, interaction); !errors.Is(err, ErrSuperseded) {
			t.Fatalf("BeginAttempt = %v, want ErrSuperseded", err)
		}
		if m := markerOf(t, s, interaction); m != "superseded" {
			t.Fatalf("marker = %q", m)
		}
		if n := operationRows(t, s, call); n != 0 {
			t.Fatalf("agent_operations rows for %s = %d", call, n)
		}
	}

	t.Run("a replied legacy pending approval is superseded, never dispatched", func(t *testing.T) {
		c := Command{ThreadID: "approval", RunID: "approval-reply", CommunicationID: "approval-reply", Principal: seedPrincipal,
			InteractionID: seedApprovalInteractionID, ReplyKind: "approval", ReplyJSON: `{"decision":"approve","binding":"` + seedApprovalBinding + `"}`}
		if _, fresh, err := s.Admit(ctx, c); err != nil || !fresh {
			t.Fatalf("Admit: fresh=%v err=%v", fresh, err)
		}
		if m := markerOf(t, s, seedApprovalInteractionID); m != "decided" {
			t.Fatalf("marker after approve = %q", m)
		}
		o := continued(t, c)
		superseded(t, o, seedApprovalInteractionID, seedApprovalCallID)
	})

	t.Run("a consumed-approve legacy approval that never dispatched is superseded", func(t *testing.T) {
		c := Command{ThreadID: "m", RunID: "m-c1", CommunicationID: "m-c1", Principal: seedPrincipal,
			InteractionID: "i-undispatched", ReplyKind: "approval", ReplyJSON: `{"decision":"approve","binding":"` + seedApprovalBinding + `"}`}
		o := continued(t, c)
		cont, err := s.Continuation(ctx, o)
		if err != nil || cont == nil || cont.Marker != "decided" {
			t.Fatalf("Continuation = %+v %v", cont, err)
		}
		superseded(t, o, "i-undispatched", "undispatched")
	})

	t.Run("Migrate again is a no-op", func(t *testing.T) {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	})
}
