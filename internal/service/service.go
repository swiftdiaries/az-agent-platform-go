// Package service owns the process-level HTTP and drain boundary.
package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/chat"
	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	platformmcp "github.com/swiftdiaries/az-agent-platform-go/internal/mcp"
	"github.com/swiftdiaries/az-agent-platform-go/internal/platform"
	agentruntime "github.com/swiftdiaries/az-agent-platform-go/internal/runtime"
)

type Dependencies struct {
	Pool          *pgxpool.Pool
	Registry      *definitions.Registry
	Authenticator chat.Authenticator
	Model         agentruntime.Model
}

type Service struct {
	platform *platform.Platform
	handler  http.Handler

	mu     sync.Mutex
	server *http.Server
}

func New(deps Dependencies) (*Service, error) {
	if deps.Pool == nil || deps.Registry == nil || deps.Authenticator == nil || deps.Model == nil {
		return nil, errors.New("service dependencies are required")
	}
	runner := agentruntime.NewRunner(deps.Registry, platformmcp.NewClient(), deps.Model)
	service := &Service{platform: platform.New(runner, journal.New(deps.Pool))}
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ready, err := service.platform.Ready(r.Context())
		if err != nil || !ready {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", chat.NewHandler(deps.Authenticator, service.platform, deps.Pool))
	service.handler = mux
	return service, nil
}

func (s *Service) Handler() http.Handler { return s.handler }

func (s *Service) BeginDrain() { s.platform.BeginDrain() }

func (s *Service) Drain(ctx context.Context) error { return s.platform.Drain(ctx) }

// Serve owns a real HTTP server for the command entrypoint. Shutdown is
// bounded by its context; expiry closes long-lived observe streams.
func (s *Service) Serve(listener net.Listener) error {
	s.mu.Lock()
	if s.server != nil {
		s.mu.Unlock()
		return errors.New("service already serving")
	}
	server := &http.Server{Handler: s.handler}
	s.server = server
	s.mu.Unlock()
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.BeginDrain()
	s.mu.Lock()
	server := s.server
	s.mu.Unlock()
	serverDone := make(chan error, 1)
	if server != nil {
		go func() { serverDone <- server.Shutdown(ctx) }()
	} else {
		serverDone <- nil
	}
	drainErr := s.Drain(ctx)
	if ctx.Err() != nil && server != nil {
		_ = server.Close()
	}
	serverErr := <-serverDone
	if drainErr != nil {
		return drainErr
	}
	return serverErr
}

func (s *Service) Close() { s.platform.Close() }
