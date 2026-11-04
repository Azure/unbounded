// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"encoding/json"
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
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/imagepull"
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

func TestHTTPServer_ServeFiles(t *testing.T) {
	vmlinuzData := []byte("test-vmlinuz-binary-data")

	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
			Puller:            puller,
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

	ts := newTestPeerServer(mux)
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

	// Test serving an image file (with source IP identification)
	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set(testPeerHeader, "10.0.1.50")

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
	req.Header.Set(testPeerHeader, "10.0.1.50")

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
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
					Puller: puller,
					Reader: fc,
				},
				StatusRecorder: recorder,
			}

			mux := http.NewServeMux()
			mux.HandleFunc("GET /", srv.handleFile)

			ts := newTestPeerServer(mux)
			defer ts.Close()

			req, _ := http.NewRequest("GET", ts.URL+"/"+tt.path, nil)
			req.Header.Set(testPeerHeader, "10.0.1.60")

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
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
			Puller: puller,
			Reader: fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
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
			req.Header.Set(testPeerHeader, "10.0.1.61")

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
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
			Puller: puller,
			Reader: fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/revocations.efi", nil)
	req.Header.Set(testPeerHeader, "10.0.1.62")

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

	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
			Puller:            puller,
			Reader:            fc,
			Cluster:           &StaticClusterInfo{Info: ClusterInfo{ApiserverURL: "https://k8s.example.com"}},
			ServeURL:          "http://10.0.1.1:8080",
			DefaultNetbootRef: "ghcr.io/test/image:v1",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/grub/grub.cfg", nil)
	req.Header.Set(testPeerHeader, "10.0.1.10")

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
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
			Puller:            puller,
			Reader:            fc,
			DefaultNetbootRef: "ghcr.io/test/image:v1",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/cloud-init/network-config", nil)
	req.Header.Set(testPeerHeader, "10.0.1.51")

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

	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
			Puller:            puller,
			Reader:            fc,
			DefaultNetbootRef: "ghcr.io/test/image:v1",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/cloud-init/network-config", nil)
	req.Header.Set(testPeerHeader, "10.0.1.52")

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
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
		"vmlinuz": []byte("some-binary-data"),
	})

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			Puller: puller,
			Reader: fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	// No node registered -- requests from any IP should get 404
	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set(testPeerHeader, "10.99.99.99")

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
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
		FileResolver:   FileResolver{Puller: puller},
		StatusRecorder: recorder,
	}

	img, err := server.resolveImage(t.Context(), ImageSource{Ref: "ghcr.io/test/image:v1"})
	require.NoError(t, err)

	server.recordBootLoaderDownloaded(t.Context(), testLogger(), node, img, "grubx64.efi")
	require.Empty(t, recorder.calls)

	server.recordBootLoaderDownloaded(t.Context(), testLogger(), node, img, "shimx64.efi")
	require.Equal(t, []string{"machine-1:shimx64.efi"}, recorder.calls)
}

func TestTFTPServerRecordsInitialBootLoaderForTargetArchitecture(t *testing.T) {
	images := newTestImages(t)
	images.PushArch(t, "ghcr.io/test/image:v1", map[string]map[string][]byte{
		v1alpha3.PXEArchitectureAMD64: {"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\n")},
		v1alpha3.PXEArchitectureARM64: {"metadata.yaml": []byte("dhcpBootImageName: shimaa64.efi\n")},
	})
	puller := images.Puller

	recorder := &recordingTFTPStatusRecorder{}
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-arm"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{Architecture: v1alpha3.PXEArchitectureARM64},
		},
	}
	server := &TFTPServer{
		FileResolver:   FileResolver{Puller: puller},
		StatusRecorder: recorder,
	}

	img, err := server.resolveImage(t.Context(), ImageSource{Ref: "ghcr.io/test/image:v1", Architecture: v1alpha3.PXEArchitectureARM64})
	require.NoError(t, err)

	server.recordBootLoaderDownloaded(t.Context(), testLogger(), node, img, "shimx64.efi")
	require.Empty(t, recorder.calls)

	server.recordBootLoaderDownloaded(t.Context(), testLogger(), node, img, "shimaa64.efi")
	require.Equal(t, []string{"machine-arm:shimaa64.efi"}, recorder.calls)
}

func TestHTTPServerDoesNotRecordBootImageWriteOnFileDownload(t *testing.T) {
	diskImageData := []byte("test-disk-image")
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
			Puller: puller,
			Reader: fc,
		},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set(testPeerHeader, "10.0.1.50")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, recorder.bootImageWrittenEvents())

	req, _ = http.NewRequest("GET", ts.URL+"/disk.img.gz", nil)
	req.Header.Set(testPeerHeader, "10.0.1.50")

	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, diskImageData, body)
	require.Empty(t, recorder.bootImageWrittenEvents())

	req, _ = http.NewRequest("GET", ts.URL+"/missing", nil)
	req.Header.Set(testPeerHeader, "10.0.1.50")

	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Empty(t, recorder.bootImageWrittenEvents())
}

func TestHTTPServer_UsesMachineImageForBootFilesWhenNetbootImageUnset(t *testing.T) {
	vmlinuzData := []byte("kernel")
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
			Puller: puller,
			Reader: fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set(testPeerHeader, "10.0.1.50")

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
					DNS:        []string{"10.0.1.53", "10.0.1.54"},
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
		"unbounded.dns=10.0.1.53,10.0.1.54",
		"unbounded.disk=/dev/disk/by-id/test-os-disk",
		"ip=10.0.1.20::10.0.1.1:255.255.255.0:::none",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in rendered grub.cfg, got:\n%s", want, body)
		}
	}

	require.NotContains(t, body, "eth0")
}

