package journal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
)

// waitWithRecordedSettings pins a run to journey A, records the given settings on it, waits on an
// approval and returns how far in the future the stored interaction expires.
func waitWithRecordedSettings(t *testing.T, recorded json.RawMessage) time.Duration {
	t.Helper()
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	if _, err := s.Start(ctx, o, "A", activeDigest); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.RecordRunScope(ctx, o, RunScope{ProductVersion: testProductVersion, EndpointID: "e1", Settings: recorded}); err != nil {
		t.Fatalf("RecordRunScope: %v", err)
	}
	drain(t, s, o)
	if err := wait(t, s, o, boundApproval(t, s, o, "i1", "call-1", "A")); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	var ttl time.Duration
	var ms float64
	if err := s.pool.QueryRow(ctx, "SELECT extract(epoch FROM expires_at-clock_timestamp())*1000 FROM agent_interactions WHERE thread_id='t1' AND id='i1'").Scan(&ms); err != nil {
		t.Fatal(err)
	}
	ttl = time.Duration(ms * float64(time.Millisecond))
	return ttl
}

func assertTTL(t *testing.T, got, want time.Duration) {
	t.Helper()
	if got < want-10*time.Second || got > want+10*time.Second {
		t.Fatalf("interaction expires in %v, want %v (+-10s)", got, want)
	}
}

func TestWaitInteractionTTLFromRunSettings(t *testing.T) {
	rec := settings.Merge(settings.Layer{}, settings.Layer{}, settings.Layer{}).Record()
	assertTTL(t, waitWithRecordedSettings(t, rec), 5*time.Minute)
}

func TestWaitInteractionTTLTenantOverridesDefault(t *testing.T) {
	two := 2 * time.Hour
	c, err := settings.Compile(settings.Document{Tenants: map[string]settings.Layer{"tenant-a": {InteractionTTL: &two}}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := c.Resolve(context.Background(), settings.Scope{Tenant: "tenant-a", Principal: "p"})
	if err != nil {
		t.Fatal(err)
	}
	assertTTL(t, waitWithRecordedSettings(t, resolved.Record()), two)
}

func TestWaitInteractionTTLDefaultWhenUnrecorded(t *testing.T) {
	assertTTL(t, waitWithRecordedSettings(t, json.RawMessage(`{}`)), settings.DefaultInteractionTTL)
	assertTTL(t, waitWithRecordedSettings(t, json.RawMessage(`{"interaction_ttl":0}`)), settings.DefaultInteractionTTL)
}

func TestWaitInteractionTTLMalformedSettings(t *testing.T) {
	ctx := context.Background()
	s := scopedStore(t)
	o := admitClaim(t, s, threadCommand("t1", "r1", "c1", "hi"))
	if _, err := s.Start(ctx, o, "A", activeDigest); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRunScope(ctx, o, RunScope{ProductVersion: testProductVersion, EndpointID: "e1", Settings: json.RawMessage(`{"interaction_ttl":"soon"}`)}); err != nil {
		t.Fatal(err)
	}
	drain(t, s, o)
	if err := wait(t, s, o, boundApproval(t, s, o, "i1", "call-1", "A")); !errors.Is(err, ErrState) {
		t.Fatalf("err = %v, want ErrState", err)
	}
}
