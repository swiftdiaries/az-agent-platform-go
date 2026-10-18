package journal

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// definitionLock serializes conversation pinning/bootstrapping with activation, so
// activation cannot miss a pin that is being written.
const definitionLock = "SELECT pg_advisory_xact_lock(789134628)"

// ResolveDefinition returns the conversation's pinned definition set digest. An
// unpinned conversation (new, or migrated by 000009) is pinned to current; current
// is bootstrapped to bootstrap when absent. A pinned or current digest that this
// replica cannot serve is ErrDefinition and nothing is written.
func (s *Store) ResolveDefinition(ctx context.Context, o Owner, bootstrap string, served func(string) bool) (string, error) {
	if len(bootstrap) != 64 || served == nil || !served(bootstrap) {
		return "", ErrDefinition
	}
	var digest string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, definitionLock); err != nil {
			return err
		}
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		thread := o.Command.ThreadID
		var pin *string
		if err := tx.QueryRow(ctx, "SELECT definition_digest FROM agent_conversations WHERE id=$1", thread).Scan(&pin); err != nil {
			return err
		}
		if pin != nil {
			if !served(*pin) {
				return ErrDefinition
			}
			digest = *pin
			return nil
		}
		err := tx.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current").Scan(&digest)
		if err == pgx.ErrNoRows {
			digest = bootstrap
			if _, err = tx.Exec(ctx, "INSERT INTO agent_definition_current(definition_digest) VALUES($1)", digest); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !served(digest) {
			return ErrDefinition
		}
		_, err = tx.Exec(ctx, "UPDATE agent_conversations SET definition_digest=$2 WHERE id=$1", thread, digest)
		return err
	})
	if err != nil {
		return "", err
	}
	return digest, nil
}

// CurrentDefinition returns the singleton current set digest; ErrDefinition when absent.
func (s *Store) CurrentDefinition(ctx context.Context) (string, error) {
	var digest string
	if err := s.pool.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current").Scan(&digest); err != nil {
		if err == pgx.ErrNoRows {
			return "", ErrDefinition
		}
		return "", err
	}
	return digest, nil
}

// ActivateDefinition conditionally advances current from expected ("" = absent) to
// candidate after the candidate, the present current and every conversation pin are
// served by this replica.
func (s *Store) ActivateDefinition(ctx context.Context, expected, candidate string, served func(string) bool) error {
	if len(candidate) != 64 || served == nil || !served(candidate) {
		return ErrDefinition
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, definitionLock); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT DISTINCT definition_digest FROM agent_conversations WHERE definition_digest IS NOT NULL")
		if err != nil {
			return err
		}
		for rows.Next() {
			var pin string
			if err := rows.Scan(&pin); err != nil {
				rows.Close()
				return err
			}
			if !served(pin) {
				rows.Close()
				return ErrDefinition
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var current string
		err = tx.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current FOR UPDATE").Scan(&current)
		if err == pgx.ErrNoRows {
			if expected != "" {
				return ErrDefinition
			}
			_, err = tx.Exec(ctx, "INSERT INTO agent_definition_current(definition_digest) VALUES($1)", candidate)
			return err
		}
		if err != nil {
			return err
		}
		if !served(current) || current != expected {
			return ErrDefinition
		}
		_, err = tx.Exec(ctx, "UPDATE agent_definition_current SET definition_digest=$1", candidate)
		return err
	})
}

// DefinitionsReady reports whether current (if any) and every distinct non-NULL
// conversation pin are served. The agent_*_archived tables are never consulted.
func (s *Store) DefinitionsReady(ctx context.Context, served func(string) bool) (bool, error) {
	if served == nil {
		return false, ErrDefinition
	}
	rows, err := s.pool.Query(ctx, `SELECT definition_digest FROM agent_definition_current UNION SELECT definition_digest FROM agent_conversations WHERE definition_digest IS NOT NULL`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return false, err
		}
		if !served(digest) {
			return false, nil
		}
	}
	return true, rows.Err()
}
