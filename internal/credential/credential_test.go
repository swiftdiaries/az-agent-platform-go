package credential

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const (
	sentinelValue = "s1-sentinel-7f3c"
	sentinelName  = "X-Sentinel-Hdr-7f3c"
)

func sentinelCred(t *testing.T) Credential {
	t.Helper()
	r, _ := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	r.Header.Set(sentinelName, sentinelValue)
	r.Header.Add("Authorization", "Bearer "+sentinelValue)
	c := FromRequest(r, []string{sentinelName, "Authorization"})
	if c.s == nil {
		t.Fatal("expected non-empty credential")
	}
	return c
}

func assertNoLeak(t *testing.T, label, out string) {
	t.Helper()
	low := strings.ToLower(out)
	if strings.Contains(low, strings.ToLower(sentinelValue)) ||
		strings.Contains(low, strings.ToLower(sentinelName)) ||
		strings.Contains(low, "authorization") ||
		strings.Contains(low, "bearer") {
		t.Errorf("%s leaked credential material: %q", label, out)
	}
}

var verbs = []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%T", "%10s", "%-20v", "%08d", "%e", "%c", "%U"}

func TestRedactFmtVerbs(t *testing.T) {
	c := sentinelCred(t)

	type exported struct{ C Credential }
	type unexported struct{ c Credential }

	// Methods return the constant.
	if c.String() != redacted || c.GoString() != redacted {
		t.Fatalf("String/GoString = %q/%q", c.String(), c.GoString())
	}

	for _, verb := range verbs {
		for label, arg := range map[string]any{
			"value":    c,
			"pointer":  &c,
			"exported": exported{C: c},
			"slice":    []Credential{c},
			"map":      map[string]Credential{"k": c},
		} {
			out := fmt.Sprintf(verb, arg)
			assertNoLeak(t, label+" "+verb, out)
		}
	}

	// Value and pointer: redacted text for every verb except %T.
	for _, verb := range verbs {
		if verb == "%T" {
			continue
		}
		for _, arg := range []any{c, &c} {
			out := fmt.Sprintf(verb, arg)
			if !strings.Contains(out, redacted) {
				t.Errorf("%s of %T = %q, want redacted text", verb, arg, out)
			}
		}
	}

	// Unexported field: fmt reflects without calling methods.
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		out := fmt.Sprintf(verb, unexported{c: c})
		assertNoLeak(t, "unexported "+verb, out)
		out = fmt.Sprintf(verb, []unexported{{c: c}})
		assertNoLeak(t, "unexported slice "+verb, out)
		out = fmt.Sprintf(verb, &unexported{c: c})
		assertNoLeak(t, "unexported ptr "+verb, out)
	}

	// %p goes through fmt's badVerb path (methods disabled).
	assertNoLeak(t, "%p value", fmt.Sprintf("%p", c))
	assertNoLeak(t, "%p exported", fmt.Sprintf("%p", exported{C: c}))
	assertNoLeak(t, "%p unexported", fmt.Sprintf("%p", unexported{c: c}))

	assertNoLeak(t, "Sprint", fmt.Sprint(c))
	assertNoLeak(t, "Sprint pair", fmt.Sprint(c, &c, exported{C: c}))
	assertNoLeak(t, "Sprintln", fmt.Sprintln(c, &c))
	assertNoLeak(t, "Errorf", fmt.Errorf("failed: %v", c).Error())
	assertNoLeak(t, "Errorf %w-less ptr", fmt.Errorf("failed: %+v", &c).Error())
	if got := fmt.Sprint(c); got != redacted {
		t.Errorf("Sprint = %q", got)
	}

	// Zero credential also redacts (no information about emptiness).
	var zero Credential
	if got := fmt.Sprintf("%v", zero); got != redacted {
		t.Errorf("zero %%v = %q", got)
	}
}

func TestRedactJSON(t *testing.T) {
	c := sentinelCred(t)
	type wrap struct {
		C Credential `json:"c"`
	}
	type wrapPtr struct {
		C *Credential `json:"c"`
	}
	cases := map[string]any{
		"value":   c,
		"pointer": &c,
		"wrapper": wrap{C: c},
		"wrapPtr": wrapPtr{C: &c},
		"slice":   []Credential{c},
		"map":     map[string]Credential{"k": c},
	}
	for label, v := range cases {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		assertNoLeak(t, "json "+label, string(b))
		if !strings.Contains(string(b), `"[REDACTED]"`) {
			t.Errorf("json %s = %s, want redacted string", label, b)
		}
	}
	b, err := json.Marshal(c)
	if err != nil || string(b) != `"[REDACTED]"` {
		t.Errorf("Marshal = %s, %v", b, err)
	}
	b, err = json.MarshalIndent(wrap{C: c}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, "json indent", string(b))
}

