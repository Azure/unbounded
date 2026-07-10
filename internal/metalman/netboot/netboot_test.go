// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/indexing"
	"github.com/Azure/unbounded/internal/provision"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	return s
}

// populateOCICache creates a fake OCI cache directory structure for testing.
// Files are placed under {cacheDir}/oci/{digest}/amd64/disk/.
func populateOCICache(cacheDir, digest string, files map[string][]byte) error {
	return populateOCICacheForArchitecture(cacheDir, digest, v1alpha3.DefaultPXEArchitecture, files)
}

func populateOCICacheForArchitecture(cacheDir, digest, architecture string, files map[string][]byte) error {
	safe := fmt.Sprintf("sha256_%s", digest)
	diskDir := filepath.Join(cacheDir, "oci", safe, architecture, "disk")

	for path, content := range files {
		fullPath := filepath.Join(diskDir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return err
		}

		if err := os.WriteFile(fullPath, content, 0o644); err != nil {
			return err
		}
	}

	return nil
}

// setupOCICache creates an OCICache populated with test files.
func setupOCICache(t *testing.T, imageRef, digest string, files map[string][]byte) *OCICache {
	t.Helper()

	cacheDir := t.TempDir()
	cache := NewOCICache(cacheDir)

	if err := populateOCICache(cacheDir, digest, files); err != nil {
		t.Fatal(err)
	}

	cache.SetDigest(imageRef, "sha256:"+digest)

	return cache
}

func TestHTTPServer_ServeFiles(t *testing.T) {
	vmlinuzData := []byte("test-vmlinuz-binary-data")

	cache := setupOCICache(t, "ghcr.io/test/image:v1", "abc123", map[string][]byte{
		"vmlinuz": vmlinuzData,
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-serve"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:01", IPv4: "10.0.1.50", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:             cache,
			Reader:            fc,
			DefaultNetbootRef: "ghcr.io/test/image:v1",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Test healthz
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status: got %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != "ok" {
		t.Errorf("healthz body: got %q, want %q", body, "ok")
	}

	// Test serving cached file (with source IP identification)
	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set("X-Forwarded-For", "10.0.1.50")

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET vmlinuz: %v", err)
	}

	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("vmlinuz status: got %d, want 200", resp.StatusCode)
	}

	if string(body) != string(vmlinuzData) {
		t.Errorf("vmlinuz body mismatch: got %q", body)
	}

	// Test 404 for unknown file
	req, _ = http.NewRequest("GET", ts.URL+"/nonexistent/foo", nil)
	req.Header.Set("X-Forwarded-For", "10.0.1.50")

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET nonexistent: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("nonexistent status: got %d, want 404", resp.StatusCode)
	}

	// Test 404 for unknown source IP
	resp, err = http.Get(ts.URL + "/vmlinuz")
	if err != nil {
		t.Fatalf("GET vmlinuz (unknown IP): %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown IP status: got %d, want 404", resp.StatusCode)
	}
}

func TestHTTPServer_HTTPBootLoaderRequiresActiveInstallOperation(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "httpboot123", map[string][]byte{
		"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\nhttpBootPath: shimx64.efi\n"),
		"shimx64.efi":   []byte("shim"),
		"vmlinuz":       []byte("kernel"),
	})

	tests := []struct {
		name          string
		path          string
		installActive bool
		wantStatus    int
		wantBody      []byte
		wantBootEvent []string
	}{
		{
			name:          "active install serves bootloader",
			path:          "shimx64.efi",
			installActive: true,
			wantStatus:    http.StatusOK,
			wantBody:      []byte("shim"),
			wantBootEvent: []string{"node-http:shimx64.efi"},
		},
		{
			name:       "no active install hides bootloader",
			path:       "shimx64.efi",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "no active install serves follow-on artifacts",
			path:       "vmlinuz",
			wantStatus: http.StatusOK,
			wantBody:   []byte("kernel"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-http"},
				Spec: v1alpha3.MachineSpec{
					PXE: &v1alpha3.PXESpec{
						Image:        "ghcr.io/test/image:v1",
						BootProtocol: v1alpha3.PXEBootProtocolHTTP,
						DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:10", IPv4: "10.0.1.60", SubnetMask: "255.255.255.0"}},
					},
				},
			}

			objects := []client.Object{node}
			if tt.installActive {
				objects = append(objects, &v1alpha3.MachineOperation{
					ObjectMeta: metav1.ObjectMeta{Name: "replace-node-http"},
					Spec: v1alpha3.MachineOperationSpec{
						OperationKind: v1alpha3.OperationHostReplace,
						MachineRef:    node.Name,
					},
					Status: v1alpha3.MachineOperationStatus{
						Phase: v1alpha3.OperationPhaseInProgress,
						Targets: []v1alpha3.MachineOperationTargetStatus{{
							MachineRef: node.Name,
							Phase:      v1alpha3.OperationPhaseInProgress,
							Stage:      v1alpha3.OperationStageWaitingRepave,
						}},
					},
				})
			}

			scheme := newScheme(t)
			fc := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
				Build()
			recorder := &recordingStatusRecorder{}
			srv := &HTTPServer{
				FileResolver: FileResolver{
					Cache:  cache,
					Reader: fc,
				},
				StatusRecorder: recorder,
			}

			mux := http.NewServeMux()
			mux.HandleFunc("GET /", srv.handleFile)

			ts := httptest.NewServer(mux)
			defer ts.Close()

			req, _ := http.NewRequest("GET", ts.URL+"/"+tt.path, nil)
			req.Header.Set("X-Forwarded-For", "10.0.1.60")

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)

			require.Equal(t, tt.wantStatus, resp.StatusCode)

			if tt.wantBody != nil {
				require.Equal(t, tt.wantBody, body)
			}

			require.Equal(t, tt.wantBootEvent, recorder.bootLoaderEvents())
		})
	}
}

func TestHTTPServer_MissingOptionalShimRevocationsFilesHTTPBoot(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "revocations123", map[string][]byte{
		"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\nhttpBootPath: shimx64.efi\n"),
		"shimx64.efi":   []byte("shim"),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-revocations"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:        "ghcr.io/test/image:v1",
				BootProtocol: v1alpha3.PXEBootProtocolHTTP,
				DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:11", IPv4: "10.0.1.61", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()
	srv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:  cache,
			Reader: fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	tests := []struct {
		path       string
		wantStatus int
		wantBody   []byte
	}{
		{path: "revocations.efi", wantStatus: http.StatusOK, wantBody: []byte(shimRevocationsNotPresentBody)},
		{path: "revocations_sbat.efi", wantStatus: http.StatusOK, wantBody: []byte(shimRevocationsNotPresentBody)},
		{path: "revocations_sku.efi", wantStatus: http.StatusOK, wantBody: []byte(shimRevocationsNotPresentBody)},
		{path: "nested/revocations.efi", wantStatus: http.StatusOK, wantBody: []byte(shimRevocationsNotPresentBody)},
		{path: "missing.efi", wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req, _ := http.NewRequest("GET", ts.URL+"/"+tt.path, nil)
			req.Header.Set("X-Forwarded-For", "10.0.1.61")

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)

			require.Equal(t, tt.wantStatus, resp.StatusCode)

			if tt.wantBody != nil {
				require.Equal(t, tt.wantBody, body)
				require.Equal(t, fmt.Sprint(len(tt.wantBody)), resp.Header.Get("Content-Length"))
			}
		})
	}
}

func TestHTTPServer_MissingShimRevocationsFilePXEStill404(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "pxerevocations123", map[string][]byte{
		"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\n"),
		"shimx64.efi":   []byte("shim"),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-pxe-revocations"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:12", IPv4: "10.0.1.62", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()
	srv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:  cache,
			Reader: fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/revocations.efi", nil)
	req.Header.Set("X-Forwarded-For", "10.0.1.62")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHTTPServer_TemplateRendered(t *testing.T) {
	bootTemplate := `set default=0
menuentry "Install" {
  linux /vmlinuz hostname={{ .Machine.Name }} ip={{ (index .Machine.Spec.PXE.DHCPLeases 0).IPv4 }}
}`

	cache := setupOCICache(t, "ghcr.io/test/image:v1", "tmpl123", map[string][]byte{
		"grub/grub.cfg.tmpl": []byte(bootTemplate),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f0", IPv4: "10.0.1.10", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:             cache,
			Reader:            fc,
			Cluster:           &StaticClusterInfo{Info: ClusterInfo{ApiserverURL: "https://k8s.example.com"}},
			ServeURL:          "http://10.0.1.1:8080",
			DefaultNetbootRef: "ghcr.io/test/image:v1",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/grub/grub.cfg", nil)
	req.Header.Set("X-Forwarded-For", "10.0.1.10")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /grub/grub.cfg: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grub.cfg status: got %d, want 200, body: %s", resp.StatusCode, body)
	}

	bodyStr := string(body)
	if !strings.Contains(bodyStr, "hostname=node-01") {
		t.Errorf("rendered config should contain hostname=node-01, got:\n%s", bodyStr)
	}

	if !strings.Contains(bodyStr, "ip=10.0.1.10") {
		t.Errorf("rendered config should contain ip=10.0.1.10, got:\n%s", bodyStr)
	}
}

func TestHTTPServer_TemplateVerbatim(t *testing.T) {
	staticConfig := "network:\n  version: 2\n  ethernets:\n    eth0:\n      dhcp4: false\n"

	// Static file (no .tmpl suffix) served verbatim from disk
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "verb123", map[string][]byte{
		"cloud-init/network-config": []byte(staticConfig),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-verbatim"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:02", IPv4: "10.0.1.51", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:             cache,
			Reader:            fc,
			DefaultNetbootRef: "ghcr.io/test/image:v1",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/cloud-init/network-config", nil)
	req.Header.Set("X-Forwarded-For", "10.0.1.51")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET config: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config status: got %d, want 200", resp.StatusCode)
	}

	if string(body) != staticConfig {
		t.Errorf("config body mismatch: got %q, want %q", body, staticConfig)
	}
}

func TestHTTPServer_StaticFile(t *testing.T) {
	staticContent := "autoinstall:\n  version: 1\n  identity:\n    hostname: server\n"

	cache := setupOCICache(t, "ghcr.io/test/image:v1", "static123", map[string][]byte{
		"cloud-init/network-config": []byte(staticContent),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-static"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:03", IPv4: "10.0.1.52", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:             cache,
			Reader:            fc,
			DefaultNetbootRef: "ghcr.io/test/image:v1",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/cloud-init/network-config", nil)
	req.Header.Set("X-Forwarded-For", "10.0.1.52")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET config: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("config status: got %d, want 200", resp.StatusCode)
	}

	if string(body) != staticContent {
		t.Errorf("static body mismatch: got %q, want %q", body, staticContent)
	}
}

func TestHTTPServer_UnknownSourceIP(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "unkn123", map[string][]byte{
		"vmlinuz": []byte("some-binary-data"),
	})

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:  cache,
			Reader: fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// No node registered -- requests from any IP should get 404
	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set("X-Forwarded-For", "10.99.99.99")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown source IP, got %d", resp.StatusCode)
	}
}

func TestTFTPServerRecordsOnlyInitialBootLoader(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "boot123", map[string][]byte{
		"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\n"),
	})
	recorder := &recordingTFTPStatusRecorder{}
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{},
		},
	}
	server := &TFTPServer{
		FileResolver:   FileResolver{Cache: cache},
		StatusRecorder: recorder,
	}

	server.recordBootLoaderDownloaded(t.Context(), testLogger(), node, "ghcr.io/test/image:v1", "grubx64.efi")
	require.Empty(t, recorder.calls)

	server.recordBootLoaderDownloaded(t.Context(), testLogger(), node, "ghcr.io/test/image:v1", "shimx64.efi")
	require.Equal(t, []string{"machine-1:shimx64.efi"}, recorder.calls)
}

