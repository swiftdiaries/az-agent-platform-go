package chat

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

var errUnauthorized = errors.New("unauthorized")

type Authenticator interface {
	Authenticate(*http.Request) (string, error)
}

// StaticBearerTokens is a deterministic adapter for tests and local journeys.
// Production OAuth verification is supplied through Authenticator.
type StaticBearerTokens map[string]string

func (tokens StaticBearerTokens) Authenticate(request *http.Request) (string, error) {
	value := request.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return "", errUnauthorized
	}
	presented := strings.TrimPrefix(value, "Bearer ")
	for token, principal := range tokens {
		if len(token) == len(presented) && subtle.ConstantTimeCompare([]byte(token), []byte(presented)) == 1 && principal != "" {
			return principal, nil
		}
	}
	return "", errUnauthorized
}
