package chat

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticBearerTokensTenantForm(t *testing.T) {
	tokens := StaticBearerTokens{
		"legacy":   "alice",
		"a":        "tenant-a/alice",
		"b":        "tenant-b/alice",
		"slashes":  "t/a/b",
		"notenant": "/alice",
		"nouser":   "t/",
	}
	request := func(header string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "https://chat.example.test/", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		return req
	}
	for name, test := range map[string]struct {
		token string
		want  Identity
	}{
		"legacy":      {"legacy", Identity{Tenant: "", Principal: "alice"}},
		"tenant form": {"a", Identity{Tenant: "tenant-a", Principal: "tenant-a/alice"}},
		"other":       {"b", Identity{Tenant: "tenant-b", Principal: "tenant-b/alice"}},
		"slashes":     {"slashes", Identity{Tenant: "t", Principal: "t/a/b"}},
	} {
		got, err := tokens.Authenticate(request("Bearer " + test.token))
		if err != nil || got != test.want {
			t.Errorf("%s: got %+v, %v; want %+v", name, got, err, test.want)
		}
	}
	a, _ := tokens.Authenticate(request("Bearer a"))
	b, _ := tokens.Authenticate(request("Bearer b"))
	if a.Principal == b.Principal {
		t.Fatalf("principals must differ across tenants: %q", a.Principal)
	}
	for name, header := range map[string]string{
		"empty tenant":   "Bearer notenant",
		"empty user":     "Bearer nouser",
		"missing header": "",
		"malformed":      "Basic a",
		"no scheme":      "a",
		"wrong token":    "Bearer nope",
	} {
		got, err := tokens.Authenticate(request(header))
		if !errors.Is(err, errUnauthorized) || got != (Identity{}) {
			t.Errorf("%s: got %+v, %v; want unauthorized", name, got, err)
		}
	}
}
