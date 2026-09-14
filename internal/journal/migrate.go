package journal

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(789134627)"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS agent_schema_migrations (version bigint PRIMARY KEY, checksum text NOT NULL)"); err != nil {
			return err
		}
		var version int
		if err := tx.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM agent_schema_migrations").Scan(&version); err != nil {
			return err
		}
		if version > 1 {
			return fmt.Errorf("database schema is newer than this binary")
		}
		data, err := migrations.ReadFile("migrations/000001_conversations.sql")
		if err != nil {
			return err
		}
		checksum := fmt.Sprintf("%x", sha256.Sum256(data))
		var existing string
		err = tx.QueryRow(ctx, "SELECT checksum FROM agent_schema_migrations WHERE version=1").Scan(&existing)
		if err == nil {
			if existing != checksum {
				return fmt.Errorf("migration 1 checksum changed")
			}
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		if _, err = tx.Exec(ctx, string(data)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO agent_schema_migrations VALUES(1,$1)", checksum)
		return err
	})
}