func TestNetworkConfigTemplate_SelectsBootLease(t *testing.T) {
	networkTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "network-config.tmpl"))
	require.NoError(t, err)

	node := &v1alpha3.Machine{
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				DHCPLeases: []v1alpha3.DHCPLease{
					{IPv4: "10.0.1.20", MAC: "aa:bb:cc:dd:ee:20", Gateway: "10.0.1.1", SubnetMask: "255.255.255.0"},
					{
						IPv4:       "10.0.2.21",
						MAC:        "aa:bb:cc:dd:ee:21",
						Gateway:    "10.0.2.1",
						SubnetMask: "255.255.254.0",
						DNS:        []string{"10.0.2.53", "2001:db8::53"},
					},
				},
			},
		},
	}

	data := newTemplateData(node, ClusterInfo{}, "", "", "10.0.2.21", true)
	result, err := renderTemplate(string(networkTmpl), data)
	require.NoError(t, err)

	body := string(result)
	require.Contains(t, body, `macaddress: "aa:bb:cc:dd:ee:21"`)
	require.Contains(t, body, `- "10.0.2.21/23"`)
	require.Contains(t, body, `via: "10.0.2.1"`)
	require.Contains(t, body, `- "10.0.2.53"`)
	require.Contains(t, body, `- "2001:db8::53"`)
	require.Contains(t, body, "dhcp4: false")
	require.Contains(t, body, "dhcp6: false")
	require.NotContains(t, body, "10.0.1.20")
}

func TestNetworkConfigTemplate_NoLeaseDisablesDynamicConfiguration(t *testing.T) {
	networkTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "network-config.tmpl"))
	require.NoError(t, err)

	result, err := renderTemplate(string(networkTmpl), newTemplateData(&v1alpha3.Machine{}, ClusterInfo{}, "", "", "", false))
	require.NoError(t, err)
	require.Equal(t, "\nversion: 2\nethernets: {}\n", string(result))
}

func TestNetworkConfigTemplate_InvalidSubnetMask(t *testing.T) {
	networkTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "network-config.tmpl"))
	require.NoError(t, err)

	node := &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{DHCPLeases: []v1alpha3.DHCPLease{{
		IPv4: "10.0.1.20", MAC: "aa:bb:cc:dd:ee:20", Gateway: "10.0.1.1", SubnetMask: "255.0.255.0",
	}}}}}

	_, err = renderTemplate(string(networkTmpl), newTemplateData(node, ClusterInfo{}, "", "", "10.0.1.20", true))
	require.ErrorContains(t, err, "non-contiguous IPv4 subnet mask")
}

func TestHTTPServer_NetworkConfigOmitsGatewayAndInvalidDNS(t *testing.T) {
	networkTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "network-config.tmpl"))
	require.NoError(t, err)

	const imageRef = "ghcr.io/test/netboot:v1"

	puller := setupImagePuller(t, imageRef, map[string][]byte{
		"cloud-init/network-config.tmpl": networkTmpl,
	})
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-on-link"},
		Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{DHCPLeases: []v1alpha3.DHCPLease{{
			IPv4: "10.0.1.20", MAC: "aa:bb:cc:dd:ee:20", SubnetMask: "255.255.255.0",
			DNS: []string{"not-an-address", "10.0.1.53"},
		}}}},
	}
	fc := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()
	server := &HTTPServer{FileResolver: FileResolver{
		Puller: puller, Reader: fc, Cluster: &StaticClusterInfo{}, DefaultNetbootRef: imageRef,
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", server.handleFile)

	httpServer := newTestPeerServer(mux)
	defer httpServer.Close()

	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/cloud-init/network-config", nil)
	require.NoError(t, err)
	request.Header.Set(testPeerHeader, "10.0.1.20")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	defer response.Body.Close()

	require.Equal(t, http.StatusOK, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	var config struct {
		Ethernets map[string]struct {
			Addresses   []string `yaml:"addresses"`
			Routes      []any    `yaml:"routes"`
			Nameservers struct {
				Addresses []string `yaml:"addresses"`
			} `yaml:"nameservers"`
		} `yaml:"ethernets"`
	}
	require.NoError(t, yaml.Unmarshal(body, &config), "rendered network configuration:\n%s", body)
	provisioning := config.Ethernets["provisioning"]
	require.Equal(t, []string{"10.0.1.20/24"}, provisioning.Addresses)
	require.Empty(t, provisioning.Routes)
	require.Equal(t, []string{"10.0.1.53"}, provisioning.Nameservers.Addresses)
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

func TestGrubTemplate_SerialConsoleByArchitecture(t *testing.T) {
	grubTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "grub.cfg.tmpl"))
	require.NoError(t, err)

	for _, tc := range []struct {
		arch    string
		console bool
	}{
		{arch: "", console: true},
		{arch: v1alpha3.PXEArchitectureAMD64, console: true},
		// arm64 servers often use a PL011 UART, not ttyS0. Leave the
		// choice to the firmware SPCR table.
		{arch: v1alpha3.PXEArchitectureARM64, console: false},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			node := &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-console"},
				Spec: v1alpha3.MachineSpec{
					Host: &v1alpha3.HostSpec{Netboot: &v1alpha3.PXESpec{Architecture: tc.arch}},
				},
			}

			data := newTemplateData(node, ClusterInfo{ApiserverURL: "https://k8s.example.com"}, "http://10.0.1.1:8080", "", "10.0.1.21", true)
			result, err := renderTemplate(string(grubTmpl), data)
			require.NoError(t, err)

			if tc.console {
				require.Contains(t, string(result), "console=ttyS0,115200n8")
			} else {
				require.NotContains(t, string(result), "console=")
			}
		})
	}
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
		"systemctl list-units --all --state=failed,auto-restart --plain --no-legend",
		`journalctl -b --no-pager -u "$unit" 2>&1 | grep -v ' systemd-nspawn\[' | tail -n 60`,
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

	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
		Puller: puller,
		Reader: fc,
		Cluster: &StaticClusterInfo{Info: ClusterInfo{
			ApiserverURL: "https://k8s.example.com",
		}},
		ServeURL: "http://10.0.8.1:8080",
	}

	resolved, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if string(resolved.Data) != customUserData {
		t.Errorf("expected ConfigMap user-data, got %q", resolved.Data)
	}
}

func TestResolveFileByPath_UserDataFallsBackToDefault(t *testing.T) {
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
		Puller: puller,
		Reader: fc,
		Cluster: &StaticClusterInfo{Info: ClusterInfo{
			ApiserverURL: "https://k8s.example.com",
		}},
		ServeURL: "http://10.0.8.1:8080",
	}

	resolved, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if resolved.File != nil {
		t.Errorf("expected no streamed file for default user-data, got %q", resolved.File.Name())
	}

	if string(resolved.Data) != defaultUserData {
		t.Errorf("expected default user-data %q, got %q", defaultUserData, resolved.Data)
	}
}

