// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func imageTestConfig(t *testing.T) config {
	t.Helper()

	c, err := parseConfig([]string{"-mode=container-image", "-role=registry", "-footprint=4096", "-object-size=1024", "-layers-per-image=2", "-seed=42"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func imageTestRegistry(t *testing.T) (*imageRegistry, config) {
	t.Helper()
	c := imageTestConfig(t)

	s := newImageRegistry(newImageMetrics(prometheus.NewRegistry()))
	if err := s.prepare(t.Context(), c); err != nil {
		t.Fatal(err)
	}

	return s, c
}

func TestImageConfig(t *testing.T) {
	for _, args := range [][]string{
		{"-mode=unknown"},
		{"-role=load"},
		{"-mode=container-image"},
		{"-mode=container-image", "-role=both"},
		{"-mode=container-image", "-role=unknown"},
		{"-mode=container-image", "-role=both", "-registry-namespace=fixture.test", "-layers-per-image=3"},
		{"-mode=container-image", "-role=both", "-registry-namespace=fixture.test", "-registry-url=http://registry"},
		{"-mode=container-image", "-role=both", "-registry-namespace=fixture.test", "-gantry-ready-timeout=0"},
		{"-mode=container-image", "-role=both", "-registry-namespace=fixture.test", "-gantry-endpoint=unix:///client"},
		{"-mode=container-image", "-role=registry", "-layers-per-image=0"},
		{"-mode=container-image", "-role=registry", "-layers-per-image=1025"},
		{"-mode=container-image", "-role=registry", "-layer-concurrency=0"},
		{"-mode=container-image", "-role=registry", "-layers-per-image=3"},
		{"-mode=container-image", "-role=registry", "-footprint=100001", "-object-size=1", "-layers-per-image=1"},
		{"-mode=container-image", "-role=load"},
	} {
		if _, err := parseConfig(args, io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}

	for _, field := range []string{"registry-url", "gantry-endpoint"} {
		for _, value := range []string{"", "unix:///client", "http://", "http://user:pass@host", "http://host/path", "http://host/?ns=x", "http://host/#x"} {
			args := []string{"-mode=container-image", "-role=load", "-registry-url=http://registry:8081", "-registry-namespace=fixture.test", "-" + field + "=" + value}
			if _, err := parseConfig(args, io.Discard); err == nil {
				t.Errorf("accepted %v", args)
			}
		}
	}

	for _, ns := range []string{"", "http://registry", "registry/repo", "registry?ns=x"} {
		if _, err := parseConfig([]string{"-mode=container-image", "-role=load", "-registry-url=http://registry", "-registry-namespace=" + ns}, io.Discard); err == nil {
			t.Errorf("accepted namespace %q", ns)
		}
	}

	if _, err := parseConfig([]string{"-mode=container-image", "-role=load", "-registry-url=http://registry:8081", "-registry-namespace=registry:8081"}, io.Discard); err != nil {
		t.Fatal(err)
	}

	c, err := parseConfig([]string{"-mode=container-image", "-role=both", "-registry-namespace=fixture.test", "-footprint=80GB", "-object-size=1GB", "-layers-per-image=80", "-concurrency=8", "-layer-concurrency=8", "-timeout=30m"}, io.Discard)
	if err != nil || c.footprint != 80_000_000_000 || c.layersPerImage != 80 || c.registryURL != "" {
		t.Fatalf("combined config: %+v, %v", c, err)
	}
}

func TestImageRegistryPreparation(t *testing.T) {
	s := newImageRegistry(newImageMetrics(prometheus.NewRegistry()))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/loadgen/catalog", nil))

	if w.Code != 503 {
		t.Fatal(w.Code)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := s.prepare(ctx, imageTestConfig(t)); err != context.Canceled || s.ready.Load() {
		t.Fatal(err)
	}

	a, c := imageTestRegistry(t)
	b := newImageRegistry(newImageMetrics(prometheus.NewRegistry()))

	c.seed++
	if err := b.prepare(t.Context(), c); err != nil {
		t.Fatal(err)
	}

	x, _ := json.Marshal(a.catalog)

	y, _ := json.Marshal(b.catalog)
	if !bytes.Equal(x, y) || len(a.catalog.Images) != 2 {
		t.Fatal("catalog identity depends on sampling seed")
	}

	for key, object := range a.objects {
		data, err := io.ReadAll(io.NewSectionReader(object.source, 0, object.descriptor.Size))
		if err != nil || digest.FromBytes(data) != object.descriptor.Digest {
			t.Fatalf("bad digest %s: %v", key, err)
		}
	}
}

func TestImageRegistryProtocol(t *testing.T) {
	s, _ := imageTestRegistry(t)

	var (
		object imageObject
		path   string
	)

	for key, item := range s.objects {
		if item.descriptor.MediaType == oci.MediaTypeImageLayer {
			object, path = item, "/v2/"+imageRepository+"/"+key
			break
		}
	}

	full, _ := io.ReadAll(io.NewSectionReader(object.source, 0, object.descriptor.Size))
	for _, tc := range []struct {
		method, rangeValue string
		status             int
		body               []byte
		contentRange       string
	}{
		{"GET", "", 200, full, ""},
		{"HEAD", "", 200, nil, ""},
		{"GET", "bytes=7-35", 206, full[7:36], "bytes 7-35/1024"},
		{"GET", "bytes=-17", 206, full[len(full)-17:], "bytes 1007-1023/1024"},
		{"GET", "bytes=1024-", 416, nil, "bytes */1024"},
		{"POST", "", 405, nil, ""},
	} {
		r := httptest.NewRequest(tc.method, path, nil)
		r.Header.Set("Range", tc.rangeValue)

		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)

		if w.Code != tc.status || (tc.status < 400 && !bytes.Equal(w.Body.Bytes(), tc.body)) || w.Header().Get("Content-Range") != tc.contentRange {
			t.Fatalf("%+v: %d %v", tc, w.Code, w.Header())
		}

		if tc.status < 400 && (w.Header().Get("Docker-Content-Digest") != object.descriptor.Digest.String() || w.Header().Get("Content-Type") != object.descriptor.MediaType) {
			t.Fatal(w.Header())
		}
	}

	for _, path := range []string{"/v2/other/blobs/" + object.descriptor.Digest.String(), "/v2/" + imageRepository + "/manifests/latest", "/v2/" + imageRepository + "/blobs/sha256:bad"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))

		if w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
}

func TestImagePullThroughMirror(t *testing.T) {
	s, c := imageTestRegistry(t)

	registry := httptest.NewServer(s)
	defer registry.Close()

	c.registryURL = registry.URL
	c.registryNamespace = "fixture.test:5000"

	client := imageHTTPClient()
	defer client.CloseIdleConnections()

	catalog, err := fetchCatalog(t.Context(), client, c)
	if err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int64

	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ns") != c.registryNamespace || r.Header.Get("Gantry-Mirrored") != "" || r.URL.Path == "/loadgen/catalog" {
			t.Error("incorrect mirror route", r.URL)
		}

		requests.Add(1)
		s.ServeHTTP(w, r)
	}))
	defer mirror.Close()

	c.gantryEndpoint = mirror.URL

	m := newImageMetrics(prometheus.NewRegistry())
	for range 2 {
		if err := pullImage(t.Context(), client, c, catalog.Images[0], m); err != nil {
			t.Fatal(err)
		}
	}

	if requests.Load() != 8 || testutil.ToFloat64(m.pulls.WithLabelValues("success")) != 2 || testutil.ToFloat64(m.bytes) <= 4096 {
		t.Fatal("missing full repeated pulls or metrics")
	}
}

