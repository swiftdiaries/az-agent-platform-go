package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

func TestExporterPostsToConfiguredTracePath(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Path != "/prefix/v1/traces" {
			t.Fatalf("export path = %q", request.URL.Path)
		}
		if !strings.HasPrefix(request.Header.Get("Content-Type"), "application/x-protobuf") {
			t.Fatalf("export content type = %q", request.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(request.Body)
		if err != nil || len(body) == 0 {
			t.Fatalf("export body = %d bytes, %v", len(body), err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})
	provider, err := New(Config{Endpoint: server.URL + "/prefix/v1/traces", ShutdownTimeout: time.Second, ExportTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("telemetry-test").Start(context.Background(), "exported")
	span.End()
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("export calls = %d", calls.Load())
	}
}
