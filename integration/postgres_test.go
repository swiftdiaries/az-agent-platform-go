package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
)

var postgres struct {
	sync.Once
	pool    *pgxpool.Pool
	cleanup func()
	err     error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if postgres.cleanup != nil {
		postgres.cleanup()
	}
	os.Exit(code)
}
func randomName() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	postgres.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			name := "agent-platform-task2-" + randomName()
			password := randomName()
			cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-d", "--name", name, "-e", "POSTGRES_PASSWORD", "-p", "127.0.0.1::5432", "postgres:16")
			cmd.Env = append(os.Environ(), "POSTGRES_PASSWORD="+password)
			if output, err := cmd.CombinedOutput(); err != nil {
				postgres.err = fmt.Errorf("PostgreSQL 16 container start: %w: %s", err, output)
				return
			}
			postgres.cleanup = func() { _ = exec.Command("docker", "rm", "-f", name).Run() }
			output, err := exec.CommandContext(ctx, "docker", "port", name, "5432/tcp").Output()
			if err != nil {
				postgres.err = err
				return
			}
			dsn = "postgres://postgres:" + password + "@" + strings.TrimSpace(string(output)) + "/postgres?sslmode=disable"
		}
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			postgres.err = fmt.Errorf("invalid test database configuration")
			return
		}
		for pool.Ping(ctx) != nil {
			if ctx.Err() != nil {
				pool.Close()
				postgres.err = fmt.Errorf("PostgreSQL 16 unavailable before deadline")
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		var version string
		if err = pool.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil || !strings.HasPrefix(version, "16.") {
			pool.Close()
			postgres.err = fmt.Errorf("tests require PostgreSQL 16, got %q", version)
			return
		}
		fmt.Fprintf(os.Stderr, "durability fixture: PostgreSQL %s\n", version)
		cleanup := postgres.cleanup
		postgres.cleanup = func() {
			pool.Close()
			if cleanup != nil {
				cleanup()
			}
		}
		postgres.pool = pool
	})
	if postgres.err != nil {
		t.Fatal(postgres.err)
	}
	ctx := context.Background()
	name := "test_" + randomName()
	if _, err := postgres.pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config := postgres.pool.Config().Copy()
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := postgres.pool.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("cleanup isolated database: %v", err)
		}
	})
	if err := journal.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Never include a DSN in diagnostics: it contains the fixture password.
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
