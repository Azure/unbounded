// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/pkg/racersdk"
)

type startupRacerClient struct{ closed atomic.Bool }

func (*startupRacerClient) Get(context.Context, racersdk.Request) (*racersdk.Value, error) {
	return nil, errors.New("unexpected content request")
}

func (c *startupRacerClient) Close() error { c.closed.Store(true); return nil }

func TestRacerStartupSelection(t *testing.T) {
	// Both modes read the same config. The Racer path reaches origin construction
	// despite missing legacy requirements; legacy mode rejects those requirements.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("containerd_socket: ''\ntransfer_listen: ''\nchair_listen: ''\nupstream_registries:\n- name: registry.example.com\n  endpoint: https://registry.example.com\n  credentials_path: "+filepath.Join(t.TempDir(), "missing")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	previousLogger := slog.Default()

	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	for _, mode := range []string{"true", "false"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GANTRY_RACER_ENABLED", mode)

			err := runAgent([]string{"--config", path})

			want := "config:"
			if mode == "true" {
				want = "racer origin client:"
			}

			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("got %v, want prefix %q", err, want)
			}
		})
	}
}

func racerTestConfig(t *testing.T) *config.Config {
	t.Helper()

	return &config.Config{
		RacerEnabled: true, MirrorListen: reserveLoopbackAddr(t), MetricsListen: reserveLoopbackAddr(t),
		LogLevel: "info", LogFormat: "json",
		UpstreamRegistries: []config.UpstreamRegistry{{Name: "registry.example.com", Endpoint: "https://registry.example.com"}},
	}
}

func TestRacerOriginFailurePropagates(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup", true: "serve"}[delayed], func(t *testing.T) {
			c := racerTestConfig(t)
			client := &startupRacerClient{}
			failure := errors.New("origin failed")

			err := serveRacerAgent(t.Context(), c, slog.New(slog.NewTextHandler(io.Discard, nil)), racerTestCache(t), racerAgentDeps{
				client: client,
				serveOrigin: func(ctx context.Context, _ racersdk.OriginConfig, callback racersdk.Origin) error {
					if callback == nil {
						return errors.New("missing callback")
					}

					if delayed {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(50 * time.Millisecond):
						}
					}

					return failure
				},
				probe: func(context.Context, string) error { return nil },
			})
			if !errors.Is(err, failure) {
				t.Fatalf("got %v, want original origin error", err)
			}

			if !client.closed.Load() {
				t.Fatal("client not closed")
			}
		})
	}
}

func racerTestCache(t *testing.T) racersdk.CacheName {
	t.Helper()

	cache, err := racersdk.ParseCacheName("gantry")
	if err != nil {
		t.Fatal(err)
	}

	return cache
}

func TestRacerReadinessAndCancellation(t *testing.T) {
	c := racerTestConfig(t)
	client := &startupRacerClient{}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var socketsAvailable atomic.Bool

	originStopped := make(chan struct{})
	done := make(chan error, 1)
	cache := racerTestCache(t)

	go func() {
		done <- serveRacerAgent(ctx, c, slog.New(slog.NewTextHandler(io.Discard, nil)), cache, racerAgentDeps{
			client: client,
			serveOrigin: func(ctx context.Context, _ racersdk.OriginConfig, _ racersdk.Origin) error {
				defer close(originStopped)

				<-ctx.Done()

				return ctx.Err()
			},
			probe: func(context.Context, string) error {
				if !socketsAvailable.Load() {
					return errors.New("unavailable")
				}

				return nil
			},
		})
	}()

	waitRacerStatus(t, "http://"+c.MetricsListen+"/readyz", http.StatusServiceUnavailable)
	waitRacerStatus(t, "http://"+c.MirrorListen+"/v2/", http.StatusServiceUnavailable)
	socketsAvailable.Store(true)
	waitRacerStatus(t, "http://"+c.MetricsListen+"/readyz", http.StatusOK)
	waitRacerStatus(t, "http://"+c.MirrorListen+"/v2/", http.StatusOK)

	response, err := http.Get("http://" + c.MetricsListen + "/metrics")
	if err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(response.Body)
	response.Body.Close()

	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(body), "go_goroutines") || strings.Contains(string(body), "p2p_") || strings.Contains(string(body), "gantry_containerd_") {
		t.Fatalf("expected runtime metrics without legacy instruments, got %s", body)
	}

	socketsAvailable.Store(false)
	waitRacerStatus(t, "http://"+c.MetricsListen+"/readyz", http.StatusServiceUnavailable)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not stop")
	}

	if !client.closed.Load() {
		t.Fatal("client not closed")
	}

	select {
	case <-originStopped:
	default:
		t.Fatal("origin not stopped")
	}
}

func TestRacerHTTPListenFailureCleansUp(t *testing.T) {
	for _, endpoint := range []string{"mirror", "ops"} {
		t.Run(endpoint, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()

			c := racerTestConfig(t)
			if endpoint == "mirror" {
				c.MirrorListen = listener.Addr().String()
			} else {
				c.MetricsListen = listener.Addr().String()
			}

			client := &startupRacerClient{}

			var originStopped atomic.Bool

			err = serveRacerAgent(t.Context(), c, slog.New(slog.NewTextHandler(io.Discard, nil)), racerTestCache(t), racerAgentDeps{
				client: client,
				serveOrigin: func(ctx context.Context, _ racersdk.OriginConfig, _ racersdk.Origin) error {
					<-ctx.Done()
					originStopped.Store(true)

					return ctx.Err()
				},
				probe: func(context.Context, string) error { return nil },
			})
			if err == nil || !strings.Contains(err.Error(), "racer "+endpoint) {
				t.Fatalf("expected %s error, got %v", endpoint, err)
			}

			if !originStopped.Load() || !client.closed.Load() {
				t.Fatal("origin and client must stop on HTTP startup failure")
			}
		})
	}
}

func waitRacerStatus(t *testing.T, url string, status int) {
	t.Helper()

	client := &http.Client{Timeout: 100 * time.Millisecond}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			response.Body.Close()

			if response.StatusCode == status {
				return
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("%s did not return %d", url, status)
}

func TestRacerSocketAvailability(t *testing.T) {
	var paths []string

	if !racerSocketsAvailable(t.Context(), func(ctx context.Context, path string) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("probe must have a deadline")
		}

		paths = append(paths, path)

		return nil
	}) {
		t.Fatal("successful probes should be available")
	}

	if strings.Join(paths, ",") != "/run/racer/gantry/origin/socket,/run/racer/gantry/client/socket" {
		t.Fatalf("wrong sockets: %v", paths)
	}

	if racerSocketsAvailable(t.Context(), func(context.Context, string) error { return errors.New("unavailable") }) {
		t.Fatal("failed probe must not be ready")
	}
}
