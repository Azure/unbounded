// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	ispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRegistryServesPrewarmedImages(t *testing.T) {
	cacheDir := t.TempDir()
	digest := "sha256:" + repeatHex("e")
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	writeBlob(t, cacheDir, digest, manifest)

	cache := NewOCICache(cacheDir)
	cache.SetDigestForArchitecture("ghcr.io/azure/agent-ubuntu2404:v20261031", "amd64", digest)
	cache.SetDigestForArchitecture("localhost:5555/agent-ubuntu2404:smoke", "amd64", digest)

	registry := &Registry{Cache: cache}

	tests := []struct {
		name            string
		target          string
		wantStatus      int
		wantContentType string
		wantBody        string
	}{
		{
			name:       "version check",
			target:     "/v2/",
			wantStatus: http.StatusOK,
			wantBody:   "{}",
		},
		{
			name:            "manifest by tag",
			target:          "/v2/ghcr.io/azure/agent-ubuntu2404/manifests/v20261031",
			wantStatus:      http.StatusOK,
			wantContentType: ispec.MediaTypeImageIndex,
		},
		{
			name:            "manifest with containerd ns parameter",
			target:          "/v2/azure/agent-ubuntu2404/manifests/v20261031?ns=ghcr.io",
			wantStatus:      http.StatusOK,
			wantContentType: ispec.MediaTypeImageIndex,
		},
		{
			name:            "manifest for an upstream host with a port",
			target:          "/v2/localhost_5555/agent-ubuntu2404/manifests/smoke",
			wantStatus:      http.StatusOK,
			wantContentType: ispec.MediaTypeImageIndex,
		},
		{
			name:            "manifest for an upstream host with a port and ns parameter",
			target:          "/v2/agent-ubuntu2404/manifests/smoke?ns=localhost%3A5555",
			wantStatus:      http.StatusOK,
			wantContentType: ispec.MediaTypeImageIndex,
		},
		{
			name:            "manifest by digest",
			target:          "/v2/any/repository/manifests/" + digest,
			wantStatus:      http.StatusOK,
			wantContentType: ispec.MediaTypeImageIndex,
		},
		{
			name:       "blob",
			target:     "/v2/ghcr.io/azure/agent-ubuntu2404/blobs/" + digest,
			wantStatus: http.StatusOK,
			wantBody:   string(manifest),
		},
		{
			name:       "image outside the cache",
			target:     "/v2/ghcr.io/azure/agent-ubuntu2404/manifests/other",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unknown blob",
			target:     "/v2/ghcr.io/azure/agent-ubuntu2404/blobs/sha256:" + repeatHex("f"),
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "path traversal",
			target:     "/v2/ghcr.io/azure/agent-ubuntu2404/blobs/sha256:..",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unknown route",
			target:     "/v2/ghcr.io/azure/agent-ubuntu2404/tags/list",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			registry.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.target, nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantContentType != "" {
				if got := rec.Header().Get("Content-Type"); got != tt.wantContentType {
					t.Errorf("content type: got %q, want %q", got, tt.wantContentType)
				}
			}

			if tt.wantStatus == http.StatusOK && tt.wantContentType == "" && tt.wantBody != "" {
				if got := rec.Body.String(); got != tt.wantBody {
					t.Errorf("body: got %q, want %q", got, tt.wantBody)
				}
			}
		})
	}
}

func TestRegistryBlobRange(t *testing.T) {
	cacheDir := t.TempDir()
	digest := "sha256:" + repeatHex("1")
	writeBlob(t, cacheDir, digest, []byte("0123456789"))

	cache := NewOCICache(cacheDir)
	registry := &Registry{Cache: cache}

	req := httptest.NewRequest(http.MethodGet, "/v2/image/blobs/"+digest, nil)
	req.Header.Set("Range", "bytes=2-4")

	rec := httptest.NewRecorder()
	registry.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusPartialContent)
	}

	if got := rec.Body.String(); got != "234" {
		t.Fatalf("body: got %q, want %q", got, "234")
	}
}

