package cost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/az-agent-platform-go/internal/settings"
)

const secretVal = "tenant-SECRET-value"

func secretAccess(t *testing.T) Access {
	t.Helper()
	h, err := NewHeaders(DefaultHeaderNames())
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.Access(context.Background(), Attribution{Tenant: secretVal, Run: "run-SECRET"}, settings.Cost{Profile: "prof-SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRedactAccessFormatting(t *testing.T) {
	a := secretAccess(t)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d"} {
		got := fmt.Sprintf(verb, a)
		if strings.Contains(got, "SECRET") || !strings.Contains(got, "[REDACTED]") {
			t.Errorf("%s: %q", verb, got)
		}
		gotp := fmt.Sprintf(verb, &a)
		if strings.Contains(gotp, "SECRET") {
			t.Errorf("ptr %s: %q", verb, gotp)
		}
	}
	if got := fmt.Sprint(a); got != "[REDACTED]" {
		t.Errorf("Sprint: %q", got)
	}
}

func TestRedactAccessJSON(t *testing.T) {
	a := secretAccess(t)
	b, err := json.Marshal(a)
	if err != nil || string(b) != `"[REDACTED]"` {
		t.Fatalf("%s %v", b, err)
	}
	type wrap struct {
		Name string
		A    Access
		a    Access
	}
	b, err = json.Marshal(wrap{Name: "x", A: a, a: a})
	if err != nil || strings.Contains(string(b), "SECRET") || !strings.Contains(string(b), "[REDACTED]") {
		t.Fatalf("%s %v", b, err)
	}
	s := fmt.Sprintf("%+v %#v", wrap{A: a, a: a}, wrap{A: a, a: a})
	if strings.Contains(s, "SECRET") {
		t.Fatalf("struct format leaked: %s", s)
	}
}

func TestRedactAccessSlog(t *testing.T) {
	a := secretAccess(t)
	var jb, tb bytes.Buffer
	slog.New(slog.NewJSONHandler(&jb, nil)).Info("m", "access", a)
	slog.New(slog.NewTextHandler(&tb, nil)).Info("m", "access", a)
	for _, out := range []string{jb.String(), tb.String()} {
		if strings.Contains(out, "SECRET") || !strings.Contains(out, "[REDACTED]") {
			t.Errorf("slog: %s", out)
		}
	}
}

func TestRedactLimitErrorNoHeaderValues(t *testing.T) {
	h := http.Header{"Retry-After": {"7"}, "X-Secret": {"SECRET"}}
	le, ok := ClassifyLimit(429, h, []byte(`{"error":{"code":"budget_exceeded","message":"SECRET"}}`), time.Now())
	if !ok {
		t.Fatal("not ok")
	}
	if strings.Contains(le.Error(), "SECRET") {
		t.Fatal(le.Error())
	}
	if le.Error() != "limit exceeded: budget (retry after 7s)" {
		t.Fatal(le.Error())
	}
}

func TestRedactErrorsNeverEchoValues(t *testing.T) {
	for _, spec := range []string{"tenant=Bad Name SECRET", "bogusSECRET=X-A", "tenant=Authorization", "tenant=X-A,tenant=X-B"} {
		if _, err := ParseHeaderNames(spec); err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "Authorization") {
			t.Errorf("%q: %v", spec, err)
		}
	}
	if _, err := New("weirdSECRET", ""); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("New: %v", err)
	}
}

func TestHeadersDefaultNames(t *testing.T) {
	n, err := ParseHeaderNames("  ")
	if err != nil || n != DefaultHeaderNames() {
		t.Fatal(n, err)
	}
	d := DefaultHeaderNames()
	want := HeaderNames{"X-Agent-Tenant", "X-Agent-Principal", "X-Agent-Conversation", "X-Agent-Run", "X-Agent-Journey", "X-Agent-Cost-Profile"}
	if d != want {
		t.Fatalf("%+v", d)
	}
}

