package journal

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func (s *Store) ResolveDefinition(ctx context.Context, o Owner, journey, bootstrap string, identity func(string) (string, bool)) (string, error) {
	if len(bootstrap) != 64 || identity == nil || !matchesJourney(identity, journey, bootstrap) {
		return "", ErrDefinition
	}
	var digest string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(789134628)"); err != nil {
			return err
		}
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		thread := o.Command.ThreadID
		err := tx.QueryRow(ctx, "SELECT definition_digest FROM agent_sessions WHERE thread_id=$1 AND journey_id=$2", thread, journey).Scan(&digest)
		if err == nil {
			if !matchesJourney(identity, journey, digest) {
				return ErrDefinition
			}
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		err = tx.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current WHERE journey_id=$1", journey).Scan(&digest)
		if err == pgx.ErrNoRows {
			digest = bootstrap
			if _, err = tx.Exec(ctx, "INSERT INTO agent_definition_current(journey_id,definition_digest) VALUES($1,$2)", journey, digest); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if !matchesJourney(identity, journey, digest) {
			return ErrDefinition
		}
		_, err = tx.Exec(ctx, "INSERT INTO agent_sessions(thread_id,journey_id,definition_digest) VALUES($1,$2,$3)", thread, journey, digest)
		return err
	})
	return digest, err
}

func (s *Store) CurrentDefinition(ctx context.Context, journey string) (string, error) {
	var digest string
	if err := s.pool.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current WHERE journey_id=$1", journey).Scan(&digest); err != nil {
		if err == pgx.ErrNoRows {
			return "", ErrDefinition
		}
		return "", err
	}
	return digest, nil
}

// ActivateDefinition conditionally advances current after the candidate and every
// definition retained by a session are available to this replica.
func (s *Store) ActivateDefinition(ctx context.Context, journey, expected, candidate string, identity func(string) (string, bool)) error {
	if len(candidate) != 64 || identity == nil || !matchesJourney(identity, journey, candidate) {
		return ErrDefinition
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Serialize with first-session pinning so activation cannot miss a new old pin.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(789134628)"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT DISTINCT journey_id, definition_digest FROM agent_sessions")
		if err != nil {
			return err
		}
		for rows.Next() {
			var retainedJourney, digest string
			if err := rows.Scan(&retainedJourney, &digest); err != nil {
				rows.Close()
				return err
			}
			if !matchesJourney(identity, retainedJourney, digest) {
				rows.Close()
				return ErrDefinition
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", journey); err != nil {
			return err
		}
		var current string
		err = tx.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current WHERE journey_id=$1 FOR UPDATE", journey).Scan(&current)
		if err == pgx.ErrNoRows {
			if expected != "" {
				return ErrDefinition
			}
			_, err = tx.Exec(ctx, "INSERT INTO agent_definition_current(journey_id,definition_digest) VALUES($1,$2)", journey, candidate)
			return err
		}
		if err != nil {
			return err
		}
		if !matchesJourney(identity, journey, current) {
			return ErrDefinition
		}
		if current != expected {
			return ErrDefinition
		}
		_, err = tx.Exec(ctx, "UPDATE agent_definition_current SET definition_digest=$2 WHERE journey_id=$1", journey, candidate)
		return err
	})
}

func (s *Store) DefinitionsReady(ctx context.Context, identity func(string) (string, bool)) (bool, error) {
	if identity == nil {
		return false, ErrDefinition
	}
	rows, err := s.pool.Query(ctx, `SELECT journey_id, definition_digest FROM agent_definition_current UNION SELECT journey_id, definition_digest FROM agent_sessions`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var journey, digest string
		if err := rows.Scan(&journey, &digest); err != nil {
			return false, err
		}
		if !matchesJourney(identity, journey, digest) {
			return false, nil
		}
	}
	return true, rows.Err()
}

func matchesJourney(identity func(string) (string, bool), journey, digest string) bool {
	actual, ok := identity(digest)
	return ok && actual == journey
}