func TestTFTPServerRecordsInitialBootLoaderForTargetArchitecture(t *testing.T) {
	cacheDir := t.TempDir()
	cache := NewOCICache(cacheDir)

	require.NoError(t, populateOCICacheForArchitecture(cacheDir, "amd64digest", v1alpha3.PXEArchitectureAMD64, map[string][]byte{
		"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\n"),
	}))
	require.NoError(t, populateOCICacheForArchitecture(cacheDir, "arm64digest", v1alpha3.PXEArchitectureARM64, map[string][]byte{
		"metadata.yaml": []byte("dhcpBootImageName: shimaa64.efi\n"),
	}))
	cache.SetDigestForArchitecture("ghcr.io/test/image:v1", v1alpha3.PXEArchitectureAMD64, "sha256:amd64digest")
	cache.SetDigestForArchitecture("ghcr.io/test/image:v1", v1alpha3.PXEArchitectureARM64, "sha256:arm64digest")

	recorder := &recordingTFTPStatusRecorder{}
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-arm"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{Architecture: v1alpha3.PXEArchitectureARM64},
		},
	}
	server := &TFTPServer{
		FileResolver:   FileResolver{Cache: cache},
		StatusRecorder: recorder,
	}

	server.recordBootLoaderDownloaded(t.Context(), testLogger(), node, "ghcr.io/test/image:v1", "shimx64.efi")
	require.Empty(t, recorder.calls)

	server.recordBootLoaderDownloaded(t.Context(), testLogger(), node, "ghcr.io/test/image:v1", "shimaa64.efi")
	require.Equal(t, []string{"machine-arm:shimaa64.efi"}, recorder.calls)
}

func TestHTTPServerDoesNotRecordBootImageWriteOnFileDownload(t *testing.T) {
	diskImageData := []byte("test-disk-image")
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "disk123", map[string][]byte{
		"disk.img.gz": diskImageData,
		"vmlinuz":     []byte("kernel"),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-serve"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:01", IPv4: "10.0.1.50", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()
	recorder := &recordingStatusRecorder{}
	srv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:  cache,
			Reader: fc,
		},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set("X-Forwarded-For", "10.0.1.50")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, recorder.bootImageWrittenEvents())

	req, _ = http.NewRequest("GET", ts.URL+"/disk.img.gz", nil)
	req.Header.Set("X-Forwarded-For", "10.0.1.50")

	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, diskImageData, body)
	require.Empty(t, recorder.bootImageWrittenEvents())

	req, _ = http.NewRequest("GET", ts.URL+"/missing", nil)
	req.Header.Set("X-Forwarded-For", "10.0.1.50")

	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Empty(t, recorder.bootImageWrittenEvents())
}

func TestHTTPServer_UsesMachineImageForBootFilesWhenNetbootImageUnset(t *testing.T) {
	vmlinuzData := []byte("kernel")
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "fallback123", map[string][]byte{
		"disk.img.gz": []byte("disk"),
		"vmlinuz":     vmlinuzData,
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-serve"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:01", IPv4: "10.0.1.50", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()
	srv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:  cache,
			Reader: fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set("X-Forwarded-For", "10.0.1.50")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /vmlinuz: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vmlinuz status: got %d, want 200", resp.StatusCode)
	}

	if string(body) != string(vmlinuzData) {
		t.Errorf("vmlinuz body: got %q, want %q", body, vmlinuzData)
	}
}

type recordingTFTPStatusRecorder struct {
	calls []string
}

func (r *recordingTFTPStatusRecorder) RecordBootLoaderDownloaded(_ context.Context, machineName, filename string) error {
	r.calls = append(r.calls, fmt.Sprintf("%s:%s", machineName, filename))

	return nil
}

type recordedMachineCondition struct {
	machineName string
	condition   metav1.Condition
}

type recordedPXEDisabled struct {
	machineName string
	imageName   string
}

type recordingStatusRecorder struct {
	mu               sync.Mutex
	err              error
	bootLoader       []string
	bootImageWritten []string
	cloudInitDone    []string
	conditions       []recordedMachineCondition
	disabled         []recordedPXEDisabled
}

func (r *recordingStatusRecorder) RecordBootLoaderDownloaded(_ context.Context, machineName, filename string) error {
	if r.err != nil {
		return r.err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.bootLoader = append(r.bootLoader, fmt.Sprintf("%s:%s", machineName, filename))

	return nil
}

func (r *recordingStatusRecorder) RecordBootImageWritten(_ context.Context, machineName string) error {
	if r.err != nil {
		return r.err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.bootImageWritten = append(r.bootImageWritten, machineName)

	return nil
}

func (r *recordingStatusRecorder) RecordCloudInitDone(_ context.Context, machineName string) error {
	if r.err != nil {
		return r.err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.cloudInitDone = append(r.cloudInitDone, machineName)

	return nil
}

func (r *recordingStatusRecorder) RecordOperationCondition(_ context.Context, machineName string, condition metav1.Condition) error {
	if r.err != nil {
		return r.err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.conditions = append(r.conditions, recordedMachineCondition{machineName: machineName, condition: condition})

	return nil
}

func (r *recordingStatusRecorder) RecordPXEDisabled(_ context.Context, machineName, imageName string) error {
	if r.err != nil {
		return r.err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.disabled = append(r.disabled, recordedPXEDisabled{machineName: machineName, imageName: imageName})

	return nil
}

func (r *recordingStatusRecorder) bootImageWrittenEvents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.bootImageWritten...)
}

func (r *recordingStatusRecorder) bootLoaderEvents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.bootLoader...)
}

func (r *recordingStatusRecorder) cloudInitDoneEvents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.cloudInitDone...)
}

func (r *recordingStatusRecorder) conditionEvents() []recordedMachineCondition {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]recordedMachineCondition(nil), r.conditions...)
}

func (r *recordingStatusRecorder) pxeDisabledEvents() []recordedPXEDisabled {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]recordedPXEDisabled(nil), r.disabled...)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestTemplateRendering(t *testing.T) {
	tmpl := `Node: {{ .Machine.Name }}, API: {{ .ApiserverURL }}, Serve: {{ .ServeURL }}`

	data := templateData{
		Machine: &v1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		},
		ApiserverURL: "https://k8s.example.com",
		ServeURL:     "http://10.0.1.1:8080",
	}

	result, err := renderTemplate(tmpl, data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}

	expected := "Node: test-node, API: https://k8s.example.com, Serve: http://10.0.1.1:8080"
	if string(result) != expected {
		t.Errorf("template result: got %q, want %q", result, expected)
	}
}

func TestTemplateRendering_AgentConfigJSONSet(t *testing.T) {
	tmpl := `Config: {{ .AgentConfigJSON }}`

	data := templateData{
		Machine: &v1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		},
		AgentConfigJSON: `{"MachineName":"test-node"}`,
	}

	result, err := renderTemplate(tmpl, data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}

	expected := `Config: {"MachineName":"test-node"}`
	if string(result) != expected {
		t.Errorf("template result: got %q, want %q", result, expected)
	}
}

func TestTemplateRendering_AgentConfigJSONUnset(t *testing.T) {
	tmpl := `before{{ if .AgentConfigJSON }}config{{ end }}after`

	data := templateData{
		Machine: &v1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		},
	}

	result, err := renderTemplate(tmpl, data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}

	if strings.Contains(string(result), "config") {
		t.Errorf("expected no config in output when AgentConfigJSON is empty, got %q", result)
	}

	expected := "beforeafter"
	if string(result) != expected {
		t.Errorf("template result: got %q, want %q", result, expected)
	}
}

func TestGrubTemplate_NoInstallRequested(t *testing.T) {
	grubTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "grub.cfg.tmpl"))
	if err != nil {
		t.Fatalf("reading grub.cfg.tmpl: %v", err)
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-no-operations", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				TargetDisk: "/dev/disk/by-id/test-os-disk",
				DHCPLeases: []v1alpha3.DHCPLease{{
					IPv4:       "10.0.1.20",
					MAC:        "aa:bb:cc:dd:ee:20",
					Gateway:    "10.0.1.1",
					SubnetMask: "255.255.255.0",
				}},
			},
		},
	}

	data := newTemplateData(
		node,
		ClusterInfo{ApiserverURL: "https://k8s.example.com"},
		"http://10.0.1.1:8080",
		"",
		"",
		false,
	)

	result, err := renderTemplate(string(grubTmpl), data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}

	body := string(result)
	for _, want := range []string{
		"No pending repave",
		"unbounded.image_url=http://10.0.1.1:8080/disk.img.gz",
		"unbounded.node_name=node-no-operations",
		"unbounded.boot_mac=aa:bb:cc:dd:ee:20",
		"unbounded.disk=/dev/disk/by-id/test-os-disk",
		"ip=10.0.1.20::10.0.1.1:255.255.255.0:::none",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in rendered grub.cfg, got:\n%s", want, body)
		}
	}

	require.NotContains(t, body, "eth0")
}