func TestHeadersCustomSpec(t *testing.T) {
	n, err := ParseHeaderNames("tenant=x-team, profile=X-Budget")
	if err != nil {
		t.Fatal(err)
	}
	if n.Tenant != "X-Team" || n.Profile != "X-Budget" || n.Run != "" || n.Principal != "" {
		t.Fatalf("%+v", n)
	}
}

func TestHeadersParseErrors(t *testing.T) {
	for _, spec := range []string{
		"tenant", "=X-A", "tenant=", "color=X-A", "tenant=X-A,tenant=X-B",
		"tenant=X-A,run=X-A", "tenant=X-A,run=x-a", "tenant=bad name", "tenant=a;b",
		"tenant=Authorization", "tenant=proxy-authorization", "tenant=Cookie", "tenant=Api-Key",
		"tenant=X-Api-Key", "tenant=Host", "tenant=Content-Length", "tenant=Content-Type",
		"tenant=Transfer-Encoding", "tenant=Connection", "tenant=Accept", "tenant=Traceparent",
		"tenant=X-A,,run=X-B",
	} {
		if _, err := ParseHeaderNames(spec); err == nil {
			t.Errorf("%q accepted", spec)
		}
	}
}

func TestHeadersNewValidates(t *testing.T) {
	if _, err := NewHeaders(HeaderNames{Tenant: "Authorization"}); err == nil {
		t.Error("reserved accepted")
	}
	if _, err := NewHeaders(HeaderNames{Tenant: "X-A", Run: "X-A"}); err == nil {
		t.Error("dup accepted")
	}
	if _, err := NewHeaders(HeaderNames{Tenant: "bad name"}); err == nil {
		t.Error("non-token accepted")
	}
	if _, err := NewHeaders(HeaderNames{}); err != nil {
		t.Error(err)
	}
}

func TestHeadersApplySetsAndReplaces(t *testing.T) {
	h, _ := NewHeaders(DefaultHeaderNames())
	a := Attribution{Tenant: "t1", Principal: "p1", Conversation: "c1", Run: "r1", Journey: "j1"}
	acc, err := h.Access(context.Background(), a, settings.Cost{Profile: "gold"})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", "http://x", nil)
	req.Header.Set("X-Agent-Tenant", "old")
	req.Header.Add("X-Agent-Tenant", "old2")
	req.Header.Set("Other", "keep")
	acc.Apply(req)
	for k, v := range map[string]string{"X-Agent-Tenant": "t1", "X-Agent-Principal": "p1", "X-Agent-Conversation": "c1", "X-Agent-Run": "r1", "X-Agent-Journey": "j1", "X-Agent-Cost-Profile": "gold", "Other": "keep"} {
		if got := req.Header.Values(k); len(got) != 1 || got[0] != v {
			t.Errorf("%s = %v", k, got)
		}
	}
}

func TestHeadersOmitEmptyAndControl(t *testing.T) {
	h, _ := NewHeaders(DefaultHeaderNames())
	a := Attribution{Tenant: "ok", Principal: "a\r\nX-Evil: 1", Conversation: "b\nc", Run: "t\tab", Journey: ""}
	acc, _ := h.Access(context.Background(), a, settings.Cost{Profile: "p\x00q"})
	req, _ := http.NewRequest("POST", "http://x", nil)
	req.Header.Set("X-Agent-Principal", "stale")
	req.Header.Set("X-Agent-Journey", "stale")
	acc.Apply(req)
	if req.Header.Get("X-Agent-Tenant") != "ok" {
		t.Error("tenant lost")
	}
	for _, k := range []string{"X-Agent-Principal", "X-Agent-Conversation", "X-Agent-Run", "X-Agent-Journey", "X-Agent-Cost-Profile", "X-Evil"} {
		if _, ok := req.Header[k]; ok {
			t.Errorf("%s present: %v", k, req.Header[k])
		}
	}
}

func TestHeadersAdmitAllows(t *testing.T) {
	h, _ := NewHeaders(DefaultHeaderNames())
	d, err := h.Admit(context.Background(), Attribution{}, settings.Cost{})
	if err != nil || !d.Allow || d.Err() != nil {
		t.Fatal(d, err)
	}
}

