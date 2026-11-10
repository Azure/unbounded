// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestCatalogClassifiesManifestsAndPreservesMetadataFailures(t *testing.T) {
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := godigest.FromBytes(config).String()
	layer := "sha256:" + strings.Repeat("c", 64)
	manifest := func(streaming bool) []byte {
		t.Helper()

		annotations := map[string]string{}

		artifactType := ""
		if streaming {
			artifactType = "application/vnd.azure.artifact.streaming.v1"
			annotations["containerd.io/snapshot/overlaybd/blob-digest"] = layer
			annotations["containerd.io/snapshot/overlaybd/blob-size"] = "100"
		}

		body, err := json.Marshal(map[string]any{
			"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
			"artifactType": artifactType,
			"config":       map[string]any{"digest": configDigest, "size": len(config)},
			"layers":       []any{map[string]any{"digest": layer, "size": 100, "annotations": annotations}},
		})
		if err != nil {
			t.Fatal(err)
		}

		return body
	}
	ordinary, streaming := manifest(false), manifest(true)
	child := godigest.FromBytes(ordinary).String()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/v2/_catalog":
			if r.URL.Query().Get("last") == "" {
				_, _ = w.Write([]byte(`{"repositories":["smoke"]}`))
			} else {
				_, _ = w.Write([]byte(`{"repositories":[]}`))
			}
		case "/v2/smoke/tags/list":
			if r.URL.Query().Get("last") == "" {
				_, _ = w.Write([]byte(`{"tags":["ordinary","streaming","multiarch","broken"]}`))
			} else {
				_, _ = w.Write([]byte(`{"tags":[]}`))
			}
		case "/v2/smoke/manifests/ordinary", "/v2/smoke/manifests/" + child:
			_, _ = w.Write(ordinary)
		case "/v2/smoke/manifests/streaming":
			_, _ = w.Write(streaming)
		case "/v2/smoke/manifests/multiarch":
			_, _ = w.Write([]byte(`{"manifests":[{"digest":"` + child + `","platform":{"os":"linux","architecture":"amd64"}}]}`))
		case "/v2/smoke/blobs/" + configDigest:
			_, _ = w.Write(config)
		default:
			http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	registry := &Registry{host: u.Host, architecture: "amd64", client: &auth.Client{Client: server.Client()}}

	catalog, err := registry.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(catalog.Images) != 3 || len(catalog.Errors) != 1 {
		t.Fatalf("catalog: %+v", catalog)
	}

	byTag := map[string]Image{}
	for _, image := range catalog.Images {
		byTag[image.Tag] = image
		if image.Size != 100 || image.Architecture != "amd64" || !strings.Contains(image.Reference, "@sha256:") {
			t.Fatalf("incorrect image metadata: %+v", image)
		}
	}

	if byTag["ordinary"].Streaming || !byTag["streaming"].Streaming || byTag["multiarch"].Digest != child {
		t.Fatalf("classification or platform resolution failed: %+v", byTag)
	}
	// Identical manifests under multiple tags reuse one digest-pinned deployment.
	if byTag["ordinary"].ID != byTag["multiarch"].ID {
		t.Fatal("aliases of the same manifest must not create duplicate deployments")
	}
}

func TestCatalogCancellationWaitsForImageWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workerStarted := make(chan struct{})
	releaseWorker := make(chan struct{})
	laterStarted := make(chan struct{})
	registry := &Registry{host: "example.azurecr.io", client: &auth.Client{
		Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := `{}`

			switch r.URL.Path {
			case "/v2/_catalog":
				body = `{"repositories":["smoke","later"]}`
				if r.URL.Query().Get("last") != "" {
					body = `{"repositories":[]}`
				}
			case "/v2/smoke/tags/list":
				body = `{"tags":["blocked"]}`
				if r.URL.Query().Get("last") != "" {
					body = `{"tags":[]}`
				}
			case "/v2/smoke/manifests/blocked":
				close(workerStarted)
				<-releaseWorker

				return nil, ctx.Err()
			case "/v2/later/tags/list":
				<-workerStarted
				close(laterStarted)
				<-ctx.Done()

				return nil, ctx.Err()
			}

			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}}

	done := make(chan error, 1)

	go func() {
		_, err := registry.Catalog(ctx)
		done <- err
	}()

	select {
	case <-laterStarted:
	case <-time.After(5 * time.Second):
		close(releaseWorker)
		t.Fatal("catalog did not start repository enumeration")
	}

	cancel()

	select {
	case err := <-done:
		close(releaseWorker)
		t.Fatalf("catalog returned before its image worker finished: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	close(releaseWorker)

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("catalog did not finish after its worker stopped")
	}
}

func TestRegistryRejectsUntrustedHostsAndMetadataRedirects(t *testing.T) {
	for _, host := range []string{"localhost", "example.azurecr.io.attacker.test", "https://example.azurecr.io", "example.azurecr.io/path"} {
		if _, err := NewRegistry(host, "amd64", nil); err == nil {
			t.Fatalf("accepted non-ACR login server %q", host)
		}
	}

	registry, err := NewRegistry("example.azurecr.io", "amd64", nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{"http://example.blob.core.windows.net/x", "https://attacker.test/x"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Authorization", "Bearer sensitive")

		if err := registry.client.Client.CheckRedirect(request, nil); err == nil {
			t.Fatalf("accepted redirect to %s", target)
		}

		if request.Header.Get("Authorization") != "" {
			t.Fatal("registry credentials survived a data redirect")
		}
	}

	request := httptest.NewRequest(http.MethodGet, "https://example.blob.core.windows.net/data?sig=example", nil)
	request.Header.Set("Authorization", "Bearer sensitive")

	if err := registry.client.Client.CheckRedirect(request, nil); err != nil || request.Header.Get("Authorization") != "" {
		t.Fatalf("signed metadata redirect should work without credentials: %v", err)
	}

	for _, count := range []int{4, 6} {
		if err := registry.client.Client.CheckRedirect(request, make([]*http.Request, count)); err == nil {
			t.Fatal("unbounded metadata redirect chain accepted: " + strconv.Itoa(count))
		}
	}
}

func TestRegistryErrorsDoNotExposeSignedRedirectQueries(t *testing.T) {
	registry, err := NewRegistry("example.azurecr.io", "amd64", nil)
	if err != nil {
		t.Fatal(err)
	}

	registry.client.Client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: "https://example.canadacentral.data.azurecr.io/data?sig=private-test-marker&token=another-test-marker", Err: context.DeadlineExceeded}
	})

	_, err = registry.get(context.Background(), "/v2/demo/manifests/tag", nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request failure was lost: %v", err)
	}

	for _, secret := range []string{"private-test-marker", "another-test-marker", "?sig=", "&token="} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("registry error leaked query parameters: %s", err)
		}
	}

	if !strings.Contains(err.Error(), "example.canadacentral.data.azurecr.io/data") || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("redaction removed diagnostic context: %s", err)
	}
}
