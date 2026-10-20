package cost

import (
	"context"
	"sync"

	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
)

// Scripted is the test gate. Admit calls the scripted function (default:
// allow) and records every attribution; Access delegates to inner (nil ->
// Open) and records every attribution.
type Scripted struct {
	mu       sync.Mutex
	fn       func(Attribution, settings.Cost) (Decision, error)
	inner    Gate
	admits   []Attribution
	accesses []Attribution
}

var _ Gate = (*Scripted)(nil)

// NewScripted returns a Scripted gate delegating Access to inner.
func NewScripted(inner Gate) *Scripted {
	if inner == nil {
		inner = Open{}
	}
	return &Scripted{inner: inner}
}

// SetAdmit installs the admit function; nil restores allow-all.
func (s *Scripted) SetAdmit(fn func(Attribution, settings.Cost) (Decision, error)) {
	s.mu.Lock()
	s.fn = fn
	s.mu.Unlock()
}

func (s *Scripted) Admit(_ context.Context, a Attribution, c settings.Cost) (Decision, error) {
	s.mu.Lock()
	s.admits = append(s.admits, a)
	fn := s.fn
	s.mu.Unlock()
	if fn == nil {
		return Allow(), nil
	}
	return fn(a, c)
}

func (s *Scripted) Access(ctx context.Context, a Attribution, c settings.Cost) (Access, error) {
	s.mu.Lock()
	s.accesses = append(s.accesses, a)
	s.mu.Unlock()
	return s.inner.Access(ctx, a, c)
}

// Admits returns a copy of the recorded Admit attributions.
func (s *Scripted) Admits() []Attribution {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Attribution(nil), s.admits...)
}

// Accesses returns a copy of the recorded Access attributions.
func (s *Scripted) Accesses() []Attribution {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Attribution(nil), s.accesses...)
}