func TestImagePullFailures(t *testing.T) {
	for _, mode := range []string{"corrupt", "truncated", "oversized", "status", "redirect", "timeout", "manifest"} {
		t.Run(mode, func(t *testing.T) {
			s, c := imageTestRegistry(t)

			desc := s.catalog.Images[0]
			if mode == "manifest" {
				desc = s.addJSON("manifests", oci.MediaTypeImageManifest, map[string]any{"schemaVersion": 1})
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/blobs/") {
					s.ServeHTTP(w, r)
					return
				}

				switch mode {
				case "status":
					w.WriteHeader(503)
				case "redirect":
					http.Redirect(w, r, "/v2/", http.StatusTemporaryRedirect)
				case "timeout":
					<-r.Context().Done()
				default:
					key := strings.TrimPrefix(r.URL.Path, "/v2/"+imageRepository+"/")
					obj := s.objects[key]
					data, _ := io.ReadAll(io.NewSectionReader(obj.source, 0, obj.descriptor.Size))

					switch mode {
					case "corrupt":
						data[0] ^= 1
					case "truncated":
						data = data[:len(data)-1]
					case "oversized":
						data = append(data, 1)
					}

					w.WriteHeader(200)
					w.(http.Flusher).Flush() // Force chunked framing to test the streamed size check.
					_, _ = w.Write(data)
				}
			}))
			defer server.Close()

			c.gantryEndpoint, c.registryNamespace = server.URL, "fixture.test"
			if mode == "timeout" {
				c.timeout = 50 * time.Millisecond
			}

			m := newImageMetrics(prometheus.NewRegistry())

			client := imageHTTPClient()
			defer client.CloseIdleConnections()

			if err := pullImage(t.Context(), client, c, desc, m); err == nil {
				t.Fatal("bad image succeeded")
			}

			if testutil.ToFloat64(m.pulls.WithLabelValues("error")) != 1 || testutil.ToFloat64(m.pulls.WithLabelValues("success")) != 0 {
				t.Fatal("incorrect attempt accounting")
			}

			if mode == "corrupt" && testutil.ToFloat64(m.bytes) <= float64(desc.Size) {
				t.Fatal("failed bytes not counted")
			}
		})
	}
}

