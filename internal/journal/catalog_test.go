package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

func TestMigration000008(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	if err := migrateTo(ctx, pool, 7); err != nil {
		t.Fatal(err)
	}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		payload := `{"ThreadID":"t1","RunID":"r1","CommunicationID":"c1","Principal":"p","Text":"hi"}`
		for _, q := range []string{
			"INSERT INTO agent_conversations(id,principal) VALUES('t1','p')",
			"INSERT INTO agent_commands(thread_id,communication_id,run_id,payload,execution_run_id) VALUES('t1','c1','r1','" + payload + "','r1')",
			"INSERT INTO agent_runs(id,thread_id,state) VALUES('r1','t1','completed')",
			"INSERT INTO agent_operations(call_id,thread_id,run_id,name,arguments,binding,policy,outcome) VALUES('k1','t1','r1','dedup_tool','{}','b','" + legacyDedupPolicy + "','completed'),('k2','t1','r1','effect_tool','{}','b','effectful','completed'),('k3','t1','r1','read_tool','{}','b','read_only','completed')",
			"INSERT INTO agent_events(thread_id,sequence,run_id,kind) VALUES('t1',1,'r1','run.completed')",
		} {
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

	var got string
	if err := pool.QueryRow(ctx, "SELECT string_agg(call_id||':'||alias||':'||policy||':'||requires_approval||':['||operation_key||']', ',' ORDER BY call_id) FROM agent_operations").Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := "k1:dedup_tool:idempotent:true:[],k2:effect_tool:effectful:true:[],k3:read_tool:read_only:false:[]"
	if got != want {
		t.Fatalf("operations = %s", got)
	}
	var nulls int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_conversations WHERE catalog_digest IS NULL AND product_version IS NULL AND catalog_validated_at IS NULL").Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("conversation columns not NULL: %d %v", nulls, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_runs WHERE catalog_digest IS NULL").Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("run column not NULL: %d %v", nulls, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_events WHERE detail IS NULL").Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("event detail not NULL: %d %v", nulls, err)
	}

	digest := strings.Repeat("a", 64)
	insertOp := "INSERT INTO agent_operations(call_id,thread_id,run_id,alias,arguments,binding,policy,outcome,requires_approval) VALUES($1,'t1','r1','x','{}','b',$2,'completed',$3)"
	mustFail(t, pool, insertOp, "n1", legacyDedupPolicy, true)
	mustFail(t, pool, insertOp, "n2", "bogus", true)
	mustFail(t, pool, insertOp, "n3", "effectful", false)
	mustFail(t, pool, insertOp, "n4", "idempotent", false)
	mustExec(t, pool, insertOp, "n5", "idempotent", true)
	mustExec(t, pool, insertOp, "n6", "read_only", false)
	mustExec(t, pool, insertOp, "n7", "read_only", true)

	insertSnap := "INSERT INTO agent_catalog_snapshots(digest,tenant,principal,product_version,endpoint_id,server_version,descriptors) VALUES($1,'tn','p','1','e','s',$2::jsonb)"
	mustFail(t, pool, insertSnap, digest, `{}`)
	mustFail(t, pool, insertSnap, digest, `"x"`)
	mustFail(t, pool, insertSnap, strings.Repeat("a", 63), `[]`)
	mustFail(t, pool, "INSERT INTO agent_catalog_snapshots(digest,tenant,principal,product_version,endpoint_id,server_version,descriptors) VALUES($1,'tn','p','','e','s','[]')", digest)
	mustExec(t, pool, insertSnap, digest, `[]`)

	mustFail(t, pool, "UPDATE agent_conversations SET catalog_digest=$1 WHERE id='t1'", digest)
	mustFail(t, pool, "UPDATE agent_conversations SET catalog_digest=$1,product_version='1' WHERE id='t1'", digest)
	mustFail(t, pool, "UPDATE agent_conversations SET product_version='1' WHERE id='t1'")
	mustExec(t, pool, "UPDATE agent_conversations SET catalog_digest=$1,product_version='1',catalog_validated_at=now() WHERE id='t1'", digest)
	mustFail(t, pool, "UPDATE agent_conversations SET catalog_digest='missing' WHERE id='t1'")
	mustExec(t, pool, "UPDATE agent_runs SET catalog_digest=$1 WHERE id='r1'", digest)
	mustFail(t, pool, "UPDATE agent_runs SET catalog_digest='missing' WHERE id='r1'")
	mustFail(t, pool, "UPDATE agent_events SET detail='[]' WHERE sequence=1")
	mustExec(t, pool, "UPDATE agent_events SET detail='{}' WHERE sequence=1")

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_schema_migrations").Scan(&count); err != nil || count != len(migrationNames) {
		t.Fatalf("migrations = %d %v", count, err)
	}
	mustExec(t, pool, "UPDATE agent_schema_migrations SET checksum='x' WHERE version=8")
	if err := Migrate(ctx, pool); err == nil {
		t.Fatal("drifted checksum accepted")
	}
}

