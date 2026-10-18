package chat

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

var errUnauthorized = errors.New("unauthorized")

// Identity is the authenticated caller. Principal is the stable, tenant-unique
// subject Chat stores and Agent fences on; Tenant selects the product version,
// endpoint and settings at run start.
type Identity struct {
	Tenant    string
	Principal string
}

type Authenticator interface {
	Authenticate(*http.Request) (Identity, error)
}

// StaticBearerTokens is a deterministic adapter for tests and local journeys.
// Production OAuth verification is supplied through Authenticator.
//
// Each value is either a legacy principal (no "/"), which authenticates with an
// empty Tenant, or the tenant form "<tenant>/<user>". The tenant is everything
// before the first "/"; the principal is the whole value unchanged, so it stays
// unique across tenants. A tenant-form value with an empty tenant or user never
// authenticates.
type StaticBearerTokens map[string]string

func (tokens StaticBearerTokens) Authenticate(request *http.Request) (Identity, error) {
	value := request.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return Identity{}, errUnauthorized
	}
	presented := strings.TrimPrefix(value, "Bearer ")
	for token, principal := range tokens {
		if len(token) == len(presented) && subtle.ConstantTimeCompare([]byte(token), []byte(presented)) == 1 && principal != "" {
			tenant, user, tenantForm := strings.Cut(principal, "/")
			if !tenantForm {
				return Identity{Principal: principal}, nil
			}
			if tenant == "" || user == "" {
				return Identity{}, errUnauthorized
			}
			return Identity{Tenant: tenant, Principal: principal}, nil
		}
	}
	return Identity{}, errUnauthorized
}
