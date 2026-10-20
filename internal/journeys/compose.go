package journeys

import "strings"

// Metadata describes one available journey.
type Metadata struct{ ID, Description string }

// Instructions is one active journey's SKILL.md body.
type Instructions struct{ ID, Body string }

// Compose renders the per-request instructions, deterministically: the trimmed base (omitted when empty),
// an "Available journeys" section listing each available journey in the given order (omitted when there are
// none), then one "Active journey" section per active journey in the given order. Sections are separated by
// one blank line and the result has no trailing newline.
func Compose(base string, available []Metadata, active []Instructions) string {
	var sections []string
	if b := strings.TrimSpace(base); b != "" {
		sections = append(sections, b)
	}
	if len(available) > 0 {
		var sb strings.Builder
		sb.WriteString("## Available journeys\nCall activate_journey with a journey ID before using that journey's tools.")
		for _, m := range available {
			sb.WriteString("\n- " + m.ID + ": " + m.Description)
		}
		sections = append(sections, sb.String())
	}
	for _, in := range active {
		s := "## Active journey: " + in.ID
		if body := strings.TrimSpace(in.Body); body != "" {
			s += "\n" + body
		}
		sections = append(sections, s)
	}
	return strings.Join(sections, "\n\n")
}