func sampleSnapshot(version string, descriptors string) CatalogSnapshot {
	return CatalogSnapshot{ProductVersion: version, EndpointID: "ep-1", ServerVersion: "srv-1", Descriptors: json.RawMessage(descriptors)}
}

const descsA = `[{"name":"remote_lookup","_meta":{"operation_key":"lookup_destination"}}]`
const descsB = `[{"name":"remote_lookup","_meta":{"operation_key":"lookup_destination"}},{"name":"remote_book","_meta":{"operation_key":"book"}}]`

func runByID(t *testing.T, s *Store, id string) Run {
	t.Helper()
	snap, err := s.Snapshot(context.Background(), "t1", "p")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range snap.Runs {
		if r.RunID == id {
			return r
		}
	}
	t.Fatalf("run %s not found", id)
	return Run{}
}

func catalogEvents(t *testing.T, s *Store, thread string) []Event {
	t.Helper()
	events, err := s.Events(context.Background(), thread, "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range events {
		if e.Type == "catalog_changed" {
			out = append(out, e)
		}
	}
	return out
}

func secondRun(t *testing.T, s *Store, first Owner) Owner {
	t.Helper()
	// the first run consumed its command; Finish refuses to complete with input pending
	mustExec(t, s.pool, "UPDATE agent_commands SET included=true WHERE thread_id='t1' AND execution_run_id='r1'")
	if err := s.Finish(context.Background(), first, RunCompleted, nil, "", "", ""); err != nil {
		t.Fatal(err)
	}
	return admitClaim(t, s, Command{ThreadID: "t1", RunID: "r2", CommunicationID: "c2", Principal: "p", Tenant: "tenant-a", Text: "again"})
}

func TestSnapshotCommitAndPin(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())

	if _, ok, err := s.ConversationPin(ctx, o); err != nil || ok {
		t.Fatalf("unpinned: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.LatestSnapshot(ctx, "tenant-a", "p"); err != nil || ok {
		t.Fatalf("latest before commit: ok=%v err=%v", ok, err)
	}
	digest, err := s.CommitSnapshot(ctx, o, sampleSnapshot("2026.2", descsA))
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		t.Fatalf("digest = %q", digest)
	}
	pin, ok, err := s.ConversationPin(ctx, o)
	if err != nil || !ok {
		t.Fatalf("pin: ok=%v err=%v", ok, err)
	}
	if pin.Digest != digest || pin.Tenant != "tenant-a" || pin.Principal != "p" || pin.ProductVersion != "2026.2" || pin.EndpointID != "ep-1" || pin.ServerVersion != "srv-1" {
		t.Fatalf("pin = %+v", pin)
	}
	var descs []map[string]any
	if err := json.Unmarshal(pin.Descriptors, &descs); err != nil || len(descs) != 1 || descs[0]["name"] != "remote_lookup" {
		t.Fatalf("descriptors = %s %v", pin.Descriptors, err)
	}
	if pin.CapturedAt.IsZero() || pin.ValidatedAt.IsZero() || pin.Age < 0 || pin.Age > time.Minute {
		t.Fatalf("times = %+v", pin)
	}
	want, err := CatalogDigest(CatalogSnapshot{Tenant: "tenant-a", Principal: "p", ProductVersion: "2026.2", EndpointID: "ep-1", ServerVersion: "srv-1", Descriptors: json.RawMessage(descsA)})
	if err != nil || want != digest {
		t.Fatalf("CatalogDigest = %s %v, committed %s", want, err, digest)
	}
	if got := runByID(t, s, "r1").CatalogDigest; got != digest {
		t.Fatalf("run digest = %q", got)
	}
	if ev := catalogEvents(t, s, "t1"); len(ev) != 0 {
		t.Fatalf("first capture emitted %+v", ev)
	}
	latest, ok, err := s.LatestSnapshot(ctx, "tenant-a", "p")
	if err != nil || !ok || latest.Digest != digest || latest.Tenant != "tenant-a" || latest.ProductVersion != "2026.2" {
		t.Fatalf("latest = %+v ok=%v err=%v", latest, ok, err)
	}
	if _, ok, err := s.LatestSnapshot(ctx, "tenant-a", "other"); err != nil || ok {
		t.Fatalf("other principal latest: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.LatestSnapshot(ctx, "other", "p"); err != nil || ok {
		t.Fatalf("other tenant latest: ok=%v err=%v", ok, err)
	}
}