func TestResolveFileByPath_UserDataConfigMapCustomKey(t *testing.T) {
	customUserData := "#cloud-config\npackages:\n  - htop\n"

	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
		Puller: puller,
		Reader: fc,
		Cluster: &StaticClusterInfo{Info: ClusterInfo{
			ApiserverURL: "https://k8s.example.com",
		}},
		ServeURL: "http://10.0.8.1:8080",
	}

	resolved, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if string(resolved.Data) != customUserData {
		t.Errorf("expected custom-key user-data, got %q", resolved.Data)
	}
}

func TestResolveFileByPath_UserDataConfigMapMissing(t *testing.T) {
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
		Puller: puller,
		Reader: fc,
	}

	resolved, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err != nil {
		t.Fatalf("expected fallback to default user-data, got error: %v", err)
	}

	if string(resolved.Data) != defaultUserData {
		t.Errorf("expected default user-data %q, got %q", defaultUserData, resolved.Data)
	}
}

func TestResolveFileByPath_UserDataConfigMapGetError(t *testing.T) {
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
		Puller: puller,
		Reader: fc,
	}

	_, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err == nil {
		t.Fatal("expected error for non-NotFound client failure, got nil")
	}

	if !strings.Contains(err.Error(), "simulated network timeout") {
		t.Errorf("expected error to contain injected message, got: %v", err)
	}
}

func TestResolveFileByPath_UserDataConfigMapMissingKey(t *testing.T) {
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
		Puller: puller,
		Reader: fc,
	}

	_, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err == nil {
		t.Fatal("expected error when ConfigMap key is missing")
	}

	if !strings.Contains(err.Error(), "user-data") {
		t.Errorf("expected error to mention missing key, got: %v", err)
	}
}

func TestResolveFileByPath_UserDataFromBinaryData(t *testing.T) {
	binaryUserData := []byte("#cloud-config\npackages:\n  - curl\n")

	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
		Puller: puller,
		Reader: fc,
	}

	resolved, err := resolver.ResolveFileByPath(t.Context(), "cloud-init/user-data", node, ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if string(resolved.Data) != string(binaryUserData) {
		t.Errorf("expected BinaryData user-data, got %q", resolved.Data)
	}
}

func TestHTTPServer_UserDataConfigMapMissing(t *testing.T) {
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
			Puller:            puller,
			Reader:            fc,
			Cluster:           &StaticClusterInfo{Info: ClusterInfo{ApiserverURL: "https://k8s.example.com"}},
			ServeURL:          "http://10.0.9.1:8080",
			DefaultNetbootRef: "ghcr.io/test/image:v1",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/cloud-init/user-data", nil)
	req.Header.Set(testPeerHeader, "10.0.9.11")

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

	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
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
			Puller:            puller,
			Reader:            fc,
			Cluster:           &StaticClusterInfo{Info: ClusterInfo{ApiserverURL: "https://k8s.example.com"}},
			ServeURL:          "http://10.0.9.1:8080",
			DefaultNetbootRef: "ghcr.io/test/image:v1",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/cloud-init/user-data", nil)
	req.Header.Set(testPeerHeader, "10.0.9.10")

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

	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
		"config.tmpl": []byte(tmplContent),
	})

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	resolver := FileResolver{
		Puller: puller,
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

	resolved, err := resolver.ResolveFileByPath(t.Context(), "config", nodeWithAgent, ImageSource{Ref: "ghcr.io/test/image:v1"})
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

	resolved, err = resolver.ResolveFileByPath(t.Context(), "config", nodeWithoutAgent, ImageSource{Ref: "ghcr.io/test/image:v1"})
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

	// The test serves no image files, so the puller needs no registry.
	puller := &imagepull.Puller{}

	srv := &HTTPServer{
		BindAddr: "127.0.0.1",
		Port:     port,
		FileResolver: FileResolver{
			Puller: puller,
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

func TestHTTPServer_StartWithMediaRoutes(t *testing.T) {
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	port := freePort(t)

	srv := &HTTPServer{
		BindAddr: "127.0.0.1",
		Port:     port,
		FileResolver: FileResolver{
			Puller: &imagepull.Puller{},
			Reader: fc,
		},
		Client: fc,
		Media:  &MediaProvider{Client: fc},
	}

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)

	go func() {
		errCh <- srv.Start(ctx)
	}()

	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHTTP(t, addr+"/healthz", 3*time.Second)

	id := strings.Repeat("a", 32)

	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/bootmedia/" + id + "/grub.cfg", http.StatusNotFound},
		{http.MethodPost, "/bootmedia/" + id + "/complete", http.StatusNotFound},
		{http.MethodPut, "/bootmedia/" + id + "/complete", http.StatusMethodNotAllowed},
	} {
		req, err := http.NewRequestWithContext(t.Context(), tc.method, addr+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}

		resp.Body.Close()

		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: got %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}

		if got := resp.Header.Get("Cache-Control"); tc.want == http.StatusNotFound && got != "no-store" {
			t.Errorf("%s %s: request did not reach media handler (Cache-Control %q)", tc.method, tc.path, got)
		}
	}

	cancel()

	if err := <-errCh; err != nil {
		t.Fatalf("server returned error: %v", err)
	}
}

