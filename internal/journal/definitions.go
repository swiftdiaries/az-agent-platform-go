package journal

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"

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
// an absent current is ErrDefinition and nothing is written (activation creates
// it, see ActivateDefinition). A pinned idle conversation (no
// pending interaction, not a reply continuation, no unresolved operation) is
// re-pinned to a different current in the same transaction and a definition_changed
// event records the old and new digest; a non-idle conversation keeps its pin,
// because approval bindings include the set digest. A pinned or current digest that
// this replica must use and cannot serve is ErrDefinition and nothing is written.
func (s *Store) ResolveDefinition(ctx context.Context, o Owner, served func(string) bool) (string, error) {
	if served == nil {
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
			return ErrDefinition
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
// candidate, forward-only: a candidate that was activated before (and is not the
// present current) is ErrDefinitionSuperseded and nothing is written; only
// RollbackDefinition moves current back. That check precedes the expected and
// served checks, so a stale pod never gets ErrDefinition retries. Otherwise the
// candidate, the present current and every other referenced digest (see
// referencedDigests: the pins of non-idle conversations) must be served by this
// replica. When current already equals candidate it returns nil at once with no
// move and no further checks, whatever expected is, so repeated init containers
// succeed. A stored candidate records its first activation time.
func (s *Store) ActivateDefinition(ctx context.Context, expected, candidate string, served func(string) bool) error {
	return s.activate(ctx, expected, candidate, served, false)
}

// RollbackDefinition is ActivateDefinition without the forward-only rule: it may
// move current to a set that was activated before. The expected, served and
// referenced-digest checks still apply.
func (s *Store) RollbackDefinition(ctx context.Context, expected, candidate string, served func(string) bool) error {
	return s.activate(ctx, expected, candidate, served, true)
}

func (s *Store) activate(ctx context.Context, expected, candidate string, served func(string) bool, rollback bool) error {
	if len(candidate) != 64 || served == nil || !served(candidate) {
		return ErrDefinition
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, definitionLock); err != nil {
			return err
		}
		var current string
		err := tx.QueryRow(ctx, "SELECT definition_digest FROM agent_definition_current FOR UPDATE").Scan(&current)
		absent := err == pgx.ErrNoRows
		if err != nil && !absent {
			return err
		}
		const markActivated = "UPDATE agent_definition_sets SET activated_at = now() WHERE definition_digest = $1 AND activated_at IS NULL"
		if !absent && current == candidate {
			_, err := tx.Exec(ctx, markActivated, candidate)
			return err
		}
		if !rollback {
			var before bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM agent_definition_sets WHERE definition_digest = $1 AND activated_at IS NOT NULL)", candidate).Scan(&before); err != nil {
				return err
			}
			if before {
				return ErrDefinitionSuperseded
			}
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
		if absent {
			if expected != "" {
				return ErrDefinition
			}
			if _, err = tx.Exec(ctx, "INSERT INTO agent_definition_current(definition_digest) VALUES($1)", candidate); err != nil {
				return err
			}
		} else {
			if !served(current) || current != expected {
				return ErrDefinition
			}
			if _, err = tx.Exec(ctx, "UPDATE agent_definition_current SET definition_digest=$1", candidate); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, markActivated, candidate)
		return err
	})
}

// DefinitionActivated reports whether the stored set digest has ever been
// activated (its activated_at is set); false for an unknown digest.
func (s *Store) DefinitionActivated(ctx context.Context, digest string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM agent_definition_sets WHERE definition_digest = $1 AND activated_at IS NOT NULL)", digest).Scan(&ok)
	return ok, err
}

// DefinitionsReady reports whether a current set exists and every referenced digest (the current set, if
// any, and the pins of non-idle conversations; see referencedDigests) is served. The agent_*_archived tables are never consulted.
func (s *Store) DefinitionsReady(ctx context.Context, served func(string) bool) (bool, error) {
	if served == nil {
		return false, ErrDefinition
	}
	var hasCurrent bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM agent_definition_current)").Scan(&hasCurrent); err != nil || !hasCurrent {
		return false, err
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

var digestPattern = regexp.MustCompile("^[0-9a-f]{64}$")

// PutDefinitionSet stores the files behind digest, append-only. A digest that is
// already stored is a no-op that keeps the first stored bytes, whatever files are
// passed: the digest is the identity, and the first writer wins.
func (s *Store) PutDefinitionSet(ctx context.Context, digest string, files map[string][]byte) error {
	if !digestPattern.MatchString(digest) || len(files) == 0 {
		return ErrDefinition
	}
	for path := range files {
		if path == "" {
			return ErrDefinition
		}
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "INSERT INTO agent_definition_sets(definition_digest) VALUES($1) ON CONFLICT DO NOTHING", digest)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		for path, content := range files {
			if _, err := tx.Exec(ctx, "INSERT INTO agent_definition_set_files(definition_digest,path,content) VALUES($1,$2,$3)", digest, path, content); err != nil {
				return err
			}
		}
		return nil
	})
}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func readDefinitionSet(ctx context.Context, q queryer, digest string) (map[string][]byte, error) {
	rows, err := q.Query(ctx, "SELECT path, content FROM agent_definition_set_files WHERE definition_digest=$1", digest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := map[string][]byte{}
	for rows.Next() {
		var path string
		var content []byte
		if err := rows.Scan(&path, &content); err != nil {
			return nil, err
		}
		files[path] = content
	}
	return files, rows.Err()
}

// DefinitionSet returns the stored files for digest; ErrDefinition when absent.
func (s *Store) DefinitionSet(ctx context.Context, digest string) (map[string][]byte, error) {
	var files map[string][]byte
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, "SELECT 1 FROM agent_definition_sets WHERE definition_digest=$1", digest).Scan(&one); err != nil {
			if err == pgx.ErrNoRows {
				return ErrDefinition
			}
			return err
		}
		var err error
		files, err = readDefinitionSet(ctx, tx, digest)
		return err
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// ReferencedDefinitions returns, sorted, every digest this replica must serve
// (see referencedDigests).
func (s *Store) ReferencedDefinitions(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, referencedDigests)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	sort.Strings(out)
	return out, rows.Err()
}
