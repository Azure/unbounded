// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orca

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/orca/app"
	"github.com/Azure/unbounded/internal/orca/cachestore"
	"github.com/Azure/unbounded/internal/orca/chunk"
	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// TestResolveLogLevel_PrecedenceAndDefault covers the resolution
// order documented on resolveLogLevel: ORCA_LOG_LEVEL wins when
// set and non-empty (after trim), otherwise the YAML-configured
// value is used, otherwise the empty string defaults through
// config.ParseLogLevel to info.
func TestResolveLogLevel_PrecedenceAndDefault(t *testing.T) {
	tests := []struct {
		name      string
		yamlLevel string
		envLevel  string // "" -> simulate unset via Setenv with ""
		want      slog.Level
		wantErr   bool
	}{
		{"empty yaml, no env -> info", "", "", slog.LevelInfo, false},
		{"yaml info, no env", "info", "", slog.LevelInfo, false},
		{"yaml debug, no env", "debug", "", slog.LevelDebug, false},
		{"yaml info overridden by env debug", "info", "debug", slog.LevelDebug, false},
		{"yaml debug overridden by env warn", "debug", "warn", slog.LevelWarn, false},
		{"whitespace env falls back to yaml", "warn", "   ", slog.LevelWarn, false},
		{"invalid yaml fails", "trace", "", 0, true},
		{"invalid env fails even when yaml valid", "info", "trace", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCA_LOG_LEVEL", tt.envLevel)

			got, err := resolveLogLevel(tt.yamlLevel)
			if tt.wantErr {
				if err == nil {
					t.Errorf("resolveLogLevel(%q) = %v, want error", tt.yamlLevel, got)
				}

				return
			}

			if err != nil {
				t.Errorf("resolveLogLevel(%q) unexpected err: %v", tt.yamlLevel, err)
				return
			}

			if got != tt.want {
				t.Errorf("resolveLogLevel(yaml=%q, env=%q) = %v, want %v",
					tt.yamlLevel, tt.envLevel, got, tt.want)
			}
		})
	}
}

// TestServe_ListenerErrorReturnsNonNil locks the exit-code contract
// for a listener that dies on its own: the error App.Wait reports must
// reach serve's return value even when the subsequent Shutdown is
// clean. Without that, a crashed listener leaves the process exiting
// 0 and a Deployment records a successful exit.
func TestServe_ListenerErrorReturnsNonNil(t *testing.T) {
	t.Setenv("ORCA_LOG_LEVEL", "")

	cfgPath := writeServeTestConfig(t)

	acceptErr := errors.New("accept boom")

	withStartApp(t, func(ctx context.Context, cfg *config.Config, opts ...app.Option) (*app.App, error) {
		opts = append(opts,
			app.WithOrigin(stubOrigin{}),
			app.WithCacheStore(stubCacheStore{}),
			app.WithSkipCachestoreSelfTest(),
			app.WithEdgeListener(failingListener{err: acceptErr}),
		)

		return app.Start(ctx, cfg, opts...)
	})

	err := serve(context.Background(), cfgPath)
	if err == nil {
		t.Fatal("serve returned nil, want non-nil for a listener that failed to serve")
	}

	if !errors.Is(err, acceptErr) {
		t.Errorf("serve err = %v, want it to wrap %v", err, acceptErr)
	}

	if !strings.Contains(err.Error(), "edge listener") {
		t.Errorf("serve err = %v, want it to identify the edge listener", err)
	}
}

// TestServe_CleanCancellationReturnsNil locks the counterpart: a
// normal context cancellation (SIGTERM path) with a clean Shutdown
// still returns nil, so the process exits 0 as before.
func TestServe_CleanCancellationReturnsNil(t *testing.T) {
	t.Setenv("ORCA_LOG_LEVEL", "")

	cfgPath := writeServeTestConfig(t)

	started := make(chan struct{})

	withStartApp(t, func(ctx context.Context, cfg *config.Config, opts ...app.Option) (*app.App, error) {
		opts = append(opts,
			app.WithOrigin(stubOrigin{}),
			app.WithCacheStore(stubCacheStore{}),
			app.WithSkipCachestoreSelfTest(),
		)

		a, err := app.Start(ctx, cfg, opts...)
		if err != nil {
			return nil, err
		}

		close(started)

		return a, nil
	})

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-started
		cancel()
	}()

	if err := serve(parent, cfgPath); err != nil {
		t.Errorf("serve err = %v, want nil on clean cancellation", err)
	}
}

// withStartApp replaces the startApp seam for the duration of the
// test.
func withStartApp(t *testing.T, fn func(context.Context, *config.Config, ...app.Option) (*app.App, error)) {
	t.Helper()

	prev := startApp

	startApp = fn

	t.Cleanup(func() { startApp = prev })
}

// writeServeTestConfig writes the smallest valid Orca config to a
// temp file. The origin, cachestore, and listeners are replaced by
// the startApp seam, so the addresses inside point nowhere.
func writeServeTestConfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")

	body := `
server:
  listen: 127.0.0.1:0
  ops_listen: 127.0.0.1:0
origin:
  id: serve-test
  driver: awss3
  target_global: 1
  awss3:
    bucket: origin
cachestore:
  driver: s3
  s3:
    endpoint: http://127.0.0.1:1
    bucket: cache
cluster:
  service: orca-peers.test.svc.cluster.local
  self_pod_ip: 127.0.0.1
  internal_listen: 127.0.0.1:0
  target_replicas: 1
chunking:
  size: 1 MiB
logging:
  level: info
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// failingListener makes http.Server.Serve return err immediately,
// simulating a listener that died under the server.
type failingListener struct {
	err error
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (l failingListener) Close() error              { return nil }
func (l failingListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// stubOrigin satisfies origin.Origin without any network access.
type stubOrigin struct{}

func (stubOrigin) Head(context.Context, string, string) (origin.ObjectInfo, error) {
	return origin.ObjectInfo{}, origin.ErrNotFound
}

func (stubOrigin) GetRange(context.Context, string, string, string, int64, int64) (io.ReadCloser, error) {
	return nil, origin.ErrNotFound
}

// stubCacheStore satisfies cachestore.CacheStore without any network
// access; the self-test is skipped by the seam's options.
type stubCacheStore struct{}

func (stubCacheStore) GetChunk(context.Context, chunk.Key, int64, int64) (io.ReadCloser, error) {
	return nil, cachestore.ErrNotFound
}

func (stubCacheStore) PutChunk(context.Context, chunk.Key, int64, io.Reader) error { return nil }

func (stubCacheStore) Stat(context.Context, chunk.Key) (cachestore.Info, error) {
	return cachestore.Info{}, cachestore.ErrNotFound
}

func (stubCacheStore) Delete(context.Context, chunk.Key) error { return nil }
func (stubCacheStore) SelfTest(context.Context) error          { return nil }