func TestRedactSlog(t *testing.T) {
	c := sentinelCred(t)
	type wrap struct{ C Credential }
	type hidden struct{ c Credential }

	mk := map[string]func(*bytes.Buffer) *slog.Logger{
		"text": func(b *bytes.Buffer) *slog.Logger { return slog.New(slog.NewTextHandler(b, nil)) },
		"json": func(b *bytes.Buffer) *slog.Logger { return slog.New(slog.NewJSONHandler(b, nil)) },
	}
	for name, newLogger := range mk {
		var buf bytes.Buffer
		l := newLogger(&buf)
		l.Info("direct", slog.Any("cred", c))
		l.Info("pointer", slog.Any("cred", &c))
		l.Info("wrapper", slog.Any("w", wrap{C: c}))
		l.Info("hidden", slog.Any("h", hidden{c: c}))
		l.Info("slice", slog.Any("s", []Credential{c}))
		l.Info("group", slog.Group("g", slog.Any("cred", c), slog.String("k", "v")))
		l.With("cred", c).Info("with")
		l.Info("fmt", "msg", fmt.Sprintf("%v", c))
		out := buf.String()
		assertNoLeak(t, "slog "+name, out)
		if !strings.Contains(out, redacted) {
			t.Errorf("slog %s output lacks redaction marker: %s", name, out)
		}
	}
	if v := c.LogValue(); v.String() != redacted || v.Kind() != slog.KindString {
		t.Errorf("LogValue = %v", v)
	}
}

func TestFromRequestCopiesOnlyNamed(t *testing.T) {
	if got := FromRequest(nil, []string{"Authorization"}); got.s != nil {
		t.Error("nil request must yield zero credential")
	}
	r, _ := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	r.Header.Set("Authorization", "x")
	if got := FromRequest(r, nil); got.s != nil {
		t.Error("no names must yield zero credential")
	}
	if got := FromRequest(r, []string{"X-Absent"}); got.s != nil {
		t.Error("absent headers must yield zero credential")
	}
	c := FromRequest(r, []string{"authorization"})
	if c.s == nil || len(c.s.headers) != 1 {
		t.Fatalf("headers = %v", c.s)
	}
	if !reflect.DeepEqual(c.s.headers["Authorization"], []string{"x"}) {
		t.Errorf("headers = %v", c.s.headers)
	}
}

func TestFromRequestDeepCopiesAndCanonicalizes(t *testing.T) {
	r := &http.Request{Header: http.Header{"X-Multi": {"a", "b"}, "X-Skip": {"no"}}}
	c := FromRequest(r, []string{"x-multi", "X-Missing"})
	if c.s == nil || len(c.s.headers) != 1 {
		t.Fatalf("headers = %v", c.s)
	}
	got := c.s.headers["X-Multi"]
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("X-Multi = %v", got)
	}
	r.Header["X-Multi"][0] = "mutated"
	if got[0] != "a" {
		t.Error("credential aliases the request's value slice")
	}
	// A request with a nil header map yields the zero credential.
	if FromRequest(&http.Request{}, []string{"Authorization"}).s != nil {
		t.Error("nil header map must yield zero credential")
	}
}

func TestRedactErrorfWrap(t *testing.T) {
	c := sentinelCred(t)
	err := fmt.Errorf("wrapped: %w", fmt.Errorf("inner %v %s %+v", c, &c, struct{ C Credential }{c}))
	assertNoLeak(t, "Errorf wrap", err.Error())
}

func TestApplyForwardsOnlyConfigured(t *testing.T) {
	in, _ := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	in.Header.Add("Authorization", "Bearer a")
	in.Header.Add("x-tenant", "t1")
	in.Header.Add("X-Tenant", "t2")
	in.Header.Add("Cookie", "should-not-forward")
	in.Header.Add("X-Other", "nope")

	c := FromRequest(in, []string{"authorization", "X-TENANT"})

	out, _ := http.NewRequest(http.MethodPost, "http://mcp.invalid/", nil)
	c.Apply(out)

	if got := out.Header.Values("Authorization"); !reflect.DeepEqual(got, []string{"Bearer a"}) {
		t.Errorf("Authorization = %v", got)
	}
	if got := out.Header.Values("X-Tenant"); !reflect.DeepEqual(got, []string{"t1", "t2"}) {
		t.Errorf("X-Tenant = %v (order must be preserved)", got)
	}
	if len(out.Header) != 2 {
		t.Errorf("unexpected headers forwarded: %v", out.Header)
	}
}

func TestApplyOverridesExistingHeader(t *testing.T) {
	in, _ := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	in.Header.Set("Authorization", "Bearer user")
	c := FromRequest(in, []string{"Authorization"})

	out, _ := http.NewRequest(http.MethodPost, "http://mcp.invalid/", nil)
	out.Header.Add("Authorization", "Bearer static-1")
	out.Header.Add("Authorization", "Bearer static-2")
	out.Header.Set("X-Keep", "keep")
	c.Apply(out)

	if got := out.Header.Values("Authorization"); !reflect.DeepEqual(got, []string{"Bearer user"}) {
		t.Errorf("Authorization = %v", got)
	}
	if out.Header.Get("X-Keep") != "keep" {
		t.Error("unrelated header lost")
	}
}

