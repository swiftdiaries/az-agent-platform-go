package chat

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	maxJWTBytes            = 16 << 10
	maxJWKSBytes           = 1 << 20
	maxJWKSKeys            = 128
	jwksFetchTimeout       = 5 * time.Second
	jwksCacheLifetime      = 5 * time.Minute
	jwksRefreshMinInterval = 30 * time.Second
)

type JWTSource string

const (
	JWTSourceAuthorizationBearer JWTSource = "authorization_bearer"
	JWTSourceHeader              JWTSource = "header"
)

// KeycloakConfig is the explicit, nonsecret Keycloak identity mapping.
// No issuer, audience, claim name, token source, or signature method is inferred.
type KeycloakConfig struct {
	Issuer            string
	JWKSURL           string
	Audience          string
	JWTSource         JWTSource
	JWTHeader         string
	UserClaimPath     string
	TenantClaimPath   string
	AllowedAlgorithms []string
}

// KeycloakAuthenticator verifies signed Keycloak access tokens at Chat ingress.
type KeycloakAuthenticator struct {
	issuer, audience, header string
	source                   JWTSource
	userPath, tenantPath     []string
	algorithms               []string
	allowedAlgorithms        map[string]struct{}
	jwksURL                  string
	client                   *http.Client
	parser                   *jwt.Parser
	fetchTimeout             time.Duration

	mu          sync.Mutex
	keys        map[string]rsaJWK
	cacheExpiry time.Time
	nextRefresh time.Time
	fetchGate   chan struct{}
}

type rsaJWK struct {
	algorithm string
	key       *rsa.PublicKey
}

// NewKeycloakAuthenticator creates the production JWT authenticator. It makes
// no network request until the first authenticated request arrives.
func NewKeycloakAuthenticator(config KeycloakConfig) (*KeycloakAuthenticator, error) {
	return newKeycloakAuthenticator(config, &http.Client{
		Timeout: jwksFetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	})
}

func newKeycloakAuthenticator(config KeycloakConfig, client *http.Client) (*KeycloakAuthenticator, error) {
	if err := validateKeycloakConfig(config); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("Keycloak HTTP client is required")
	}
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	userPath, _ := parseJSONPointer(config.UserClaimPath)
	tenantPath, _ := parseJSONPointer(config.TenantClaimPath)
	algorithms := make([]string, 0, len(config.AllowedAlgorithms))
	allowed := make(map[string]struct{}, len(config.AllowedAlgorithms))
	for _, algorithm := range config.AllowedAlgorithms {
		if _, seen := allowed[algorithm]; !seen {
			allowed[algorithm] = struct{}{}
			algorithms = append(algorithms, algorithm)
		}
	}
	return &KeycloakAuthenticator{
		issuer: config.Issuer, audience: config.Audience, source: config.JWTSource,
		header: config.JWTHeader, userPath: userPath, tenantPath: tenantPath,
		algorithms: algorithms, allowedAlgorithms: allowed, jwksURL: config.JWKSURL,
		client: &safeClient, fetchTimeout: jwksFetchTimeout, keys: make(map[string]rsaJWK), fetchGate: make(chan struct{}, 1),
		parser: jwt.NewParser(jwt.WithValidMethods(algorithms), jwt.WithIssuer(config.Issuer),
			jwt.WithAudience(config.Audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithStrictDecoding()),
	}, nil
}

