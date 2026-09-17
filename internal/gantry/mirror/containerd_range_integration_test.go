// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	containerddigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Azure/unbounded/internal/gantry/config"
	gantrydigest "github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
)

const generatedLayerByte = byte('x')

type generatedLayerReader struct {
	remaining int64
}

func (r *generatedLayerReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}

	read := min(int64(len(p)), r.remaining)
	for index := range int(read) {
		p[index] = generatedLayerByte
	}

	r.remaining -= read

	return int(read), nil
}

func generatedLayerDigest(t *testing.T, size int64) gantrydigest.Digest {
	t.Helper()

	hash := sha256.New()
	if _, err := io.Copy(hash, &generatedLayerReader{remaining: size}); err != nil {
		t.Fatalf("hash generated layer: %v", err)
	}

	d, err := gantrydigest.Parse("sha256:" + fmt.Sprintf("%x", hash.Sum(nil)))
	if err != nil {
		t.Fatalf("parse generated digest: %v", err)
	}

	return d
}

func containerdRegistryHost(t *testing.T, server *httptest.Server) docker.RegistryHost {
	t.Helper()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	return docker.RegistryHost{
		Client:       server.Client(),
		Host:         u.Host,
		Scheme:       u.Scheme,
		Path:         "/v2",
		Capabilities: docker.HostCapabilityPull,
	}
}

func fetchGeneratedLayer(t *testing.T, hosts []docker.RegistryHost, d gantrydigest.Digest, size int64) {
	t.Helper()

	resolver := docker.NewResolver(docker.ResolverOptions{
		Hosts: func(string) ([]docker.RegistryHost, error) { return hosts, nil },
	})

	fetcher, err := resolver.Fetcher(context.Background(), "registry.example.com/repo/image:latest")
	if err != nil {
		t.Fatalf("Fetcher: %v", err)
	}

	rc, err := fetcher.Fetch(context.Background(), ocispec.Descriptor{
		Digest:    containerddigest.Digest(d.String()),
		Size:      size,
		MediaType: ocispec.MediaTypeImageLayer,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer rc.Close()

	hash := sha256.New()

	written, err := io.Copy(hash, rc)
	if err != nil {
		t.Fatalf("copy fetched layer: %v", err)
	}

	if written != size {
		t.Fatalf("written = %d; want %d", written, size)
	}

	got := "sha256:" + fmt.Sprintf("%x", hash.Sum(nil))
	if got != d.String() {
		t.Fatalf("digest = %s; want %s", got, d)
	}
}

func TestContainerdResumesInterruptedGantryBodyWithoutOriginReplay(t *testing.T) {
	const (
		layerSize = int64(8 << 20)
		prefix    = int64(1 << 20)
	)

	d := generatedLayerDigest(t, layerSize)

	var (
		firstRequest atomic.Bool
		originBytes  atomic.Int64
	)

	originServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" && firstRequest.CompareAndSwap(false, true) {
			w.Header().Set("Content-Length", strconv.FormatInt(layerSize, 10))
			w.WriteHeader(http.StatusOK)

			written, _ := io.CopyN(w, &generatedLayerReader{remaining: prefix}, prefix)
			originBytes.Add(written)

			return
		}

		if rangeHeader != "bytes="+strconv.FormatInt(prefix, 10)+"-" {
			t.Errorf("origin Range = %q; want bytes=%d-", rangeHeader, prefix)
			http.Error(w, "unexpected range", http.StatusBadRequest)

			return
		}

		remaining := layerSize - prefix
		w.Header().Set("Content-Length", strconv.FormatInt(remaining, 10))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", prefix, layerSize-1, layerSize))
		w.WriteHeader(http.StatusPartialContent)

		written, _ := io.CopyN(w, &generatedLayerReader{remaining: remaining}, remaining)
		originBytes.Add(written)
	}))
	defer originServer.Close()

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "registry.example.com", Endpoint: originServer.URL}}}

	originClient, err := origin.New(cfg)
	if err != nil {
		t.Fatalf("origin.New: %v", err)
	}

	gantryServer := httptest.NewServer(mirror.New(cfg, fakes.NewCache(), originClient, mirror.WithLiveStreamThrough()).Handler())
	defer gantryServer.Close()

	fetchGeneratedLayer(t, []docker.RegistryHost{containerdRegistryHost(t, gantryServer)}, d, layerSize)

	if got := originBytes.Load(); got != layerSize {
		t.Fatalf("origin bytes = %d; want %d without prefix replay", got, layerSize)
	}
}

func TestContainerdFallsBackAfterPreHeaderGantryFailure(t *testing.T) {
	const layerSize = int64(1 << 20)

	d := generatedLayerDigest(t, layerSize)

	gantryFailure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer gantryFailure.Close()

	var directHits atomic.Int64

	directOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/blobs/"+d.String()) {
			http.NotFound(w, r)

			return
		}

		directHits.Add(1)
		w.Header().Set("Content-Length", strconv.FormatInt(layerSize, 10))
		_, _ = io.CopyN(w, &generatedLayerReader{remaining: layerSize}, layerSize) //nolint:errcheck // test response
	}))
	defer directOrigin.Close()

	hosts := []docker.RegistryHost{
		containerdRegistryHost(t, gantryFailure),
		containerdRegistryHost(t, directOrigin),
	}
	fetchGeneratedLayer(t, hosts, d, layerSize)

	if directHits.Load() != 1 {
		t.Fatalf("direct origin hits = %d; want 1", directHits.Load())
	}
}