func TestGrubTemplate_SelectsBootLeaseByRequestIP(t *testing.T) {
	grubTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "grub.cfg.tmpl"))
	require.NoError(t, err)

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-multi-lease", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				DHCPLeases: []v1alpha3.DHCPLease{
					{IPv4: "10.0.1.20", MAC: "aa:bb:cc:dd:ee:20", Gateway: "10.0.1.1", SubnetMask: "255.255.255.0"},
					{IPv4: "10.0.1.21", MAC: "aa:bb:cc:dd:ee:21", Gateway: "10.0.1.1", SubnetMask: "255.255.255.0"},
				},
			},
		},
	}

	data := newTemplateData(
		node,
		ClusterInfo{ApiserverURL: "https://k8s.example.com"},
		"http://10.0.1.1:8080",
		"",
		"10.0.1.21",
		true,
	)

	result, err := renderTemplate(string(grubTmpl), data)
	require.NoError(t, err)

	body := string(result)
	require.Contains(t, body, "unbounded.boot_mac=aa:bb:cc:dd:ee:21")
	require.Contains(t, body, "ip=10.0.1.21::10.0.1.1:255.255.255.0:::none")
	require.NotContains(t, body, "unbounded.boot_mac=aa:bb:cc:dd:ee:20")
	require.NotContains(t, body, "unbounded.disk=")
}

func TestInstallRequestedStopsAfterBootImageWritten(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-install", Namespace: "default"},
	}
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "replace-node-install", Namespace: "default"},
		Spec: v1alpha3.MachineOperationSpec{
			OperationKind: v1alpha3.OperationHostReplace,
			MachineRef:    node.Name,
		},
		Status: v1alpha3.MachineOperationStatus{
			Phase: v1alpha3.OperationPhaseInProgress,
			Targets: []v1alpha3.MachineOperationTargetStatus{{
				MachineRef: node.Name,
				Phase:      v1alpha3.OperationPhaseInProgress,
				Stage:      v1alpha3.OperationStageWaitingCloudInit,
			}},
			Conditions: []metav1.Condition{{
				Type:   v1alpha3.MachineOperationConditionBootImageWritten,
				Status: metav1.ConditionTrue,
				Reason: "Succeeded",
			}},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(node, op).
		Build()

	resolver := FileResolver{Reader: fc}
	installRequested, err := resolver.installRequested(t.Context(), node)
	require.NoError(t, err)
	require.False(t, installRequested)
}

func TestInstallRequestedBeforeBootImageWritten(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-install", Namespace: "default"},
	}
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "replace-node-install", Namespace: "default"},
		Spec: v1alpha3.MachineOperationSpec{
			OperationKind: v1alpha3.OperationHostReplace,
			MachineRef:    node.Name,
		},
		Status: v1alpha3.MachineOperationStatus{
			Phase: v1alpha3.OperationPhaseInProgress,
			Targets: []v1alpha3.MachineOperationTargetStatus{{
				MachineRef: node.Name,
				Phase:      v1alpha3.OperationPhaseInProgress,
				Stage:      v1alpha3.OperationStageWaitingRepave,
			}},
			Conditions: []metav1.Condition{{
				Type:   v1alpha3.MachineOperationConditionBootImageWritten,
				Status: metav1.ConditionUnknown,
				Reason: "Pending",
			}},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(node, op).
		Build()

	resolver := FileResolver{Reader: fc}
	installRequested, err := resolver.installRequested(t.Context(), node)
	require.NoError(t, err)
	require.True(t, installRequested)
}

