package journal

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

func setFiles() map[string][]byte {
	return map[string][]byte{"agent.yaml": []byte("secret-body"), "skills/a.md": []byte("skill-body")}
}

func setCounts(t *testing.T, s *Store) (sets, files int) {
	t.Helper()
	ctx := context.Background()
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM agent_definition_sets").Scan(&sets); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM agent_definition_set_files").Scan(&files); err != nil {
		t.Fatal(err)
	}
	return
}

func TestMigration000013(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if len(migrationNames) < 13 || migrationNames[12] != "000013_definition_store.sql" {
		t.Fatalf("migration 13 = %v", migrationNames)
	}
	for table, cols := range map[string][]string{
		"agent_definition_sets":      {"definition_digest", "created_at", "activated_at"},
		"agent_definition_set_files": {"definition_digest", "path", "content"},
	} {
		for _, col := range cols {
			var n int
			if err := pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name=$1 AND column_name=$2", table, col).Scan(&n); err != nil || n != 1 {
				t.Fatalf("%s.%s missing: %d %v", table, col, n, err)
			}
		}
	}
	for _, bad := range []string{"xyz", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		if _, err := pool.Exec(ctx, "INSERT INTO agent_definition_sets(definition_digest) VALUES($1)", bad); err == nil {
			t.Fatalf("digest %q accepted", bad)
		}
	}
	good := strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, "INSERT INTO agent_definition_sets(definition_digest) VALUES($1)", good); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO agent_definition_set_files VALUES($1,'',''::bytea)", good); err == nil {
		t.Fatal("empty path accepted")
	}
	if _, err := pool.Exec(ctx, "INSERT INTO agent_definition_set_files VALUES($1,'p','')", strings.Repeat("b", 64)); err == nil {
		t.Fatal("file without set accepted")
	}
}

func TestDefinitionSetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	d := strings.Repeat("a", 64)
	if err := s.PutDefinitionSet(ctx, d, setFiles()); err != nil {
		t.Fatal(err)
	}
	got, err := s.DefinitionSet(ctx, d)
	if err != nil || !reflect.DeepEqual(got, setFiles()) {
		t.Fatalf("DefinitionSet = %v %v", got, err)
	}
}

