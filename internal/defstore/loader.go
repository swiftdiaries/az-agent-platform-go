// Package defstore serves compiled definition sets from the journal's definition store.
package defstore

import (
	"context"
	"fmt"
	"sync"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
)

// Loader serves compiled definition sets from the journal's definition store, cached by set digest.
type Loader struct {
	store *journal.Store
	mu    sync.Mutex
	sets  map[string]*definitions.Set
}

func NewLoader(store *journal.Store) *Loader {
	return &Loader{store: store, sets: map[string]*definitions.Set{}}
}

// Load returns the compiled set for digest, from the cache or the store. An absent digest, stored files that
// do not compile, or files that compile to another digest are journal.ErrDefinition (wrapped; the error text
// never carries bundle content). Database errors are returned as they are. Only successes are cached.
func (l *Loader) Load(ctx context.Context, digest string) (*definitions.Set, error) {
	l.mu.Lock()
	set, ok := l.sets[digest]
	l.mu.Unlock()
	if ok {
		return set, nil
	}
	files, err := l.store.DefinitionSet(ctx, digest)
	if err != nil {
		return nil, err
	}
	set, err = definitions.CompileBundle(definitions.Bundle(files))
	if err != nil {
		return nil, fmt.Errorf("%w: stored definition set does not compile", journal.ErrDefinition)
	}
	if set.Digest() != digest {
		return nil, fmt.Errorf("%w: stored definition set digest mismatch", journal.ErrDefinition)
	}
	l.mu.Lock()
	l.sets[digest] = set
	l.mu.Unlock()
	return set, nil
}

// Served reports whether digest is already cached. It never does I/O, so it is safe inside a journal transaction.
func (l *Loader) Served(digest string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.sets[digest]
	return ok
}
