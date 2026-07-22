// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pin/tftp/v3"
)

func TestTFTPServerFetchesTokenizedSessionArtifactFromBackend(t *testing.T) {
	t.Parallel()

	backend := &recordingTFTPBackend{data: []byte("firmware")}
	server := &TFTPServer{Backend: backend}
	transfer := &memoryOutgoingTransfer{}
	filename := "v1/netboot/sessions/session-1/capability/artifacts/shimx64.efi"

	if err := server.readHandler(filename, transfer); err != nil {
		t.Fatal(err)
	}
	if backend.filename != filename {
		t.Errorf("backend filename = %q, want %q", backend.filename, filename)
	}
	if got := transfer.String(); got != "firmware" {
		t.Errorf("transfer = %q", got)
	}
}

func TestTFTPServerRejectsLegacySourceIPFilenameWithBackend(t *testing.T) {
	t.Parallel()

	backend := &recordingTFTPBackend{data: []byte("firmware")}
	server := &TFTPServer{Backend: backend}
	if err := server.readHandler("shimx64.efi", &memoryOutgoingTransfer{}); err == nil {
		t.Fatal("expected legacy filename to be rejected")
	}
	if backend.filename != "" {
		t.Errorf("backend filename = %q, want empty", backend.filename)
	}
}

func TestTFTPServerReportsCompletedSessionTransfer(t *testing.T) {
	t.Parallel()

	backend := &recordingTFTPBackend{data: []byte("firmware")}
	server := &TFTPServer{Backend: backend}
	filename := "v1/netboot/sessions/session-1/capability/artifacts/shimx64.efi"

	if err := server.readHandler(filename, &memoryOutgoingTransfer{}); err != nil {
		t.Fatal(err)
	}
	if backend.completed != filename {
		t.Errorf("completed filename = %q, want %q", backend.completed, filename)
	}
}

type recordingTFTPBackend struct {
	filename  string
	completed string
	data      []byte
}

func (b *recordingTFTPBackend) Open(_ context.Context, filename string) (io.ReadCloser, error) {
	b.filename = filename

	return io.NopCloser(bytes.NewReader(b.data)), nil
}

func (b *recordingTFTPBackend) RecordBootLoaderDownloaded(_ context.Context, filename string) error {
	b.completed = filename

	return nil
}

type memoryOutgoingTransfer struct {
	bytes.Buffer
}

func (m *memoryOutgoingTransfer) ReadFrom(reader io.Reader) (int64, error) {
	return m.Buffer.ReadFrom(reader)
}

func (m *memoryOutgoingTransfer) RemoteAddr() net.UDPAddr {
	return net.UDPAddr{IP: net.ParseIP("10.0.1.20"), Port: 12345}
}

func (m *memoryOutgoingTransfer) LocalIP() net.IP {
	return net.ParseIP("10.0.1.254")
}

func (m *memoryOutgoingTransfer) SetSize(int64) {}

var _ tftp.OutgoingTransfer = (*memoryOutgoingTransfer)(nil)

func TestHTTPArtifactBackendResumesTruncatedResponse(t *testing.T) {
	t.Parallel()

	const artifact = "immutable-firmware"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			w.Header().Set("Content-Length", "18")
			_, _ = io.WriteString(w, artifact[:9])
		case 2:
			if got := r.Header.Get("Range"); got != "bytes=9-17" {
				t.Errorf("Range = %q", got)
			}
			w.Header().Set("Content-Length", "9")
			w.Header().Set("Content-Range", "bytes 9-17/18")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, artifact[9:])
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	backend, err := NewHTTPArtifactBackend(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := backend.Open(t.Context(), "v1/netboot/sessions/session/capability/artifacts/shimx64.efi")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close() //nolint:errcheck // Test cleanup.
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != artifact {
		t.Errorf("artifact = %q, want %q", got, artifact)
	}
}

func TestHTTPArtifactBackendRetriesFailedResumeRequest(t *testing.T) {
	t.Parallel()

	const artifact = "immutable-firmware"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			w.Header().Set("Content-Length", "18")
			_, _ = io.WriteString(w, artifact[:9])
		case 2:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijacking failed resume request: %v", err)
				return
			}
			_ = conn.Close()
		case 3:
			if got := r.Header.Get("Range"); got != "bytes=9-17" {
				t.Errorf("Range = %q", got)
			}
			w.Header().Set("Content-Length", "9")
			w.Header().Set("Content-Range", "bytes 9-17/18")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, artifact[9:])
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	backend, err := NewHTTPArtifactBackend(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := backend.Open(t.Context(), "v1/netboot/sessions/session/capability/artifacts/shimx64.efi")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close() //nolint:errcheck // Test cleanup.
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != artifact {
		t.Errorf("artifact = %q, want %q", got, artifact)
	}
	if got := requests.Load(); got != 3 {
		t.Errorf("backend requests = %d, want 3", got)
	}
}

func TestHTTPArtifactBackendReportsSessionBootLoaderMilestone(t *testing.T) {
	t.Parallel()

	var callbackPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callbackPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	backend, err := NewHTTPArtifactBackend(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	artifactPath := "v1/netboot/sessions/session/capability/artifacts/shimx64.efi"
	if err := backend.RecordBootLoaderDownloaded(t.Context(), artifactPath); err != nil {
		t.Fatal(err)
	}
	if want := "/v1/netboot/sessions/session/capability/callbacks/boot-loader-downloaded"; callbackPath != want {
		t.Errorf("callback path = %q, want %q", callbackPath, want)
	}
}
