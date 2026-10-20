// Package journeys holds the pure logic behind per-conversation journey
// activation: the least-recently-used active set with pinned journeys and a
// per-tenant limit (recency is bumped by activation and by Touch on use), and the composition of the per-request instructions from
// the base prompt, available journey metadata and active journey bodies.
// It performs no I/O and imports only the standard library.
package journeys

import (
	"errors"
	"slices"
)

var (
	// ErrInvalid reports an activation with an empty ID or a limit below 1.
	ErrInvalid = errors.New("journeys: invalid activation")
	// ErrRefused reports an activation that cannot make room because too many active journeys are pinned.
	ErrRefused = errors.New("journeys: activation refused: active journeys are pinned")
)

// ActiveSet is the ordered set of active journey IDs, least recently used first.
// The zero value is an empty set.
type ActiveSet struct {
	ids []string
}

// NewActiveSet copies ids (LRU first), dropping empty and duplicate IDs (first occurrence wins).
func NewActiveSet(ids []string) *ActiveSet {
	a := &ActiveSet{ids: make([]string, 0, len(ids))}
	for _, id := range ids {
		if id == "" || a.Contains(id) {
			continue
		}
		a.ids = append(a.ids, id)
	}
	return a
}

// IDs returns a copy of the active IDs, least recently used first (never nil).
func (a *ActiveSet) IDs() []string {
	return append(make([]string, 0, len(a.ids)), a.ids...)
}

// Contains reports whether id is active.
func (a *ActiveSet) Contains(id string) bool { return slices.Contains(a.ids, id) }

// Len returns the number of active journeys.
func (a *ActiveSet) Len() int { return len(a.ids) }

// Remove drops ids (no error if absent) and returns the ones that were present, in set order.
func (a *ActiveSet) Remove(ids ...string) []string {
	var removed []string
	kept := a.ids[:0:0]
	for _, id := range a.ids {
		if slices.Contains(ids, id) {
			removed = append(removed, id)
			continue
		}
		kept = append(kept, id)
	}
	a.ids = kept
	return removed
}

// Touch moves id to most recently used. No eviction; no-op (false) when id is absent or already MRU.
func (a *ActiveSet) Touch(id string) (changed bool) {
	i := slices.Index(a.ids, id)
	if i < 0 || i == len(a.ids)-1 {
		return false
	}
	a.ids = append(slices.Delete(a.ids, i, i+1), id)
	return true
}

// Activate makes id the most recently used journey.
//   - id already active: it only moves to MRU; nothing is evicted, even when the set exceeds limit.
//   - otherwise the least recently used unpinned journeys are evicted until Len() < limit, then id is
//     appended. If that is impossible (too many pinned journeys), nothing changes and ErrRefused is returned.
//
// id == "" or limit < 1 returns ErrInvalid and changes nothing. evicted is in eviction order (LRU first).
func (a *ActiveSet) Activate(id string, limit int, pinned []string) (evicted []string, err error) {
	if id == "" || limit < 1 {
		return nil, ErrInvalid
	}
	if i := slices.Index(a.ids, id); i >= 0 {
		a.ids = append(slices.Delete(a.ids, i, i+1), id)
		return nil, nil
	}
	evicted, enough := a.victims(limit-1, pinned)
	if !enough {
		return nil, ErrRefused
	}
	a.Remove(evicted...)
	a.ids = append(a.ids, id)
	return evicted, nil
}

// Fit evicts least recently used unpinned journeys until Len() <= limit. Pinned journeys are never
// evicted, so the result may still exceed limit. limit < 1 returns ErrInvalid and changes nothing.
func (a *ActiveSet) Fit(limit int, pinned []string) (evicted []string, err error) {
	if limit < 1 {
		return nil, ErrInvalid
	}
	evicted, _ = a.victims(limit, pinned)
	a.Remove(evicted...)
	return evicted, nil
}

// victims returns the least recently used unpinned journeys that must go for Len() <= max, and whether
// that is enough to reach max. It does not modify the set.
func (a *ActiveSet) victims(max int, pinned []string) (victims []string, enough bool) {
	excess := len(a.ids) - max
	for _, id := range a.ids {
		if len(victims) >= excess {
			break
		}
		if !slices.Contains(pinned, id) {
			victims = append(victims, id)
		}
	}
	return victims, len(victims) >= excess
}