func TestDefinitionSetIdempotent(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	d := strings.Repeat("a", 64)
	for i := 0; i < 2; i++ {
		if err := s.PutDefinitionSet(ctx, d, setFiles()); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if sets, files := setCounts(t, s); sets != 1 || files != 2 {
		t.Fatalf("sets=%d files=%d", sets, files)
	}
}

func TestDefinitionSetSameDigestKeepsFirstBytes(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	d := strings.Repeat("a", 64)
	if err := s.PutDefinitionSet(ctx, d, setFiles()); err != nil {
		t.Fatal(err)
	}
	changed := setFiles()
	changed["agent.yaml"] = []byte("other-body")
	extra := setFiles()
	extra["extra.md"] = []byte("x")
	missing := setFiles()
	delete(missing, "skills/a.md")
	renamed := map[string][]byte{"agent.yaml": []byte("secret-body"), "skills/b.md": []byte("skill-body")}
	for name, files := range map[string]map[string][]byte{"changed": changed, "extra": extra, "missing": missing, "renamed": renamed} {
		if err := s.PutDefinitionSet(ctx, d, files); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got, _ := s.DefinitionSet(ctx, d); !reflect.DeepEqual(got, setFiles()) {
		t.Fatalf("stored set changed: %v", got)
	}
	if sets, files := setCounts(t, s); sets != 1 || files != 2 {
		t.Fatalf("sets=%d files=%d", sets, files)
	}
}

func activatedAt(t *testing.T, s *Store, digest string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := s.pool.QueryRow(context.Background(), "SELECT activated_at FROM agent_definition_sets WHERE definition_digest=$1", digest).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func storeSets(t *testing.T, s *Store, digests ...string) {
	t.Helper()
	for _, d := range digests {
		if err := s.PutDefinitionSet(context.Background(), d, setFiles()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDefinitionActivated(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	d1, d2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	storeSets(t, s, d1)
	if ok, err := s.DefinitionActivated(ctx, d2); err != nil || ok {
		t.Fatalf("unknown: %v %v", ok, err)
	}
	if ok, err := s.DefinitionActivated(ctx, d1); err != nil || ok {
		t.Fatalf("stored, not activated: %v %v", ok, err)
	}
	if err := s.ActivateDefinition(ctx, "", d1, servedAll); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DefinitionActivated(ctx, d1); err != nil || !ok {
		t.Fatalf("activated: %v %v", ok, err)
	}
}

func TestConversationActivateDefinitionForwardOnly(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	d1, d2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	storeSets(t, s, d1, d2)
	if err := s.ActivateDefinition(ctx, "", d1, servedAll); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateDefinition(ctx, d1, d2, servedAll); err != nil {
		t.Fatal(err)
	}
	// stale pod: wrong expected and unserved references must still report Superseded, not ErrDefinition
	for name, tc := range map[string]struct {
		expected string
		served   func(string) bool
	}{"matching expected": {d2, servedAll}, "stale expected": {"", servedAll}, "unserved": {d2, servedOnly(d1)}} {
		err := s.ActivateDefinition(ctx, tc.expected, d1, tc.served)
		if !errors.Is(err, ErrDefinitionSuperseded) || errors.Is(err, ErrDefinition) {
			t.Fatalf("%s: %v", name, err)
		}
		if cur, _ := s.CurrentDefinition(ctx); cur != d2 {
			t.Fatalf("%s: current = %q", name, cur)
		}
	}
	// activating the current set again stays a no-op
	if err := s.ActivateDefinition(ctx, d1, d2, servedAll); err != nil {
		t.Fatalf("current == candidate: %v", err)
	}
}

func TestConversationRollbackDefinition(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	d1, d2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	storeSets(t, s, d1, d2)
	current := func() string {
		cur, err := s.CurrentDefinition(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return cur
	}
	if err := s.ActivateDefinition(ctx, "", d1, servedAll); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateDefinition(ctx, d1, d2, servedAll); err != nil {
		t.Fatal(err)
	}
	if err := s.RollbackDefinition(ctx, d1, d1, servedAll); !errors.Is(err, ErrDefinition) || current() != d2 {
		t.Fatalf("wrong expected: %v %q", err, current())
	}
	if err := s.RollbackDefinition(ctx, d2, d1, servedOnly(d2)); !errors.Is(err, ErrDefinition) || current() != d2 {
		t.Fatalf("unserved candidate: %v %q", err, current())
	}
	if err := s.RollbackDefinition(ctx, d2, d1, servedOnly(d1)); !errors.Is(err, ErrDefinition) || current() != d2 {
		t.Fatalf("unserved current: %v %q", err, current())
	}
	if err := s.RollbackDefinition(ctx, d2, d1, servedAll); err != nil || current() != d1 {
		t.Fatalf("rollback: %v %q", err, current())
	}
	if err := s.ActivateDefinition(ctx, d1, d2, servedAll); !errors.Is(err, ErrDefinitionSuperseded) || current() != d1 {
		t.Fatalf("forward after rollback: %v %q", err, current())
	}
}

func TestConversationActivateDefinitionRecordsFirstActivation(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	d1, d2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	storeSets(t, s, d1, d2)
	if activatedAt(t, s, d1) != nil || activatedAt(t, s, d2) != nil {
		t.Fatal("activated_at set before activation")
	}
	if err := s.ActivateDefinition(ctx, "", d1, servedAll); err != nil {
		t.Fatal(err)
	}
	first := activatedAt(t, s, d1)
	if first == nil || activatedAt(t, s, d2) != nil {
		t.Fatalf("after first activation: %v %v", first, activatedAt(t, s, d2))
	}
	if err := s.ActivateDefinition(ctx, d1, d1, servedAll); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateDefinition(ctx, d1, d2, servedAll); err != nil {
		t.Fatal(err)
	}
	second := activatedAt(t, s, d2)
	if second == nil {
		t.Fatal("d2 not recorded")
	}
	if err := s.RollbackDefinition(ctx, d2, d1, servedAll); err != nil {
		t.Fatal(err)
	}
	if err := s.RollbackDefinition(ctx, d1, d2, servedAll); err != nil {
		t.Fatal(err)
	}
	if got := activatedAt(t, s, d1); got == nil || !got.Equal(*first) {
		t.Fatalf("d1 activated_at changed: %v -> %v", first, got)
	}
	if got := activatedAt(t, s, d2); got == nil || !got.Equal(*second) {
		t.Fatalf("d2 activated_at changed: %v -> %v", second, got)
	}
}

func TestConversationActivateDefinitionConcurrentSameCandidate(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	d1, d2 := strings.Repeat("a", 64), strings.Repeat("b", 64)
	storeSets(t, s, d1, d2)
	if err := s.ActivateDefinition(ctx, "", d1, servedAll); err != nil {
		t.Fatal(err)
	}
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.ActivateDefinition(ctx, d1, d2, servedAll)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("activate %d: %v", i, err)
		}
	}
	if cur, _ := s.CurrentDefinition(ctx); cur != d2 {
		t.Fatalf("current = %q", cur)
	}
}

func TestDefinitionSetAbsent(t *testing.T) {
	s := scopedStore(t)
	if got, err := s.DefinitionSet(context.Background(), strings.Repeat("a", 64)); !errors.Is(err, ErrDefinition) || got != nil {
		t.Fatalf("%v %v", got, err)
	}
}

func TestDefinitionSetInvalid(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	good := strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		digest string
		files  map[string][]byte
	}{
		"short":     {good[:63], setFiles()},
		"uppercase": {strings.Repeat("A", 64), setFiles()},
		"non-hex":   {strings.Repeat("g", 64), setFiles()},
		"nil files": {good, nil},
		"empty map": {good, map[string][]byte{}},
		"no path":   {good, map[string][]byte{"": []byte("x")}},
	} {
		if err := s.PutDefinitionSet(ctx, tc.digest, tc.files); !errors.Is(err, ErrDefinition) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if sets, files := setCounts(t, s); sets != 0 || files != 0 {
		t.Fatalf("invalid put wrote: %d %d", sets, files)
	}
}

func TestDefinitionSetConcurrentPut(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	d := strings.Repeat("c", 64)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.PutDefinitionSet(ctx, d, setFiles())
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if sets, files := setCounts(t, s); sets != 1 || files != 2 {
		t.Fatalf("sets=%d files=%d", sets, files)
	}
}

func TestReferencedDefinitions(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	if got, err := s.ReferencedDefinitions(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	c1, p1 := strings.Repeat("9", 64), strings.Repeat("1", 64)
	mustExec(t, s.pool, "INSERT INTO agent_definition_current(definition_digest) VALUES($1)", c1)
	o := admitClaim(t, s, threadCommand("w", "rw", "cw", "hi"))
	mustExec(t, s.pool, "UPDATE agent_conversations SET definition_digest=$1 WHERE id='w'", p1)
	if got, _ := s.ReferencedDefinitions(ctx); !reflect.DeepEqual(got, []string{c1}) {
		t.Fatalf("idle pin: %v", got)
	}
	rawInteraction(t, s, o, approvalArtifact("i1", "call1", "j"), "j", "pending", "", "")
	got, err := s.ReferencedDefinitions(ctx)
	if err != nil || !reflect.DeepEqual(got, []string{p1, c1}) {
		t.Fatalf("pending pin: %v %v", got, err)
	}
}
