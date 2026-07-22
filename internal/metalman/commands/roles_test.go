// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	for _, name := range []string{"backend-url", "bind-address", "http-port"} {
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
