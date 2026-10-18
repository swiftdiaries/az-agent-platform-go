package journeys

import "testing"

const availableHeader = "## Available journeys\nCall activate_journey with a journey ID before using that journey's tools."

func TestComposeGolden(t *testing.T) {
	got := Compose(
		"  You are the conversation agent.\n\n",
		[]Metadata{{ID: "billing", Description: "Handle invoices"}, {ID: "travel", Description: "Book trips"}},
		[]Instructions{{ID: "travel", Body: "\n  Ask for dates first.\n"}, {ID: "billing", Body: "Check the ledger."}},
	)
	want := "You are the conversation agent.\n\n" +
		availableHeader + "\n" +
		"- billing: Handle invoices\n" +
		"- travel: Book trips\n\n" +
		"## Active journey: travel\nAsk for dates first.\n\n" +
		"## Active journey: billing\nCheck the ledger."
	if got != want {
		t.Errorf("Compose mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestComposeEmptyBase(t *testing.T) {
	got := Compose(" \n\t", []Metadata{{ID: "a", Description: "A"}}, []Instructions{{ID: "a", Body: "body"}})
	want := availableHeader + "\n- a: A\n\n## Active journey: a\nbody"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeNoAvailable(t *testing.T) {
	got := Compose("base", nil, []Instructions{{ID: "a", Body: "body"}})
	want := "base\n\n## Active journey: a\nbody"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeNoActive(t *testing.T) {
	got := Compose("base", []Metadata{{ID: "a", Description: "A"}}, nil)
	want := "base\n\n" + availableHeader + "\n- a: A"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeNothingAtAll(t *testing.T) {
	if got := Compose("", nil, nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := Compose("  base \n", nil, nil); got != "base" {
		t.Errorf("got %q, want %q", got, "base")
	}
}

func TestComposeOrderPreserved(t *testing.T) {
	got := Compose("",
		[]Metadata{{ID: "z", Description: "Z"}, {ID: "m", Description: "M"}, {ID: "a", Description: "A"}},
		[]Instructions{{ID: "m", Body: "M body"}, {ID: "z", Body: "Z body"}, {ID: "a", Body: "A body"}},
	)
	want := availableHeader + "\n- z: Z\n- m: M\n- a: A\n\n" +
		"## Active journey: m\nM body\n\n" +
		"## Active journey: z\nZ body\n\n" +
		"## Active journey: a\nA body"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestComposeNoTrailingNewline(t *testing.T) {
	got := Compose("base\n", []Metadata{{ID: "a", Description: "A"}}, []Instructions{{ID: "a", Body: "body\n\n"}})
	if got == "" || got[len(got)-1] == '\n' {
		t.Errorf("trailing newline in %q", got)
	}
}

func TestComposeEmptyBodyActiveJourney(t *testing.T) {
	got := Compose("base", nil, []Instructions{{ID: "a", Body: "  "}})
	want := "base\n\n## Active journey: a"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