func TestHeadersZeroAccess(t *testing.T) {
	var a Access
	a.Apply(nil)
	req, _ := http.NewRequest("GET", "http://x", nil)
	a.Apply(req)
	if len(req.Header) != 0 {
		t.Fatal(req.Header)
	}
}

func TestScriptedDefaultAllowAndRecords(t *testing.T) {
	s := NewScripted(nil)
	a1 := Attribution{Run: "r1"}
	d, err := s.Admit(context.Background(), a1, settings.Cost{})
	if err != nil || !d.Allow {
		t.Fatal(d, err)
	}
	acc, err := s.Access(context.Background(), Attribution{Run: "r2"}, settings.Cost{})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "http://x", nil)
	acc.Apply(req)
	if len(req.Header) != 0 {
		t.Fatal("open inner decorated")
	}
	if got := s.Admits(); len(got) != 1 || got[0] != a1 {
		t.Fatal(got)
	}
	if got := s.Accesses(); len(got) != 1 || got[0].Run != "r2" {
		t.Fatal(got)
	}
	s.Admits()[0].Run = "mutated"
	if s.Admits()[0].Run != "r1" {
		t.Fatal("Admits not a copy")
	}
}

func TestScriptedDeny(t *testing.T) {
	s := NewScripted(nil)
	s.SetAdmit(func(a Attribution, c settings.Cost) (Decision, error) {
		return Deny(LimitRate, 3*time.Second), nil
	})
	d, err := s.Admit(context.Background(), Attribution{}, settings.Cost{})
	var le *LimitError
	if err != nil || !errors.As(d.Err(), &le) || le.Kind != LimitRate || le.RetryAfter != 3*time.Second {
		t.Fatal(d, err)
	}
	boom := errors.New("boom")
	s.SetAdmit(func(Attribution, settings.Cost) (Decision, error) { return Decision{}, boom })
	if _, err := s.Admit(context.Background(), Attribution{}, settings.Cost{}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if len(s.Admits()) != 2 {
		t.Fatal(len(s.Admits()))
	}
}

func TestScriptedAccessDelegates(t *testing.T) {
	h, _ := NewHeaders(DefaultHeaderNames())
	s := NewScripted(h)
	acc, err := s.Access(context.Background(), Attribution{Tenant: "t9"}, settings.Cost{})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "http://x", nil)
	acc.Apply(req)
	if req.Header.Get("X-Agent-Tenant") != "t9" {
		t.Fatal(req.Header)
	}
}

func TestOpenGate(t *testing.T) {
	var g Gate = Open{}
	d, err := g.Admit(context.Background(), Attribution{}, settings.Cost{})
	if err != nil || !d.Allow {
		t.Fatal(d, err)
	}
	acc, err := g.Access(context.Background(), Attribution{Tenant: "t"}, settings.Cost{})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "http://x", nil)
	acc.Apply(req)
	if len(req.Header) != 0 {
		t.Fatal(req.Header)
	}
}

