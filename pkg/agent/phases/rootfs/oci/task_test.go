// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package oci

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

func TestParseHTTPSArchiveReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		image   string
		wantURL string
		wantOK  bool
		wantErr bool
	}{
		{
			name:    "archive auto-selects its image",
			image:   "https://artifacts.example.test/rootfs.oci.tar.gz",
			wantURL: "https://artifacts.example.test/rootfs.oci.tar.gz",
			wantOK:  true,
		},
		{
			name:   "registry reference",
			image:  "registry.example.test/rootfs:v1",
			wantOK: false,
		},
		{
			name:    "missing archive path",
			image:   "https://artifacts.example.test",
			wantOK:  true,
			wantErr: true,
		},
		{
			name:    "query not supported",
			image:   "https://artifacts.example.test/rootfs.tar?token=value",
			wantOK:  true,
			wantErr: true,
		},
		{
			name:    "fragment not supported",
			image:   "https://artifacts.example.test/rootfs.tar#v1",
			wantOK:  true,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotURL, gotOK, err := parseHTTPSArchiveReference(tt.image)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}

			if tt.wantErr {
				if err == nil {
					t.Fatal("err = nil, want error")
				}

				return
			}

			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}

			if gotURL != tt.wantURL {
				t.Fatalf("URL = %q, want %q", gotURL, tt.wantURL)
			}
		})
	}
}

func TestFindOCILayoutRoot(t *testing.T) {
	t.Parallel()

	extractDir := t.TempDir()
	layoutDir := filepath.Join(extractDir, "rootfs")

	if err := os.MkdirAll(layoutDir, 0o755); err != nil {
		t.Fatalf("create layout: %v", err)
	}

	if err := os.WriteFile(filepath.Join(layoutDir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatalf("write oci-layout: %v", err)
	}

	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), []byte(`{"schemaVersion":2}`), 0o644); err != nil {
		t.Fatalf("write index.json: %v", err)
	}

	blobDir := filepath.Join(layoutDir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("create blob dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(blobDir, "oci-layout"), []byte("blob"), 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	got, err := findOCILayoutRoot(extractDir)
	if err != nil {
		t.Fatalf("findOCILayoutRoot() error = %v", err)
	}

	if got != layoutDir {
		t.Fatalf("layout = %q, want %q", got, layoutDir)
	}
}

func TestSingleOCILayoutReference(t *testing.T) {
	t.Parallel()

	layoutDir := t.TempDir()
	index := `{
		"schemaVersion": 2,
		"manifests": [
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
				"size": 1,
				"annotations": {"org.opencontainers.image.ref.name": "v1"}
			},
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				"size": 1
			}
		]
	}`

	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), []byte(index), 0o644); err != nil {
		t.Fatalf("write index.json: %v", err)
	}

	got, err := singleOCILayoutReference(layoutDir)
	if err != nil {
		t.Fatalf("singleOCILayoutReference() error = %v", err)
	}

	if got != "v1" {
		t.Fatalf("reference = %q, want v1", got)
	}
}

func TestSingleOCILayoutReferenceRejectsMultipleImages(t *testing.T) {
	t.Parallel()

	layoutDir := t.TempDir()
	index := `{
		"schemaVersion": 2,
		"manifests": [
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
				"size": 1,
				"annotations": {"org.opencontainers.image.ref.name": "v1"}
			},
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				"size": 1,
				"annotations": {"org.opencontainers.image.ref.name": "v2"}
			}
		]
	}`

	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), []byte(index), 0o644); err != nil {
		t.Fatalf("write index.json: %v", err)
	}

	if _, err := singleOCILayoutReference(layoutDir); err == nil {
		t.Fatal("singleOCILayoutReference() error = nil, want multiple image error")
	}
}

func TestConfigureOCIPullRetryUsesORASRetryClient(t *testing.T) {
	repo := &remote.Repository{}
	configureOCIPullRetry(repo)

	authClient, ok := repo.Client.(*auth.Client)
	if !ok {
		t.Fatalf("repo.Client = %T, want *auth.Client", repo.Client)
	}

	transport, ok := authClient.Client.Transport.(*retry.Transport)
	if !ok {
		t.Fatalf("auth client transport = %T, want *retry.Transport", authClient.Client.Transport)
	}

	if transport.Policy == nil {
		t.Fatal("retry transport policy is nil")
	}
}

func TestOCIPullRetryPolicyRetriesTransportErrors(t *testing.T) {
	policy := newOCIPullRetryPolicy()

	delay, err := policy.Retry(0, nil, &net.DNSError{
		Err:        "no such host",
		Name:       "registry.example.test",
		IsNotFound: true,
	})
	if err != nil {
		t.Fatalf("policy.Retry returned error: %v", err)
	}

	if delay != ociPullRetryDelay {
		t.Fatalf("retry delay = %v, want %v", delay, ociPullRetryDelay)
	}

	delay, err = policy.Retry(0, nil, &net.OpError{
		Op:  "dial",
		Err: syscall.ECONNREFUSED,
	})
	if err != nil {
		t.Fatalf("policy.Retry for connection refused returned error: %v", err)
	}

	if delay != ociPullRetryDelay {
		t.Fatalf("retry delay for connection refused = %v, want %v", delay, ociPullRetryDelay)
	}

	delay, err = policy.Retry(ociPullMaxAttempts-1, nil, &net.DNSError{
		Err:        "no such host",
		Name:       "registry.example.test",
		IsNotFound: true,
	})
	if err != nil {
		t.Fatalf("policy.Retry at max attempts returned error: %v", err)
	}

	if delay >= 0 {
		t.Fatalf("retry delay at max attempts = %v, want negative", delay)
	}
}