func TestVendorDataTemplate_WithAgentImage(t *testing.T) {
	vendorDataTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "vendor-data.tmpl"))
	if err != nil {
		t.Fatalf("reading vendor-data.tmpl: %v", err)
	}

	agentConfigJSON := `{
    "MachineName": "agent-img-node",
    "Cluster": {
      "CaCertBase64": "",
      "ClusterDNS": "10.96.0.10",
      "Version": "v1.30.0"
    },
    "Kubelet": {
      "ApiServer": "https://k8s.example.com",
      "Labels": {},
      "RegisterWithTaints": null
    },
    "OCIImage": "ghcr.io/org/rootfs:v1",
    "Attest": {
      "URL": "http://10.0.1.1:8080"
    }
  }`

	data := templateData{
		Machine: &v1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "agent-img-node"},
		},
		ApiserverURL:    "https://k8s.example.com",
		ServeURL:        "http://10.0.1.1:8080",
		AgentConfigJSON: agentConfigJSON,
		InstallScript:   provision.UnboundedAgentInstallScript(),
	}

	result, err := renderTemplate(string(vendorDataTmpl), data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}

	body := string(result)
	if !strings.Contains(body, `"OCIImage": "ghcr.io/org/rootfs:v1"`) {
		t.Errorf("expected OCIImage in rendered vendor-data, got:\n%s", body)
	}

	if !strings.Contains(body, `"MachineName": "agent-img-node"`) {
		t.Errorf("expected MachineName in rendered vendor-data, got:\n%s", body)
	}

	for _, want := range []string{
		"/usr/local/bin/unbounded-agent-install.sh",
		"/latest/download/unbounded-agent-linux-",
		"export UNBOUNDED_AGENT_CONFIG_FILE=/etc/unbounded/agent/config.json",
		"install_log=/var/log/unbounded-agent-install.log",
		"trap report_unbounded_failure ERR",
		"last 120 lines of ${install_log}",
		`curl -fsS -m 10 -X POST -H "Content-Type: text/plain" --data-binary @- "http://10.0.1.1:8080/unbounded-agent/install-log"`,
		`bash /usr/local/bin/unbounded-agent-install.sh 2>&1 | tee "$install_log"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in rendered vendor-data, got:\n%s", want, body)
		}
	}

	if strings.Contains(body, "python3") {
		t.Errorf("expected Bash-only failure reporting, got:\n%s", body)
	}

	if strings.Contains(body, "{{ .ServeURL }}/unbounded-agent") {
		t.Errorf("expected no baked netboot agent URL in rendered vendor-data, got:\n%s", body)
	}

	if !strings.Contains(body, "/cloudinit/log") {
		t.Errorf("expected webhook reporting endpoint in rendered vendor-data, got:\n%s", body)
	}

	// Hardening drop-ins: the vendor-data must lay down apt and needrestart
	// drop-ins that prevent unattended-upgrades from restarting
	// systemd-machined out from under the running nspawn container. See
	// cmd/agent/internal/phases/host/apt.go for the matching agent-side
	// task that also writes these files.
	for _, want := range []string{
		"/etc/apt/apt.conf.d/99-unbounded-no-restart-systemd",
		"Unattended-Upgrade::Package-Blacklist",
		`"libcap2";`,
		`"systemd-container";`,
		"/etc/needrestart/conf.d/99-unbounded.conf",
		"$nrconf{restart} = 'l';",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in rendered vendor-data, got:\n%s", want, body)
		}
	}
}

func TestVendorDataTemplate_WithoutAgentImage(t *testing.T) {
	vendorDataTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "vendor-data.tmpl"))
	if err != nil {
		t.Fatalf("reading vendor-data.tmpl: %v", err)
	}

	agentConfigJSON := `{
    "MachineName": "no-agent-node",
    "Cluster": {
      "CaCertBase64": "",
      "ClusterDNS": "10.96.0.10",
      "Version": "v1.30.0"
    },
    "Kubelet": {
      "ApiServer": "https://k8s.example.com",
      "Labels": {},
      "RegisterWithTaints": null
    },
    "Attest": {
      "URL": "http://10.0.1.1:8080"
    }
  }`

	data := templateData{
		Machine: &v1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "no-agent-node"},
		},
		ApiserverURL:    "https://k8s.example.com",
		ServeURL:        "http://10.0.1.1:8080",
		AgentConfigJSON: agentConfigJSON,
		InstallScript:   provision.UnboundedAgentInstallScript(),
	}

	result, err := renderTemplate(string(vendorDataTmpl), data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}

	body := string(result)
	if strings.Contains(body, "OCIImage") {
		t.Errorf("expected no OCIImage in rendered vendor-data when AgentImage is empty, got:\n%s", body)
	}

	if !strings.Contains(body, `"MachineName": "no-agent-node"`) {
		t.Errorf("expected MachineName in rendered vendor-data, got:\n%s", body)
	}
}

func TestVendorDataTemplate_AgentInstallEnv(t *testing.T) {
	vendorDataTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "vendor-data.tmpl"))
	if err != nil {
		t.Fatalf("reading vendor-data.tmpl: %v", err)
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-url-node"},
		Spec: v1alpha3.MachineSpec{
			Agent: &v1alpha3.AgentSpec{
				Version: "v1.2.3",
				URL:     "https://downloads.example.com/unbounded-agent-linux-amd64.tar.gz",
			},
		},
	}

	data := templateData{
		Machine:         node,
		ApiserverURL:    "https://k8s.example.com",
		ServeURL:        "http://10.0.1.1:8080",
		AgentConfigJSON: `{"MachineName":"agent-url-node"}`,
		InstallScript:   provision.UnboundedAgentInstallScript(),
		InstallEnv:      provision.AgentInstallEnv(node.Spec.Agent),
	}

	result, err := renderTemplate(string(vendorDataTmpl), data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}

	body := string(result)
	for _, want := range []string{
		"export AGENT_VERSION='v1.2.3'",
		"export AGENT_URL='https://downloads.example.com/unbounded-agent-linux-amd64.tar.gz'",
		"bash /usr/local/bin/unbounded-agent-install.sh",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in rendered vendor-data, got:\n%s", want, body)
		}
	}
}

func TestResolveFileByPath_UserDataFromConfigMap(t *testing.T) {
	customUserData := "#cloud-config\nssh_authorized_keys:\n  - ssh-rsa AAAA...\npackages:\n  - vim\n"

	cache := setupOCICache(t, "ghcr.io/test/image:v1", "cmud123", map[string][]byte{
		"vmlinuz": []byte("kernel"),
	})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-userdata",
			Namespace: "default",
		},
		Data: map[string]string{
			"user-data": customUserData,
		},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-cm-ud"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:40", IPv4: "10.0.8.10", SubnetMask: "255.255.255.0"}},
				CloudInit: &v1alpha3.CloudInitSpec{
					UserDataConfigMapRef: &v1alpha3.ConfigMapKeySelector{
						Name:      "my-userdata",
						Namespace: "default",
						Key:       "user-data",
					},
				},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, cm).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	resolver := FileResolver{
		Cache:  cache,
		Reader: fc,
		Cluster: &StaticClusterInfo{Info: ClusterInfo{
			ApiserverURL: "https://k8s.example.com",
		}},
		ServeURL: "http://10.0.8.1:8080",
	}

	resolved, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, "ghcr.io/test/image:v1")
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if string(resolved.Data) != customUserData {
		t.Errorf("expected ConfigMap user-data, got %q", resolved.Data)
	}
}

func TestResolveFileByPath_UserDataFallsBackToDefault(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "fblud123", map[string][]byte{
		"vmlinuz": []byte("kernel"),
	})

	// Node without cloudInit configured should return the built-in default.
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-no-cm"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:41", IPv4: "10.0.8.11", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	resolver := FileResolver{
		Cache:  cache,
		Reader: fc,
		Cluster: &StaticClusterInfo{Info: ClusterInfo{
			ApiserverURL: "https://k8s.example.com",
		}},
		ServeURL: "http://10.0.8.1:8080",
	}

	resolved, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, "ghcr.io/test/image:v1")
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if resolved.DiskPath != "" {
		t.Errorf("expected no DiskPath for default user-data, got %q", resolved.DiskPath)
	}

	if string(resolved.Data) != defaultUserData {
		t.Errorf("expected default user-data %q, got %q", defaultUserData, resolved.Data)
	}
}

func TestResolveFileByPath_UserDataConfigMapCustomKey(t *testing.T) {
	customUserData := "#cloud-config\npackages:\n  - htop\n"

	cache := setupOCICache(t, "ghcr.io/test/image:v1", "cmkey123", map[string][]byte{
		"vmlinuz": []byte("kernel"),
	})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi-key-cm",
			Namespace: "infra",
		},
		Data: map[string]string{
			"my-custom-key": customUserData,
		},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-custom-key"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:42", IPv4: "10.0.8.12", SubnetMask: "255.255.255.0"}},
				CloudInit: &v1alpha3.CloudInitSpec{
					UserDataConfigMapRef: &v1alpha3.ConfigMapKeySelector{
						Name:      "multi-key-cm",
						Namespace: "infra",
						Key:       "my-custom-key",
					},
				},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, cm).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	resolver := FileResolver{
		Cache:  cache,
		Reader: fc,
		Cluster: &StaticClusterInfo{Info: ClusterInfo{
			ApiserverURL: "https://k8s.example.com",
		}},
		ServeURL: "http://10.0.8.1:8080",
	}

	resolved, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, "ghcr.io/test/image:v1")
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if string(resolved.Data) != customUserData {
		t.Errorf("expected custom-key user-data, got %q", resolved.Data)
	}
}

func TestResolveFileByPath_UserDataConfigMapMissing(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "cmmiss123", map[string][]byte{
		"vmlinuz": []byte("kernel"),
	})

	// ConfigMap doesn't exist, so this should fall back to default cloud-init.
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-cm-missing"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:43", IPv4: "10.0.8.13", SubnetMask: "255.255.255.0"}},
				CloudInit: &v1alpha3.CloudInitSpec{
					UserDataConfigMapRef: &v1alpha3.ConfigMapKeySelector{
						Name:      "nonexistent",
						Namespace: "default",
					},
				},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	resolver := FileResolver{
		Cache:  cache,
		Reader: fc,
	}

	resolved, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, "ghcr.io/test/image:v1")
	if err != nil {
		t.Fatalf("expected fallback to default user-data, got error: %v", err)
	}

	if string(resolved.Data) != defaultUserData {
		t.Errorf("expected default user-data %q, got %q", defaultUserData, resolved.Data)
	}
}

func TestResolveFileByPath_UserDataConfigMapGetError(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "cmerr123", map[string][]byte{
		"vmlinuz": []byte("kernel"),
	})

	// Node references a ConfigMap, but the client returns a non-NotFound error.
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-cm-err"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:45", IPv4: "10.0.8.15", SubnetMask: "255.255.255.0"}},
				CloudInit: &v1alpha3.CloudInitSpec{
					UserDataConfigMapRef: &v1alpha3.ConfigMapKeySelector{
						Name:      "some-cm",
						Namespace: "default",
					},
				},
			},
		},
	}

	scheme := newScheme(t)
	injectedErr := fmt.Errorf("simulated network timeout")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return injectedErr
				}

				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	resolver := FileResolver{
		Cache:  cache,
		Reader: fc,
	}

	_, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, "ghcr.io/test/image:v1")
	if err == nil {
		t.Fatal("expected error for non-NotFound client failure, got nil")
	}

	if !strings.Contains(err.Error(), "simulated network timeout") {
		t.Errorf("expected error to contain injected message, got: %v", err)
	}
}

func TestResolveFileByPath_UserDataConfigMapMissingKey(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "cmnokey123", map[string][]byte{
		"vmlinuz": []byte("kernel"),
	})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wrong-key-cm",
			Namespace: "default",
		},
		Data: map[string]string{
			"other-key": "some data",
		},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-cm-nokey"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:44", IPv4: "10.0.8.14", SubnetMask: "255.255.255.0"}},
				CloudInit: &v1alpha3.CloudInitSpec{
					UserDataConfigMapRef: &v1alpha3.ConfigMapKeySelector{
						Name:      "wrong-key-cm",
						Namespace: "default",
						Key:       "user-data",
					},
				},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, cm).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	resolver := FileResolver{
		Cache:  cache,
		Reader: fc,
	}

	_, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, "ghcr.io/test/image:v1")
	if err == nil {
		t.Fatal("expected error when ConfigMap key is missing")
	}

	if !strings.Contains(err.Error(), "user-data") {
		t.Errorf("expected error to mention missing key, got: %v", err)
	}
}

func TestResolveFileByPath_UserDataFromBinaryData(t *testing.T) {
	binaryUserData := []byte("#cloud-config\npackages:\n  - curl\n")

	cache := setupOCICache(t, "ghcr.io/test/image:v1", "cmbin123", map[string][]byte{
		"vmlinuz": []byte("kernel"),
	})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "binary-ud",
			Namespace: "default",
		},
		BinaryData: map[string][]byte{
			"user-data": binaryUserData,
		},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-bindata"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:60", IPv4: "10.0.10.10", SubnetMask: "255.255.255.0"}},
				CloudInit: &v1alpha3.CloudInitSpec{
					UserDataConfigMapRef: &v1alpha3.ConfigMapKeySelector{
						Name:      "binary-ud",
						Namespace: "default",
					},
				},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, cm).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	resolver := FileResolver{
		Cache:  cache,
		Reader: fc,
	}

	resolved, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, "ghcr.io/test/image:v1")
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if string(resolved.Data) != string(binaryUserData) {
		t.Errorf("expected BinaryData user-data, got %q", resolved.Data)
	}
}

func TestHTTPServer_UserDataConfigMapMissing(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "httpcmmiss123", map[string][]byte{
		"vmlinuz": []byte("kernel"),
	})

	// Node references a ConfigMap that doesn't exist, so this should fall back to default cloud-init.
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-http-cm-miss"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:51", IPv4: "10.0.9.11", SubnetMask: "255.255.255.0"}},
				CloudInit: &v1alpha3.CloudInitSpec{
					UserDataConfigMapRef: &v1alpha3.ConfigMapKeySelector{
						Name:      "nonexistent",
						Namespace: "default",
					},
				},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:             cache,
			Reader:            fc,
			Cluster:           &StaticClusterInfo{Info: ClusterInfo{ApiserverURL: "https://k8s.example.com"}},
			ServeURL:          "http://10.0.9.1:8080",
			DefaultNetbootRef: "ghcr.io/test/image:v1",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/cloud-init/user-data", nil)
	req.Header.Set("X-Forwarded-For", "10.0.9.11")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /cloud-init/user-data: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for missing ConfigMap (fallback to default), got %d", resp.StatusCode)
	}

	if string(body) != defaultUserData {
		t.Errorf("expected default user-data %q, got %q", defaultUserData, body)
	}
}

func TestHTTPServer_UserDataFromConfigMap(t *testing.T) {
	customUserData := "#cloud-config\nssh_authorized_keys:\n  - ssh-rsa AAAA...\n"

	cache := setupOCICache(t, "ghcr.io/test/image:v1", "httpud123", map[string][]byte{
		"vmlinuz": []byte("kernel"),
	})

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ssh-keys",
			Namespace: "default",
		},
		Data: map[string]string{
			"user-data": customUserData,
		},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-http-ud"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:50", IPv4: "10.0.9.10", SubnetMask: "255.255.255.0"}},
				CloudInit: &v1alpha3.CloudInitSpec{
					UserDataConfigMapRef: &v1alpha3.ConfigMapKeySelector{
						Name:      "ssh-keys",
						Namespace: "default",
						Key:       "user-data",
					},
				},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, cm).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:             cache,
			Reader:            fc,
			Cluster:           &StaticClusterInfo{Info: ClusterInfo{ApiserverURL: "https://k8s.example.com"}},
			ServeURL:          "http://10.0.9.1:8080",
			DefaultNetbootRef: "ghcr.io/test/image:v1",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/cloud-init/user-data", nil)
	req.Header.Set("X-Forwarded-For", "10.0.9.10")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /cloud-init/user-data: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("user-data status: got %d, want 200", resp.StatusCode)
	}

	if string(body) != customUserData {
		t.Errorf("expected ConfigMap user-data, got %q", body)
	}
}

func TestResolveFileByPath_AgentConfig(t *testing.T) {
	tmplContent := `{{ .AgentConfigJSON }}`

	cache := setupOCICache(t, "ghcr.io/test/image:v1", "agentimg123", map[string][]byte{
		"config.tmpl": []byte(tmplContent),
	})

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	resolver := FileResolver{
		Cache:  cache,
		Reader: fc,
		Cluster: &StaticClusterInfo{Info: ClusterInfo{
			ApiserverURL: "https://k8s.example.com",
		}},
		ServeURL:          "http://10.0.1.1:8080",
		KubernetesVersion: "1.30.0",
		ClusterDNS:        "10.96.0.10",
	}

	// With agent image set
	nodeWithAgent := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-with-agent"},
		Spec: v1alpha3.MachineSpec{
			Agent: &v1alpha3.AgentSpec{
				Image: "ghcr.io/org/rootfs:v1",
			},
		},
	}

	resolved, err := resolver.ResolveFileByPath(t.Context(), "config", nodeWithAgent, "ghcr.io/test/image:v1")
	if err != nil {
		t.Fatalf("ResolveFileByPath with agent: %v", err)
	}

	if !strings.Contains(string(resolved.Data), "ghcr.io/org/rootfs:v1") {
		t.Errorf("expected agent image in rendered template, got %q", resolved.Data)
	}

	if !strings.Contains(string(resolved.Data), `"MachineName": "node-with-agent"`) {
		t.Errorf("expected MachineName in rendered template, got %q", resolved.Data)
	}

	// Without agent image (spec.agent is nil)
	nodeWithoutAgent := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-without-agent"},
	}

	resolved, err = resolver.ResolveFileByPath(t.Context(), "config", nodeWithoutAgent, "ghcr.io/test/image:v1")
	if err != nil {
		t.Fatalf("ResolveFileByPath without agent: %v", err)
	}

	if strings.Contains(string(resolved.Data), "OCIImage") {
		t.Errorf("expected no OCIImage in rendered template when agent image is empty, got %q", resolved.Data)
	}

	if !strings.Contains(string(resolved.Data), `"MachineName": "node-without-agent"`) {
		t.Errorf("expected MachineName in rendered template, got %q", resolved.Data)
	}
}

func TestHTTPServer_Start_Shutdown(t *testing.T) {
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	port := freePort(t)

	cache := NewOCICache(t.TempDir())

	srv := &HTTPServer{
		BindAddr: "127.0.0.1",
		Port:     port,
		FileResolver: FileResolver{
			Cache:  cache,
			Reader: fc,
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)

	go func() {
		errCh <- srv.Start(ctx)
	}()

	// Wait for server to be ready
	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHTTP(t, addr+"/healthz", 3*time.Second)

	resp, err := http.Get(addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: got %d, want 200", resp.StatusCode)
	}

	cancel()

	if err := <-errCh; err != nil {
		t.Fatalf("server returned error: %v", err)
	}
}

func TestTFTPServer_ResolveFileByPath(t *testing.T) {
	vmlinuzData := []byte("tftp-vmlinuz-data")

	cache := setupOCICache(t, "ghcr.io/test/image:v1", "tftp123", map[string][]byte{
		"vmlinuz": vmlinuzData,
	})

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	srv := &TFTPServer{
		FileResolver: FileResolver{
			Cache:  cache,
			Reader: fc,
		},
	}

	resolved, err := srv.ResolveFileByPath(t.Context(), "vmlinuz", nil, "ghcr.io/test/image:v1")
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if resolved.DiskPath == "" {
		t.Fatal("expected DiskPath to be set for static file")
	}

	data, err := os.ReadFile(resolved.DiskPath)
	if err != nil {
		t.Fatalf("reading resolved disk path: %v", err)
	}

	if string(data) != string(vmlinuzData) {
		t.Errorf("data mismatch: got %q", data)
	}

	// Test not found (wrong image)
	_, err = srv.ResolveFileByPath(t.Context(), "vmlinuz", nil, "ghcr.io/test/nonexistent:v1")
	if err == nil {
		t.Error("expected error for nonexistent image")
	}

	// Test not found (wrong path)
	_, err = srv.ResolveFileByPath(t.Context(), "nonexistent/foo", nil, "ghcr.io/test/image:v1")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestTFTPServer_TemplateVerbatim(t *testing.T) {
	staticData := "static-config-data"

	// When requesting "configs/static", it finds configs/static.tmpl and renders as template
	// Since node is nil, template content is returned verbatim
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "tftptmpl123", map[string][]byte{
		"configs/static.tmpl": []byte(staticData),
	})

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	srv := &TFTPServer{
		FileResolver: FileResolver{
			Cache:  cache,
			Reader: fc,
		},
	}

	resolved, err := srv.ResolveFileByPath(t.Context(), "configs/static", nil, "ghcr.io/test/image:v1")
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if string(resolved.Data) != staticData {
		t.Errorf("data mismatch: got %q, want %q", resolved.Data, staticData)
	}
}

func TestTFTPServer_StaticFile(t *testing.T) {
	staticData := "static-config-no-template"

	cache := setupOCICache(t, "ghcr.io/test/image:v1", "tftpstatic123", map[string][]byte{
		"configs/static": []byte(staticData),
	})

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	srv := &TFTPServer{
		FileResolver: FileResolver{
			Cache:  cache,
			Reader: fc,
		},
	}

	resolved, err := srv.ResolveFileByPath(t.Context(), "configs/static", nil, "ghcr.io/test/image:v1")
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if resolved.DiskPath == "" {
		t.Fatal("expected DiskPath for static file")
	}

	data, err := os.ReadFile(resolved.DiskPath)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}

	if string(data) != staticData {
		t.Errorf("data mismatch: got %q, want %q", data, staticData)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name     string
		fwdFor   string
		remote   string
		expected string
	}{
		{"forwarded", "10.0.1.10", "192.168.1.1:1234", "10.0.1.10"},
		{"forwarded multiple", "10.0.1.10, 192.168.1.1", "192.168.1.1:1234", "10.0.1.10"},
		{"remote only", "", "10.0.1.10:1234", "10.0.1.10"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{
				RemoteAddr: tt.remote,
				Header:     http.Header{},
			}
			if tt.fwdFor != "" {
				r.Header.Set("X-Forwarded-For", tt.fwdFor)
			}

			got := clientIP(r)
			if got != tt.expected {
				t.Errorf("ClientIP: got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestHTTPServer_EndToEnd_MixedSources(t *testing.T) {
	vmlinuzData := []byte("e2e-vmlinuz-binary")
	initrdData := []byte("e2e-initrd-binary")
	bootTemplate := `set root=(tftp)
menuentry "Install {{ .Machine.Name }}" {
  linux /vmlinuz
  initrd /initrd
}`

	cache := setupOCICache(t, "ghcr.io/test/e2e:v1", "e2e123", map[string][]byte{
		"vmlinuz":            vmlinuzData,
		"initrd":             initrdData,
		"grub/grub.cfg.tmpl": []byte(bootTemplate),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/e2e:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:00:11:22", IPv4: "10.0.3.10", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	httpSrv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:             cache,
			Reader:            fc,
			Cluster:           &StaticClusterInfo{Info: ClusterInfo{ApiserverURL: "https://k8s.example.com"}},
			ServeURL:          "http://10.0.3.1:8080",
			DefaultNetbootRef: "ghcr.io/test/e2e:v1",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /", httpSrv.handleFile)

	httpTS := httptest.NewServer(mux)
	defer httpTS.Close()

	// Test vmlinuz via HTTP
	req, _ := http.NewRequest("GET", httpTS.URL+"/vmlinuz", nil)
	req.Header.Set("X-Forwarded-For", "10.0.3.10")
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != string(vmlinuzData) {
		t.Errorf("HTTP vmlinuz mismatch")
	}

	// Test boot config (template rendered)
	req, _ = http.NewRequest("GET", httpTS.URL+"/grub/grub.cfg", nil)
	req.Header.Set("X-Forwarded-For", "10.0.3.10")
	resp, _ = http.DefaultClient.Do(req)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), "Install e2e-node") {
		t.Errorf("grub.cfg should contain node name, got:\n%s", body)
	}

	// Test default user-data (no ConfigMap configured, so metalman returns built-in default)
	req, _ = http.NewRequest("GET", httpTS.URL+"/cloud-init/user-data", nil)
	req.Header.Set("X-Forwarded-For", "10.0.3.10")
	resp, _ = http.DefaultClient.Do(req)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != defaultUserData {
		t.Errorf("default user-data mismatch: got %q, want %q", body, defaultUserData)
	}

	// Test 404 for file not in this image
	req, _ = http.NewRequest("GET", httpTS.URL+"/nonexistent/foo", nil)
	req.Header.Set("X-Forwarded-For", "10.0.3.10")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent, got %d", resp.StatusCode)
	}
}

func TestHTTPServer_RoutesDiskFromMachineImageAndBootFromNetbootImage(t *testing.T) {
	machineData := []byte("machine-disk-data")
	netbootData := []byte("netboot-kernel-data")

	cacheDir := t.TempDir()
	cache := NewOCICache(cacheDir)

	if err := populateOCICache(cacheDir, "machine123", map[string][]byte{"disk.img.gz": machineData}); err != nil {
		t.Fatal(err)
	}

	if err := populateOCICache(cacheDir, "netboot123", map[string][]byte{"vmlinuz": netbootData}); err != nil {
		t.Fatal(err)
	}

	cache.SetDigest("ghcr.io/test/machine:v1", "sha256:machine123")
	cache.SetDigest("ghcr.io/test/netboot:v1", "sha256:netboot123")

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "split-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:        "ghcr.io/test/machine:v1",
				NetbootImage: "ghcr.io/test/netboot:v1",
				DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:80", IPv4: "10.0.30.10", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{FileResolver: FileResolver{Cache: cache, Reader: fc}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, tt := range []struct {
		path string
		want []byte
	}{
		{path: "/disk.img.gz", want: machineData},
		{path: "/vmlinuz", want: netbootData},
	} {
		req, _ := http.NewRequest("GET", ts.URL+tt.path, nil)
		req.Header.Set("X-Forwarded-For", "10.0.30.10")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", tt.path, err)
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status: got %d, want 200", tt.path, resp.StatusCode)
		}

		if string(body) != string(tt.want) {
			t.Errorf("%s body: got %q, want %q", tt.path, body, tt.want)
		}
	}
}

func TestHTTPServer_CrossImageIsolation(t *testing.T) {
	alphaData := []byte("alpha-vmlinuz")
	betaData := []byte("beta-vmlinuz")

	cacheDir := t.TempDir()
	cache := NewOCICache(cacheDir)

	populateOCICache(cacheDir, "alpha111", map[string][]byte{"vmlinuz": alphaData})
	populateOCICache(cacheDir, "beta222", map[string][]byte{"vmlinuz": betaData})

	cache.SetDigest("ghcr.io/test/alpha:v1", "sha256:alpha111")
	cache.SetDigest("ghcr.io/test/beta:v1", "sha256:beta222")

	alphaNode := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:        "ghcr.io/test/alpha:v1",
				NetbootImage: "ghcr.io/test/alpha:v1",
				DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "aa:aa:aa:aa:aa:aa", IPv4: "10.0.10.1", SubnetMask: "255.255.255.0"}},
			},
		},
	}
	betaNode := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "beta-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:        "ghcr.io/test/beta:v1",
				NetbootImage: "ghcr.io/test/beta:v1",
				DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "bb:bb:bb:bb:bb:bb", IPv4: "10.0.10.2", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(alphaNode, betaNode).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			Cache:  cache,
			Reader: fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Alpha node can access its own image's files
	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set("X-Forwarded-For", "10.0.10.1")
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("alpha accessing own image: got %d, want 200", resp.StatusCode)
	}

	if string(body) != string(alphaData) {
		t.Errorf("alpha vmlinuz mismatch: got %q", body)
	}

	// Beta node can access its own image's files
	req, _ = http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set("X-Forwarded-For", "10.0.10.2")
	resp, _ = http.DefaultClient.Do(req)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("beta accessing own image: got %d, want 200", resp.StatusCode)
	}

	if string(body) != string(betaData) {
		t.Errorf("beta vmlinuz mismatch: got %q", body)
	}
}

func TestHTTPServer_UsesMachineArchitectureForSameImageRef(t *testing.T) {
	cacheDir := t.TempDir()
	cache := NewOCICache(cacheDir)

	imageRef := "ghcr.io/test/netboot:v1"
	digest := "multiarch-http"

	if err := populateOCICacheForArchitecture(cacheDir, digest, v1alpha3.PXEArchitectureAMD64, map[string][]byte{
		"vmlinuz": []byte("amd64-vmlinuz"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := populateOCICacheForArchitecture(cacheDir, digest, v1alpha3.PXEArchitectureARM64, map[string][]byte{
		"vmlinuz": []byte("arm64-vmlinuz"),
	}); err != nil {
		t.Fatal(err)
	}

	cache.SetDigestForArchitecture(imageRef, v1alpha3.PXEArchitectureAMD64, "sha256:"+digest)
	cache.SetDigestForArchitecture(imageRef, v1alpha3.PXEArchitectureARM64, "sha256:"+digest)

	amd64Node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "amd64-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:        imageRef,
				NetbootImage: imageRef,
				Architecture: v1alpha3.PXEArchitectureAMD64,
				DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "aa:aa:aa:aa:aa:01", IPv4: "10.0.20.1", SubnetMask: "255.255.255.0"}},
			},
		},
	}
	arm64Node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "arm64-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:        imageRef,
				NetbootImage: imageRef,
				Architecture: v1alpha3.PXEArchitectureARM64,
				DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "aa:aa:aa:aa:aa:02", IPv4: "10.0.20.2", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(amd64Node, arm64Node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{FileResolver: FileResolver{Cache: cache, Reader: fc}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, tt := range []struct {
		ip   string
		want string
	}{
		{ip: "10.0.20.1", want: "amd64-vmlinuz"},
		{ip: "10.0.20.2", want: "arm64-vmlinuz"},
	} {
		req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
		req.Header.Set("X-Forwarded-For", tt.ip)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /vmlinuz for %s: %v", tt.ip, err)
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status: got %d, want 200", tt.ip, resp.StatusCode)
		}

		if string(body) != tt.want {
			t.Errorf("%s body: got %q, want %q", tt.ip, body, tt.want)
		}
	}
}

func TestHTTPServer_503WhenFileNotDownloaded(t *testing.T) {
	// Cache with NO digest set for the image
	cache := NewOCICache(t.TempDir())

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/pending:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:10", IPv4: "10.0.5.10", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{Cache: cache, Reader: fc, DefaultNetbootRef: "ghcr.io/test/pending:v1"},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// File not yet pulled -- should get 503
	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set("X-Forwarded-For", "10.0.5.10")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for not-yet-downloaded file, got %d", resp.StatusCode)
	}

	if ra := resp.Header.Get("Retry-After"); ra != "5" {
		t.Errorf("expected Retry-After: 5, got %q", ra)
	}
}

func TestResolveFile_NotYetDownloaded(t *testing.T) {
	cache := NewOCICache(t.TempDir())

	resolver := FileResolver{Cache: cache, Reader: nil}

	_, err := resolver.ResolveFileByPath(t.Context(), "vmlinuz", nil, "ghcr.io/test/missing:v1")
	if !errors.Is(err, ErrNotYetDownloaded) {
		t.Fatalf("expected ErrNotYetDownloaded, got: %v", err)
	}
}

func TestHTTPServer_DisablePXE(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "pxe-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:20", IPv4: "10.0.6.10", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	cache := NewOCICache(t.TempDir())

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	recorder := &recordingStatusRecorder{}
	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Cache: cache, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /pxe/disable", srv.handleDisablePXE)
	mux.HandleFunc("GET /pxe/disable", srv.handleDisablePXE)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Disable PXE via GET (matches busybox wget behavior in initrd)
	req, _ := http.NewRequest("GET", ts.URL+"/pxe/disable", nil)
	req.Header.Set("X-Forwarded-For", "10.0.6.10")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /pxe/disable: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	require.Equal(t, []string{"pxe-node"}, recorder.bootImageWrittenEvents())
	pxeDisabled := recorder.pxeDisabledEvents()
	require.Equal(t, []recordedPXEDisabled{{machineName: "pxe-node", imageName: "ghcr.io/test/image:v1"}}, pxeDisabled)

	// Second call should be idempotent (still 200)
	req, _ = http.NewRequest("GET", ts.URL+"/pxe/disable", nil)
	req.Header.Set("X-Forwarded-For", "10.0.6.10")

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("second GET /pxe/disable: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("idempotent call: expected 200, got %d", resp.StatusCode)
	}
}

func TestHTTPServer_DisablePXE_RecordFailure(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "pxe-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:21", IPv4: "10.0.6.11", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	cache := NewOCICache(t.TempDir())
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()
	recorder := &recordingStatusRecorder{err: fmt.Errorf("simulated status failure")}
	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Cache: cache, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pxe/disable", srv.handleDisablePXE)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/pxe/disable", nil)
	req.Header.Set("X-Forwarded-For", "10.0.6.11")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /pxe/disable: %v", err)
	}

	resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Empty(t, recorder.pxeDisabledEvents())
}

func TestHTTPServer_DisablePXE_UnknownIP(t *testing.T) {
	cache := NewOCICache(t.TempDir())

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Cache: cache, Reader: fc},
		StatusRecorder: &recordingStatusRecorder{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /pxe/disable", srv.handleDisablePXE)
	mux.HandleFunc("GET /pxe/disable", srv.handleDisablePXE)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/pxe/disable", nil)
	req.Header.Set("X-Forwarded-For", "10.99.99.99")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /pxe/disable: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown IP, got %d", resp.StatusCode)
	}
}

func TestOCICache_Metadata(t *testing.T) {
	cacheDir := t.TempDir()
	cache := NewOCICache(cacheDir)

	digest := "testdigest123"
	metadataContent := "dhcpBootImageName: shimx64.efi\nhttpBootPath: http/shimx64.efi\n"

	if err := populateOCICache(cacheDir, digest, map[string][]byte{
		"metadata.yaml": []byte(metadataContent),
	}); err != nil {
		t.Fatal(err)
	}

	cache.SetDigest("ghcr.io/test/image:v1", "sha256:"+digest)

	meta, err := cache.MetadataForRef("ghcr.io/test/image:v1")
	if err != nil {
		t.Fatalf("MetadataForRef: %v", err)
	}

	if meta.DHCPBootImageName != "shimx64.efi" {
		t.Errorf("DHCPBootImageName: got %q, want %q", meta.DHCPBootImageName, "shimx64.efi")
	}

	if meta.HTTPBootPath != "http/shimx64.efi" {
		t.Errorf("HTTPBootPath: got %q, want %q", meta.HTTPBootPath, "http/shimx64.efi")
	}
}

func TestFileResolverHTTPBootURL(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "httpurl123", map[string][]byte{
		"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\nhttpBootPath: /efi/bootx64.efi\n"),
	})

	resolver := &FileResolver{Cache: cache, ServeURL: "http://10.0.0.10:8880/base/"}
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-http-url"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{Image: "ghcr.io/test/image:v1"},
		},
	}

	bootURL, err := resolver.HTTPBootURL(node)
	require.NoError(t, err)
	require.Equal(t, "http://10.0.0.10:8880/base/efi/bootx64.efi", bootURL)
}

func TestFileResolverHTTPBootURLFallsBackToDHCPBootImageName(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "httpfallback123", map[string][]byte{
		"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\n"),
	})

	resolver := &FileResolver{Cache: cache, ServeURL: "http://10.0.0.10:8880"}
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-http-url"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{Image: "ghcr.io/test/image:v1"},
		},
	}

	bootURL, err := resolver.HTTPBootURL(node)
	require.NoError(t, err)
	require.Equal(t, "http://10.0.0.10:8880/shimx64.efi", bootURL)
}

func TestJoinServeURLPathRejectsTraversal(t *testing.T) {
	_, err := JoinServeURLPath("http://10.0.0.10:8880", "../shimx64.efi")
	require.Error(t, err)
}

func TestOCICache_MetadataNoFile(t *testing.T) {
	cacheDir := t.TempDir()
	cache := NewOCICache(cacheDir)

	digest := "nometa123"

	if err := populateOCICache(cacheDir, digest, map[string][]byte{
		"vmlinuz": []byte("kernel"),
	}); err != nil {
		t.Fatal(err)
	}

	cache.SetDigest("ghcr.io/test/image:v1", "sha256:"+digest)

	meta, err := cache.MetadataForRef("ghcr.io/test/image:v1")
	if err != nil {
		t.Fatalf("MetadataForRef: %v", err)
	}

	if meta.DHCPBootImageName != "" {
		t.Errorf("expected empty DHCPBootImageName, got %q", meta.DHCPBootImageName)
	}
}

func TestOCICache_ResolvePath(t *testing.T) {
	cacheDir := t.TempDir()
	cache := NewOCICache(cacheDir)

	digest := "resolve123"

	if err := populateOCICache(cacheDir, digest, map[string][]byte{
		"vmlinuz":            []byte("kernel"),
		"grub/grub.cfg.tmpl": []byte("template content"),
	}); err != nil {
		t.Fatal(err)
	}

	cache.SetDigest("ghcr.io/test/image:v1", "sha256:"+digest)

	// Static file
	diskPath, isTemplate, err := cache.ResolvePath("ghcr.io/test/image:v1", "vmlinuz")
	if err != nil {
		t.Fatalf("ResolvePath static: %v", err)
	}

	if isTemplate {
		t.Error("vmlinuz should not be a template")
	}

	if !filepath.IsAbs(diskPath) {
		t.Errorf("expected absolute path, got %q", diskPath)
	}

	// Template file (.tmpl suffix stripped in request)
	_, isTemplate, err = cache.ResolvePath("ghcr.io/test/image:v1", "grub/grub.cfg")
	if err != nil {
		t.Fatalf("ResolvePath template: %v", err)
	}

	if !isTemplate {
		t.Error("grub/grub.cfg should be resolved as template (via grub/grub.cfg.tmpl)")
	}

	// Not found
	_, _, err = cache.ResolvePath("ghcr.io/test/image:v1", "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestOCICache_ResolvePathForArchitecture(t *testing.T) {
	cacheDir := t.TempDir()
	cache := NewOCICache(cacheDir)

	digest := "multiarch123"
	imageRef := "ghcr.io/test/multiarch:v1"

	if err := populateOCICacheForArchitecture(cacheDir, digest, v1alpha3.PXEArchitectureAMD64, map[string][]byte{
		"vmlinuz": []byte("amd64 kernel"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := populateOCICacheForArchitecture(cacheDir, digest, v1alpha3.PXEArchitectureARM64, map[string][]byte{
		"vmlinuz": []byte("arm64 kernel"),
	}); err != nil {
		t.Fatal(err)
	}

	cache.SetDigestForArchitecture(imageRef, v1alpha3.PXEArchitectureAMD64, "sha256:"+digest)
	cache.SetDigestForArchitecture(imageRef, v1alpha3.PXEArchitectureARM64, "sha256:"+digest)

	amd64Path, _, err := cache.ResolvePathForArchitecture(imageRef, v1alpha3.PXEArchitectureAMD64, "vmlinuz")
	if err != nil {
		t.Fatalf("ResolvePathForArchitecture amd64: %v", err)
	}

	arm64Path, _, err := cache.ResolvePathForArchitecture(imageRef, v1alpha3.PXEArchitectureARM64, "vmlinuz")
	if err != nil {
		t.Fatalf("ResolvePathForArchitecture arm64: %v", err)
	}

	if amd64Path == arm64Path {
		t.Fatalf("architecture-specific paths should differ: %q", amd64Path)
	}

	amd64Data, err := os.ReadFile(amd64Path)
	if err != nil {
		t.Fatal(err)
	}

	arm64Data, err := os.ReadFile(arm64Path)
	if err != nil {
		t.Fatal(err)
	}

	if string(amd64Data) != "amd64 kernel" {
		t.Errorf("amd64 data = %q", amd64Data)
	}

	if string(arm64Data) != "arm64 kernel" {
		t.Errorf("arm64 data = %q", arm64Data)
	}
}

func TestOCICache_ResolvePath_PathTraversal(t *testing.T) {
	cacheDir := t.TempDir()
	cache := NewOCICache(cacheDir)

	digest := "traversal123"

	// Place a file outside the cache that a traversal might reach.
	secret := filepath.Join(cacheDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := populateOCICache(cacheDir, digest, map[string][]byte{
		"vmlinuz": []byte("kernel"),
	}); err != nil {
		t.Fatal(err)
	}

	cache.SetDigest("ghcr.io/test/image:v1", "sha256:"+digest)

	traversalCases := []string{
		"../../../secret.txt",     // escapes diskDir to cacheDir root
		"../../../../etc/passwd",  // escapes past cacheDir
		"sub/../../../secret.txt", // sub-dir traversal escaping diskDir
	}

	for _, tc := range traversalCases {
		_, _, err := cache.ResolvePath("ghcr.io/test/image:v1", tc)
		if err == nil {
			t.Errorf("expected error for traversal path %q, got none", tc)
		}
	}

	// Absolute path must be rejected.
	_, _, err := cache.ResolvePath("ghcr.io/test/image:v1", "/etc/passwd")
	if err == nil {
		t.Error("expected error for absolute path, got none")
	}

	// Path resolving to diskDir itself must be rejected.
	for _, dots := range []string{".", ""} {
		_, _, err := cache.ResolvePath("ghcr.io/test/image:v1", dots)
		if err == nil {
			t.Errorf("expected error for path %q resolving to diskDir, got none", dots)
		}
	}

	// Valid relative path must still work.
	_, _, err = cache.ResolvePath("ghcr.io/test/image:v1", "vmlinuz")
	if err != nil {
		t.Errorf("expected success for valid relative path, got: %v", err)
	}
}

func TestHandleCloudInitLog(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "start event",
			body: `{"name":"init-network/config-ssh","description":"running config-ssh with frequency once-per-instance","event_type":"start","origin":"cloudinit","timestamp":1775657336.9020026}`,
		},
		{
			name: "finish event",
			body: `{"name":"init-network/config-ssh","description":"config-ssh ran successfully and took 0.001 seconds","event_type":"finish","origin":"cloudinit","timestamp":1775657336.9020026,"result":"SUCCESS"}`,
		},
		{
			name: "invalid JSON",
			body: `not-json-at-all`,
		},
		{
			name: "empty body",
			body: "",
		},
		{
			name: "unknown event type",
			body: `{"name":"modules-config","description":"some custom event","event_type":"custom","origin":"cloudinit","timestamp":1775657336.0}`,
		},
	}

	srv := &HTTPServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(tt.body))
			req.Header.Set("X-Forwarded-For", "10.0.1.50")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST /cloudinit/log: %v", err)
			}

			resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected 200, got %d", resp.StatusCode)
			}
		})
	}
}

func TestBuildCloudInitCondition(t *testing.T) {
	generation := int64(3)

	tests := []struct {
		name       string
		event      cloudInitEvent
		wantNil    bool
		wantStatus metav1.ConditionStatus
		wantReason string
		wantSubstr string // substring expected in message
	}{
		{
			name: "start event sets Running",
			event: cloudInitEvent{
				Name:        "init-local",
				Description: "starting init-local",
				EventType:   "start",
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "Running",
			wantSubstr: `stage "init-local" started`,
		},
		{
			name: "early stage finish SUCCESS sets Running",
			event: cloudInitEvent{
				Name:        "init-local",
				Description: "init-local ran successfully",
				EventType:   "finish",
				Result:      "SUCCESS",
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "Running",
			wantSubstr: `stage "init-local" finished successfully`,
		},
		{
			name: "modules-config finish SUCCESS sets Running",
			event: cloudInitEvent{
				Name:        "modules-config",
				Description: "modules-config ran successfully",
				EventType:   "finish",
				Result:      "SUCCESS",
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "Running",
			wantSubstr: `stage "modules-config" finished successfully`,
		},
		{
			name: "modules-final finish SUCCESS sets Succeeded",
			event: cloudInitEvent{
				Name:        "modules-final",
				Description: "modules-final ran successfully and took 1.23 seconds",
				EventType:   "finish",
				Result:      "SUCCESS",
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: "Succeeded",
			wantSubstr: "cloud-init completed successfully",
		},
		{
			name: "finish with failure sets Failed",
			event: cloudInitEvent{
				Name:        "modules-config",
				Description: "running modules-config",
				EventType:   "finish",
				Result:      "FAIL: command [apt-get install -y badpkg] failed with exit code 100",
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "Failed",
			wantSubstr: `stage "modules-config" failed`,
		},
		{
			name: "finish with failure includes result in message",
			event: cloudInitEvent{
				Name:        "modules-final",
				Description: "running modules-final",
				EventType:   "finish",
				Result:      "EXCEPTION: Traceback (most recent call last): runcmd failed",
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "Failed",
			wantSubstr: "EXCEPTION: Traceback",
		},
		{
			name: "unknown event type returns nil",
			event: cloudInitEvent{
				Name:        "modules-config",
				Description: "custom event",
				EventType:   "custom",
			},
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := buildCloudInitCondition(&tt.event, generation)

			if tt.wantNil {
				if cond != nil {
					t.Fatalf("expected nil condition, got %+v", cond)
				}

				return
			}

			if cond == nil {
				t.Fatal("expected non-nil condition")
			}

			if cond.Type != v1alpha3.MachineOperationConditionCloudInitDone {
				t.Errorf("type: got %q, want %q", cond.Type, v1alpha3.MachineOperationConditionCloudInitDone)
			}

			if cond.Status != tt.wantStatus {
				t.Errorf("status: got %q, want %q", cond.Status, tt.wantStatus)
			}

			if cond.Reason != tt.wantReason {
				t.Errorf("reason: got %q, want %q", cond.Reason, tt.wantReason)
			}

			if !strings.Contains(cond.Message, tt.wantSubstr) {
				t.Errorf("message %q should contain %q", cond.Message, tt.wantSubstr)
			}

			if cond.ObservedGeneration != generation {
				t.Errorf("observedGeneration: got %d, want %d", cond.ObservedGeneration, generation)
			}
		})
	}
}

func TestBuildCloudInitCondition_MessageTruncation(t *testing.T) {
	// Build a result string that will exceed maxConditionMessageLen when
	// formatted into the failure message.
	longResult := strings.Repeat("x", maxConditionMessageLen+500)

	ev := cloudInitEvent{
		Name:        "modules-config",
		Description: "running modules-config",
		EventType:   "finish",
		Result:      longResult,
	}

	cond := buildCloudInitCondition(&ev, 1)
	if cond == nil {
		t.Fatal("expected non-nil condition")
	}

	if len(cond.Message) > maxConditionMessageLen {
		t.Errorf("message length %d exceeds max %d", len(cond.Message), maxConditionMessageLen)
	}

	if !strings.HasSuffix(cond.Message, "...") {
		t.Errorf("truncated message should end with '...', got %q", cond.Message[len(cond.Message)-10:])
	}

	// Verify that the message still starts with the stage info.
	if !strings.Contains(cond.Message, "modules-config") {
		t.Errorf("truncated message should contain stage name, got %q", cond.Message[:100])
	}
}

func TestInstallLogEndpointAcceptsPlainTextWithoutRecordingCondition(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "install-log-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:78", IPv4: "10.0.20.18", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	cache := NewOCICache(t.TempDir())
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()
	recorder := &recordingStatusRecorder{}

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Cache: cache, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /unbounded-agent/install-log", srv.handleInstallLog)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := "unbounded-agent install exited with status 42\n\nlast 120 lines of /var/log/unbounded-agent-install.log:\nfailed to download agent"
	req, _ := http.NewRequest("POST", ts.URL+"/unbounded-agent/install-log", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Forwarded-For", "10.0.20.18")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, recorder.conditionEvents())
}

func TestCloudInitCondition_StageStartSetsRunning(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-start-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:70", IPv4: "10.0.20.10", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	cache := NewOCICache(t.TempDir())
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Cache: cache, Reader: fc},
		StatusRecorder: &recordingStatusRecorder{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := `{"name":"init-local","description":"starting init-local","event_type":"start","origin":"cloudinit","timestamp":1775657336.0}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set("X-Forwarded-For", "10.0.20.10")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /cloudinit/log: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	recorder := srv.StatusRecorder.(*recordingStatusRecorder)
	conditions := recorder.conditionEvents()
	require.Len(t, conditions, 1)
	require.Equal(t, node.Name, conditions[0].machineName)
	cond := &conditions[0].condition

	if cond.Status != metav1.ConditionFalse {
		t.Errorf("status: got %q, want %q", cond.Status, metav1.ConditionFalse)
	}

	if cond.Reason != "Running" {
		t.Errorf("reason: got %q, want %q", cond.Reason, "Running")
	}
}