func TestTFTPServer_ResolveFileByPath(t *testing.T) {
	vmlinuzData := []byte("tftp-vmlinuz-data")

	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
		"vmlinuz": vmlinuzData,
	})

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	srv := &TFTPServer{
		FileResolver: FileResolver{
			Puller: puller,
			Reader: fc,
		},
	}

	resolved, err := srv.ResolveFileByPath(t.Context(), "vmlinuz", nil, ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if resolved.File == nil {
		t.Fatal("expected a streamed file for static file")
	}

	data, err := io.ReadAll(resolved.File)
	resolved.Close() //nolint:errcheck // test

	if err != nil {
		t.Fatalf("reading resolved file: %v", err)
	}

	if string(data) != string(vmlinuzData) {
		t.Errorf("data mismatch: got %q", data)
	}

	// Test not found (wrong image)
	_, err = srv.ResolveFileByPath(t.Context(), "vmlinuz", nil, ImageSource{Ref: "ghcr.io/test/nonexistent:v1"})
	if err == nil {
		t.Error("expected error for nonexistent image")
	}

	// Test not found (wrong path)
	_, err = srv.ResolveFileByPath(t.Context(), "nonexistent/foo", nil, ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestTFTPServer_TemplateVerbatim(t *testing.T) {
	staticData := "static-config-data"

	// When requesting "configs/static", it finds configs/static.tmpl and renders as template
	// Since node is nil, template content is returned verbatim
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
		"configs/static.tmpl": []byte(staticData),
	})

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	srv := &TFTPServer{
		FileResolver: FileResolver{
			Puller: puller,
			Reader: fc,
		},
	}

	resolved, err := srv.ResolveFileByPath(t.Context(), "configs/static", nil, ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if string(resolved.Data) != staticData {
		t.Errorf("data mismatch: got %q, want %q", resolved.Data, staticData)
	}
}

func TestTFTPServer_StaticFile(t *testing.T) {
	staticData := "static-config-no-template"

	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
		"configs/static": []byte(staticData),
	})

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	srv := &TFTPServer{
		FileResolver: FileResolver{
			Puller: puller,
			Reader: fc,
		},
	}

	resolved, err := srv.ResolveFileByPath(t.Context(), "configs/static", nil, ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if resolved.File == nil {
		t.Fatal("expected a streamed file for static file")
	}

	data, err := io.ReadAll(resolved.File)
	resolved.Close() //nolint:errcheck // test

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
		{"forwarded ignored", "10.0.1.10", "192.168.1.1:1234", "192.168.1.1"},
		{"forwarded multiple ignored", "10.0.1.10, 192.168.1.1", "192.168.1.2:1234", "192.168.1.2"},
		{"remote only", "", "10.0.1.10:1234", "10.0.1.10"},
		{"ipv6", "", "[2001:db8::1]:1234", "2001:db8::1"},
		{"missing port", "", "10.0.1.10", "10.0.1.10"},
		{"empty", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{
				RemoteAddr: tt.remote,
				Header:     http.Header{},
			}
			if tt.fwdFor != "" {
				r.Header.Set("X-Forwarded-For", tt.fwdFor)
				r.Header.Set("X-Real-IP", tt.fwdFor)
				r.Header.Set("Forwarded", "for="+tt.fwdFor)
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

	puller := setupImagePuller(t, "ghcr.io/test/e2e:v1", map[string][]byte{
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
			Puller:            puller,
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

	httpTS := newTestPeerServer(mux)
	defer httpTS.Close()

	// Test vmlinuz via HTTP
	req, _ := http.NewRequest("GET", httpTS.URL+"/vmlinuz", nil)
	req.Header.Set(testPeerHeader, "10.0.3.10")
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != string(vmlinuzData) {
		t.Errorf("HTTP vmlinuz mismatch")
	}

	// Test boot config (template rendered)
	req, _ = http.NewRequest("GET", httpTS.URL+"/grub/grub.cfg", nil)
	req.Header.Set(testPeerHeader, "10.0.3.10")
	resp, _ = http.DefaultClient.Do(req)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), "Install e2e-node") {
		t.Errorf("grub.cfg should contain node name, got:\n%s", body)
	}

	// Test default user-data (no ConfigMap configured, so metalman returns built-in default)
	req, _ = http.NewRequest("GET", httpTS.URL+"/cloud-init/user-data", nil)
	req.Header.Set(testPeerHeader, "10.0.3.10")
	resp, _ = http.DefaultClient.Do(req)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != defaultUserData {
		t.Errorf("default user-data mismatch: got %q, want %q", body, defaultUserData)
	}

	// Test 404 for file not in this image
	req, _ = http.NewRequest("GET", httpTS.URL+"/nonexistent/foo", nil)
	req.Header.Set(testPeerHeader, "10.0.3.10")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent, got %d", resp.StatusCode)
	}
}

func TestHTTPServer_RoutesDiskFromMachineImageAndBootFromNetbootImage(t *testing.T) {
	machineData := []byte("machine-disk-data")
	netbootData := []byte("netboot-kernel-data")

	images := newTestImages(t)
	images.Push(t, "ghcr.io/test/machine:v1", map[string][]byte{"disk.img.gz": machineData})
	images.Push(t, "ghcr.io/test/netboot:v1", map[string][]byte{"vmlinuz": netbootData})
	puller := images.Puller

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

	srv := &HTTPServer{FileResolver: FileResolver{Puller: puller, Reader: fc}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	for _, tt := range []struct {
		path string
		want []byte
	}{
		{path: "/disk.img.gz", want: machineData},
		{path: "/vmlinuz", want: netbootData},
	} {
		req, _ := http.NewRequest("GET", ts.URL+tt.path, nil)
		req.Header.Set(testPeerHeader, "10.0.30.10")

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

	images := newTestImages(t)
	images.Push(t, "ghcr.io/test/alpha:v1", map[string][]byte{"vmlinuz": alphaData})
	images.Push(t, "ghcr.io/test/beta:v1", map[string][]byte{"vmlinuz": betaData})
	puller := images.Puller

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
			Puller: puller,
			Reader: fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	// Alpha node can access its own image's files
	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set(testPeerHeader, "10.0.10.1")
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
	req.Header.Set(testPeerHeader, "10.0.10.2")
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
	imageRef := "ghcr.io/test/netboot:v1"

	images := newTestImages(t)
	images.PushArch(t, imageRef, map[string]map[string][]byte{
		v1alpha3.PXEArchitectureAMD64: {"vmlinuz": []byte("amd64-vmlinuz")},
		v1alpha3.PXEArchitectureARM64: {"vmlinuz": []byte("arm64-vmlinuz")},
	})
	puller := images.Puller

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

	srv := &HTTPServer{FileResolver: FileResolver{Puller: puller, Reader: fc}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	for _, tt := range []struct {
		ip   string
		want string
	}{
		{ip: "10.0.20.1", want: "amd64-vmlinuz"},
		{ip: "10.0.20.2", want: "arm64-vmlinuz"},
	} {
		req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
		req.Header.Set(testPeerHeader, tt.ip)

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

func TestHTTPServer_503WhenImageUnavailable(t *testing.T) {
	// Cache with NO digest set for the image
	puller := newTestImages(t).Puller

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
		FileResolver: FileResolver{Puller: puller, Reader: fc, DefaultNetbootRef: "ghcr.io/test/pending:v1"},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	// The image is not in the registry -- should get 503
	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set(testPeerHeader, "10.0.5.10")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for unavailable image, got %d", resp.StatusCode)
	}

	if ra := resp.Header.Get("Retry-After"); ra != "5" {
		t.Errorf("expected Retry-After: 5, got %q", ra)
	}
}

func TestResolveFile_ImageNotInRegistry(t *testing.T) {
	puller := newTestImages(t).Puller

	resolver := FileResolver{Puller: puller, Reader: nil}

	_, err := resolver.ResolveFileByPath(t.Context(), "vmlinuz", nil, ImageSource{Ref: "ghcr.io/test/missing:v1"})
	if !errors.Is(err, ErrImageUnavailable) {
		t.Fatalf("expected ErrImageUnavailable, got: %v", err)
	}
}

func TestImageMetadata_ImageNotInRegistry(t *testing.T) {
	resolver := FileResolver{Puller: newTestImages(t).Puller}

	_, err := resolver.ImageMetadata(t.Context(), ImageSource{Ref: "ghcr.io/test/missing:v1", Architecture: "amd64"})
	require.ErrorIs(t, err, ErrImageUnavailable)
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

	puller := newTestImages(t).Puller

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
		FileResolver:   FileResolver{Puller: puller, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /pxe/disable", srv.handleDisablePXE)
	mux.HandleFunc("GET /pxe/disable", srv.handleDisablePXE)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	// Disable PXE via GET (matches busybox wget behavior in initrd)
	req, _ := http.NewRequest("GET", ts.URL+"/pxe/disable", nil)
	req.Header.Set(testPeerHeader, "10.0.6.10")

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
	req.Header.Set(testPeerHeader, "10.0.6.10")

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

	puller := newTestImages(t).Puller
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
		FileResolver:   FileResolver{Puller: puller, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pxe/disable", srv.handleDisablePXE)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/pxe/disable", nil)
	req.Header.Set(testPeerHeader, "10.0.6.11")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /pxe/disable: %v", err)
	}

	resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Empty(t, recorder.pxeDisabledEvents())
}

func TestHTTPServer_DisablePXE_UnknownIP(t *testing.T) {
	puller := newTestImages(t).Puller

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Puller: puller, Reader: fc},
		StatusRecorder: &recordingStatusRecorder{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /pxe/disable", srv.handleDisablePXE)
	mux.HandleFunc("GET /pxe/disable", srv.handleDisablePXE)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/pxe/disable", nil)
	req.Header.Set(testPeerHeader, "10.99.99.99")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /pxe/disable: %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown IP, got %d", resp.StatusCode)
	}
}

func TestFileResolverImageMetadata(t *testing.T) {
	metadataContent := "dhcpBootImageName: shimx64.efi\nhttpBootPath: http/shimx64.efi\n"

	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
		"metadata.yaml": []byte(metadataContent),
	})
	resolver := &FileResolver{Puller: puller}

	meta, err := resolver.ImageMetadata(t.Context(), ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err != nil {
		t.Fatalf("ImageMetadata: %v", err)
	}

	if meta.DHCPBootImageName != "shimx64.efi" {
		t.Errorf("DHCPBootImageName: got %q, want %q", meta.DHCPBootImageName, "shimx64.efi")
	}

	if meta.HTTPBootPath != "http/shimx64.efi" {
		t.Errorf("HTTPBootPath: got %q, want %q", meta.HTTPBootPath, "http/shimx64.efi")
	}
}

func TestFileResolverHTTPBootURL(t *testing.T) {
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
		"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\nhttpBootPath: /efi/bootx64.efi\n"),
	})

	resolver := &FileResolver{Puller: puller, ServeURL: "http://10.0.0.10:8880/base/"}
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-http-url"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{Image: "ghcr.io/test/image:v1"},
		},
	}

	bootURL, err := resolver.HTTPBootURL(t.Context(), node)
	require.NoError(t, err)
	require.Equal(t, "http://10.0.0.10:8880/base/efi/bootx64.efi", bootURL)
}