func TestSnapshotRecommitSameDigest(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	digest, err := s.CommitSnapshot(ctx, o, sampleSnapshot("2026.2", descsA))
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := s.ConversationPin(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s.pool, "UPDATE agent_conversations SET catalog_validated_at=clock_timestamp()-interval '1 hour' WHERE id='t1'")
	aged, _, err := s.ConversationPin(ctx, o)
	if err != nil || aged.Age < 59*time.Minute {
		t.Fatalf("aged = %v %v", aged.Age, err)
	}
	// identical content, different whitespace: same canonical digest
	again, err := s.CommitSnapshot(ctx, o, sampleSnapshot("2026.2", ` [ {"name":"remote_lookup", "_meta":{"operation_key":"lookup_destination"}} ] `))
	if err != nil || again != digest {
		t.Fatalf("recommit = %s %v, want %s", again, err, digest)
	}
	fresh, ok, err := s.ConversationPin(ctx, o)
	if err != nil || !ok || fresh.Age > time.Minute || !fresh.ValidatedAt.After(aged.ValidatedAt) {
		t.Fatalf("not refreshed: %+v ok=%v err=%v", fresh, ok, err)
	}
	if !fresh.CapturedAt.Equal(first.CapturedAt) {
		t.Fatalf("snapshot rewritten: %v vs %v", fresh.CapturedAt, first.CapturedAt)
	}
	if ev := catalogEvents(t, s, "t1"); len(ev) != 0 {
		t.Fatalf("events = %+v", ev)
	}
	var snapshots int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM agent_catalog_snapshots").Scan(&snapshots); err != nil || snapshots != 1 {
		t.Fatalf("snapshots = %d %v", snapshots, err)
	}
}

func TestSnapshotChangedEmitsEvent(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o1 := admitClaim(t, s, testCommand())
	d1, err := s.CommitSnapshot(ctx, o1, sampleSnapshot("2026.2", descsA))
	if err != nil {
		t.Fatal(err)
	}
	o2 := secondRun(t, s, o1)
	if _, err := s.CommitSnapshot(ctx, o2, sampleSnapshot("2026.2", descsA)); err != nil {
		t.Fatal(err)
	}
	if ev := catalogEvents(t, s, "t1"); len(ev) != 0 {
		t.Fatalf("identical recommit emitted %+v", ev)
	}
	d2, err := s.CommitSnapshot(ctx, o2, sampleSnapshot("2026.3", descsB))
	if err != nil || d2 == d1 {
		t.Fatalf("d2 = %s %v", d2, err)
	}
	ev := catalogEvents(t, s, "t1")
	if len(ev) != 1 || ev[0].RunID != "r2" {
		t.Fatalf("events = %+v", ev)
	}
	var detail map[string]string
	if err := json.Unmarshal(ev[0].Detail, &detail); err != nil {
		t.Fatalf("detail = %s %v", ev[0].Detail, err)
	}
	if detail["old_digest"] != d1 || detail["new_digest"] != d2 || detail["old_product_version"] != "2026.2" || detail["new_product_version"] != "2026.3" {
		t.Fatalf("detail = %v", detail)
	}
	snap, err := s.Snapshot(ctx, "t1", "p")
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, e := range snap.Events {
		if e.Type == "catalog_changed" && len(e.Detail) > 0 {
			seen = true
		}
		if e.Type != "catalog_changed" && e.Detail != nil {
			t.Fatalf("unexpected detail on %s: %s", e.Type, e.Detail)
		}
	}
	if !seen {
		t.Fatal("Snapshot events lack the detail")
	}
	if runByID(t, s, "r1").CatalogDigest != d1 || runByID(t, s, "r2").CatalogDigest != d2 {
		t.Fatalf("run digests: %q %q", runByID(t, s, "r1").CatalogDigest, runByID(t, s, "r2").CatalogDigest)
	}
	pin, ok, err := s.ConversationPin(ctx, o2)
	if err != nil || !ok || pin.Digest != d2 || pin.ProductVersion != "2026.3" {
		t.Fatalf("pin = %+v ok=%v err=%v", pin, ok, err)
	}
}