func TestNewSelectsAdapter(t *testing.T) {
	for _, k := range []string{"", "open"} {
		g, err := New(k, "ignored-not-parsed")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := g.(Open); !ok {
			t.Fatalf("%q: %T", k, g)
		}
	}
	g, err := New("headers", "tenant=X-T")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.(*Headers); !ok {
		t.Fatalf("%T", g)
	}
	if _, err := New("headers", "bogus"); err == nil {
		t.Error("bad spec accepted")
	}
	if _, err := New("nope", ""); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestDecisionAllowDeny(t *testing.T) {
	if a := Allow(); !a.Allow || a.Err() != nil {
		t.Fatal(a)
	}
	d := Deny(LimitRate, 2*time.Second)
	var le *LimitError
	if d.Allow || !errors.As(d.Err(), &le) || le.Kind != LimitRate || le.RetryAfter != 2*time.Second {
		t.Fatal(d)
	}
	if !errors.Is(d.Err(), ErrLimitExceeded) {
		t.Fatal("no sentinel")
	}
	if d.Err().Error() != "limit exceeded: rate (retry after 2s)" {
		t.Fatal(d.Err())
	}
}

func TestDecisionDefaultKindIsBudget(t *testing.T) {
	d := Decision{}
	var le *LimitError
	if !errors.As(d.Err(), &le) || le.Kind != LimitBudget {
		t.Fatal(d.Err())
	}
	if d.Err().Error() != "limit exceeded: budget" {
		t.Fatal(d.Err())
	}
}

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestClassifyLimitAgentgateway(t *testing.T) {
	le, ok := ClassifyLimit(429, nil, []byte(`{"error":{"code":"budget_exceeded","message":"m"}}`), now)
	if !ok || le.Kind != LimitBudget {
		t.Fatal(le, ok)
	}
}

func TestClassifyLimitLiteLLM(t *testing.T) {
	le, ok := ClassifyLimit(429, nil, []byte(`{"error":{"type":"budget_exceeded"}}`), now)
	if !ok || le.Kind != LimitBudget {
		t.Fatal(le, ok)
	}
	le, ok = ClassifyLimit(429, nil, []byte(`{"type":"budget_exceeded"}`), now)
	if !ok || le.Kind != LimitBudget {
		t.Fatal("top-level type", le, ok)
	}
	le, ok = ClassifyLimit(429, nil, []byte(`{"code":"budget_exceeded"}`), now)
	if !ok || le.Kind != LimitBudget {
		t.Fatal("top-level code", le, ok)
	}
}

func TestClassifyLimitPlain429IsRate(t *testing.T) {
	for _, body := range []string{"", "not json", `{"error":"slow down"}`, `{"error":{"code":"rate_limited"}}`, `[1]`, `{"error":{"code":5}}`} {
		le, ok := ClassifyLimit(429, nil, []byte(body), now)
		if !ok || le.Kind != LimitRate {
			t.Errorf("%q: %v %v", body, le, ok)
		}
	}
}

func TestClassifyLimitNon429(t *testing.T) {
	for _, st := range []int{200, 400, 500, 503} {
		if le, ok := ClassifyLimit(st, nil, []byte(`{"error":{"code":"budget_exceeded"}}`), now); ok || le != nil {
			t.Errorf("%d ok", st)
		}
	}
}

func TestClassifyLimitRetryAfter(t *testing.T) {
	le, _ := ClassifyLimit(429, http.Header{"Retry-After": {"12"}}, nil, now)
	if le.RetryAfter != 12*time.Second {
		t.Fatal(le.RetryAfter)
	}
	date := now.Add(90 * time.Second).UTC().Format(http.TimeFormat)
	le, _ = ClassifyLimit(429, http.Header{"Retry-After": {date}}, nil, now)
	if le.RetryAfter != 90*time.Second {
		t.Fatal(le.RetryAfter)
	}
	le, _ = ClassifyLimit(429, http.Header{"Retry-After-Ms": {"1500"}, "Retry-After": {"9"}}, nil, now)
	if le.RetryAfter != 1500*time.Millisecond {
		t.Fatal(le.RetryAfter)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		h    http.Header
		want time.Duration
	}{
		{nil, 0},
		{http.Header{"Retry-After": {"-5"}}, 0},
		{http.Header{"Retry-After": {"soon"}}, 0},
		{http.Header{"Retry-After": {now.Add(-time.Hour).UTC().Format(http.TimeFormat)}}, 0},
		{http.Header{"Retry-After-Ms": {"-1"}}, 0},
		{http.Header{"Retry-After-Ms": {"x"}, "Retry-After": {"4"}}, 4 * time.Second},
		{http.Header{"Retry-After-Ms": {"99999999999999999999"}}, 0},
		{http.Header{"Retry-After": {"99999999999999999999"}}, 0},
		{http.Header{"Retry-After": {" 3 "}}, 3 * time.Second},
	}
	for i, c := range cases {
		if got := ParseRetryAfter(c.h, now); got != c.want {
			t.Errorf("%d: %v want %v", i, got, c.want)
		}
	}
}
