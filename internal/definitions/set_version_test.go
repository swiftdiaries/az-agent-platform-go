package definitions

import "testing"

func TestSetVersion(t *testing.T) {
	current := compileRetainTree(t, "j", "v2 prompt\n")
	old := compileRetainTree(t, "j", "v1 prompt\n")
	gone := compileRetainTree(t, "other", "v1 prompt\n")

	t.Run("current digest returns the receiver", func(t *testing.T) {
		got, ok := current.Version(current.Digest())
		if !ok || got != current || !current.Serves(current.Digest()) {
			t.Fatalf("Version(current) = %p %v want %p", got, ok, current)
		}
	})

	retained, err := Retain(current, old, gone)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("retained copy serves its own digest as the receiver", func(t *testing.T) {
		got, ok := retained.Version(current.Digest())
		if !ok || got != retained {
			t.Fatalf("Version(current) = %p %v want receiver %p", got, ok, retained)
		}
	})

	t.Run("retained set returns the original pointer", func(t *testing.T) {
		got, ok := retained.Version(old.Digest())
		if !ok || got != old || !retained.Serves(old.Digest()) {
			t.Fatalf("Version(old) = %p %v want %p", got, ok, old)
		}
		j, ok := got.Journey("j")
		if !ok || j.Prompt != "v1 prompt\n" {
			t.Fatalf("old journey = %q %v", j.Prompt, ok)
		}
		cj, ok := retained.Journey("j")
		if !ok || cj.Prompt != "v2 prompt\n" {
			t.Fatalf("current journey = %q %v", cj.Prompt, ok)
		}
	})

	t.Run("journeys absent from current are reachable only via the old version", func(t *testing.T) {
		if _, ok := retained.Journey("other"); ok {
			t.Fatal("retained-only journey is served as current")
		}
		got, ok := retained.Version(gone.Digest())
		if !ok || got != gone {
			t.Fatalf("Version(gone) = %p %v want %p", got, ok, gone)
		}
		if j, ok := got.Journey("other"); !ok || j.Prompt != "v1 prompt\n" {
			t.Fatalf("gone journey = %q %v", j.Prompt, ok)
		}
	})

	t.Run("chained Retain keeps the original pointer", func(t *testing.T) {
		again, err := Retain(retained, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := again.Version(old.Digest()); !ok || got != old {
			t.Fatalf("retained copy retained again: %p %v want %p", got, ok, old)
		}
		if got, ok := again.Version(again.Digest()); !ok || got != again {
			t.Fatalf("own digest after chain: %p %v want %p", got, ok, again)
		}
		chained, err := Retain(current, retained)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := chained.Version(old.Digest()); !ok || got != old {
			t.Fatalf("current retaining a retained copy: %p %v want %p", got, ok, old)
		}
		if got, ok := chained.Version(gone.Digest()); !ok || got != gone {
			t.Fatalf("chained gone: %p %v want %p", got, ok, gone)
		}
		other, err := Retain(gone, old)
		if err != nil {
			t.Fatal(err)
		}
		viaCopy, err := Retain(current, other)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := viaCopy.Version(gone.Digest()); !ok || got != gone {
			t.Fatalf("retained copy's own digest must map to the original: %p %v want %p", got, ok, gone)
		}
		if got, ok := viaCopy.Version(old.Digest()); !ok || got != old {
			t.Fatalf("viaCopy old: %p %v want %p", got, ok, old)
		}
		if got, ok := viaCopy.Version(current.Digest()); !ok || got != viaCopy {
			t.Fatalf("current digest must stay the receiver: %p %v want %p", got, ok, viaCopy)
		}
	})

	t.Run("unknown digest and empty string are not served", func(t *testing.T) {
		for _, digest := range []string{"nope", ""} {
			if got, ok := retained.Version(digest); ok || got != nil || retained.Serves(digest) {
				t.Fatalf("Version(%q) = %p %v", digest, got, ok)
			}
			if got, ok := current.Version(digest); ok || got != nil || current.Serves(digest) {
				t.Fatalf("current Version(%q) = %p %v", digest, got, ok)
			}
		}
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var s *Set
		if got, ok := s.Version(current.Digest()); ok || got != nil || s.Serves(current.Digest()) {
			t.Fatalf("nil Version = %p %v", got, ok)
		}
	})

	t.Run("Retain does not mutate its inputs", func(t *testing.T) {
		if current.Serves(old.Digest()) || current.Serves(gone.Digest()) {
			t.Fatal("Retain made the input current set serve old digests")
		}
		if old.Serves(current.Digest()) || gone.Serves(current.Digest()) {
			t.Fatal("Retain made a retained input serve the current digest")
		}
		if got, ok := current.Version(current.Digest()); !ok || got != current {
			t.Fatal("current no longer serves itself")
		}
	})
}
