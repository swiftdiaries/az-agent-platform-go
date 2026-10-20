package defstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

func writeBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"journeys.yaml":         "conversation_prompt: conversation.md\ncatalog:\n  operation_meta_key: operation_key\n  call_id_header: X-Platform-Call-ID\njourneys:\n  - id: alpha\n    skill: skills/alpha\n    tools:\n      - op: alpha.lookup\n        class: read_only\n",
		"conversation.md":       "You are the conversation agent.\n",
		"skills/alpha/SKILL.md": "---\nname: alpha\ndescription: Does alpha things\n---\nBody.\n",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestActivateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	dir := writeBundle(t)
	first, err := Activate(ctx, pool, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Activate(ctx, pool, dir, Options{})
	if err != nil || second.Digest != first.Digest || second.Outcome != AlreadyCurrent {
		t.Fatalf("second = %+v %v, want %+v", second, err, first)
	}
	if cur, err := journal.New(pool).CurrentDefinition(ctx); err != nil || cur != first.Digest {
		t.Fatalf("current = %q %v", cur, err)
	}
}

func TestActivateUnservableReferencedDigest(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	if err := journal.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := journal.New(pool)
	x := strings.Repeat("cd", 32)
	if err := store.PutDefinitionSet(ctx, x, map[string][]byte{"journeys.yaml": []byte("not: [valid")}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateDefinition(ctx, "", x, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	_, err := Activate(ctx, pool, writeBundle(t), Options{})
	if !errors.Is(err, journal.ErrDefinition) || !strings.Contains(err.Error(), x) || strings.Contains(err.Error(), "not: [valid") {
		t.Fatalf("err = %v", err)
	}
}

func TestActivateOutcomes(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	d1dir := writeBundle(t)
	d2dir := writeBundle(t)
	if err := os.WriteFile(filepath.Join(d2dir, "conversation.md"), []byte("Another prompt.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r1, err := Activate(ctx, pool, d1dir, Options{})
	if err != nil || r1.Outcome != Activated || r1.Current != r1.Digest {
		t.Fatalf("d1 = %+v %v", r1, err)
	}
	if r, err := Activate(ctx, pool, d1dir, Options{}); err != nil || r.Outcome != AlreadyCurrent || r.Current != r1.Digest {
		t.Fatalf("d1 again = %+v %v", r, err)
	}
	r2, err := Activate(ctx, pool, d2dir, Options{})
	if err != nil || r2.Outcome != Activated || r2.Digest == r1.Digest || r2.Current != r2.Digest {
		t.Fatalf("d2 = %+v %v", r2, err)
	}
	r, err := Activate(ctx, pool, d1dir, Options{})
	if err != nil || r.Outcome != Superseded || r.Digest != r1.Digest || r.Current != r2.Digest {
		t.Fatalf("d1 after d2 = %+v %v", r, err)
	}
	if cur, _ := journal.New(pool).CurrentDefinition(ctx); cur != r2.Digest {
		t.Fatalf("current moved to %q", cur)
	}
	r, err = Activate(ctx, pool, d1dir, Options{Rollback: true})
	if err != nil || r.Outcome != Activated || r.Current != r1.Digest {
		t.Fatalf("rollback = %+v %v", r, err)
	}
	if r, err = Activate(ctx, pool, d1dir, Options{Rollback: true}); err != nil || r.Outcome != AlreadyCurrent {
		t.Fatalf("rollback again = %+v %v", r, err)
	}
	if r, err = Activate(ctx, pool, d2dir, Options{}); err != nil || r.Outcome != Superseded || r.Current != r1.Digest {
		t.Fatalf("d2 after rollback = %+v %v", r, err)
	}
}

func TestActivateRollbackNeverActivatedSet(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	d1dir := writeBundle(t)
	if _, err := Activate(ctx, pool, d1dir, Options{}); err != nil {
		t.Fatal(err)
	}
	d2dir := writeBundle(t)
	if err := os.WriteFile(filepath.Join(d2dir, "conversation.md"), []byte("Third prompt.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Activate(ctx, pool, d2dir, Options{Rollback: true})
	if err != nil || r.Outcome != Activated || r.Current != r.Digest {
		t.Fatalf("rollback to new set = %+v %v", r, err)
	}
}

func TestActivateErrorKinds(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	dir := writeBundle(t)
	if err := os.WriteFile(filepath.Join(dir, "journeys.yaml"), []byte("bogus: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(ctx, pool, dir, Options{}); !errors.Is(err, ErrBundle) || errors.Is(err, ErrDatabase) {
		t.Fatalf("err = %v", err)
	}
	if _, err := Activate(ctx, pool, t.TempDir(), Options{}); !errors.Is(err, ErrBundle) {
		t.Fatalf("empty dir err = %v", err)
	}
	pool.Close()
	if _, err := Activate(ctx, pool, writeBundle(t), Options{}); !errors.Is(err, ErrDatabase) {
		t.Fatalf("closed pool err = %v", err)
	}
}
