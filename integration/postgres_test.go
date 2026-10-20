package integration_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m))
}

// database returns a fresh isolated PostgreSQL database with the journal schema applied.
func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.Database(t)
	if err := journal.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestPostgresIsolationAndMigrations(t *testing.T) {
	ctx := context.Background()
	a, b := database(t), database(t)
	for _, pool := range []*pgxpool.Pool{a, b} {
		if err := journal.Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	sa, sb := journal.New(a), journal.New(b)
	command := journal.Command{ThreadID: "same", RunID: "same", CommunicationID: "same", Principal: "alice", Text: "hello"}
	if _, _, err := sa.Admit(ctx, command); err != nil {
		t.Fatal(err)
	}
	command.Principal = "bob"
	if _, _, err := sb.Admit(ctx, command); err != nil {
		t.Fatal(err)
	}
	if _, err := sa.Snapshot(ctx, "same", "bob"); err != journal.ErrForbidden {
		t.Fatalf("principal isolation: %v", err)
	}
	if _, err := a.Exec(ctx, "UPDATE agent_runs SET state='invented'"); err == nil {
		t.Fatal("invalid run state accepted")
	}
}

func TestPostgresMigrationDrift(t *testing.T) {
	ctx := context.Background()
	pool := database(t)
	if _, err := pool.Exec(ctx, "UPDATE agent_schema_migrations SET checksum='changed'"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Migrate(ctx, pool); err == nil {
		t.Fatal("modified migration accepted")
	}
	if _, err := pool.Exec(ctx, "INSERT INTO agent_schema_migrations VALUES(999,'future')"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Migrate(ctx, pool); err == nil {
		t.Fatal("newer schema accepted")
	}
}