func TestApplyZeroCredentialIsNoop(t *testing.T) {
	var c Credential
	out, _ := http.NewRequest(http.MethodPost, "http://mcp.invalid/", nil)
	out.Header.Set("Authorization", "static")
	c.Apply(out)
	if !reflect.DeepEqual(out.Header, http.Header{"Authorization": {"static"}}) {
		t.Errorf("headers changed: %v", out.Header)
	}
	c.Apply(nil) // must not panic
	out.Header = nil
	c.Apply(out)
	if out.Header != nil {
		t.Errorf("zero credential must not allocate headers: %v", out.Header)
	}
}

func TestApplyIsolatedFromSourceRequest(t *testing.T) {
	in, _ := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	in.Header.Add("Authorization", "orig-1")
	in.Header.Add("Authorization", "orig-2")
	c := FromRequest(in, []string{"Authorization"})

	in.Header["Authorization"][0] = "mutated"
	in.Header.Add("Authorization", "extra")
	in.Header.Set("Authorization", "replaced")
	in.Header.Del("Authorization")

	out, _ := http.NewRequest(http.MethodPost, "http://mcp.invalid/", nil)
	c.Apply(out)
	if got := out.Header.Values("Authorization"); !reflect.DeepEqual(got, []string{"orig-1", "orig-2"}) {
		t.Errorf("Authorization = %v", got)
	}
}

func TestApplyDoesNotMutateCredentialOrAliasIt(t *testing.T) {
	in, _ := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	in.Header.Set("Authorization", "v1")
	c := FromRequest(in, []string{"Authorization"})

	out1, _ := http.NewRequest(http.MethodPost, "http://mcp.invalid/", nil)
	c.Apply(out1)
	out1.Header["Authorization"][0] = "tampered"
	out1.Header.Add("Authorization", "more")

	out2, _ := http.NewRequest(http.MethodPost, "http://mcp.invalid/", nil)
	c.Apply(out2)
	if got := out2.Header.Values("Authorization"); !reflect.DeepEqual(got, []string{"v1"}) {
		t.Errorf("credential was mutated through applied request: %v", got)
	}
}

func TestApplyNilRequestHeader(t *testing.T) {
	in, _ := http.NewRequest(http.MethodGet, "http://example.invalid/", nil)
	in.Header.Set("Authorization", "v")
	c := FromRequest(in, []string{"Authorization"})

	out := &http.Request{}
	c.Apply(out)
	if got := out.Header.Get("Authorization"); got != "v" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestParseHeaderNamesValid(t *testing.T) {
	got, err := ParseHeaderNames(" authorization , x-tenant-id,X_Custom.Hdr ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Authorization", "X-Tenant-Id", "X_custom.hdr"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	got, err = ParseHeaderNames("Authorization")
	if err != nil || !reflect.DeepEqual(got, []string{"Authorization"}) {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestParseHeaderNamesBlank(t *testing.T) {
	for _, v := range []string{"", "   ", "\t", " \n "} {
		got, err := ParseHeaderNames(v)
		if err != nil || got != nil {
			t.Errorf("%q: got %v, %v; want nil, nil", v, got, err)
		}
	}
}

func TestParseHeaderNamesRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"empty entry":       "a,,b",
		"trailing comma":    "a,",
		"leading comma":     ",a",
		"only comma":        ",",
		"space inside":      "X Foo",
		"colon":             "X-Foo:",
		"colon inside":      "X:Foo",
		"CR":                "X-Foo\rbar",
		"LF":                "X-Foo\nbar",
		"trailing LF":       "X-Foo\n",
		"CRLF injection":    "X-Foo\r\nInjected: 1",
		"non-ascii":         "X-Foö",
		"slash":             "X/Foo",
		"duplicate":         "Authorization,authorization",
		"duplicate mixed":   "x-a, X-B, X-A",
		"reserved host":     "Host",
		"reserved length":   "content-length",
		"reserved type":     "Content-Type",
		"reserved te":       "Transfer-Encoding",
		"reserved conn":     "CONNECTION",
		"reserved accept":   "Accept",
		"reserved session":  "Mcp-Session-Id",
		"reserved protocol": "mcp-protocol-version",
		"reserved last":     "Last-Event-ID",
		"reserved trace":    "Traceparent",
		"reserved state":    "tracestate",
		"reserved in list":  "Authorization, Host",
	}
	for label, v := range cases {
		got, err := ParseHeaderNames(v)
		if err == nil {
			t.Errorf("%s (%q): expected error, got %v", label, v, got)
			continue
		}
		if got != nil {
			t.Errorf("%s: names must be nil on error, got %v", label, got)
		}
		if strings.Contains(err.Error(), "Injected") || strings.Contains(err.Error(), v) {
			t.Errorf("%s: error echoes raw value: %q", label, err)
		}
	}
}