func TestSnapshotPinIsPerConversation(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o1 := admitClaim(t, s, testCommand())
	o2 := admitClaim(t, s, Command{ThreadID: "t2", RunID: "r9", CommunicationID: "c9", Principal: "p", Tenant: "tenant-a", Text: "other thread"})
	d1, err := s.CommitSnapshot(ctx, o1, sampleSnapshot("2026.2", descsA))
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.CommitSnapshot(ctx, o2, sampleSnapshot("2026.3", descsB))
	if err != nil || d1 == d2 {
		t.Fatalf("d2 = %s %v", d2, err)
	}
	latest, ok, err := s.LatestSnapshot(ctx, "tenant-a", "p")
	if err != nil || !ok || latest.Digest != d2 {
		t.Fatalf("latest = %+v ok=%v err=%v", latest, ok, err)
	}
	pin, ok, err := s.ConversationPin(ctx, o1)
	if err != nil || !ok || pin.Digest != d1 || pin.ProductVersion != "2026.2" {
		t.Fatalf("t1 pin moved: %+v ok=%v err=%v", pin, ok, err)
	}
	if err := s.RecordRunCatalog(ctx, o1, d2); !errors.Is(err, ErrState) {
		t.Fatalf("t1 recording the latest digest: %v", err)
	}
	if ev := catalogEvents(t, s, "t1"); len(ev) != 0 {
		t.Fatalf("t1 events = %+v", ev)
	}
}

