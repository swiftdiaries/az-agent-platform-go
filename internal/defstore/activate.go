package defstore

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
)

const activateAttempts = 5

// Outcome says what an Activate call did.
type Outcome int

const (
	// Activated: the candidate became current.
	Activated Outcome = iota + 1
	// AlreadyCurrent: the candidate was already current; nothing was written.
	AlreadyCurrent
	// Superseded: the candidate was activated before and is not current; nothing was written and current is unchanged.
	Superseded
)

// Result is what Activate reports. Current is the current digest after the call.
type Result struct {
	Digest, Current string
	Outcome         Outcome
}

// Options tune Activate. Rollback permits moving current back to a set activated before.
type Options struct{ Rollback bool }

var (
	// ErrBundle marks a bundle that cannot be read or compiled.
	ErrBundle = errors.New("definition bundle invalid")
	// ErrDatabase marks migration and database failures.
	ErrDatabase = errors.New("definition store unavailable")
)

// Activate is the definition rollout entry point (the `agent-platform activate` subcommand). It migrates the
// database, reads and compiles the bundle directory, stores the compiled set's files (a no-op when the digest is
// already stored), and makes the candidate current. Without opts.Rollback it never moves current to a set that
// was activated before: it writes nothing and reports Superseded. Repeat and concurrent runs succeed. Read and
// compile failures wrap ErrBundle, database failures wrap ErrDatabase, and unservable sets are
// journal.ErrDefinition. Errors never carry bundle content.
func Activate(ctx context.Context, pool *pgxpool.Pool, bundleDir string, opts Options) (Result, error) {
	if err := journal.Migrate(ctx, pool); err != nil {
		return Result{}, fmt.Errorf("migrate database: %w: %w", ErrDatabase, err)
	}
	bundle, err := definitions.ReadBundle(os.DirFS(bundleDir))
	if err != nil {
		return Result{}, fmt.Errorf("read bundle: %w: %w", ErrBundle, err)
	}
	set, err := definitions.CompileBundle(bundle)
	if err != nil {
		return Result{}, fmt.Errorf("compile bundle: %w: %w", ErrBundle, err)
	}
	candidate := set.Digest()
	store := journal.New(pool)
	if err := store.PutDefinitionSet(ctx, candidate, set.Bundle()); err != nil {
		return Result{}, fmt.Errorf("store definition set: %w: %w", ErrDatabase, err)
	}
	loader := NewLoader(store)
	activate := store.ActivateDefinition
	if opts.Rollback {
		activate = store.RollbackDefinition
	}
	for attempt := 0; attempt < activateAttempts; attempt++ {
		current, err := store.CurrentDefinition(ctx)
		if errors.Is(err, journal.ErrDefinition) {
			current = ""
		} else if err != nil {
			return Result{}, fmt.Errorf("read current definition: %w: %w", ErrDatabase, err)
		}
		if current == candidate {
			return Result{Digest: candidate, Current: current, Outcome: AlreadyCurrent}, nil
		}
		if !opts.Rollback {
			before, err := store.DefinitionActivated(ctx, candidate)
			if err != nil {
				return Result{}, fmt.Errorf("read activation history: %w: %w", ErrDatabase, err)
			}
			if before {
				return Result{Digest: candidate, Current: current, Outcome: Superseded}, nil
			}
		}
		referenced, err := store.ReferencedDefinitions(ctx)
		if err != nil {
			return Result{}, fmt.Errorf("list referenced definitions: %w: %w", ErrDatabase, err)
		}
		for _, digest := range append([]string{candidate}, referenced...) {
			if _, err := loader.Load(ctx, digest); err != nil {
				// The loader's error carries no content; name only the digest.
				if errors.Is(err, journal.ErrDefinition) {
					return Result{}, fmt.Errorf("%w: definition set %s cannot be served", journal.ErrDefinition, digest)
				}
				return Result{}, fmt.Errorf("load definition set %s: %w: %w", digest, ErrDatabase, err)
			}
		}
		err = activate(ctx, current, candidate, loader.Served)
		switch {
		case err == nil:
			return Result{Digest: candidate, Current: candidate, Outcome: Activated}, nil
		case errors.Is(err, journal.ErrDefinitionSuperseded):
			current, err := store.CurrentDefinition(ctx)
			if err != nil && !errors.Is(err, journal.ErrDefinition) {
				return Result{}, fmt.Errorf("read current definition: %w: %w", ErrDatabase, err)
			}
			return Result{Digest: candidate, Current: current, Outcome: Superseded}, nil
		case !errors.Is(err, journal.ErrDefinition):
			return Result{}, fmt.Errorf("activate definition: %w: %w", ErrDatabase, err)
		}
		// A concurrent activation moved current, or a new digest became referenced: reload and retry.
	}
	return Result{}, fmt.Errorf("activate definition %s: %w", candidate, journal.ErrDefinition)
}
