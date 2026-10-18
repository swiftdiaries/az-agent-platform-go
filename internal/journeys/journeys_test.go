package journeys

import (
	"errors"
	"reflect"
	"testing"
)

func ids(t *testing.T, a *ActiveSet) []string {
	t.Helper()
	return a.IDs()
}

func TestActivateTable(t *testing.T) {
	tests := []struct {
		name        string
		start       []string
		id          string
		limit       int
		pinned      []string
		wantIDs     []string
		wantEvicted []string
		wantErr     error
	}{
		{name: "append below limit", start: []string{"a", "b"}, id: "c", limit: 5, wantIDs: []string{"a", "b", "c"}},
		{name: "append to empty set", start: nil, id: "a", limit: 1, wantIDs: []string{"a"}},
		{name: "already active moves to MRU without eviction", start: []string{"a", "b", "c"}, id: "a", limit: 3, wantIDs: []string{"b", "c", "a"}},
		{name: "LRU eviction at limit", start: []string{"a", "b", "c"}, id: "d", limit: 3, wantIDs: []string{"b", "c", "d"}, wantEvicted: []string{"a"}},
		{name: "evicts several to get below limit", start: []string{"a", "b", "c", "d"}, id: "e", limit: 2, wantIDs: []string{"d", "e"}, wantEvicted: []string{"a", "b", "c"}},
		{name: "pinned LRU is skipped", start: []string{"a", "b", "c"}, id: "d", limit: 3, pinned: []string{"a"}, wantIDs: []string{"a", "c", "d"}, wantEvicted: []string{"b"}},
		{name: "pinned and unpinned mixed eviction order", start: []string{"a", "b", "c", "d"}, id: "e", limit: 3, pinned: []string{"b"}, wantIDs: []string{"b", "d", "e"}, wantEvicted: []string{"a", "c"}},
		{name: "all pinned refuses and leaves set unchanged", start: []string{"a", "b"}, id: "c", limit: 2, pinned: []string{"a", "b"}, wantIDs: []string{"a", "b"}, wantErr: ErrRefused},
		{name: "pinned count equal to limit refuses", start: []string{"a", "b", "c"}, id: "d", limit: 2, pinned: []string{"a", "c"}, wantIDs: []string{"a", "b", "c"}, wantErr: ErrRefused},
		{name: "over-limit pinned set refuses until below limit", start: []string{"a", "b", "c"}, id: "d", limit: 2, pinned: []string{"a", "b", "c"}, wantIDs: []string{"a", "b", "c"}, wantErr: ErrRefused},
		{name: "pinned not in set does not block", start: []string{"a", "b"}, id: "c", limit: 2, pinned: []string{"x", "y", "z"}, wantIDs: []string{"b", "c"}, wantEvicted: []string{"a"}},
		{name: "re-activation allowed when over limit", start: []string{"a", "b", "c"}, id: "a", limit: 1, pinned: []string{"a", "b", "c"}, wantIDs: []string{"b", "c", "a"}},
		{name: "re-activation of pinned journey allowed", start: []string{"a", "b"}, id: "a", limit: 2, pinned: []string{"a", "b"}, wantIDs: []string{"b", "a"}},
		{name: "empty id is invalid", start: []string{"a"}, id: "", limit: 3, wantIDs: []string{"a"}, wantErr: ErrInvalid},
		{name: "zero limit is invalid", start: []string{"a"}, id: "b", limit: 0, wantIDs: []string{"a"}, wantErr: ErrInvalid},
		{name: "negative limit is invalid", start: []string{"a"}, id: "b", limit: -1, wantIDs: []string{"a"}, wantErr: ErrInvalid},
		{name: "invalid limit even for active id", start: []string{"a", "b"}, id: "a", limit: 0, wantIDs: []string{"a", "b"}, wantErr: ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewActiveSet(tc.start)
			evicted, err := a.Activate(tc.id, tc.limit, tc.pinned)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got := ids(t, a); !reflect.DeepEqual(got, tc.wantIDs) {
				t.Errorf("ids = %v, want %v", got, tc.wantIDs)
			}
			if len(evicted) != len(tc.wantEvicted) || (len(evicted) > 0 && !reflect.DeepEqual(evicted, tc.wantEvicted)) {
				t.Errorf("evicted = %v, want %v", evicted, tc.wantEvicted)
			}
		})
	}
}

func TestActivateErrorsAreDistinct(t *testing.T) {
	if errors.Is(ErrInvalid, ErrRefused) || errors.Is(ErrRefused, ErrInvalid) {
		t.Fatal("ErrInvalid and ErrRefused must be distinct")
	}
}

func TestActivateZeroValue(t *testing.T) {
	var a ActiveSet
	if _, err := a.Activate("a", 2, nil); err != nil {
		t.Fatal(err)
	}
	if got := a.IDs(); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("ids = %v", got)
	}
}

func TestActivateLeavesPinnedSliceAlone(t *testing.T) {
	a := NewActiveSet([]string{"a", "b"})
	pinned := []string{"a"}
	if _, err := a.Activate("c", 2, pinned); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pinned, []string{"a"}) {
		t.Errorf("pinned mutated: %v", pinned)
	}
}