func validateKeycloakConfig(config KeycloakConfig) error {
	for field, value := range map[string]string{
		"issuer": config.Issuer, "JWKS URL": config.JWKSURL, "audience": config.Audience,
		"JWT header": config.JWTHeader, "user claim path": config.UserClaimPath, "tenant claim path": config.TenantClaimPath,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("Keycloak %s is required", field)
		}
	}
	if err := validateHTTPSURL(config.Issuer); err != nil {
		return fmt.Errorf("Keycloak issuer: %w", err)
	}
	if err := validateHTTPSURL(config.JWKSURL); err != nil {
		return fmt.Errorf("Keycloak JWKS URL: %w", err)
	}
	if !validHeaderName(config.JWTHeader) {
		return errors.New("Keycloak JWT header is invalid")
	}
	if config.JWTSource != JWTSourceAuthorizationBearer && config.JWTSource != JWTSourceHeader {
		return errors.New("Keycloak JWT source is invalid")
	}
	if _, err := parseJSONPointer(config.UserClaimPath); err != nil {
		return fmt.Errorf("Keycloak user claim path: %w", err)
	}
	if _, err := parseJSONPointer(config.TenantClaimPath); err != nil {
		return fmt.Errorf("Keycloak tenant claim path: %w", err)
	}
	if len(config.AllowedAlgorithms) == 0 {
		return errors.New("Keycloak allowed algorithms are required")
	}
	for _, algorithm := range config.AllowedAlgorithms {
		if algorithm != "RS256" && algorithm != "RS384" && algorithm != "RS512" {
			return fmt.Errorf("Keycloak signing algorithm %q is unsupported", algorithm)
		}
	}
	return nil
}

func validateHTTPSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("must be an absolute HTTPS URL without credentials or fragment")
	}
	return nil
}

func validHeaderName(header string) bool {
	for _, r := range header {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-') {
			return false
		}
	}
	return true
}

func (a *KeycloakAuthenticator) Authenticate(request *http.Request) (string, error) {
	tokenString, err := a.token(request)
	if err != nil || len(tokenString) > maxJWTBytes || rejectDuplicateJWTJSON(tokenString) != nil {
		return "", errUnauthorized
	}
	claims := jwt.MapClaims{}
	_, err = a.parser.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("JWT key ID is required")
		}
		key, keyErr := a.key(request.Context(), kid, token.Method.Alg())
		if keyErr != nil {
			return nil, keyErr
		}
		return key, nil
	})
	if err != nil {
		return "", errUnauthorized
	}
	user, ok := stringClaim(claims, a.userPath)
	if !ok {
		return "", errUnauthorized
	}
	tenant, ok := stringClaim(claims, a.tenantPath)
	if !ok {
		return "", errUnauthorized
	}
	return canonicalPrincipal(tenant, user), nil
}

func (a *KeycloakAuthenticator) token(request *http.Request) (string, error) {
	if request == nil {
		return "", errUnauthorized
	}
	values := request.Header.Values(a.header)
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "", errUnauthorized
	}
	value := values[0]
	if a.source == JWTSourceAuthorizationBearer {
		if !strings.HasPrefix(value, "Bearer ") || strings.Count(value, " ") != 1 {
			return "", errUnauthorized
		}
		value = strings.TrimPrefix(value, "Bearer ")
	}
	if value == "" {
		return "", errUnauthorized
	}
	return value, nil
}