func TestSnapshotFencing(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	digest, err := s.CommitSnapshot(ctx, o, sampleSnapshot("2026.2", descsA))
	if err != nil {
		t.Fatal(err)
	}

	check := func(name string, stale Owner) {
		t.Helper()
		if _, err := s.CommitSnapshot(ctx, stale, sampleSnapshot("2026.3", descsB)); !errors.Is(err, ErrOwnership) {
			t.Fatalf("%s CommitSnapshot: %v", name, err)
		}
		if err := s.RecordRunCatalog(ctx, stale, digest); !errors.Is(err, ErrOwnership) {
			t.Fatalf("%s RecordRunCatalog: %v", name, err)
		}
		if _, _, err := s.ConversationPin(ctx, stale); !errors.Is(err, ErrOwnership) {
			t.Fatalf("%s ConversationPin: %v", name, err)
		}
	}
	check("epoch", Owner{Command: o.Command, ID: o.ID, Epoch: o.Epoch + 1})
	check("id", Owner{Command: o.Command, ID: "other", Epoch: o.Epoch})
	var snapshots int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM agent_catalog_snapshots").Scan(&snapshots); err != nil || snapshots != 1 {
		t.Fatalf("stale write landed: %d %v", snapshots, err)
	}

	// RecordRunCatalog needs the digest to be the conversation pin
	if err := s.RecordRunCatalog(ctx, o, digest); err != nil {
		t.Fatalf("pinned digest: %v", err)
	}
	if err := s.RecordRunCatalog(ctx, o, strings.Repeat("0", 64)); !errors.Is(err, ErrState) {
		t.Fatalf("unpinned digest: %v", err)
	}
	if err := s.RecordRunCatalog(ctx, o, ""); !errors.Is(err, ErrState) {
		t.Fatalf("empty digest: %v", err)
	}

	// validation
	for name, snap := range map[string]CatalogSnapshot{
		"no version":        sampleSnapshot("", descsA),
		"no endpoint":       {ProductVersion: "1", Descriptors: json.RawMessage(descsA)},
		"object":            sampleSnapshot("1", `{}`),
		"null":              sampleSnapshot("1", `null`),
		"empty":             sampleSnapshot("1", ``),
		"truncated":         sampleSnapshot("1", `[`),
		"scalar":            sampleSnapshot("1", `"x"`),
		"foreign tenant":    {Tenant: "tenant-b", ProductVersion: "1", EndpointID: "e", Descriptors: json.RawMessage(`[]`)},
		"foreign principal": {Principal: "q", ProductVersion: "1", EndpointID: "e", Descriptors: json.RawMessage(`[]`)},
	} {
		if _, err := s.CommitSnapshot(ctx, o, snap); !errors.Is(err, ErrState) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// matching explicit owner values are accepted
	if _, err := s.CommitSnapshot(ctx, o, CatalogSnapshot{Tenant: "tenant-a", Principal: "p", ProductVersion: "2026.2", EndpointID: "ep-1", ServerVersion: "srv-1", Descriptors: json.RawMessage(descsA)}); err != nil {
		t.Fatalf("explicit owner values: %v", err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM agent_catalog_snapshots").Scan(&snapshots); err != nil || snapshots != 1 {
		t.Fatalf("invalid snapshots landed: %d %v", snapshots, err)
	}

	// an expired lease fences the owner out too
	mustExec(t, s.pool, "UPDATE agent_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id='r1'")
	check("lease", o)
}

func TestRecordRunCatalogReusesPin(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o1 := admitClaim(t, s, testCommand())
	if err := s.RecordRunCatalog(ctx, o1, strings.Repeat("0", 64)); !errors.Is(err, ErrState) {
		t.Fatalf("before any pin: %v", err)
	}
	digest, err := s.CommitSnapshot(ctx, o1, sampleSnapshot("2026.2", descsA))
	if err != nil {
		t.Fatal(err)
	}
	o2 := secondRun(t, s, o1)
	if got := runByID(t, s, "r2").CatalogDigest; got != "" {
		t.Fatalf("r2 digest before = %q", got)
	}
	if err := s.RecordRunCatalog(ctx, o2, digest); err != nil {
		t.Fatal(err)
	}
	if got := runByID(t, s, "r2").CatalogDigest; got != digest {
		t.Fatalf("r2 digest = %q", got)
	}
	if ev := catalogEvents(t, s, "t1"); len(ev) != 0 {
		t.Fatalf("events = %+v", ev)
	}
}

func TestCatalogDigestCanonical(t *testing.T) {
	a, err := CatalogDigest(CatalogSnapshot{Tenant: "t", Principal: "p", ProductVersion: "1", EndpointID: "e", ServerVersion: "s", Descriptors: json.RawMessage(`[ {"a": 1} ]`)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := CatalogDigest(CatalogSnapshot{Tenant: "t", Principal: "p", ProductVersion: "1", EndpointID: "e", ServerVersion: "s", Descriptors: json.RawMessage(`[{"a":1}]`)})
	c, _ := CatalogDigest(CatalogSnapshot{Tenant: "u", Principal: "p", ProductVersion: "1", EndpointID: "e", ServerVersion: "s", Descriptors: json.RawMessage(`[{"a":1}]`)})
	if a != b || a == c || len(a) != 64 {
		t.Fatalf("digests: %s %s %s", a, b, c)
	}
	if _, err := CatalogDigest(CatalogSnapshot{Descriptors: json.RawMessage(`[`)}); err == nil {
		t.Fatal("invalid descriptors accepted")
	}
}

// legacyDedupPolicy is the removed pre-catalog name of the idempotent class; the
// migration still rewrites stored rows that carry it.
const legacyDedupPolicy = "deduplicated"

// policyOp builds an operation on journey "j" whose binding matches its arguments and the owner's run state.
func policyOp(t *testing.T, s *Store, o Owner, callID, policy string) Operation {
	t.Helper()
	op := Operation{CallID: callID, Name: "lookup_alias", OperationKey: "lookup.destination", NameDigest: NameDigest("lookup destination"),
		SchemaDigest: mustSchemaDigest(`{"type":"object"}`), Arguments: `{"city":"A"}`, Policy: policy, RequiresApproval: policy == "idempotent" || policy == "effectful", JourneyID: "j"}
	return rebind(t, s, o, op)
}

// approve inserts a consumed, decided approval for op on the owner's run.
func approve(t *testing.T, s *Store, o Owner, op Operation, id string) {
	t.Helper()
	artifact, err := json.Marshal(Interaction{ID: id, Kind: "approval", Call: ProposedCall{CallID: op.CallID, ProviderCallID: "p", Name: op.Name, Arguments: json.RawMessage(op.Arguments), Binding: op.Binding}})
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := json.Marshal(Reply{Decision: "approve", Binding: op.Binding})
	mustExec(t, s.pool, "INSERT INTO agent_interactions(id,thread_id,run_id,journey_id,definition_digest,kind,artifact,checkpoint,history,expires_at,state,continuation_run_id,reply,marker) VALUES($1,$2,$3,'j',$4,'approval',$5,'{}','[]',clock_timestamp()+interval '1 hour','consumed',$3,$6,'decided')", id, o.Command.ThreadID, o.Command.RunID, strings.Repeat("d", 64), artifact, reply)
}

func storedOp(t *testing.T, s *Store, callID string) (alias, key, policy string, requires bool) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(), "SELECT alias,operation_key,policy,requires_approval FROM agent_operations WHERE call_id=$1", callID).Scan(&alias, &key, &policy, &requires); err != nil {
		t.Fatalf("stored op %s: %v", callID, err)
	}
	return
}

func TestBeginAttemptPolicyReadOnly(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	startScoped(t, s, o, "j")

	plain := policyOp(t, s, o, "k1", "read_only")
	if err := s.BeginAttempt(ctx, o, plain, 1, ""); err != nil {
		t.Fatal(err)
	}
	if alias, key, policy, requires := storedOp(t, s, "k1"); alias != "lookup_alias" || key != "lookup.destination" || policy != "read_only" || requires {
		t.Fatalf("stored = %s %s %s %v", alias, key, policy, requires)
	}
	if err := s.EndAttempt(ctx, o, plain, 1, "completed"); err != nil {
		t.Fatal(err)
	}

	gated := policyOp(t, s, o, "k2", "read_only")
	gated.RequiresApproval = true
	if err := s.BeginAttempt(ctx, o, gated, 1, ""); !errors.Is(err, ErrState) {
		t.Fatalf("read_only with RequiresApproval and no approval: %v", err)
	}
	if err := s.BeginAttempt(ctx, o, gated, 1, "missing"); !errors.Is(err, ErrState) {
		t.Fatalf("unknown approval: %v", err)
	}
	var rows int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM agent_operations WHERE call_id='k2'").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rejected attempt left %d rows (%v)", rows, err)
	}
	approve(t, s, o, gated, "a1")
	if err := s.BeginAttempt(ctx, o, gated, 1, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, _, policy, requires := storedOp(t, s, "k2"); policy != "read_only" || !requires {
		t.Fatalf("stored = %s %v", policy, requires)
	}
}