func TestFileResolverHTTPBootURLFallsBackToDHCPBootImageName(t *testing.T) {
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
		"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\n"),
	})

	resolver := &FileResolver{Puller: puller, ServeURL: "http://10.0.0.10:8880"}
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-http-url"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{Image: "ghcr.io/test/image:v1"},
		},
	}

	bootURL, err := resolver.HTTPBootURL(t.Context(), node)
	require.NoError(t, err)
	require.Equal(t, "http://10.0.0.10:8880/shimx64.efi", bootURL)
}

func TestJoinServeURLPathRejectsTraversal(t *testing.T) {
	_, err := JoinServeURLPath("http://10.0.0.10:8880", "../shimx64.efi")
	require.Error(t, err)
}

func TestFileResolverImageMetadataNoFile(t *testing.T) {
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
		"vmlinuz": []byte("kernel"),
	})
	resolver := &FileResolver{Puller: puller}

	meta, err := resolver.ImageMetadata(t.Context(), ImageSource{Ref: "ghcr.io/test/image:v1"})
	if err != nil {
		t.Fatalf("ImageMetadata: %v", err)
	}

	if meta.DHCPBootImageName != "" {
		t.Errorf("expected empty DHCPBootImageName, got %q", meta.DHCPBootImageName)
	}
}

func TestFileResolverOpenImageFile(t *testing.T) {
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
		"vmlinuz":            []byte("kernel"),
		"grub/grub.cfg.tmpl": []byte("template content"),
	})
	resolver := &FileResolver{Puller: puller}
	src := ImageSource{Ref: "ghcr.io/test/image:v1"}

	// Static file
	file, isTemplate, err := resolver.openImageFile(t.Context(), src, "vmlinuz")
	require.NoError(t, err)
	require.False(t, isTemplate, "vmlinuz should not be a template")
	require.Equal(t, "disk/vmlinuz", file.Name())
	require.NoError(t, file.Close())

	// Template file (.tmpl suffix stripped in request)
	file, isTemplate, err = resolver.openImageFile(t.Context(), src, "grub/grub.cfg")
	require.NoError(t, err)
	require.True(t, isTemplate, "grub/grub.cfg should be resolved as template (via grub/grub.cfg.tmpl)")
	require.NoError(t, file.Close())

	// Not found
	_, _, err = resolver.openImageFile(t.Context(), src, "nonexistent")
	require.ErrorIs(t, err, imagepull.ErrNotFound)
	require.NotErrorIs(t, err, ErrImageUnavailable)
}

