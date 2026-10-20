package runtime

import (
	"encoding/json"
	"slices"

	"github.com/microsoft/agent-framework-go/message"
)

// bytesPerToken is the fixed estimate over the JSON-serialized messages. There
// is no tokenizer dependency, so the same history and budget always agree.
const bytesPerToken = 4

// Truncation reports what trimHistory dropped. All counts; never content.
type Truncation struct {
	TurnsDropped, MessagesDropped int
	EstimateBefore, EstimateAfter int
	Budget                        int
}

func estimateTokens(history []*message.Message) int {
	data, err := json.Marshal(history)
	if err != nil {
		return 0
	}
	return (len(data) + bytesPerToken - 1) / bytesPerToken
}

// trimHistory drops the oldest whole turns (a turn starts at a user message and
// runs to the next one; messages before the first user message form a leading
// turn) until the estimate fits budget. The latest turn and every turn holding a
// function call whose CallID is in protected are kept regardless. A call and its
// result are never split: if a result lies in a later turn than its call, the
// turns between them are one unit. If the kept turns alone exceed the budget they
// are returned as is. budget <= 0 or an already fitting history returns a copy
// and a zero Truncation. The result is always a new slice and deterministic.
func trimHistory(history []*message.Message, budget int, protected map[string]bool) ([]*message.Message, Truncation) {
	out := slices.Clone(history)
	if budget <= 0 || len(history) == 0 {
		return out, Truncation{}
	}
	before := estimateTokens(history)
	if before <= budget {
		return out, Truncation{}
	}
	// turnOf[i] is the turn index of message i.
	turnOf := make([]int, len(history))
	turns := 0
	for i, m := range history {
		if m.Role == message.RoleUser && i > 0 {
			turns++
		}
		turnOf[i] = turns
	}
	turns++
	// unit[t] is the lowest turn index of the unit holding turn t.
	unit := make([]int, turns)
	for t := range unit {
		unit[t] = t
	}
	callTurn := map[string]int{}
	keep := make([]bool, turns)
	keep[turns-1] = true
	for i, m := range history {
		for _, c := range m.Contents {
			switch content := c.(type) {
			case *message.FunctionCallContent:
				callTurn[content.CallID] = turnOf[i]
				if protected[content.CallID] {
					keep[turnOf[i]] = true
				}
			case *message.FunctionResultContent:
				if from, ok := callTurn[content.CallID]; ok && from < turnOf[i] {
					for t := from + 1; t <= turnOf[i]; t++ {
						unit[t] = unit[from]
					}
				}
			}
		}
	}
	// Units are contiguous turn ranges; a unit is kept when any of its turns is.
	unitKept := map[int]bool{}
	for t, k := range keep {
		if k {
			unitKept[unit[t]] = true
		}
	}
	dropped := make([]bool, len(history))
	current := slices.Clone(history)
	estimate := before
	var droppedTurns, droppedMessages int
	for start := 0; start < turns && estimate > budget; {
		end := start
		for end+1 < turns && unit[end+1] == unit[start] {
			end++
		}
		if !unitKept[unit[start]] {
			for i, t := range turnOf {
				if t >= start && t <= end {
					dropped[i] = true
					droppedMessages++
				}
			}
			droppedTurns += end - start + 1
			current = current[:0]
			for i, m := range history {
				if !dropped[i] {
					current = append(current, m)
				}
			}
			estimate = estimateTokens(current)
		}
		start = end + 1
	}
	if droppedTurns == 0 {
		return out, Truncation{}
	}
	return slices.Clone(current), Truncation{TurnsDropped: droppedTurns, MessagesDropped: droppedMessages, EstimateBefore: before, EstimateAfter: estimate, Budget: budget}
}
