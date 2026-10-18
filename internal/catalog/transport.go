package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/swiftdiaries/az-agent-platform-go/internal/credential"
)

type callIDKey struct{}

// unavailable classifies a transport or protocol failure as a dependency
// failure, keeping ErrAuthRequired's identity. The cause text can embed the URL
// (net/http errors do), so it is deliberately not included.
func unavailable(op string, err error) error {
	if errors.Is(err, ErrAuthRequired) {
		return fmt.Errorf("%s: %w", op, ErrAuthRequired)
	}
	return fmt.Errorf("%w: %s failed", ErrEndpointUnavailable, op)
}

// headerTransport applies the credential to every request, adds the call-ID
// header to requests whose context carries a call ID, injects W3C trace
// context only, and turns 401/403 into ErrAuthRequired.
type headerTransport struct {
	lifetime     context.Context
	base         http.RoundTripper
	credential   credential.Credential
	callIDHeader string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(t.lifetime, cancel)
	if t.lifetime.Err() != nil {
		cancel()
	}
	release := func() { stop(); cancel() }
	clone := req.Clone(ctx)
	clone.Header = req.Header.Clone()
	t.credential.Apply(clone)
	if t.callIDHeader != "" {
		if callID, _ := req.Context().Value(callIDKey{}).(string); callID != "" {
			clone.Header.Set(t.callIDHeader, callID)
		}
	}
	// Inject only W3C trace context; prompts, tool arguments, credentials and
	// business identifiers never enter propagation headers.
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(clone.Header))
	response, err := t.base.RoundTrip(clone)
	if err != nil {
		release()
	} else {
		response.Body = &sessionBody{ReadCloser: response.Body, release: release}
	}
	if err == nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
		response.Body.Close()
		return nil, ErrAuthRequired
	}
	return response, err
}

// SDK session cleanup/reconnect can outlive the request context. Keep HTTP bodies
// usable until Close, but cancel every session request when the owning run ends.
type sessionBody struct {
	io.ReadCloser
	release func()
}

func (b *sessionBody) Close() error {
	defer b.release()
	return b.ReadCloser.Close()
}