func (a *KeycloakAuthenticator) key(ctx context.Context, kid, algorithm string) (*rsa.PublicKey, error) {
	if _, ok := a.allowedAlgorithms[algorithm]; !ok {
		return nil, errors.New("JWT signing algorithm is not allowed")
	}
	now := time.Now()
	a.mu.Lock()
	key, found := a.keys[kid]
	fresh := now.Before(a.cacheExpiry)
	cooldown := now.Before(a.nextRefresh)
	a.mu.Unlock()
	if fresh && found && key.algorithm == algorithm {
		return key.key, nil
	}
	if cooldown {
		return nil, errors.New("JWT key ID is unknown")
	}
	select {
	case a.fetchGate <- struct{}{}:
		defer func() { <-a.fetchGate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	now = time.Now()
	a.mu.Lock()
	key, found = a.keys[kid]
	fresh = now.Before(a.cacheExpiry)
	if fresh && found && key.algorithm == algorithm {
		a.mu.Unlock()
		return key.key, nil
	}
	if now.Before(a.nextRefresh) {
		a.mu.Unlock()
		return nil, errors.New("JWT key ID is unknown")
	}
	keepCooldown := fresh
	a.nextRefresh = now.Add(jwksRefreshMinInterval)
	a.mu.Unlock()
	keys, err := a.fetchKeys(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.keys = keys
	a.cacheExpiry = time.Now().Add(jwksCacheLifetime)
	key, found = a.keys[kid]
	if found && key.algorithm == algorithm && !keepCooldown {
		a.nextRefresh = time.Time{}
	}
	a.mu.Unlock()
	if !found || key.algorithm != algorithm {
		return nil, errors.New("JWT key ID is unknown")
	}
	return key.key, nil
}

func (a *KeycloakAuthenticator) fetchKeys(ctx context.Context) (map[string]rsaJWK, error) {
	ctx, cancel := context.WithTimeout(ctx, a.fetchTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("JWKS request failed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxJWKSBytes+1))
	if err != nil || len(body) > maxJWKSBytes {
		return nil, errors.New("JWKS response is too large")
	}
	var document struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &document); err != nil || len(document.Keys) == 0 || len(document.Keys) > maxJWKSKeys {
		return nil, errors.New("JWKS response is invalid")
	}
	keys := make(map[string]rsaJWK, len(document.Keys))
	for _, raw := range document.Keys {
		if raw.Kty != "RSA" || raw.Kid == "" || raw.Alg == "" || (raw.Use != "" && raw.Use != "sig") {
			continue
		}
		if _, allowed := a.allowedAlgorithms[raw.Alg]; !allowed {
			continue
		}
		if _, duplicate := keys[raw.Kid]; duplicate {
			return nil, errors.New("JWKS has duplicate key IDs")
		}
		n, err := base64.RawURLEncoding.DecodeString(raw.N)
		if err != nil {
			return nil, errors.New("JWKS modulus is invalid")
		}
		e, err := base64.RawURLEncoding.DecodeString(raw.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("JWKS exponent is invalid")
		}
		exponent := 0
		for _, b := range e {
			exponent = exponent<<8 | int(b)
		}
		if exponent < 3 || exponent%2 == 0 || len(n) == 0 {
			return nil, errors.New("JWKS RSA key is invalid")
		}
		keys[raw.Kid] = rsaJWK{algorithm: raw.Alg, key: &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exponent}}
	}
	if len(keys) == 0 {
		return nil, errors.New("JWKS has no usable signing keys")
	}
	return keys, nil
}

func parseJSONPointer(path string) ([]string, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("must be an RFC 6901 JSON Pointer")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for index, part := range parts {
		var decoded strings.Builder
		for offset := 0; offset < len(part); offset++ {
			if part[offset] != '~' {
				decoded.WriteByte(part[offset])
				continue
			}
			if offset+1 == len(part) || (part[offset+1] != '0' && part[offset+1] != '1') {
				return nil, errors.New("has an invalid escape")
			}
			if part[offset+1] == '0' {
				decoded.WriteByte('~')
			} else {
				decoded.WriteByte('/')
			}
			offset++
		}
		parts[index] = decoded.String()
	}
	return parts, nil
}

func stringClaim(claims jwt.MapClaims, path []string) (string, bool) {
	var value any = map[string]any(claims)
	for _, component := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return "", false
		}
		value, ok = object[component]
		if !ok {
			return "", false
		}
	}
	stringValue, ok := value.(string)
	return stringValue, ok && stringValue != ""
}

func canonicalPrincipal(tenant, user string) string {
	return fmt.Sprintf("%d:%s%d:%s", len(tenant), tenant, len(user), user)
}

func rejectDuplicateJWTJSON(token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("JWT has invalid segments")
	}
	for _, part := range parts[:2] {
		decoded, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || rejectDuplicateJSON(decoded) != nil {
			return errors.New("JWT JSON is invalid")
		}
	}
	return nil
}

func rejectDuplicateJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("JSON has trailing data")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("JSON object key is invalid")
			}
			if _, duplicate := seen[name]; duplicate {
				return errors.New("JSON has duplicate object key")
			}
			seen[name] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("JSON delimiter is invalid")
	}
}
