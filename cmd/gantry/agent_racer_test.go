// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

func TestRacerCacheSocketReadinessIsLocalAndFailsClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace socket via proc fd")
	}

	dir, err := os.MkdirTemp(".", ".racer-cache-readiness-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	folder, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer folder.Close()

	socket := fmt.Sprintf("/proc/self/fd/%d/cache", folder.Fd())
	if racerCacheSocketReady(t.Context(), socket) {
		t.Fatal("missing socket reported ready")
	}

	for _, tc := range []struct {
		name          string
		status        int
		allow, length string
		want          bool
	}{
		{name: "local parser with origins unavailable", status: 405, allow: "GET, HEAD", length: "0", want: true},
		{name: "missing allow", status: 405, length: "0"},
		{name: "wrong allow", status: 405, allow: "GET", length: "0"},
		{name: "unexpected body", status: 405, allow: "GET, HEAD", length: "1"},
		{name: "unavailable", status: 503, allow: "GET, HEAD", length: "0"},
		{name: "bad gateway", status: 502, allow: "GET, HEAD", length: "0"},
		{name: "not found", status: 404, allow: "GET, HEAD", length: "0"},
		{name: "unexpected success", status: 200, allow: "GET, HEAD", length: "0"},
		{name: "redirect", status: 307, allow: "GET, HEAD", length: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64

			server, _, err := startRacerOrigin(socket, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)

				if r.Method != http.MethodOptions || r.RequestURI != "/" {
					t.Errorf("probe entered content path: %s %q", r.Method, r.RequestURI)
					w.WriteHeader(http.StatusBadGateway)

					return
				}

				w.Header().Set("Allow", tc.allow)
				w.Header().Set("Content-Length", tc.length)
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(tc.status)
			}))
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()

			if got := racerCacheSocketReady(t.Context(), socket); got != tc.want {
				t.Fatalf("ready=%v, want %v", got, tc.want)
			}

			if requests.Load() != 1 {
				t.Fatalf("probe followed a redirect or retried: requests=%d", requests.Load())
			}
		})
	}

	// A bound socket without a serving worker is not sufficient. The caller's
	// deadline must bound a stalled parser, not merely its connect operation.
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if racerCacheSocketReady(ctx, socket) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("stalled listener did not fail at the probe deadline")
	}

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	if racerCacheSocketReady(t.Context(), socket) {
		t.Fatal("closed socket reported ready")
	}
}