func TestRegistrySetsDigestHeader(t *testing.T) {
	cacheDir := t.TempDir()
	digest := "sha256:" + repeatHex("2")
	writeBlob(t, cacheDir, digest, []byte(`{"schemaVersion":2}`))

	cache := NewOCICache(cacheDir)
	cache.SetDigestForArchitecture("ghcr.io/test/image:v1", "amd64", digest)

	rec := httptest.NewRecorder()
	(&Registry{Cache: cache}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/ghcr.io/test/image/manifests/v1", nil))

	if got := rec.Header().Get("Docker-Content-Digest"); got != digest {
		t.Fatalf("digest header: got %q, want %q", got, digest)
	}
}

func TestRegistryRejectsDigestTraversal(t *testing.T) {
	cacheDir := t.TempDir()
	writeLayout(t, cacheDir, "sha256:"+repeatHex("a"))

	secretPath := filepath.Join(cacheDir, "secret")
	if err := os.WriteFile(secretPath, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	registry := &Registry{Cache: NewOCICache(cacheDir)}

	targets := []string{
		"/v2/image/blobs/..:secret",
		"/v2/image/blobs/sha256:../secret",
		"/v2/image/blobs/sha256:..",
		"/v2/image/blobs/:",
	}

	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			rec := httptest.NewRecorder()
			registry.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status: got %d, want %d (body %q)", rec.Code, http.StatusNotFound, rec.Body.String())
			}
		})
	}
}

func TestSplitImageReference(t *testing.T) {
	tests := []struct {
		imageRef   string
		repository string
		reference  string
		ok         bool
	}{
		{imageRef: "ghcr.io/org/image:v1", repository: "ghcr.io/org/image", reference: "v1", ok: true},
		{imageRef: "oci://ghcr.io/org/image:v1", repository: "ghcr.io/org/image", reference: "v1", ok: true},
		{imageRef: "localhost:5000/org/image:v1", repository: "localhost:5000/org/image", reference: "v1", ok: true},
		{imageRef: "ghcr.io/org/image@sha256:abc", repository: "ghcr.io/org/image", reference: "sha256:abc", ok: true},
		{imageRef: "image", ok: false},
		{imageRef: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.imageRef, func(t *testing.T) {
			repository, reference, ok := splitImageReference(tt.imageRef)
			if ok != tt.ok || repository != tt.repository || reference != tt.reference {
				t.Fatalf("got (%q, %q, %t), want (%q, %q, %t)", repository, reference, ok, tt.repository, tt.reference, tt.ok)
			}
		})
	}
}

// writeBlob stores a manifest or blob in the layout of a digest.
func writeBlob(t *testing.T, cacheDir, digest string, content []byte) {
	t.Helper()

	algorithm, encoded, ok := splitDigest(digest)
	if !ok {
		t.Fatalf("invalid digest %q", digest)
	}

	writeLayout(t, cacheDir, digest)

	path := filepath.Join(NewOCICache(cacheDir).LayoutDir(digest), "blobs", algorithm, encoded)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create blob dir: %v", err)
	}

	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
}

func splitDigest(digest string) (algorithm, encoded string, ok bool) {
	for i := range digest {
		if digest[i] == ':' {
			return digest[:i], digest[i+1:], true
		}
	}

	return "", "", false
}

// ensure the manifest media type probe sees a manifest without a media type.
func TestManifestMediaType(t *testing.T) {
	if got := manifestMediaType([]byte(`{"schemaVersion":2}`)); got != ispec.MediaTypeImageManifest {
		t.Fatalf("got %q", got)
	}

	if got := manifestMediaType([]byte(`{"manifests":[]}`)); got != ispec.MediaTypeImageIndex {
		t.Fatalf("got %q", got)
	}
}
