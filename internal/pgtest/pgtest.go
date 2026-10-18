// Package pgtest provides PostgreSQL 16 fixtures for tests.
//
// It deliberately has no dependency on internal/journal and runs no
// migrations, so any package (including journal) can import it from tests.
package pgtest

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
)

// shared is the server-level fixture, created once per test binary.
var shared struct {
	sync.Once
	pool    *pgxpool.Pool
	cleanup func()
	err     error
}

func randomName() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Run runs m, then tears down the shared server pool and any container that
// was started, and returns the exit code. Use it as:
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }
func Run(m *testing.M) int {
	code := m.Run()
	if shared.cleanup != nil {
		shared.cleanup()
	}
	return code
}

func setup() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	dsn := os.Getenv("TEST_DATABASE_URL")
	var containerCleanup func()
	if dsn == "" {
		name := "agent-platform-task2-" + randomName()
		password := randomName()
		cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "-d", "--name", name, "-e", "POSTGRES_PASSWORD", "-p", "127.0.0.1::5432", "postgres:16")
		cmd.Env = append(os.Environ(), "POSTGRES_PASSWORD="+password)
		if output, err := cmd.CombinedOutput(); err != nil {
			shared.err = fmt.Errorf("PostgreSQL 16 container start: %w: %s", err, output)
			return
		}
		containerCleanup = func() { _ = exec.Command("docker", "rm", "-f", name).Run() }
		shared.cleanup = containerCleanup
		output, err := exec.CommandContext(ctx, "docker", "port", name, "5432/tcp").Output()
		if err != nil {
			shared.err = err
			return
		}
		dsn = "postgres://postgres:" + password + "@" + strings.TrimSpace(string(output)) + "/postgres?sslmode=disable"
	}
	// Never include a DSN in diagnostics: it contains the fixture password.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		shared.err = fmt.Errorf("invalid test database configuration")
		return
	}
	for pool.Ping(ctx) != nil {
		if ctx.Err() != nil {
			pool.Close()
			shared.err = fmt.Errorf("PostgreSQL 16 unavailable before deadline")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	var version string
	if err = pool.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil || !strings.HasPrefix(version, "16.") {
		pool.Close()
		shared.err = fmt.Errorf("tests require PostgreSQL 16, got %q", version)
		return
	}
	fmt.Fprintf(os.Stderr, "durability fixture: PostgreSQL %s\n", version)
	shared.cleanup = func() {
		pool.Close()
		if containerCleanup != nil {
			containerCleanup()
		}
	}
	shared.pool = pool
}

// Database returns a pool on a fresh, isolated, empty database
// (CREATE DATABASE test_<random>). The database is dropped WITH (FORCE) when
// the test finishes, after the returned pool is closed. The shared server is
// TEST_DATABASE_URL if set, otherwise a postgres:16 docker container. Setup
// failures fail the test; a missing database never skips or passes.
func Database(t testing.TB) *pgxpool.Pool {
	t.Helper()
	shared.Do(setup)
	if shared.err != nil {
		t.Fatal(shared.err)
	}
	ctx := context.Background()
	name := "test_" + randomName()
	if _, err := shared.pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	config := shared.pool.Config().Copy()
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := shared.pool.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("cleanup isolated database: %v", err)
		}
	})
	return pool
}
