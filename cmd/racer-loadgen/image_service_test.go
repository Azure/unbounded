// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func imageServiceAddress(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	address := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	return address
}

func startImageServiceTest(t *testing.T, c config) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- serveImages(ctx, c) }()

	t.Cleanup(cancel)

	return cancel, done
}

func awaitImageService(t *testing.T, done <-chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("image service did not exit")
		return nil
	}
}

func awaitImageHTTP(t *testing.T, target string, status int, contains string) []byte {
	t.Helper()

	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(target)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)

			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == status && regexp.MustCompile(contains).Match(body) {
				return body
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("%s did not return %d containing %q", target, status, contains)

	return nil
}

func TestImageCombinedService(t *testing.T) {
	for _, mode := range []string{"success", "payload redirect"} {
		t.Run(mode, func(t *testing.T) {
			standalone, c := imageTestRegistry(t)
			c.role, c.registryNamespace = "both", "fixture.test"
			c.listen, c.registryListen = imageServiceAddress(t), imageServiceAddress(t)
			c.concurrency, c.layerConcurrency = 1, 2

			target, err := url.Parse("http://" + c.registryListen)
			if err != nil {
				t.Fatal(err)
			}

			proxy := httputil.NewSingleHostReverseProxy(target)

			var (
				ready            atomic.Bool
				probes, payloads atomic.Int64
			)

			mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/" {
					probes.Add(1)
					w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")

					if !ready.Load() {
						w.WriteHeader(http.StatusServiceUnavailable)
					}

					return
				}

				if !ready.Load() || r.URL.Query().Get("ns") != c.registryNamespace || !strings.HasPrefix(r.URL.Path, "/v2/"+imageRepository+"/") {
					t.Error("payload bypassed readiness or namespace", r.URL)
				}

				payloads.Add(1)

				if mode == "payload redirect" {
					http.Redirect(w, r, target.String()+r.URL.Path, http.StatusTemporaryRedirect)
					return
				}

				proxy.ServeHTTP(w, r)
			}))
			defer mirror.Close()

			c.gantryEndpoint = mirror.URL

			cancel, done := startImageServiceTest(t, c)
			catalog := awaitImageHTTP(t, target.String()+"/loadgen/catalog", 200, "images")

			expected, err := json.Marshal(standalone.catalog)
			if err != nil || !bytes.Equal(bytes.TrimSpace(catalog), expected) {
				t.Fatalf("combined and standalone catalogs differ: %s, %v", catalog, err)
			}

			awaitImageHTTP(t, "http://"+c.listen+"/readyz", 503, "")
			awaitImageHTTP(t, "http://"+c.listen+"/healthz", 200, "")

			metrics := awaitImageHTTP(t, "http://"+c.listen+"/metrics", 200, `racer_loadgen_image_pulls_total\{result="success"\} 0`)
			if payloads.Load() != 0 || strings.Contains(string(metrics), "racer_loadgen_registry_requests_total{") {
				t.Fatal("payload started before Gantry readiness")
			}

			ready.Store(true)
			awaitImageHTTP(t, "http://"+c.listen+"/readyz", 200, "")

			result := "success"
			if mode != "success" {
				result = "error"
			}

			metrics = awaitImageHTTP(t, "http://"+c.listen+"/metrics", 200, `racer_loadgen_image_pulls_total\{result="`+result+`"\} [1-9]`)
			if payloads.Load() == 0 || probes.Load() == 0 {
				t.Fatal("missing Gantry readiness or payload requests")
			}

			if mode == "payload redirect" {
				if strings.Contains(string(metrics), "racer_loadgen_registry_requests_total{") || !strings.Contains(string(metrics), `racer_loadgen_image_pulls_total{result="success"} 0`) {
					t.Fatal("client followed a redirect directly to its registry")
				}
			} else if !strings.Contains(string(metrics), "racer_loadgen_registry_sent_bytes_total{") {
				t.Fatal("registry and client metrics were not exposed together")
			}

			cancel()

			if err := awaitImageService(t, done); err != nil {
				t.Fatal(err)
			}

			for _, address := range []string{c.listen, c.registryListen} {
				l, err := net.Listen("tcp", address)
				if err != nil {
					t.Fatal("listener leaked", err)
				}

				_ = l.Close()
			}
		})
	}
}

func TestImageCombinedCancellationDuringPreparation(t *testing.T) {
	c := imageTestConfig(t)
	c.role, c.registryNamespace = "both", "fixture.test"
	c.footprint, c.objectSize, c.layersPerImage = 80_000_000_000, 1_000_000_000, 80
	c.listen, c.registryListen = imageServiceAddress(t), imageServiceAddress(t)

	var requests atomic.Int64

	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1) }))
	defer mirror.Close()

	c.gantryEndpoint = mirror.URL
	cancel, done := startImageServiceTest(t, c)
	awaitImageHTTP(t, "http://"+c.registryListen+"/v2/", 503, "preparing dataset")
	awaitImageHTTP(t, "http://"+c.listen+"/healthz", 200, "")
	awaitImageHTTP(t, "http://"+c.listen+"/metrics", 200, "racer_loadgen_image_received_bytes_total 0")
	awaitImageHTTP(t, "http://"+c.listen+"/readyz", 503, "")
	cancel()

	if err := awaitImageService(t, done); err != nil || requests.Load() != 0 {
		t.Fatalf("preparation cancellation: %v, Gantry requests %d", err, requests.Load())
	}
}

func TestImageCombinedReadinessFailure(t *testing.T) {
	for _, mode := range []string{"status", "redirect", "missing header", "hung", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			requested := make(chan struct{}, 1)

			var payloads atomic.Int64

			mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v2/" {
					payloads.Add(1)
				}

				select {
				case requested <- struct{}{}:
				default:
				}

				switch mode {
				case "status":
					w.WriteHeader(503)
				case "redirect":
					http.Redirect(w, r, "/payload", http.StatusTemporaryRedirect)
				case "hung", "cancel":
					<-r.Context().Done()
				}
			}))
			defer mirror.Close()

			c := imageTestConfig(t)
			c.role, c.registryNamespace, c.gantryEndpoint = "both", "fixture.test", mirror.URL
			c.listen, c.registryListen = imageServiceAddress(t), imageServiceAddress(t)

			c.gantryReadyTimeout = 200 * time.Millisecond
			if mode == "cancel" {
				c.gantryReadyTimeout = time.Minute
			}

			cancel, done := startImageServiceTest(t, c)

			select {
			case <-requested:
			case <-time.After(5 * time.Second):
				t.Fatal("no readiness probe")
			}

			if mode == "cancel" {
				cancel()
			}

			err := awaitImageService(t, done)
			if (mode == "cancel" && err != nil) || (mode != "cancel" && !errors.Is(err, context.DeadlineExceeded)) || payloads.Load() != 0 {
				t.Fatalf("readiness failure: %v, payloads %d", err, payloads.Load())
			}
		})
	}
}
