package journal

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// definitionLock serializes conversation pinning/bootstrapping with activation, so
// activation cannot miss a pin that is being written.
const definitionLock = "SELECT pg_advisory_xact_lock(789134628)"

// referencedDigests selects every digest this replica must serve: the current set,
// plus the conversation pin of each conversation that is not idle, meaning it has a
// pending interaction, a consumed interaction whose continuation run is still
// pending or running, or an unresolved operation. Idle conversations re-pin to the
// current set at their next run start, so their pins need not stay loadable. The
// conversation pin is used, never the interaction's digest, which pre-000009 rows
// carry as an old per-journey digest.
const referencedDigests = `SELECT definition_digest FROM agent_definition_current
UNION
SELECT c.definition_digest FROM agent_conversations c WHERE c.definition_digest IS NOT NULL AND (
  EXISTS (SELECT 1 FROM agent_interactions i WHERE i.thread_id = c.id AND i.state = 'pending')
  OR EXISTS (SELECT 1 FROM agent_interactions i JOIN agent_runs r ON r.thread_id = i.thread_id AND r.id = i.continuation_run_id
             WHERE i.thread_id = c.id AND i.state = 'consumed' AND r.state IN ('pending','running'))
  OR EXISTS (SELECT 1 FROM agent_operations o WHERE o.thread_id = c.id AND o.outcome IN ('dispatching','outcome_unknown')))`

// conversationIdle reports whether the run starting under o may re-pin its
// conversation: it is not a reply continuation and the thread has no pending
// interaction and no unresolved operation.
func conversationIdle(ctx context.Context, tx pgx.Tx, o Owner) (bool, error) {
	if o.Command.InteractionID != "" {
		return false, nil
	}
	var busy bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_interactions WHERE thread_id = $1 AND state = 'pending')
		OR EXISTS (SELECT 1 FROM agent_operations WHERE thread_id = $1 AND outcome IN ('dispatching','outcome_unknown'))`, o.Command.ThreadID).Scan(&busy)
	return !busy, err
}

// ResolveDefinition returns the conversation's definition set digest for this run.
// An unpinned conversation (new, or migrated by 000009) is pinned to current;
// current is bootstrapped to bootstrap when absent. A pinned idle conversation (no
// pending interaction, not a reply continuation, no unresolved operation) is
// re-pinned to a different current in the same transaction and a definition_changed
// event records the old and new digest; a non-idle conversation keeps its pin,
// because approval bindings include the set digest. A pinned or current digest that
// this replica must use and cannot serve is ErrDefinition and nothing is written.
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
			digest = *pin
			var current string
			err := tx.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current").Scan(&current)
			if err != nil && err != pgx.ErrNoRows {
				return err
			}
			if err == nil && current != *pin {
				idle, err := conversationIdle(ctx, tx, o)
				if err != nil {
					return err
				}
				if idle {
					if !served(current) {
						return ErrDefinition
					}
					if _, err := tx.Exec(ctx, "UPDATE agent_conversations SET definition_digest=$2 WHERE id=$1", thread, current); err != nil {
						return err
					}
					detail, err := json.Marshal(map[string]string{"old_digest": *pin, "new_digest": current})
					if err != nil {
						return err
					}
					digest = current
					return appendEvent(ctx, tx, thread, Event{RunID: o.Command.RunID, Type: "definition_changed", Detail: detail})
				}
			}
			if !served(*pin) {
				return ErrDefinition
			}
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
// candidate after the candidate, the present current and every other referenced
// digest (see referencedDigests: the pins of non-idle conversations) are served by
// this replica.
func (s *Store) ActivateDefinition(ctx context.Context, expected, candidate string, served func(string) bool) error {
	if len(candidate) != 64 || served == nil || !served(candidate) {
		return ErrDefinition
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, definitionLock); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, referencedDigests)
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

// DefinitionsReady reports whether every referenced digest (the current set, if
// any, and the pins of non-idle conversations; see referencedDigests) is served. The agent_*_archived tables are never consulted.
func (s *Store) DefinitionsReady(ctx context.Context, served func(string) bool) (bool, error) {
	if served == nil {
		return false, ErrDefinition
	}
	rows, err := s.pool.Query(ctx, referencedDigests)
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