func TestFileResolverResolvesFileForArchitecture(t *testing.T) {
	imageRef := "ghcr.io/test/multiarch:v1"

	images := newTestImages(t)
	images.PushArch(t, imageRef, map[string]map[string][]byte{
		v1alpha3.PXEArchitectureAMD64: {"vmlinuz": []byte("amd64 kernel")},
		v1alpha3.PXEArchitectureARM64: {"vmlinuz": []byte("arm64 kernel")},
	})
	resolver := &FileResolver{Puller: images.Puller}

	for arch, want := range map[string]string{
		v1alpha3.PXEArchitectureAMD64: "amd64 kernel",
		v1alpha3.PXEArchitectureARM64: "arm64 kernel",
	} {
		resolved, err := resolver.ResolveFileByPath(t.Context(), "vmlinuz", nil, ImageSource{Ref: imageRef, Architecture: arch})
		require.NoError(t, err, arch)

		data, err := io.ReadAll(resolved.File)
		require.NoError(t, err, arch)
		require.NoError(t, resolved.Close())
		require.Equal(t, want, string(data), arch)
	}
}

func TestFileResolverRejectsPathTraversal(t *testing.T) {
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
		"vmlinuz": []byte("kernel"),
		// A file outside disk/ that a traversal might reach.
		"../secret.txt": []byte("secret"),
	})
	resolver := &FileResolver{Puller: puller}
	src := ImageSource{Ref: "ghcr.io/test/image:v1"}

	traversalCases := []string{
		"../secret.txt",          // escapes disk/ to the image root
		"../../../../etc/passwd", // escapes past the image root
		"sub/../../secret.txt",   // sub-dir traversal escaping disk/
	}

	for _, tc := range traversalCases {
		if _, _, err := resolver.openImageFile(t.Context(), src, tc); err == nil {
			t.Errorf("expected error for traversal path %q, got none", tc)
		}
	}

	// Absolute path must be rejected.
	if _, _, err := resolver.openImageFile(t.Context(), src, "/etc/passwd"); err == nil {
		t.Error("expected error for absolute path, got none")
	}

	// Path resolving to the disk directory itself must be rejected.
	for _, dots := range []string{".", ""} {
		if _, _, err := resolver.openImageFile(t.Context(), src, dots); err == nil {
			t.Errorf("expected error for path %q resolving to the disk directory, got none", dots)
		}
	}

	// Valid relative path must still work.
	file, _, err := resolver.openImageFile(t.Context(), src, "vmlinuz")
	if err != nil {
		t.Fatalf("expected success for valid relative path, got: %v", err)
	}

	file.Close() //nolint:errcheck // test
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

	ts := newTestPeerServer(mux)
	defer ts.Close()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(tt.body))
			req.Header.Set(testPeerHeader, "10.0.1.50")

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

	puller := newTestImages(t).Puller
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
		FileResolver:   FileResolver{Puller: puller, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /unbounded-agent/install-log", srv.handleInstallLog)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	body := "unbounded-agent install exited with status 42\n\nlast 120 lines of /var/log/unbounded-agent-install.log:\nfailed to download agent"
	req, _ := http.NewRequest("POST", ts.URL+"/unbounded-agent/install-log", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set(testPeerHeader, "10.0.20.18")

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

	puller := newTestImages(t).Puller
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Puller: puller, Reader: fc},
		StatusRecorder: &recordingStatusRecorder{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	body := `{"name":"init-local","description":"starting init-local","event_type":"start","origin":"cloudinit","timestamp":1775657336.0}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set(testPeerHeader, "10.0.20.10")

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

	puller := newTestImages(t).Puller
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Puller: puller, Reader: fc},
		StatusRecorder: &recordingStatusRecorder{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	body := `{"name":"modules-final","description":"modules-final ran successfully and took 1.23 seconds","event_type":"finish","origin":"cloudinit","timestamp":1775657336.0,"result":"SUCCESS"}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set(testPeerHeader, "10.0.20.11")

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

	puller := newTestImages(t).Puller
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Puller: puller, Reader: fc},
		StatusRecorder: &recordingStatusRecorder{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	body := `{"name":"modules-config","description":"running modules-config","event_type":"finish","origin":"cloudinit","timestamp":1775657336.0,"result":"FAIL: command [apt-get install -y badpkg] failed with exit code 100"}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set(testPeerHeader, "10.0.20.12")

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

	ts := newTestPeerServer(mux)
	defer ts.Close()

	body := `{"name":"modules-final","description":"done","event_type":"finish","origin":"cloudinit","timestamp":1775657336.0,"result":"SUCCESS"}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set(testPeerHeader, "10.0.20.99")

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
	puller := newTestImages(t).Puller
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:         fc,
		FileResolver:   FileResolver{Puller: puller, Reader: fc},
		StatusRecorder: &recordingStatusRecorder{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	body := `{"name":"modules-final","description":"done","event_type":"finish","origin":"cloudinit","timestamp":1775657336.0,"result":"SUCCESS"}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set(testPeerHeader, "10.99.99.99")

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

	puller := newTestImages(t).Puller
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
		FileResolver:   FileResolver{Puller: puller, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	body := `{"name":"modules-final","description":"done","event_type":"finish","origin":"cloudinit","timestamp":1775657336.0,"result":"SUCCESS"}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set(testPeerHeader, "10.0.20.15")

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

	puller := newTestImages(t).Puller
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
		FileResolver:   FileResolver{Puller: puller, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	postEvent := func(body string) {
		t.Helper()

		req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
		req.Header.Set(testPeerHeader, "10.0.20.13")

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

	puller := newTestImages(t).Puller
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
		FileResolver:   FileResolver{Puller: puller, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	postEvent := func(body string) {
		t.Helper()

		req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
		req.Header.Set(testPeerHeader, "10.0.20.16")

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

	puller := newTestImages(t).Puller
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
		FileResolver:   FileResolver{Puller: puller, Reader: fc},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", srv.handleCloudInitLog)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	body := `{"name":"modules-final","description":"running modules-final","event_type":"finish","origin":"cloudinit","timestamp":1.0,"result":"FAIL: command [apt-get install -y badpkg] failed with exit code 100"}`
	req, _ := http.NewRequest("POST", ts.URL+"/cloudinit/log", strings.NewReader(body))
	req.Header.Set(testPeerHeader, "10.0.20.17")

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

func TestCleanRequestPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"", "/"},
		{"/", "/"},
		{"/bootx64.efi", "/bootx64.efi"},
		{"//grubx64.efi", "/grubx64.efi"},
		{"/a//b", "/a/b"},
		{"/dir/", "/dir"},
		{"/dir//", "/dir"},
		{"/grub/./grub.cfg/", "/grub/grub.cfg"},
		{"/unused/../vmlinuz//", "/vmlinuz"},
		{"//cloud-init//vendor-data/", "/cloud-init/vendor-data"},
		{"/cloud-init/vendor-data/child/..", "/cloud-init/vendor-data"},
		{"/../../", "/"},
	}

	for _, tc := range cases {
		if got := cleanRequestPath(tc.in); got != tc.want {
			t.Errorf("cleanRequestPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNormalizePathServesUncleanPath verifies that a request whose raw target
// contains a double slash (as shim generates for its second-stage loader when
// the shim is served from the web root, e.g. "//grubx64.efi") is normalized in
// place and served directly with a 200, instead of the ServeMux emitting a 307
// redirect that shim would refuse to follow (EFI_HTTP_ERROR / 0x23).
func TestNormalizePathServesUncleanPath(t *testing.T) {
	t.Parallel()

	var handlerHits []string

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		handlerHits = append(handlerHits, r.URL.Path)

		w.WriteHeader(http.StatusOK)
	})

	ts := httptest.NewServer(normalizePath(mux))
	defer ts.Close()

	host := strings.TrimPrefix(ts.URL, "http://")

	statusLine := func(target string) string {
		t.Helper()

		conn, err := net.Dial("tcp", host)
		require.NoError(t, err)

		defer conn.Close()

		_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target, host)
		require.NoError(t, err)

		buf := make([]byte, 256)
		n, _ := conn.Read(buf)

		line := string(buf[:n])
		if i := strings.Index(line, "\r\n"); i >= 0 {
			line = line[:i]
		}

		return line
	}

	require.Contains(t, statusLine("/bootx64.efi"), "200")
	// The double-slash request must be normalized and served (200), not redirected.
	require.Contains(t, statusLine("//grubx64.efi"), "200")

	// The handler must have seen both requests with clean, collapsed paths.
	require.Equal(t, []string{"/bootx64.efi", "/grubx64.efi"}, handlerHits)
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

func TestResolveFile_AgentConfigPinsNodeName(t *testing.T) {
	cases := []struct {
		name       string
		kubernetes *v1alpha3.KubernetesSpec
		want       string
	}{
		{name: "defaults to machine name", want: "node-pin"},
		{name: "empty nodeRef falls back", kubernetes: &v1alpha3.KubernetesSpec{NodeRef: &v1alpha3.LocalObjectReference{}}, want: "node-pin"},
		{name: "nodeRef override", kubernetes: &v1alpha3.KubernetesSpec{NodeRef: &v1alpha3.LocalObjectReference{Name: "k8s-node-7"}}, want: "k8s-node-7"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
				"agent.json.tmpl": []byte(`{{ .AgentConfigJSON }}`),
			})
			node := &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-pin"},
				Spec: v1alpha3.MachineSpec{
					Kubernetes: tc.kubernetes,
					PXE: &v1alpha3.PXESpec{
						Image:      "ghcr.io/test/image:v1",
						DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f9", IPv4: "10.0.1.19", SubnetMask: "255.255.255.0"}},
					},
				},
			}
			f := &FileResolver{
				Puller:  puller,
				Cluster: &StaticClusterInfo{Info: ClusterInfo{ApiserverURL: "https://k8s.example.com"}},
			}

			resolved, err := f.ResolveFileByPath(t.Context(), "agent.json", node, ImageSource{Ref: "ghcr.io/test/image:v1"})
			require.NoError(t, err)

			var cfg provision.UnboundedAgentConfig
			require.NoError(t, json.Unmarshal(resolved.Data, &cfg))
			require.Equal(t, tc.want, cfg.NodeName)
			require.Equal(t, "node-pin", cfg.MachineName)
		})
	}
}

func TestHTTPServer_HTTPBootLoaderGateFailsClosedOnMetadataError(t *testing.T) {
	const ref = "ghcr.io/test/image:v1"

	puller := setupImagePuller(t, ref, map[string][]byte{
		"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\nhttpBootPath: shimx64.efi\n"),
		"shimx64.efi":   []byte("shim"),
		"vmlinuz":       []byte("kernel"),
	})

	for _, path := range []string{"shimx64.efi", "vmlinuz"} {
		t.Run(path, func(t *testing.T) {
			node := &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-http"},
				Spec: v1alpha3.MachineSpec{
					PXE: &v1alpha3.PXESpec{
						Image:        ref,
						BootProtocol: v1alpha3.PXEBootProtocolHTTP,
						DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:10", IPv4: "10.0.1.60", SubnetMask: "255.255.255.0"}},
					},
				},
			}

			fc := fake.NewClientBuilder().
				WithScheme(newScheme(t)).
				WithObjects(node).
				WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
				Build()
			recorder := &recordingStatusRecorder{}
			srv := &HTTPServer{
				FileResolver: FileResolver{
					Puller:   puller,
					Reader:   fc,
					Metadata: &MetadataMemo{},
				},
				StatusRecorder: recorder,
			}

			// Seed the memo with a transient failure while the registry
			// itself still serves files.
			seedPinnedMetadataFailure(t, &srv.FileResolver, srv.NetbootImageSource(node))

			mux := http.NewServeMux()
			mux.HandleFunc("GET /", srv.handleFile)

			ts := newTestPeerServer(mux)
			defer ts.Close()

			req, _ := http.NewRequest("GET", ts.URL+"/"+path, nil)
			req.Header.Set(testPeerHeader, "10.0.1.60")

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)

			require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			require.Equal(t, "5", resp.Header.Get("Retry-After"))
			require.NotContains(t, string(body), "shim")
			require.NotContains(t, string(body), "kernel")
			require.Empty(t, recorder.bootLoaderEvents())
		})
	}
}

