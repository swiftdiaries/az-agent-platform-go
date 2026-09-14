package chat

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testIssuer = "https://keycloak.example.test/realms/agents"

type testJWKS struct {
	mu    sync.Mutex
	keys  []map[string]string
	hits  int
	delay time.Duration
}

func (s *testJWKS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits++
	keys, delay := append([]map[string]string(nil), s.keys...), s.delay
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

func (s *testJWKS) set(keys ...map[string]string) {
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
}

func (s *testJWKS) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func newTestAuthenticator(t *testing.T, keys ...map[string]string) (*KeycloakAuthenticator, *testJWKS) {
	t.Helper()
	endpoint := &testJWKS{keys: keys}
	server := httptest.NewTLSServer(endpoint)
	t.Cleanup(server.Close)
	auth, err := newKeycloakAuthenticator(KeycloakConfig{
		Issuer:            testIssuer,
		JWKSURL:           server.URL,
		Audience:          "agent-platform",
		JWTSource:         JWTSourceAuthorizationBearer,
		JWTHeader:         "Authorization",
		UserClaimPath:     "/sub",
		TenantClaimPath:   "/tenant",
		AllowedAlgorithms: []string{"RS256"},
	}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return auth, endpoint
}

func newSigningKey(t *testing.T, kid string) (*rsa.PrivateKey, map[string]string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e := big.NewInt(int64(key.PublicKey.E)).Bytes()
	return key, map[string]string{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(e),
	}
}

func signedToken(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	if claims["iss"] == nil {
		claims["iss"] = testIssuer
	}
	if claims["aud"] == nil {
		claims["aud"] = "agent-platform"
	}
	if claims["exp"] == nil {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}
	if claims["iat"] == nil {
		claims["iat"] = time.Now().Add(-time.Minute).Unix()
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	encoded, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func signedTokenWithoutExpiry(t *testing.T, key *rsa.PrivateKey, kid string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": testIssuer, "aud": "agent-platform", "iat": time.Now().Add(-time.Minute).Unix(),
		"sub": "alice", "tenant": "tenant-a",
	})
	token.Header["kid"] = kid
	encoded, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func bearerRequest(token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "https://chat.example.test/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func TestKeycloakAuthenticatorValidatesAndCanonicalizesPrincipal(t *testing.T) {
	key, jwk := newSigningKey(t, "one")
	auth, _ := newTestAuthenticator(t, jwk)
	principal, err := auth.Authenticate(bearerRequest(signedToken(t, key, "one", jwt.MapClaims{"sub": "alice", "tenant": "tenant-a"})))
	if err != nil {
		t.Fatal(err)
	}
	if principal != "8:tenant-a5:alice" {
		t.Fatalf("principal = %q", principal)
	}
	other, err := auth.Authenticate(bearerRequest(signedToken(t, key, "one", jwt.MapClaims{"sub": "lice", "tenant": "tenant-aa"})))
	if err != nil || principal == other {
		t.Fatalf("tenant/user identity collision: principal=%q other=%q err=%v", principal, other, err)
	}
}

func TestKeycloakAuthenticatorRejectsInvalidTokensAndAmbiguousInput(t *testing.T) {
	key, jwk := newSigningKey(t, "one")
	auth, _ := newTestAuthenticator(t, jwk)
	validClaims := jwt.MapClaims{"sub": "alice", "tenant": "tenant-a"}
	for name, token := range map[string]string{
		"expired":   signedToken(t, key, "one", jwt.MapClaims{"sub": "alice", "tenant": "tenant-a", "exp": time.Now().Add(-time.Minute).Unix()}),
		"issuer":    signedToken(t, key, "one", jwt.MapClaims{"sub": "alice", "tenant": "tenant-a", "iss": "https://wrong.example"}),
		"audience":  signedToken(t, key, "one", jwt.MapClaims{"sub": "alice", "tenant": "tenant-a", "aud": "wrong"}),
		"no expiry": signedTokenWithoutExpiry(t, key, "one"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := auth.Authenticate(bearerRequest(token)); !errors.Is(err, errUnauthorized) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, validClaims)
	encoded, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(bearerRequest(encoded)); !errors.Is(err, errUnauthorized) {
		t.Fatalf("unsigned token error = %v", err)
	}
	request := bearerRequest(signedToken(t, key, "one", validClaims))
	request.Header.Add("Authorization", "Bearer another-token")
	if _, err := auth.Authenticate(request); !errors.Is(err, errUnauthorized) {
		t.Fatalf("duplicate source header error = %v", err)
	}
	duplicateClaims := signedToken(t, key, "one", validClaims)
	parts := strings.Split(duplicateClaims, ".")
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + testIssuer + `","aud":"agent-platform","exp":9999999999,"iat":1,"sub":"alice","sub":"mallory","tenant":"tenant-a"}`))
	unsignedData := parts[0] + "." + payload
	signature, err := jwt.SigningMethodRS256.Sign(unsignedData, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(bearerRequest(unsignedData + "." + base64.RawURLEncoding.EncodeToString(signature))); !errors.Is(err, errUnauthorized) {
		t.Fatalf("duplicate JSON claim error = %v", err)
	}
}

func TestKeycloakAuthenticatorRefreshesForRotationAndBoundsUnknownKids(t *testing.T) {
	first, firstJWK := newSigningKey(t, "first")
	second, secondJWK := newSigningKey(t, "second")
	auth, endpoint := newTestAuthenticator(t, firstJWK)
	if _, err := auth.Authenticate(bearerRequest(signedToken(t, first, "first", jwt.MapClaims{"sub": "alice", "tenant": "t"}))); err != nil {
		t.Fatal(err)
	}
	if got := endpoint.count(); got != 1 {
		t.Fatalf("initial fetches = %d", got)
	}
	endpoint.set(secondJWK)
	if _, err := auth.Authenticate(bearerRequest(signedToken(t, second, "second", jwt.MapClaims{"sub": "alice", "tenant": "t"}))); err != nil {
		t.Fatalf("rotation token: %v", err)
	}
	if got := endpoint.count(); got != 2 {
		t.Fatalf("rotation fetches = %d", got)
	}
	if _, err := auth.Authenticate(bearerRequest(signedToken(t, first, "first", jwt.MapClaims{"sub": "alice", "tenant": "t"}))); !errors.Is(err, errUnauthorized) {
		t.Fatalf("retired key error = %v", err)
	}
	if got := endpoint.count(); got != 2 {
		t.Fatalf("unknown-kid cooldown fetches = %d", got)
	}
}

func TestKeycloakAuthenticatorBoundsJWKSResponseAndTimeout(t *testing.T) {
	key, jwk := newSigningKey(t, "one")
	auth, endpoint := newTestAuthenticator(t, jwk)
	endpoint.set(map[string]string{"oversize": strings.Repeat("x", maxJWKSBytes+1)})
	if _, err := auth.Authenticate(bearerRequest(signedToken(t, key, "one", jwt.MapClaims{"sub": "alice", "tenant": "t"}))); !errors.Is(err, errUnauthorized) {
		t.Fatalf("oversized JWKS error = %v", err)
	}
	endpoint.set(jwk)
	endpoint.mu.Lock()
	endpoint.delay = time.Second
	endpoint.mu.Unlock()
	auth.fetchTimeout = 10 * time.Millisecond
	started := time.Now()
	if _, err := auth.Authenticate(bearerRequest(signedToken(t, key, "one", jwt.MapClaims{"sub": "alice", "tenant": "t"}))); !errors.Is(err, errUnauthorized) {
		t.Fatalf("timed out JWKS error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("JWKS fetch took %v", elapsed)
	}
}

func TestKeycloakAuthenticatorDoesNotFollowJWKSRedirects(t *testing.T) {
	key, _ := newSigningKey(t, "one")
	redirected := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected++
	}))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	auth, err := newKeycloakAuthenticator(KeycloakConfig{
		Issuer: testIssuer, JWKSURL: redirect.URL, Audience: "agent-platform",
		JWTSource: JWTSourceAuthorizationBearer, JWTHeader: "Authorization",
		UserClaimPath: "/sub", TenantClaimPath: "/tenant", AllowedAlgorithms: []string{"RS256"},
	}, redirect.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(bearerRequest(signedToken(t, key, "one", jwt.MapClaims{"sub": "alice", "tenant": "t"}))); !errors.Is(err, errUnauthorized) {
		t.Fatalf("redirect error = %v", err)
	}
	if redirected != 0 {
		t.Fatalf("followed JWKS redirect %d times", redirected)
	}
}

func TestNewKeycloakAuthenticatorRequiresExplicitIdentityConfiguration(t *testing.T) {
	if _, err := NewKeycloakAuthenticator(KeycloakConfig{}); err == nil {
		t.Fatal("accepted missing configuration")
	}
}