func TestRacerOriginStartupReadinessAndCollision(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace socket via proc fd")
	}

	dir, err := os.MkdirTemp(".", ".racer-origin-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	folder, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer folder.Close()

	socket := fmt.Sprintf("/proc/self/fd/%d/origin", folder.Fd())

	target, err := racerReadinessTarget("node-a", os.Hostname)
	if err != nil {
		t.Fatal(err)
	}

	if racerSocketReady(t.Context(), socket, target) {
		t.Fatal("missing socket ready")
	}

	handler, err := sdk.NewRangeOrigin(&gantryracer.Origin{})
	if err != nil {
		t.Fatal(err)
	}

	server, _, err := startRacerOrigin(socket, handler)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	if !racerSocketReady(t.Context(), socket, target) {
		t.Fatal("running origin not ready")
	}

	if _, _, err := startRacerOrigin(socket, handler); err == nil {
		t.Fatal("replaced live origin")
	}

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	if racerSocketReady(t.Context(), socket, target) {
		t.Fatal("closed origin ready")
	}

	bad, _, err := startRacerOrigin(socket, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()

	if racerSocketReady(t.Context(), socket, target) {
		t.Fatal("unavailable cache ready")
	}
}

func TestRacerReadinessTargetIdentity(t *testing.T) {
	seen := make(map[string]string)

	for _, identity := range []string{"node-a", "node-b", "node/a?x=1&node=other#fragment +%\r\n", "node-雪"} {
		t.Run(identity, func(t *testing.T) {
			hostname := func() (string, error) {
				t.Error("configured node name must take precedence over hostname")
				return "", errors.New("unexpected hostname lookup")
			}

			target, err := racerReadinessTarget(identity, hostname)
			if err != nil {
				t.Fatal(err)
			}

			repeated, err := racerReadinessTarget(identity, hostname)
			if err != nil || repeated != target {
				t.Fatalf("unstable target: %q, %q, %v", target, repeated, err)
			}

			if previous, exists := seen[target]; exists {
				t.Fatalf("identities %q and %q share target %q", previous, identity, target)
			}

			seen[target] = identity

			parsed, err := url.ParseRequestURI(target)
			if err != nil {
				t.Fatal(err)
			}

			if parsed.Path != "/gantry-readiness" || parsed.Fragment != "" || len(parsed.Query()) != 1 || parsed.Query().Get("node") != identity {
				t.Fatalf("identity escaped incorrectly: %q", target)
			}

			if _, err := gantryracer.ParseTarget(target); err == nil {
				t.Fatalf("readiness target is a valid OCI target: %q", target)
			}
		})
	}
}

func TestRacerReadinessTargetHostnameFallback(t *testing.T) {
	for _, hostname := range []string{"gantry-pod-a", "gantry-pod-b"} {
		fallback, err := racerReadinessTarget("", func() (string, error) { return hostname, nil })
		if err != nil {
			t.Fatal(err)
		}

		explicit, err := racerReadinessTarget(hostname, os.Hostname)
		if err != nil || fallback != explicit {
			t.Fatalf("hostname fallback: %q, explicit: %q, error: %v", fallback, explicit, err)
		}
	}

	lookupErr := errors.New("hostname unavailable")
	if _, err := racerReadinessTarget("", func() (string, error) { return "", lookupErr }); !errors.Is(err, lookupErr) {
		t.Fatalf("hostname error not preserved: %v", err)
	}

	if _, err := racerReadinessTarget("", func() (string, error) { return "", nil }); err == nil {
		t.Fatal("empty identity must not create a fleet-wide readiness key")
	}
}

func TestRacerSocketReadinessExactStatusAndTarget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace socket via proc fd")
	}

	dir, err := os.MkdirTemp(".", ".racer-readiness-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	folder, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer folder.Close()

	target, err := racerReadinessTarget("node/a?x=1&node=other#fragment +%\r\n", os.Hostname)
	if err != nil {
		t.Fatal(err)
	}

	var status atomic.Int64

	socket := fmt.Sprintf("/proc/self/fd/%d/cache", folder.Fd())

	server, _, err := startRacerOrigin(socket, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.RequestURI != target {
			t.Errorf("unexpected probe: %s %q, want HEAD %q", r.Method, r.RequestURI, target)
		}

		w.WriteHeader(int(status.Load()))
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	for _, code := range []int{404, 503, 200, 403, 500, 404} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			status.Store(int64(code))

			if ready := racerSocketReady(t.Context(), socket, target); ready != (code == http.StatusNotFound) {
				t.Fatalf("status %d: ready=%v", code, ready)
			}
		})
	}
}

func TestRacerRejectsDirectCoordProtocol(t *testing.T) {
	// Racer rejects direct coordination by never starting a libp2p host. Pin
	// that startup contract, including ignoring unusable direct-mode settings.
	dir, err := os.MkdirTemp(".", ".racer-no-libp2p-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	identity := filepath.Join(dir, "identity")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tc := range []struct {
		name, identityPath, listen string
	}{
		{"unused valid settings", identity, "/ip4/127.0.0.1/tcp/0"},
		{"invalid identity path", dir, "/ip4/127.0.0.1/tcp/0"},
		{"invalid listen address", identity, "not-a-multiaddr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewDefault()
			cfg.ContentBackend = "racer"
			cfg.NodeName = "racer-test"
			cfg.Libp2pIdentityPath = tc.identityPath
			cfg.Libp2pListen = []string{tc.listen}
			// Stop at a real, deterministic startup boundary without requiring
			// a containerd daemon or creating Racer's production UDS paths.
			cfg.ContainerdSocket = ""

			err := runRacerAgent(t.Context(), cfg, nil, nil, nil, nil, &phase9Metrics{}, nil, logger)
			if err == nil || !strings.Contains(err.Error(), "containerd content store is unavailable") {
				t.Fatalf("startup did not reach the containerd check independently of libp2p: %v", err)
			}

			if _, err := os.Stat(identity); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Racer startup touched the libp2p identity: %v", err)
			}
		})
	}
}
