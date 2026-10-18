package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	aguitypes "github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
	"github.com/swiftdiaries/az-agent-platform-go/internal/service"
)

type acceptanceReplica struct {
	app *service.Service
	url string
}

func startAcceptanceReplica(t *testing.T, pool *pgxpool.Pool, registry *definitions.Registry, model agentruntime.Model) acceptanceReplica {
	t.Helper()
	app, err := service.New(service.Dependencies{
		Pool: pool, Registry: registry, Authenticator: chat.StaticBearerTokens{"token": "alice"}, Model: model,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- app.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := app.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Error(err)
		}
		if err := <-serveErr; err != nil {
			t.Error(err)
		}
		app.Close()
	})
	return acceptanceReplica{app: app, url: "http://" + listener.Addr().String()}
}

func acceptancePost(t *testing.T, endpoint string, input aguitypes.RunAgentInput) *http.Response {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func acceptanceReconnect(t *testing.T, endpoint, thread, run string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, endpoint+"?threadId="+thread+"&runId="+run, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func acceptanceThread(t *testing.T, pool *pgxpool.Pool, external string) string {
	t.Helper()
	var thread string
	if err := pool.QueryRow(t.Context(), "SELECT id FROM chat_threads WHERE principal='alice' AND external_id=$1", external).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	return thread
}

// threeReplicas starts replicas A, B and C as separate in-process Service
// instances sharing one database, registry and model.
func threeReplicas(t *testing.T, pool *pgxpool.Pool, registry *definitions.Registry, model agentruntime.Model) (a, b, c acceptanceReplica) {
	t.Helper()
	a = startAcceptanceReplica(t, pool, registry, model)
	b = startAcceptanceReplica(t, pool, registry, model)
	c = startAcceptanceReplica(t, pool, registry, model)
	return a, b, c
}