func TestHTTPServer_HTTPBootWithoutMetadataServesFiles(t *testing.T) {
	puller := setupImagePuller(t, "ghcr.io/test/image:v1", map[string][]byte{
		"vmlinuz": []byte("kernel"),
	})

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

	fc := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()
	recorder := &recordingStatusRecorder{}
	srv := &HTTPServer{
		FileResolver:   FileResolver{Puller: puller, Reader: fc, Metadata: &MetadataMemo{}},
		StatusRecorder: recorder,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := newTestPeerServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/vmlinuz", nil)
	req.Header.Set(testPeerHeader, "10.0.1.60")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, []byte("kernel"), body)
	require.Empty(t, recorder.bootLoaderEvents())
}

func TestHTTPServer_DiskDownloadClassifiedWithNetbootImage(t *testing.T) {
	images := newTestImages(t)
	images.Push(t, "ghcr.io/test/os:v1", map[string][]byte{"disk.img.gz": []byte("disk")})
	images.Push(t, "ghcr.io/test/netboot:v1", map[string][]byte{
		"metadata.yaml": []byte("dhcpBootImageName: shimx64.efi\nhttpBootPath: shimx64.efi\n"),
		"shimx64.efi":   []byte("shim"),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-split"},
		Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
			Image:        "ghcr.io/test/os:v1",
			NetbootImage: "ghcr.io/test/netboot:v1",
			BootProtocol: v1alpha3.PXEBootProtocolHTTP,
			DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:11", IPv4: "10.0.1.61", SubnetMask: "255.255.255.0"}},
		}},
	}

	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).Build()
	srv := &HTTPServer{FileResolver: FileResolver{Puller: images.Puller, Reader: fc, Metadata: &MetadataMemo{}}}

	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "10.0.1.61:4321"
		w := httptest.NewRecorder()
		normalizePath(http.HandlerFunc(srv.handleFile)).ServeHTTP(w, r)

		return w
	}

	// A metadata lookup against the OS image would fail; the disk download
	// must not depend on it.
	_, err := srv.Metadata.get(t.Context(), DiskImageSource(node), func(context.Context, ImageSource) (*ImageMetadata, error) {
		return nil, fmt.Errorf("%w: disk metadata timed out", ErrImageUnavailable)
	})
	require.Error(t, err)

	w := get("/disk.img.gz")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "disk", w.Body.String())

	// The boot loader is still classified from the netboot image and gated.
	w = get("/shimx64.efi")
	require.Equal(t, http.StatusNotFound, w.Code)

	// A netboot metadata failure fails the disk download closed too.
	srv.Metadata = &MetadataMemo{}
	seedPinnedMetadataFailure(t, &srv.FileResolver, srv.NetbootImageSource(node))

	w = get("/disk.img.gz")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// seedPinnedMetadataFailure memoizes a transient metadata failure for the
