package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/defstore"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
)

// countingDefs fails the test when Served is called while a Load is running,
// because Served is called inside journal transactions.
type countingDefs struct {
	t      *testing.T
	inner  *defstore.Loader
	mu     sync.Mutex
	loads  int
	loadIn int
}

func (c *countingDefs) Load(ctx context.Context, digest string) (*definitions.Set, error) {
	c.mu.Lock()
	c.loadIn++
	if !c.inner.Served(digest) {
		c.loads++
	}
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.loadIn--; c.mu.Unlock() }()
	return c.inner.Load(ctx, digest)
}

func (c *countingDefs) Served(digest string) bool {
	c.mu.Lock()
	running := c.loadIn
	c.mu.Unlock()
	if running != 0 {
		c.t.Error("Served called while Load is running")
	}
	return c.inner.Served(digest)
}

func TestBindingLoadsMissingDigestOutsideTransaction(t *testing.T) {
	h := newHarness(t)
	defs := &countingDefs{t: t, inner: defstore.NewLoader(h.store)}
	runner := NewRunner(defs, h.model, Tenancy{})
	_, digest, err := runner.Binding(context.Background(), RunInput{Store: h.store, Owner: h.owner})
	if err != nil || digest != h.set.Digest() {
		t.Fatalf("Binding: %q %v", digest, err)
	}
	if defs.loads != 1 {
		t.Fatalf("loads = %d, want 1", defs.loads)
	}
}

func TestBindingNoCurrent(t *testing.T) {
	h := newHarness(t)
	if _, err := h.pool.Exec(context.Background(), "DELETE FROM agent_definition_current"); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.runner.Binding(context.Background(), RunInput{Store: h.store, Owner: h.owner})
	if !errors.Is(err, journal.ErrDefinition) {
		t.Fatalf("got %v", err)
	}
	var pinned *string
	if err := h.pool.QueryRow(context.Background(), "SELECT definition_digest FROM agent_conversations WHERE id = $1", h.command.ThreadID).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if pinned != nil {
		t.Fatalf("pinned %q", *pinned)
	}
}

func TestReadyRequiresCurrent(t *testing.T) {
	h := newHarness(t)
	if _, err := h.pool.Exec(context.Background(), "DELETE FROM agent_definition_current"); err != nil {
		t.Fatal(err)
	}
	ready, err := h.runner.Ready(context.Background(), h.store)
	if err != nil || ready {
		t.Fatalf("Ready = %v, %v", ready, err)
	}
}

func TestReadyLoadsReferenced(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	fresh := NewRunner(defstore.NewLoader(h.store), h.model, Tenancy{})
	if ready, err := fresh.Ready(ctx, h.store); err != nil || !ready {
		t.Fatalf("Ready = %v, %v", ready, err)
	}
	missing := "1111111111111111111111111111111111111111111111111111111111111111"
	if _, err := h.pool.Exec(ctx, "UPDATE agent_definition_current SET definition_digest = $1", missing); err != nil {
		t.Fatal(err)
	}
	other := NewRunner(defstore.NewLoader(h.store), h.model, Tenancy{})
	if ready, err := other.Ready(ctx, h.store); err != nil || ready {
		t.Fatalf("Ready = %v, %v", ready, err)
	}
}