func TestBeginAttemptPolicyIdempotentRetry(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	startScoped(t, s, o, "j")
	op := policyOp(t, s, o, "k1", "idempotent")
	if err := s.BeginAttempt(ctx, o, op, 1, ""); !errors.Is(err, ErrState) {
		t.Fatalf("no approval: %v", err)
	}
	approve(t, s, o, op, "a1")
	if err := s.BeginAttempt(ctx, o, op, 1, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, _, policy, requires := storedOp(t, s, "k1"); policy != "idempotent" || !requires {
		t.Fatalf("stored = %s %v", policy, requires)
	}
	if err := s.EndAttempt(ctx, o, op, 1, "outcome_unknown"); err != nil {
		t.Fatal(err)
	}
	// attempt 2 compares the stored policy with the requested one
	mismatch := op
	mismatch.Policy = "effectful"
	if err := s.BeginAttempt(ctx, o, mismatch, 2, ""); !errors.Is(err, ErrState) {
		t.Fatalf("policy mismatch: %v", err)
	}
	if err := s.BeginAttempt(ctx, o, op, 2, ""); err != nil {
		t.Fatalf("attempt 2 with idempotent: %v", err)
	}
	if err := s.EndAttempt(ctx, o, op, 2, "completed"); err != nil {
		t.Fatal(err)
	}

	again := policyOp(t, s, o, "k2", "idempotent")
	if err := s.BeginAttempt(ctx, o, again, 1, ""); !errors.Is(err, ErrState) {
		t.Fatalf("idempotent without approval: %v", err)
	}
	approve(t, s, o, again, "a2")
	if err := s.BeginAttempt(ctx, o, again, 1, "a2"); err != nil {
		t.Fatal(err)
	}
	if _, _, policy, requires := storedOp(t, s, "k2"); policy != "idempotent" || !requires {
		t.Fatalf("stored = %s %v", policy, requires)
	}
}

func TestBeginAttemptPolicyEffectfulAndUnknown(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	startScoped(t, s, o, "j")

	// A non-read_only operation without RequiresApproval is rejected, approval or not.
	noFlag := policyOp(t, s, o, "k0", "effectful")
	noFlag.RequiresApproval = false
	approve(t, s, o, noFlag, "a0")
	if err := s.BeginAttempt(ctx, o, noFlag, 1, "a0"); !errors.Is(err, ErrState) {
		t.Fatalf("effectful without RequiresApproval: %v", err)
	}
	noFlag.Policy = "idempotent"
	if err := s.BeginAttempt(ctx, o, noFlag, 1, "a0"); !errors.Is(err, ErrState) {
		t.Fatalf("idempotent without RequiresApproval: %v", err)
	}

	eff := policyOp(t, s, o, "k1", "effectful")
	if err := s.BeginAttempt(ctx, o, eff, 1, ""); !errors.Is(err, ErrState) {
		t.Fatalf("effectful without approval: %v", err)
	}
	approve(t, s, o, eff, "a1")
	if err := s.BeginAttempt(ctx, o, eff, 1, "a1"); err != nil {
		t.Fatal(err)
	}
	if _, _, policy, requires := storedOp(t, s, "k1"); policy != "effectful" || !requires {
		t.Fatalf("stored = %s %v", policy, requires)
	}
	if err := s.EndAttempt(ctx, o, eff, 1, "completed"); err != nil {
		t.Fatal(err)
	}

	// The legacy alias is no longer accepted.
	for _, policy := range []string{"", "bogus", "Read_Only", legacyDedupPolicy} {
		op := policyOp(t, s, o, "k-"+policy, policy)
		if err := s.BeginAttempt(ctx, o, op, 1, ""); !errors.Is(err, ErrState) {
			t.Fatalf("policy %q: %v", policy, err)
		}
	}
}

func TestBeginAttemptPolicyEffectfulRejectedNoSecondAttempt(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, testCommand())
	startScoped(t, s, o, "j")

	eff := policyOp(t, s, o, "k1", "effectful")
	approve(t, s, o, eff, "a1")
	if err := s.BeginAttempt(ctx, o, eff, 1, "a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.EndAttempt(ctx, o, eff, 1, "rejected"); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginAttempt(ctx, o, eff, 2, ""); !errors.Is(err, ErrState) {
		t.Fatalf("effectful attempt 2 after rejected: %v", err)
	}
	var outcome string
	var attempts int
	if err := s.pool.QueryRow(ctx, "SELECT outcome,(SELECT count(*) FROM agent_attempts WHERE call_id=$1) FROM agent_operations WHERE call_id=$1", "k1").Scan(&outcome, &attempts); err != nil {
		t.Fatal(err)
	}
	if outcome != "rejected" || attempts != 1 {
		t.Fatalf("outcome=%s attempts=%d", outcome, attempts)
	}

	idem := policyOp(t, s, o, "k2", "idempotent")
	approve(t, s, o, idem, "a2")
	if err := s.BeginAttempt(ctx, o, idem, 1, "a2"); err != nil {
		t.Fatal(err)
	}
	if err := s.EndAttempt(ctx, o, idem, 1, "rejected"); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginAttempt(ctx, o, idem, 2, ""); err != nil {
		t.Fatalf("idempotent attempt 2 after rejected: %v", err)
	}
}
