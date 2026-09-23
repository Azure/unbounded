// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racer"
)

func racerUDS(t *testing.T, handler http.Handler) *sdk.Client {
	t.Helper()
	// Short workspace-local paths also work inside a deeply nested worktree.
	dir, err := os.MkdirTemp(".", ".racer-test-")
	if err != nil {
		t.Fatal(err)
	}
	// Linux sockaddr_un has a 107 byte pathname limit. Use a relative listener
	// and /proc/self/fd directory path to preserve workspace-local storage.
	folder, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	path := fmt.Sprintf("/proc/self/fd/%d/cache", folder.Fd())

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}

	go func() { _ = server.Serve(ln) }()

	client, err := sdk.NewClient(path, sdk.ClientOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { client.CloseIdleConnections(); _ = server.Close(); _ = folder.Close(); _ = os.RemoveAll(dir) })

	return client
}

func TestRacerRawMirrorSpliceAndQuarantine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux splice and proc fd paths")
	}

	data := bytes.Repeat([]byte("verified-splice!"), 160000)
	d := digestOf(data)

	var (
		corrupt       atomic.Bool
		cacheRequests atomic.Int64
	)

	cache := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cacheRequests.Add(1)

		if r.Header.Get("Authorization") != "Bearer delegated" {
			t.Error("lost cache authorization")
		}

		ref, err := gantryracer.ParseTarget(r.RequestURI)
		if err != nil || ref.Registry != "registry.example" || ref.Digest != d {
			t.Error("incorrect cache target", r.RequestURI)
		}

		w.Header().Set("ETag", `"`+d.Hex()+`"`)
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")

		payload := data
		if corrupt.Load() {
			payload = bytes.Repeat([]byte("!"), len(data))
		}

		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(payload))
	}))
	up := &authorizationCapturingOrigin{body: data, seen: make(chan string, 4)}
	cfg := config.NewDefault()
	cfg.ContentBackend = "racer"
	cfg.UpstreamRegistries = []config.UpstreamRegistry{{Name: "registry.example"}}

	type result struct {
		stats   sdk.TransferStats
		partial bool
		err     error
	}

	results := make(chan result, 8)

	var completed atomic.Int64

	server := mirror.New(cfg, fakes.NewCache(), up,
		mirror.WithRacer(&gantryracer.Backend{Client: cache}, func(s sdk.TransferStats, p bool, err error) { results <- result{s, p, err} }, nil),
		mirror.WithLiveStreamCompletedHook(func(_ digest.Digest) { completed.Add(1) }))

	m := httptest.NewServer(server.Handler())
	defer m.Close()

	get := func(rangeHeader string) (*http.Response, []byte, error) {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, m.URL+"/v2/repo/manifests/"+d.String(), nil)
		if err != nil {
			t.Fatal(err)
		}

		r.Header.Set("Authorization", "Bearer delegated")

		if rangeHeader != "" {
			r.Header.Set("Range", rangeHeader)
		}

		resp, err := m.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		return resp, body, err
	}

	resp, body, err := get("")
	if err != nil || !bytes.Equal(body, data) || resp.Header.Get("Content-Type") != "application/vnd.oci.image.index.v1+json" || !resp.Close {
		t.Fatal(resp.Status, len(body), err)
	}

	first := <-results
	if first.err != nil || first.stats.SpliceCalls == 0 || first.stats.SpliceBytes < 1<<20 || first.stats.TeeCalls == 0 || first.stats.TeeBytes != first.stats.SpliceBytes {
		t.Fatalf("not actual verified splice: %+v", first)
	}

	t.Logf("raw mirror verified splice: calls=%d bytes=%d tee_calls=%d tee_bytes=%d buffered_bytes=%d", first.stats.SpliceCalls, first.stats.SpliceBytes, first.stats.TeeCalls, first.stats.TeeBytes, first.stats.BufferedBytes)

	resp, body, err = get("bytes=123-456")
	if err != nil || resp.StatusCode != 206 || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 123-456/%d", len(data)) || !bytes.Equal(body, data[123:457]) {
		t.Fatal(resp.Status, len(body), err)
	}

	partial := <-results
	if !partial.partial || partial.stats.TeeCalls != 0 {
		t.Fatalf("partial misclassified: %+v", partial)
	}

	corrupt.Store(true)

	_, body, err = get("")
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(body) != len(data)-1 {
		t.Fatal("corrupt body completed", len(body), err)
	}

	bad := <-results
	if !errors.Is(bad.err, sdk.ErrDigestMismatch) {
		t.Fatal(bad.err)
	}

	before := cacheRequests.Load()

	resp, body, err = get("")
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, data) || cacheRequests.Load() != before {
		t.Fatal("quarantine did not use registry", err)
	}

	if auth := <-up.seen; auth != "Bearer delegated" {
		t.Fatal(auth)
	}
}

func TestRacerRegistryRangeOriginAndFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace UDS via proc fd")
	}

	data := bytes.Repeat([]byte("registry"), 16384)
	d := digestOf(data)

	for _, mode := range []string{"ok", "auth401", "auth403", "get-auth403", "get503", "unknown-size", "unsupported-range"} {
		t.Run(mode, func(t *testing.T) {
			var ordinary atomic.Int64

			up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer request" {
					t.Error("identity substitution")
				}

				if mode == "auth401" || mode == "auth403" || mode == "get-auth403" && r.Method == "GET" {
					w.Header().Set("WWW-Authenticate", `Bearer realm="https://registry/token"`)

					status := 401
					if mode == "auth403" || mode == "get-auth403" {
						status = 403
					}

					w.WriteHeader(status)

					return
				}

				if r.Method == "GET" && r.Header.Get("Range") == "" {
					ordinary.Add(1)
				}

				if mode == "get503" && r.Header.Get("Range") != "" {
					w.WriteHeader(503)
					return
				}

				if mode == "unsupported-range" && r.Header.Get("Range") != "" {
					w.WriteHeader(416)
					return
				}

				if mode == "unknown-size" && r.Method == "HEAD" {
					w.WriteHeader(200)
					return
				}

				w.Header().Set("Content-Type", "application/octet-stream")
				http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
			}))
			defer up.Close()

			previousTransport := http.DefaultTransport
			http.DefaultTransport = up.Client().Transport

			defer func() { http.DefaultTransport = previousTransport }()

			cfg := config.NewDefault()
			cfg.ContentBackend = "racer"
			cfg.UpstreamRegistries = []config.UpstreamRegistry{{Name: "registry.example", Endpoint: up.URL}}

			registry, err := origin.New(cfg)
			if err != nil {
				t.Fatal(err)
			}

			adapter := &gantryracer.Origin{Registry: registry, Registries: map[string]bool{"registry.example": true}}

			handler, err := sdk.NewRangeOrigin(adapter)
			if err != nil {
				t.Fatal(err)
			}

			client := racerUDS(t, handler)

			m := httptest.NewServer(mirror.New(cfg, fakes.NewCache(), registry, mirror.WithRacer(&gantryracer.Backend{Client: client}, nil, nil)).Handler())
			defer m.Close()

			r, err := http.NewRequestWithContext(t.Context(), "GET", m.URL+"/v2/repo/blobs/"+d.String(), nil)
			if err != nil {
				t.Fatal(err)
			}

			r.Header.Set("Authorization", "Bearer request")

			resp, err := m.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}

			if mode == "auth401" || mode == "auth403" || mode == "get-auth403" {
				want := 401
				if mode == "auth403" || mode == "get-auth403" {
					want = 403
				}

				if resp.StatusCode != want || resp.Header.Get("WWW-Authenticate") == "" || ordinary.Load() != 0 {
					t.Fatal(resp.Status)
				}
			} else if resp.StatusCode != 200 || !bytes.Equal(body, data) {
				t.Fatal(resp.Status, len(body))
			}

			if mode != "ok" && mode != "auth401" && mode != "auth403" && mode != "get-auth403" && ordinary.Load() != 1 {
				t.Fatal("missing ordinary fallback")
			}
		})
	}
}

func TestRacerRoutingAndFallbackIntegrity(t *testing.T) {
	data := []byte("known local or upstream object")
	d := digestOf(data)
	cfg := config.NewDefault()
	cfg.ContentBackend = "racer"
	cfg.UpstreamRegistries = []config.UpstreamRegistry{{Name: "registry.example"}}
	local := fakes.NewCache()
	local.Put(d, data)
	up := &authorizationCapturingOrigin{body: data, seen: make(chan string, 4)}

	m := httptest.NewServer(mirror.New(cfg, local, up).Handler())
	defer m.Close()

	for _, tc := range []struct {
		path, mirrored string
		status         int
	}{
		{"/v2/repo/blobs/" + d.String(), "", 200},
		{"/v2/repo/manifests/latest", "", 503},
		{"/v2/repo/blobs/" + d.String(), "1", 409},
	} {
		r, err := http.NewRequestWithContext(t.Context(), "GET", m.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}

		r.Header.Set("Gantry-Mirrored", tc.mirrored)

		resp, err := m.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}

		_, _ = io.Copy(io.Discard, resp.Body)

		_ = resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatal(resp.Status)
		}
	}

	if len(up.seen) != 0 {
		t.Fatal("local/tag/incompatible request contacted registry")
	}
	// The ordinary fallback also withholds its last byte on digest mismatch.
	corrupt := &authorizationCapturingOrigin{body: bytes.Repeat([]byte("x"), 16384), seen: make(chan string, 1)}

	bad := httptest.NewServer(mirror.New(cfg, fakes.NewCache(), corrupt).Handler())
	defer bad.Close()

	resp, err := bad.Client().Get(bad.URL + "/v2/repo/blobs/" + d.String())
	if err == nil {
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if readErr == nil || len(body) >= len(corrupt.body) {
			t.Fatal("corrupt fallback completed", len(body), readErr)
		}
	}
}

func TestRacerOutageAndEmptyObject(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("workspace UDS via proc fd")
	}

	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			data := []byte("outage fallback")
			if empty {
				data = nil
			}

			d := digestOf(data)
			client := racerUDS(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if !empty {
					w.WriteHeader(503)
					return
				}

				w.Header().Set("ETag", `"`+d.Hex()+`"`)
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(200)
			}))
			cfg := config.NewDefault()
			cfg.ContentBackend = "racer"
			cfg.UpstreamRegistries = []config.UpstreamRegistry{{Name: "registry.example"}}
			up := &authorizationCapturingOrigin{body: data, seen: make(chan string, 2)}

			var fallbacks atomic.Int64

			server := mirror.New(cfg, fakes.NewCache(), up, mirror.WithRacer(&gantryracer.Backend{Client: client}, nil, func() { fallbacks.Add(1) }))

			m := httptest.NewServer(server.Handler())
			defer m.Close()

			resp, err := m.Client().Get(m.URL + "/v2/repo/blobs/" + d.String())
			if err != nil {
				t.Fatal(err)
			}

			body, err := io.ReadAll(resp.Body)

			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, data) {
				t.Fatal(resp.Status, err)
			}

			if empty && fallbacks.Load() != 0 || !empty && fallbacks.Load() != 1 {
				t.Fatal("incorrect fallback", fallbacks.Load())
			}
		})
	}
}
