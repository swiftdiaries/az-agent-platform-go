package defstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

func compiled(t *testing.T) *definitions.Set {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("journeys.yaml", "conversation_prompt: conversation.md\ncatalog:\n  operation_meta_key: operation_key\n  call_id_header: X-Platform-Call-ID\njourneys:\n  - id: alpha\n    skill: skills/alpha\n    tools:\n      - op: alpha.lookup\n        class: read_only\n")
	write("conversation.md", "You are the conversation agent.\n")
	write("skills/alpha/SKILL.md", "---\nname: alpha\ndescription: Does alpha things\n---\nBody.\n")
	set, err := definitions.Compile(filepath.Join(dir, "journeys.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func newStore(t *testing.T) (*journal.Store, interface{ Close() }) {
	t.Helper()
	pool := pgtest.Database(t)
	if err := journal.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return journal.New(pool), pool
}

func TestLoaderLoadsAndCaches(t *testing.T) {
	ctx := context.Background()
	store, pool := newStore(t)
	set := compiled(t)
	if err := store.PutDefinitionSet(ctx, set.Digest(), set.Bundle()); err != nil {
		t.Fatal(err)
	}
	l := NewLoader(store)
	if l.Served(set.Digest()) {
		t.Fatal("served before load")
	}
	got, err := l.Load(ctx, set.Digest())
	if err != nil || got.Digest() != set.Digest() {
		t.Fatalf("Load: %v", err)
	}
	if !l.Served(set.Digest()) {
		t.Fatal("not served after load")
	}
	pool.Close()
	if got, err := l.Load(ctx, set.Digest()); err != nil || got.Digest() != set.Digest() {
		t.Fatalf("cached Load: %v", err)
	}
}

func TestLoaderAbsent(t *testing.T) {
	store, _ := newStore(t)
	l := NewLoader(store)
	d := compiled(t).Digest()
	if _, err := l.Load(context.Background(), d); !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("got %v", err)
	}
}

func TestLoaderNotCompiling(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	d := compiled(t).Digest()
	if err := store.PutDefinitionSet(ctx, d, map[string][]byte{"journeys.yaml": []byte("garbage: [")}); err != nil {
		t.Fatal(err)
	}
	l := NewLoader(store)
	if _, err := l.Load(ctx, d); !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("got %v", err)
	}
	if l.Served(d) {
		t.Fatal("failure cached")
	}
}

func TestLoaderDigestMismatch(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	set := compiled(t)
	other := "0000000000000000000000000000000000000000000000000000000000000000"
	if err := store.PutDefinitionSet(ctx, other, set.Bundle()); err != nil {
		t.Fatal(err)
	}
	l := NewLoader(store)
	if _, err := l.Load(ctx, other); !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("got %v", err)
	}
	if l.Served(other) {
		t.Fatal("mismatch cached")
	}
}