func TestOCIPullRetryPolicySkipsNonNetworkTransportErrors(t *testing.T) {
	policy := newOCIPullRetryPolicy()

	delay, err := policy.Retry(0, nil, errors.New("tls: failed to verify certificate"))
	if err != nil {
		t.Fatalf("policy.Retry for TLS error returned error: %v", err)
	}

	if delay >= 0 {
		t.Fatalf("retry delay for TLS error = %v, want negative", delay)
	}

	delay, err = policy.Retry(0, nil, context.Canceled)
	if err != nil {
		t.Fatalf("policy.Retry for context cancellation returned error: %v", err)
	}

	if delay >= 0 {
		t.Fatalf("retry delay for context cancellation = %v, want negative", delay)
	}
}

func TestOCIPullRetryPolicyUsesORASStatusPredicate(t *testing.T) {
	policy := newOCIPullRetryPolicy()

	delay, err := policy.Retry(0, &http.Response{StatusCode: http.StatusServiceUnavailable}, nil)
	if err != nil {
		t.Fatalf("policy.Retry for 503 returned error: %v", err)
	}

	if delay != ociPullRetryDelay {
		t.Fatalf("retry delay for 503 = %v, want %v", delay, ociPullRetryDelay)
	}

	delay, err = policy.Retry(0, &http.Response{StatusCode: http.StatusNotFound}, nil)
	if err != nil {
		t.Fatalf("policy.Retry for 404 returned error: %v", err)
	}

	if delay >= 0 {
		t.Fatalf("retry delay for 404 = %v, want negative", delay)
	}
}

func TestMaxOCIPullRetryDelay(t *testing.T) {
	if got, want := maxOCIPullRetryDelay(), 16*time.Second; got != want {
		t.Fatalf("maxOCIPullRetryDelay() = %v, want %v", got, want)
	}
}

func TestParseOCILayoutReference(t *testing.T) {
	tests := []struct {
		name       string
		image      string
		wantLayout string
		wantTag    string
		wantOK     bool
		wantErr    bool
	}{
		{
			name:       "tagged layout",
			image:      "oci-layout:///opt/unbounded/images/agent-ubuntu2404:v20260619",
			wantLayout: "/opt/unbounded/images/agent-ubuntu2404",
			wantTag:    "v20260619",
			wantOK:     true,
		},
		{
			name:       "default latest",
			image:      "oci-layout:///opt/unbounded/images/agent-ubuntu2404",
			wantLayout: "/opt/unbounded/images/agent-ubuntu2404",
			wantTag:    "latest",
			wantOK:     true,
		},
		{
			name:   "not layout",
			image:  "ghcr.io/azure/agent-ubuntu2404:v20260619",
			wantOK: false,
		},
		{
			name:    "missing source",
			image:   "oci-layout://",
			wantOK:  true,
			wantErr: true,
		},
		{
			name:    "empty tag",
			image:   "oci-layout:///opt/unbounded/images/agent-ubuntu2404:",
			wantOK:  true,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLayout, gotTag, gotOK, err := parseOCILayoutReference(tt.image)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}

			if tt.wantErr {
				if err == nil {
					t.Fatal("err = nil, want error")
				}

				return
			}

			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}

			if gotLayout != tt.wantLayout {
				t.Fatalf("layout = %q, want %q", gotLayout, tt.wantLayout)
			}

			if gotTag != tt.wantTag {
				t.Fatalf("tag = %q, want %q", gotTag, tt.wantTag)
			}
		})
	}
}

func TestParseImageReference(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		wantRef string
		wantTag string
		wantErr bool
	}{
		{
			name:    "tagged image",
			image:   "registry.example.com/unbounded/rootfs:v1",
			wantRef: "registry.example.com/unbounded/rootfs",
			wantTag: "v1",
		},
		{
			name:    "tagged image with OCI scheme",
			image:   "oci://registry.example.com/unbounded/rootfs:v1",
			wantRef: "registry.example.com/unbounded/rootfs",
			wantTag: "v1",
		},
		{
			name:    "default latest with OCI scheme",
			image:   "oci://registry.example.com/unbounded/rootfs",
			wantRef: "registry.example.com/unbounded/rootfs",
			wantTag: "latest",
		},
		{
			name:    "digest with OCI scheme",
			image:   "oci://registry.example.com/unbounded/rootfs@sha256:abc123",
			wantRef: "registry.example.com/unbounded/rootfs",
			wantTag: "sha256:abc123",
		},
		{
			name:    "empty OCI scheme",
			image:   "oci://",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRef, gotTag, err := parseImageReference(tt.image)
			if tt.wantErr {
				if err == nil {
					t.Fatal("err = nil, want error")
				}

				return
			}

			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}

			if gotRef != tt.wantRef {
				t.Fatalf("ref = %q, want %q", gotRef, tt.wantRef)
			}

			if gotTag != tt.wantTag {
				t.Fatalf("tag = %q, want %q", gotTag, tt.wantTag)
			}
		})
	}
}