func TestFitTable(t *testing.T) {
	tests := []struct {
		name        string
		start       []string
		limit       int
		pinned      []string
		wantIDs     []string
		wantEvicted []string
		wantErr     error
	}{
		{name: "lowered limit evicts LRU first", start: []string{"a", "b", "c", "d"}, limit: 2, wantIDs: []string{"c", "d"}, wantEvicted: []string{"a", "b"}},
		{name: "no-op when within limit", start: []string{"a", "b"}, limit: 3, wantIDs: []string{"a", "b"}},
		{name: "no-op when exactly at limit", start: []string{"a", "b"}, limit: 2, wantIDs: []string{"a", "b"}},
		{name: "no-op on empty set", start: nil, limit: 1, wantIDs: []string{}},
		{name: "pinned never evicted", start: []string{"a", "b", "c", "d"}, limit: 2, pinned: []string{"a", "c"}, wantIDs: []string{"a", "c"}, wantEvicted: []string{"b", "d"}},
		{name: "pinned LRU skipped for next LRU", start: []string{"a", "b", "c"}, limit: 2, pinned: []string{"a"}, wantIDs: []string{"a", "c"}, wantEvicted: []string{"b"}},
		{name: "result may exceed limit", start: []string{"a", "b", "c", "d"}, limit: 1, pinned: []string{"a", "b", "c"}, wantIDs: []string{"a", "b", "c"}, wantEvicted: []string{"d"}},
		{name: "all pinned evicts nothing", start: []string{"a", "b", "c"}, limit: 1, pinned: []string{"a", "b", "c"}, wantIDs: []string{"a", "b", "c"}},
		{name: "zero limit is invalid", start: []string{"a", "b"}, limit: 0, wantIDs: []string{"a", "b"}, wantErr: ErrInvalid},
		{name: "negative limit is invalid", start: []string{"a", "b"}, limit: -2, wantIDs: []string{"a", "b"}, wantErr: ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewActiveSet(tc.start)
			evicted, err := a.Fit(tc.limit, tc.pinned)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got := ids(t, a); !reflect.DeepEqual(got, tc.wantIDs) {
				t.Errorf("ids = %v, want %v", got, tc.wantIDs)
			}
			if len(evicted) != len(tc.wantEvicted) || (len(evicted) > 0 && !reflect.DeepEqual(evicted, tc.wantEvicted)) {
				t.Errorf("evicted = %v, want %v", evicted, tc.wantEvicted)
			}
		})
	}
}

func TestFitThenActivateKeepsInvariant(t *testing.T) {
	a := NewActiveSet([]string{"a", "b", "c", "d"})
	if _, err := a.Fit(2, nil); err != nil {
		t.Fatal(err)
	}
	evicted, err := a.Activate("e", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evicted, []string{"c"}) || !reflect.DeepEqual(a.IDs(), []string{"d", "e"}) {
		t.Errorf("evicted=%v ids=%v", evicted, a.IDs())
	}
}

func TestActivateNewActiveSetNormalizes(t *testing.T) {
	src := []string{"a", "", "b", "a", "c", "b"}
	a := NewActiveSet(src)
	if got := a.IDs(); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("ids = %v", got)
	}
	if a.Len() != 3 {
		t.Errorf("Len = %d", a.Len())
	}
	src[0] = "zzz"
	if !a.Contains("a") || a.Contains("zzz") {
		t.Error("NewActiveSet must copy its input")
	}
}

func TestActivateIDsReturnsCopy(t *testing.T) {
	a := NewActiveSet([]string{"a", "b"})
	got := a.IDs()
	got[0] = "x"
	if !reflect.DeepEqual(a.IDs(), []string{"a", "b"}) {
		t.Errorf("IDs leaked internal storage: %v", a.IDs())
	}
	empty := NewActiveSet(nil)
	if empty.IDs() == nil || len(empty.IDs()) != 0 {
		t.Errorf("empty IDs = %#v, want empty non-nil", empty.IDs())
	}
	var zero ActiveSet
	if zero.Len() != 0 || zero.Contains("a") || len(zero.IDs()) != 0 {
		t.Error("zero value must be an empty set")
	}
}

func TestActivateRemove(t *testing.T) {
	tests := []struct {
		name        string
		start       []string
		remove      []string
		wantIDs     []string
		wantRemoved []string
	}{
		{name: "removes present in set order", start: []string{"a", "b", "c"}, remove: []string{"c", "a"}, wantIDs: []string{"b"}, wantRemoved: []string{"a", "c"}},
		{name: "absent ids ignored", start: []string{"a", "b"}, remove: []string{"x", "b", "y"}, wantIDs: []string{"a"}, wantRemoved: []string{"b"}},
		{name: "nothing to remove", start: []string{"a"}, remove: nil, wantIDs: []string{"a"}},
		{name: "duplicate remove ids", start: []string{"a", "b"}, remove: []string{"a", "a"}, wantIDs: []string{"b"}, wantRemoved: []string{"a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewActiveSet(tc.start)
			removed := a.Remove(tc.remove...)
			if len(removed) != len(tc.wantRemoved) || (len(removed) > 0 && !reflect.DeepEqual(removed, tc.wantRemoved)) {
				t.Errorf("removed = %v, want %v", removed, tc.wantRemoved)
			}
			if got := a.IDs(); !reflect.DeepEqual(got, tc.wantIDs) {
				t.Errorf("ids = %v, want %v", got, tc.wantIDs)
			}
		})
	}
}