// digest src currently resolves to, while the registry still serves files.
func seedPinnedMetadataFailure(t *testing.T, f *FileResolver, src ImageSource) {
	t.Helper()

	img, err := f.resolveImage(t.Context(), src)
	require.NoError(t, err)

	_, err = f.Metadata.getPinned(t.Context(), img.Digest(), func(context.Context) (*ImageMetadata, error) {
		return nil, fmt.Errorf("%w: registry down", ErrImageUnavailable)
	})
	require.Error(t, err)
}

func TestHTTPServer_TagMoveKeepsClassificationAndContentOnOneDigest(t *testing.T) {
	const ref = "ghcr.io/test/netboot:v1"

	images := newTestImages(t)
	images.Push(t, ref, map[string][]byte{
		"metadata.yaml": []byte("httpBootPath: shimA.efi\n"),
		"shimA.efi":     []byte("loader-a"),
		"shimB.efi":     []byte("stale-b"),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-move"},
		Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
			Image:        ref,
			BootProtocol: v1alpha3.PXEBootProtocolHTTP,
			DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:12", IPv4: "10.0.1.62", SubnetMask: "255.255.255.0"}},
		}},
	}

	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).Build()
	srv := &HTTPServer{FileResolver: FileResolver{Puller: images.Puller, Reader: fc, Metadata: &MetadataMemo{TTL: time.Hour}}}

	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "10.0.1.62:4321"
		w := httptest.NewRecorder()
		normalizePath(http.HandlerFunc(srv.handleFile)).ServeHTTP(w, r)

		return w
	}

	// Image A: shimA.efi is the gated loader, shimB.efi is a plain file.
	require.Equal(t, http.StatusNotFound, get("/shimA.efi").Code)

	w := get("/shimB.efi")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "stale-b", w.Body.String())

	// Move the tag to image B, whose loader is shimB.efi. The memo still
	// holds A's metadata for an hour, yet the request must be classified
	// with B's metadata because its content comes from B.
	images.Push(t, ref, map[string][]byte{
		"metadata.yaml": []byte("httpBootPath: shimB.efi\n"),
		"shimB.efi":     []byte("loader-b"),
	})

	w = get("/shimB.efi")
	require.Equal(t, http.StatusNotFound, w.Code, "new loader must be gated without an active install")
	require.NotContains(t, w.Body.String(), "loader-b")
	require.NotContains(t, w.Body.String(), "stale-b")

	// The old loader is gone from B and is no longer classified as a loader.
	w = get("/shimA.efi")
	require.Equal(t, http.StatusNotFound, w.Code)
	require.NotContains(t, w.Body.String(), "loader-a")
}

func TestPinnedImageMetadataIsKeyedByDigest(t *testing.T) {
	const ref = "ghcr.io/test/netboot:v1"

	images := newTestImages(t)
	images.Push(t, ref, map[string][]byte{"metadata.yaml": []byte("httpBootPath: a.efi\n")})

	resolver := &FileResolver{Puller: images.Puller, Metadata: &MetadataMemo{TTL: time.Hour}}
	src := ImageSource{Ref: ref}

	imgA, err := resolver.resolveImage(t.Context(), src)
	require.NoError(t, err)

	meta, err := resolver.PinnedImageMetadata(t.Context(), imgA)
	require.NoError(t, err)
	require.Equal(t, "a.efi", meta.HTTPBootPath)

	images.Push(t, ref, map[string][]byte{"metadata.yaml": []byte("httpBootPath: b.efi\n")})

	imgB, err := resolver.resolveImage(t.Context(), src)
	require.NoError(t, err)
	require.NotEqual(t, imgA.Digest(), imgB.Digest())

	meta, err = resolver.PinnedImageMetadata(t.Context(), imgB)
	require.NoError(t, err)
	require.Equal(t, "b.efi", meta.HTTPBootPath)

	// The old pinned image still reports its own metadata.
	meta, err = resolver.PinnedImageMetadata(t.Context(), imgA)
	require.NoError(t, err)
	require.Equal(t, "a.efi", meta.HTTPBootPath)

	// Without a memo, metadata is read directly.
	direct := &FileResolver{Puller: images.Puller}
	meta, err = direct.PinnedImageMetadata(t.Context(), imgB)
	require.NoError(t, err)
	require.Equal(t, "b.efi", meta.HTTPBootPath)
}
