package pgtest_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m))
}

func TestDatabaseIsolated(t *testing.T) {
	ctx := context.Background()
	a, b := pgtest.Database(t), pgtest.Database(t)

	var nameA, nameB string
	if err := a.QueryRow(ctx, "SELECT current_database()").Scan(&nameA); err != nil {
		t.Fatal(err)
	}
	if err := b.QueryRow(ctx, "SELECT current_database()").Scan(&nameB); err != nil {
		t.Fatal(err)
	}
	if nameA == nameB {
		t.Fatalf("both pools use database %q", nameA)
	}

	if _, err := a.Exec(ctx, "CREATE TABLE only_in_a (id int)"); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := b.QueryRow(ctx, "SELECT to_regclass('public.only_in_a') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("table created in one database is visible in the other")
	}

	if err := b.QueryRow(ctx, "SELECT to_regclass('public.agent_schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("database is not empty: agent_schema_migrations exists")
	}
	if err := a.QueryRow(ctx, "SELECT to_regclass('public.agent_schema_migrations') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("database is not empty: agent_schema_migrations exists")
	}
}

func TestDatabaseServerVersion16(t *testing.T) {
	var version string
	if err := pgtest.Database(t).QueryRow(context.Background(), "SHOW server_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(version, "16.") {
		t.Fatalf("server_version = %q, want 16.x", version)
	}
}

func TestDatabaseDroppedOnCleanup(t *testing.T) {
	ctx := context.Background()
	var name string
	t.Run("create", func(t *testing.T) {
		if err := pgtest.Database(t).QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
			t.Fatal(err)
		}
	})
	if name == "" {
		t.Fatal("database name not recorded")
	}
	var exists bool
	if err := pgtest.Database(t).QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("database %q still exists after cleanup", name)
	}
}
