package cost

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const budgetExceeded = "budget_exceeded"

// ClassifyLimit maps an enforcement rejection to a *LimitError. Status 429
// qualifies: it is a budget limit when the JSON body carries code or type
// "budget_exceeded", top-level or under "error"; otherwise a rate limit.
// Status 400 qualifies only with that budget signal (LiteLLM reports an
// exhausted budget as 400); a plain 400 is not a limit. Other statuses never
// qualify.
func ClassifyLimit(status int, header http.Header, body []byte, now time.Time) (*LimitError, bool) {
	budget := bodySignalsBudget(body)
	kind := LimitRate
	switch {
	case status == http.StatusBadRequest && budget:
		kind = LimitBudget
	case status == http.StatusTooManyRequests:
		if budget {
			kind = LimitBudget
		}
	default:
		return nil, false
	}
	return &LimitError{Kind: kind, RetryAfter: ParseRetryAfter(header, now)}, true
}

func bodySignalsBudget(body []byte) bool {
	var top map[string]any
	if json.Unmarshal(body, &top) != nil {
		return false
	}
	if hasBudget(top) {
		return true
	}
	inner, _ := top["error"].(map[string]any)
	return hasBudget(inner)
}

func hasBudget(m map[string]any) bool {
	for _, k := range []string{"code", "type"} {
		if v, _ := m[k].(string); v == budgetExceeded {
			return true
		}
	}
	return false
}

// ParseRetryAfter reads retry-after-ms (milliseconds) first, then
// Retry-After (delta-seconds or HTTP-date relative to now). Missing, invalid
// or negative yields 0.
func ParseRetryAfter(header http.Header, now time.Time) time.Duration {
	if v := strings.TrimSpace(header.Get("Retry-After-Ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 && ms < float64(math.MaxInt64/int64(time.Millisecond)) {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	v := strings.TrimSpace(header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 || secs > math.MaxInt64/int64(time.Second) {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