func TestCloudInitCondition_FinalStageSuccessSetsTrue(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-done-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:71", IPv4: "10.0.20.11", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	cache := NewOCICache(t.TempDir())
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Cache: cache, Reader: fc},
		StatusRecorder: &recordingStatusRecorder{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := `{"name":"modules-final","description":"modules-final ran successfully and took 1.23 seconds","event_type":"finish","origin":"cloudinit","timestamp":1775657336.0,"result":"SUCCESS"}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set("X-Forwarded-For", "10.0.20.11")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /cloudinit/log: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	recorder := srv.StatusRecorder.(*recordingStatusRecorder)
	conditions := recorder.conditionEvents()
	require.Len(t, conditions, 1)
	require.Equal(t, node.Name, conditions[0].machineName)
	cond := &conditions[0].condition

	if cond.Status != metav1.ConditionTrue {
		t.Errorf("status: got %q, want %q", cond.Status, metav1.ConditionTrue)
	}

	if cond.Reason != "Succeeded" {
		t.Errorf("reason: got %q, want %q", cond.Reason, "Succeeded")
	}

	if !strings.Contains(cond.Message, "cloud-init completed successfully") {
		t.Errorf("message %q should contain completion text", cond.Message)
	}
}

func TestCloudInitCondition_StageFailureSetsFailedWithDetails(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-fail-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:72", IPv4: "10.0.20.12", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	cache := NewOCICache(t.TempDir())
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Cache: cache, Reader: fc},
		StatusRecorder: &recordingStatusRecorder{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := `{"name":"modules-config","description":"running modules-config","event_type":"finish","origin":"cloudinit","timestamp":1775657336.0,"result":"FAIL: command [apt-get install -y badpkg] failed with exit code 100"}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set("X-Forwarded-For", "10.0.20.12")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /cloudinit/log: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	recorder := srv.StatusRecorder.(*recordingStatusRecorder)
	conditions := recorder.conditionEvents()
	require.Len(t, conditions, 1)
	require.Equal(t, node.Name, conditions[0].machineName)
	cond := &conditions[0].condition

	if cond.Status != metav1.ConditionFalse {
		t.Errorf("status: got %q, want %q", cond.Status, metav1.ConditionFalse)
	}

	if cond.Reason != "Failed" {
		t.Errorf("reason: got %q, want %q", cond.Reason, "Failed")
	}

	if !strings.Contains(cond.Message, "modules-config") {
		t.Errorf("message %q should contain stage name", cond.Message)
	}

	if !strings.Contains(cond.Message, "FAIL: command [apt-get install -y badpkg] failed with exit code 100") {
		t.Errorf("message %q should contain the error result", cond.Message)
	}
}

