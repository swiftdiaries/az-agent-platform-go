package journal

import (
	"context"
	"strings"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

func TestMigration000014(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	if err := migrateTo(ctx, pool, 13); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("c", 64)
	if _, err := pool.Exec(ctx, "INSERT INTO agent_catalog_snapshots(digest,tenant,principal,product_version,endpoint_id,server_version,descriptors) VALUES($1,'t','p','v','e','s','[]')", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO agent_catalog_latest(tenant,principal,digest) VALUES('t','p',$1)", digest); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate 13 with latest rows: %v", err)
	}
	if len(migrationNames) < 14 || migrationNames[13] != "000014_drop_catalog_latest.sql" {
		t.Fatalf("migration 14 = %v", migrationNames)
	}
	var gone bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('agent_catalog_latest') IS NULL").Scan(&gone); err != nil || !gone {
		t.Fatalf("agent_catalog_latest still exists: gone=%v err=%v", gone, err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM agent_catalog_snapshots WHERE digest=$1", digest).Scan(&n); err != nil || n != 1 {
		t.Fatalf("snapshot row lost: %d %v", n, err)
	}
}
