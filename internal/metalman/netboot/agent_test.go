// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestAgentArtifactSourceFor(t *testing.T) {
	const baseURL = "https://releases.example.com"

	tests := []struct {
		name        string
		artifact    *AgentArtifact
		machine     *v1alpha3.Machine
		wantURL     string
		wantChecks  string
		wantCacheID string
	}{
		{
			name:        "default release version",
			artifact:    &AgentArtifact{DefaultBaseURL: baseURL, DefaultVersion: "v1.2.3"},
			machine:     &v1alpha3.Machine{},
			wantURL:     baseURL + "/download/v1.2.3/unbounded-agent-linux-amd64.tar.gz",
			wantChecks:  baseURL + "/download/v1.2.3/checksums.txt",
			wantCacheID: "v1.2.3-amd64",
		},
		{
			name:        "latest for a dev build",
			artifact:    &AgentArtifact{DefaultBaseURL: baseURL, DefaultVersion: "dev"},
			machine:     &v1alpha3.Machine{},
			wantURL:     baseURL + "/latest/download/unbounded-agent-linux-amd64.tar.gz",
			wantCacheID: "latest-amd64",
		},
		{
			name:     "machine version and base URL",
			artifact: &AgentArtifact{DefaultBaseURL: baseURL, DefaultVersion: "v1.2.3"},
			machine: &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{
				Agent: &v1alpha3.AgentSpec{Version: "v9.9.9", BaseURL: "https://mirror.example.com/releases/"},
			}},
			wantURL:     "https://mirror.example.com/releases/download/v9.9.9/unbounded-agent-linux-amd64.tar.gz",
			wantChecks:  "https://mirror.example.com/releases/download/v9.9.9/checksums.txt",
			wantCacheID: "v9.9.9-amd64",
		},
		{
			name:     "machine URL",
			artifact: &AgentArtifact{DefaultBaseURL: baseURL, DefaultVersion: "v1.2.3"},
			machine: &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{
				Agent: &v1alpha3.AgentSpec{URL: "https://artifacts.example.com/agent.tar.gz?token=x"},
			}},
			wantURL: "https://artifacts.example.com/agent.tar.gz?token=x",
		},
		{
			name:     "arm64 machine",
			artifact: &AgentArtifact{DefaultBaseURL: baseURL, DefaultVersion: "v1.2.3"},
			machine: &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{
				PXE: &v1alpha3.PXESpec{Architecture: "arm64"},
			}},
			wantURL:     baseURL + "/download/v1.2.3/unbounded-agent-linux-arm64.tar.gz",
			wantChecks:  baseURL + "/download/v1.2.3/checksums.txt",
			wantCacheID: "v1.2.3-arm64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := tt.artifact.SourceFor(tt.machine)
			if source.URL != tt.wantURL {
				t.Errorf("URL: got %q, want %q", source.URL, tt.wantURL)
			}

			if source.ChecksumsURL != tt.wantChecks {
				t.Errorf("checksums URL: got %q, want %q", source.ChecksumsURL, tt.wantChecks)
			}

			if tt.wantCacheID != "" && source.CacheKey != tt.wantCacheID {
				t.Errorf("cache key: got %q, want %q", source.CacheKey, tt.wantCacheID)
			}
		})
	}
}

func TestAgentArtifactDownloadsAndVerifies(t *testing.T) {
	const payload = "agent tarball"

	sum := sha256.Sum256([]byte(payload))
	checksums := fmt.Sprintf("%s  unbounded-agent-linux-amd64.tar.gz\n", hex.EncodeToString(sum[:]))

	server := newAgentServer(t, payload, checksums, http.StatusOK, nil)

	artifact := &AgentArtifact{
		CacheDir:       t.TempDir(),
		DefaultBaseURL: server.URL,
		DefaultVersion: "v1.2.3",
		Client:         server.Client(),
	}

	path, err := artifact.Path(t.Context(), &v1alpha3.Machine{})
	if err != nil {
		t.Fatalf("Path: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cached tarball: %v", err)
	}

	if string(got) != payload {
		t.Fatalf("cached tarball: got %q, want %q", got, payload)
	}
}

func TestAgentArtifactRejectsChecksumMismatch(t *testing.T) {
	checksums := strings.Repeat("0", 64) + "  unbounded-agent-linux-amd64.tar.gz\n"

	server := newAgentServer(t, "agent tarball", checksums, http.StatusOK, nil)

	artifact := &AgentArtifact{
		CacheDir:       t.TempDir(),
		DefaultBaseURL: server.URL,
		DefaultVersion: "v1.2.3",
		Client:         server.Client(),
	}

	if _, err := artifact.Path(t.Context(), &v1alpha3.Machine{}); err == nil {
		t.Fatal("expected a checksum error")
	}
}

func TestAgentArtifactToleratesMissingChecksums(t *testing.T) {
	server := newAgentServer(t, "agent tarball", "not found", http.StatusNotFound, nil)

	artifact := &AgentArtifact{
		CacheDir:       t.TempDir(),
		DefaultBaseURL: server.URL,
		DefaultVersion: "v1.2.3",
		Client:         server.Client(),
	}

	if _, err := artifact.Path(t.Context(), &v1alpha3.Machine{}); err != nil {
		t.Fatalf("Path: %v", err)
	}
}

func TestAgentArtifactCachesDownload(t *testing.T) {
	const payload = "agent tarball"

	sum := sha256.Sum256([]byte(payload))
	checksums := fmt.Sprintf("%s  unbounded-agent-linux-amd64.tar.gz\n", hex.EncodeToString(sum[:]))

	var downloads atomic.Int32

	server := newAgentServer(t, payload, checksums, http.StatusOK, &downloads)

	artifact := &AgentArtifact{
		CacheDir:       t.TempDir(),
		DefaultBaseURL: server.URL,
		DefaultVersion: "v1.2.3",
		Client:         server.Client(),
	}

	for range 2 {
		if _, err := artifact.Path(t.Context(), &v1alpha3.Machine{}); err != nil {
			t.Fatalf("Path: %v", err)
		}
	}

	if got := downloads.Load(); got != 1 {
		t.Fatalf("tarball downloads: got %d, want 1", got)
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("https://example.com/agent.tar.gz?token=secret"); got != "https://example.com/agent.tar.gz" {
		t.Fatalf("got %q", got)
	}
}

// newAgentServer serves the release layout for the amd64 agent tarball.
// downloads, when non-nil, counts tarball requests.
func newAgentServer(t *testing.T, payload, checksums string, checksumsStatus int, downloads *atomic.Int32) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/download/v1.2.3/unbounded-agent-linux-amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		if downloads != nil {
			downloads.Add(1)
		}

		w.Write([]byte(payload)) //nolint:errcheck // Test server.
	})
	mux.HandleFunc("/download/v1.2.3/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(checksumsStatus)
		w.Write([]byte(checksums)) //nolint:errcheck // Test server.
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}