func TestCloudInitCondition_NoClientSkipsUpdate(t *testing.T) {
	// When Client is nil, handleCloudInitLog should still return 200
	// but not attempt any status update.
	srv := &HTTPServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := `{"name":"modules-final","description":"done","event_type":"finish","origin":"cloudinit","timestamp":1775657336.0,"result":"SUCCESS"}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set("X-Forwarded-For", "10.0.20.99")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /cloudinit/log: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestCloudInitCondition_UnknownIPSkipsUpdate(t *testing.T) {
	// When the IP doesn't match any Machine, recordCloudInitCondition
	// should log a warning but the handler should still return 200.
	cache := NewOCICache(t.TempDir())
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Cache: cache, Reader: fc},
		StatusRecorder: &recordingStatusRecorder{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := `{"name":"modules-final","description":"done","event_type":"finish","origin":"cloudinit","timestamp":1775657336.0,"result":"SUCCESS"}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set("X-Forwarded-For", "10.99.99.99")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /cloudinit/log: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestCloudInitCondition_StatusUpdateError(t *testing.T) {
	// When the status update fails, the handler should still return 200
	// because cloud-init does not retry on error responses.
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-update-err-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:75", IPv4: "10.0.20.15", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	cache := NewOCICache(t.TempDir())
	scheme := newScheme(t)

	injectedErr := fmt.Errorf("simulated conflict")
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()
	recorder := &recordingStatusRecorder{err: injectedErr}

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Cache: cache, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := `{"name":"modules-final","description":"done","event_type":"finish","origin":"cloudinit","timestamp":1775657336.0,"result":"SUCCESS"}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set("X-Forwarded-For", "10.0.20.15")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /cloudinit/log: %v", err)
	}

	resp.Body.Close()

	// Handler must still return 200 even when the status record is rejected.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 despite status record failure, got %d", resp.StatusCode)
	}
}

func TestCloudInitCondition_FullLifecycle(t *testing.T) {
	// Simulate a full cloud-init lifecycle: start -> intermediate finish -> final finish.
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-lifecycle-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:73", IPv4: "10.0.20.13", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	cache := NewOCICache(t.TempDir())
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	recorder := &recordingStatusRecorder{}
	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Cache: cache, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	postEvent := func(body string) {
		t.Helper()

		req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
		req.Header.Set("X-Forwarded-For", "10.0.20.13")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /cloudinit/log: %v", err)
		}

		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	}

	getCondition := func() *metav1.Condition {
		t.Helper()

		conditions := recorder.conditionEvents()
		if len(conditions) == 0 {
			return nil
		}

		condition := conditions[len(conditions)-1].condition

		return &condition
	}

	// Step 1: init-local starts
	postEvent(`{"name":"init-local","description":"starting init-local","event_type":"start","origin":"cloudinit","timestamp":1.0}`)

	cond := getCondition()
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "Running" {
		t.Fatalf("after init-local start: expected False/Running, got %+v", cond)
	}

	// Step 2: init-local finishes successfully
	postEvent(`{"name":"init-local","description":"init-local done","event_type":"finish","origin":"cloudinit","timestamp":2.0,"result":"SUCCESS"}`)

	cond = getCondition()
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "Running" {
		t.Fatalf("after init-local finish: expected False/Running, got %+v", cond)
	}

	// Step 3: modules-final starts
	postEvent(`{"name":"modules-final","description":"starting modules-final","event_type":"start","origin":"cloudinit","timestamp":3.0}`)

	cond = getCondition()
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "Running" {
		t.Fatalf("after modules-final start: expected False/Running, got %+v", cond)
	}

	// Step 4: modules-final finishes successfully
	postEvent(`{"name":"modules-final","description":"modules-final done","event_type":"finish","origin":"cloudinit","timestamp":4.0,"result":"SUCCESS"}`)

	cond = getCondition()
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "Succeeded" {
		t.Fatalf("after modules-final finish: expected True/Succeeded, got %+v", cond)
	}
}

func TestHTTPServerRecordsCloudInitDoneOnlyOnFinalSuccess(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-operation-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:76", IPv4: "10.0.20.16", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	cache := NewOCICache(t.TempDir())
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()
	recorder := &recordingStatusRecorder{}

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Cache: cache, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	postEvent := func(body string) {
		t.Helper()

		req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
		req.Header.Set("X-Forwarded-For", "10.0.20.16")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /cloudinit/log: %v", err)
		}

		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
	}

	postEvent(`{"name":"init-local","description":"starting init-local","event_type":"start","origin":"cloudinit","timestamp":1.0}`)
	postEvent(`{"name":"init-local","description":"init-local done","event_type":"finish","origin":"cloudinit","timestamp":2.0,"result":"SUCCESS"}`)
	postEvent(`{"name":"modules-config","description":"modules-config done","event_type":"finish","origin":"cloudinit","timestamp":3.0,"result":"SUCCESS"}`)
	postEvent(`{"name":"modules-final","description":"modules-final done","event_type":"finish","origin":"cloudinit","timestamp":4.0,"result":"SUCCESS"}`)

	require.Equal(t, []string{node.Name}, recorder.cloudInitDoneEvents())
}

func TestHTTPServerDoesNotRecordCloudInitDoneOnFailure(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "ci-operation-fail-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:77", IPv4: "10.0.20.17", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	cache := NewOCICache(t.TempDir())
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()
	recorder := &recordingStatusRecorder{}

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Cache: cache, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := `{"name":"modules-final","description":"running modules-final","event_type":"finish","origin":"cloudinit","timestamp":1.0,"result":"FAIL: command [apt-get install -y badpkg] failed with exit code 100"}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set("X-Forwarded-For", "10.0.20.17")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /cloudinit/log: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	require.Empty(t, recorder.cloudInitDoneEvents())
}

func freePort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	return port
}

func waitForHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", url)
}