func TestImageLayerConcurrencyAndCancellation(t *testing.T) {
	s, c := imageTestRegistry(t)
	c.layerConcurrency = 2
	started := make(chan struct{}, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		obj := s.objects[strings.TrimPrefix(r.URL.Path, "/v2/"+imageRepository+"/")]
		if obj.descriptor.MediaType == oci.MediaTypeImageLayer {
			started <- struct{}{}

			<-r.Context().Done()

			return
		}

		s.ServeHTTP(w, r)
	}))
	defer server.Close()

	c.gantryEndpoint = server.URL

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	client := imageHTTPClient()
	defer client.CloseIdleConnections()

	done := make(chan error, 1)

	go func() {
		done <- pullImage(ctx, client, c, s.catalog.Images[0], newImageMetrics(prometheus.NewRegistry()))
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("layers did not run concurrently")
		}
	}

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled pull succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workers did not stop")
	}
}

func TestImageCatalogValidation(t *testing.T) {
	s, c := imageTestRegistry(t)
	for _, mode := range []string{"version", "repository", "empty", "digest", "urls", "oversized", "malformed", "status"} {
		t.Run(mode, func(t *testing.T) {
			catalog := s.catalog
			catalog.Images = append([]oci.Descriptor(nil), catalog.Images...)

			switch mode {
			case "version":
				catalog.Version++
			case "repository":
				catalog.Repository = "../other"
			case "empty":
				catalog.Images = nil
			case "digest":
				catalog.Images[0].Digest = "sha256:bad"
			case "urls":
				catalog.Images[0].URLs = []string{"http://other"}
			case "oversized":
				catalog.Images[0].Size = maxImageMetadata + 1
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if mode == "status" {
					w.WriteHeader(503)
					return
				}

				if mode == "malformed" {
					_, _ = io.WriteString(w, "{")
					return
				}

				_ = json.NewEncoder(w).Encode(catalog)
			}))
			defer server.Close()

			c.registryURL = server.URL

			client := imageHTTPClient()
			defer client.CloseIdleConnections()

			if _, err := fetchCatalog(t.Context(), client, c); err == nil {
				t.Fatal("accepted bad catalog")
			}
		})
	}
}

func TestImageServiceLifecycle(t *testing.T) {
	for _, role := range []string{"registry", "load", "both"} {
		t.Run(role, func(t *testing.T) {
			s, c := imageTestRegistry(t)

			upstream := httptest.NewServer(s)
			defer upstream.Close()

			c.role, c.registryURL, c.gantryEndpoint, c.registryNamespace = role, upstream.URL, upstream.URL, "fixture.test"
			c.listen, c.registryListen = "127.0.0.1:0", "127.0.0.1:0"

			c.duration = 50 * time.Millisecond
			if err := serve(t.Context(), c); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("cancel during catalog preparation", func(t *testing.T) {
		requested := make(chan struct{}, 1)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requested <- struct{}{}

			<-r.Context().Done()
		}))
		defer server.Close()

		c := imageTestConfig(t)
		c.role, c.registryURL, c.listen = "load", server.URL, "127.0.0.1:0"

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		done := make(chan error, 1)

		go func() { done <- serve(ctx, c) }()

		select {
		case <-requested:
		case <-time.After(5 * time.Second):
			t.Fatal("catalog not requested")
		}

		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("service did not stop")
		}
	})
	t.Run("listener failure", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()

		c := imageTestConfig(t)

		c.listen, c.registryListen = "127.0.0.1:0", listener.Addr().String()
		if err := serve(t.Context(), c); err == nil {
			t.Fatal("busy listener accepted")
		}
	})
}
