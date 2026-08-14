// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// stubSnapshots answers Stat the way the real service does, and can be made to
// hang the way a wedged daemon does.
type stubSnapshots struct {
	snapshotsapi.UnimplementedSnapshotsServer

	block chan struct{}
	seen  chan string
}

func (s *stubSnapshots) Stat(
	ctx context.Context,
	req *snapshotsapi.StatSnapshotRequest,
) (*snapshotsapi.StatSnapshotResponse, error) {
	if s.seen != nil {
		ns, _ := namespaces.Namespace(ctx)

		select {
		case s.seen <- ns:
		default:
		}
	}

	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return nil, status.Errorf(codes.NotFound, "snapshot %q does not exist", req.Key)
}

// serveStub runs the stub on a unix socket and returns its path.
func serveStub(t *testing.T, stub *stubSnapshots) string {
	t.Helper()

	socket := filepath.Join(t.TempDir(), "s.sock")

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	server := grpc.NewServer()
	snapshotsapi.RegisterSnapshotsServer(server, stub)

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = server.Serve(listener) //nolint:errcheck // the test stops it
	}()

	t.Cleanup(func() {
		server.Stop()
		<-done
	})

	return socket
}

func TestProbeAnswersOverTheSocket(t *testing.T) {
	t.Parallel()

	stub := &stubSnapshots{seen: make(chan string, 1)}
	socket := serveStub(t, stub)

	p := newProber(&Config{Socket: socket, ContainerdNamespace: "k8s.io"})

	t.Cleanup(func() { _ = p.close() }) //nolint:errcheck // test cleanup

	if err := p.check(t.Context()); err != nil {
		t.Fatalf("check: %v", err)
	}

	// The namespace matters: without it the real service cannot open the
	// metadata store, so a probe that forgets it would never reach bbolt.
	select {
	case ns := <-stub.seen:
		if ns != "k8s.io" {
			t.Errorf("namespace = %q, want k8s.io", ns)
		}
	default:
		t.Error("the probe never reached the server")
	}

	// The connection is reused, so a second probe costs no dial.
	if err := p.check(t.Context()); err != nil {
		t.Fatalf("second check: %v", err)
	}
}

func TestProbeFailsWhenTheDaemonIsWedged(t *testing.T) {
	t.Parallel()

	stub := &stubSnapshots{block: make(chan struct{})}
	socket := serveStub(t, stub)

	p := newProber(&Config{Socket: socket, ContainerdNamespace: "k8s.io"})

	t.Cleanup(func() { _ = p.close() }) //nolint:errcheck // test cleanup

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	if err := p.check(ctx); err == nil {
		t.Fatal("a probe against a stuck handler reported the daemon healthy")
	}

	close(stub.block)
}

func TestProbeFailsWhenNothingIsListening(t *testing.T) {
	t.Parallel()

	p := newProber(&Config{
		Socket:              filepath.Join(t.TempDir(), "absent.sock"),
		ContainerdNamespace: "k8s.io",
	})
	t.Cleanup(func() { _ = p.close() }) //nolint:errcheck // test cleanup

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	if err := p.check(ctx); err == nil {
		t.Fatal("a probe against an unbound socket reported the daemon healthy")
	}
}

func TestHealthHandlerReportsTheFailure(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fail := func(context.Context) error { return context.DeadlineExceeded }

	rec := httptest.NewRecorder()
	healthHandler(fail, log)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	if body := rec.Body.String(); !strings.Contains(body, "deadline exceeded") {
		t.Errorf("body = %q, want the cause", body)
	}

	if out := buf.String(); !strings.Contains(out, "liveness check failed") {
		t.Errorf("log = %q, want a warning", out)
	}
}

func TestHealthHandlerReportsHealthy(t *testing.T) {
	t.Parallel()

	ok := func(context.Context) error { return nil }

	rec := httptest.NewRecorder()
	healthHandler(ok, slog.New(slog.DiscardHandler))(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if body := rec.Body.String(); body != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
}

func TestHealthHandlerWithoutACheck(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	healthHandler(nil, slog.New(slog.DiscardHandler))(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
