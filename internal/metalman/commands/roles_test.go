// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

func TestMetalmanRoleComponentsAreIsolated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		role metalmanRole
		want roleComponents
	}{
		{
			name: "controller",
			role: metalmanRoleController,
			want: roleComponents{
				leaderElection: true,
				ociReconciler:  true,
				redfish:        true,
				machineOps:     true,
				sessionManager: true,
			},
		},
		{
			name: "server",
			role: metalmanRoleServer,
			want: roleComponents{
				ociReconciler: true,
				http:          true,
				attestation:   true,
				statusUpdates: true,
				sessionHTTP:   true,
			},
		},
		{
			name: "edge",
			role: metalmanRoleEdge,
			want: roleComponents{
				http: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := componentsForRole(tt.role); got != tt.want {
				t.Fatalf("componentsForRole(%q) = %#v, want %#v", tt.role, got, tt.want)
			}
		})
	}
}

func TestMetalmanRoleCommands(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		cmd  func() *cobra.Command
	}{
		{name: "controller", cmd: ControllerCmd},
		{name: "server", cmd: ServerCmd},
		{name: "edge", cmd: EdgeCmd},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.cmd().Name(); got != tt.name {
				t.Fatalf("command name = %q, want %q", got, tt.name)
			}
		})
	}
}

func TestEdgeCommandRequiresOnlyBackendConnection(t *testing.T) {
	t.Parallel()

	cmd := EdgeCmd()
	for _, name := range []string{"backend-url", "bind-address", "http-port", "tls-cert-file", "tls-key-file", "endpoint", "edge-token-file", "dhcp-enabled", "dhcp-interface", "dhcp-server-ip", "dhcp-port", "tftp-enabled", "tftp-bind-address", "tftp-port"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("edge command has no --%s flag", name)
		}
	}

	for _, name := range []string{"site", "cache-dir", "leader-elect-lease-duration"} {
		if cmd.Flags().Lookup(name) != nil {
			t.Errorf("edge command unexpectedly has controller flag --%s", name)
		}
	}
}

func TestEdgeTLSRequiresCertificateAndKeyTogether(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		certFile string
		keyFile  string
		wantErr  bool
	}{
		{name: "plaintext"},
		{name: "TLS", certFile: "tls.crt", keyFile: "tls.key"},
		{name: "certificate only", certFile: "tls.crt", wantErr: true},
		{name: "key only", keyFile: "tls.key", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateEdgeTLSFiles(tt.certFile, tt.keyFile)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateEdgeTLSFiles(%q, %q) error = %v, wantErr %v", tt.certFile, tt.keyFile, err, tt.wantErr)
			}
		})
	}
}

func TestEdgeProxyPreservesSessionPathAndRange(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/netboot/capability/artifact/disk.img.gz" {
			t.Errorf("backend path = %q", r.URL.Path)
		}

		if got := r.Header.Get("Range"); got != "bytes=4096-8191" {
			t.Errorf("backend Range = %q", got)
		}

		w.Header().Set("Content-Range", "bytes 4096-8191/16384")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("range"))
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/netboot/capability/artifact/disk.img.gz", nil)
	request.Header.Set("Range", "bytes=4096-8191")

	response := httptest.NewRecorder()

	newEdgeProxy(backendURL).ServeHTTP(response, request)

	if response.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusPartialContent)
	}

	if got := response.Header().Get("Content-Range"); got != "bytes 4096-8191/16384" {
		t.Errorf("Content-Range = %q", got)
	}

	body, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}

	if got := string(body); got != "range" {
		t.Errorf("body = %q", got)
	}
}

func TestEdgeProxyResumesTruncatedArtifactFromBackendRange(t *testing.T) {
	t.Parallel()

	const artifact = "immutable-artifact"

	var requests atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			if got := r.Header.Get("Range"); got != "" {
				t.Errorf("initial Range = %q, want empty", got)
			}

			w.Header().Set("Content-Length", "18")
			_, _ = io.WriteString(w, artifact[:9])
		case 2:
			if got := r.Header.Get("Range"); got != "bytes=9-17" {
				t.Errorf("resume Range = %q, want %q", got, "bytes=9-17")
			}

			w.Header().Set("Content-Length", "9")
			w.Header().Set("Content-Range", "bytes 9-17/18")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, artifact[9:])
		default:
			t.Errorf("unexpected backend request %d", requests.Load())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	edge := httptest.NewServer(newEdgeProxy(backendURL))
	defer edge.Close()

	response, err := http.Get(edge.URL + "/v1/netboot/sessions/session/capability/artifacts/disk.img.gz") //nolint:noctx // Test request.
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck // Test cleanup.

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	if got := string(body); got != artifact {
		t.Errorf("body = %q, want %q", got, artifact)
	}

	if got := requests.Load(); got != 2 {
		t.Errorf("backend requests = %d, want 2", got)
	}
}

func TestEdgeProxyRetriesFailedArtifactResumeRequest(t *testing.T) {
	t.Parallel()

	const artifact = "immutable-artifact"

	var requests atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				t.Errorf("resume Range = %q, want %q", got, "bytes=9-17")
			}

			w.Header().Set("Content-Length", "9")
			w.Header().Set("Content-Range", "bytes 9-17/18")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, artifact[9:])
		default:
			t.Errorf("unexpected backend request %d", requests.Load())
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	edge := httptest.NewServer(newEdgeProxy(backendURL))
	defer edge.Close()

	response, err := http.Get(edge.URL + "/v1/netboot/sessions/session/capability/artifacts/disk.img.gz") //nolint:noctx // Test request.
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck // Test cleanup.

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	if got := string(body); got != artifact {
		t.Errorf("body = %q, want %q", got, artifact)
	}

	if got := requests.Load(); got != 3 {
		t.Errorf("backend requests = %d, want 3", got)
	}
}
