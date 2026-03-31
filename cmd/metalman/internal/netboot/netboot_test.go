package netboot

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
	"github.com/project-unbounded/unbounded-kube/cmd/metalman/internal/indexing"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	return s
}

func TestImageReconciler_DownloadAndCache(t *testing.T) {
	vmlinuzData := []byte("fake-vmlinuz-content-1234567890")
	initrdData := []byte("fake-initrd-content-0987654321")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vmlinuz":
			w.Write(vmlinuzData)
		case "/initrd":
			w.Write(initrdData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	image := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "test-image"},
		Spec: v1alpha3.ImageSpec{
			DHCPBootImageName: "shimx64.efi",
			Files: []v1alpha3.File{
				{
					Path: "images/test-image/vmlinuz",
					HTTP: &v1alpha3.HTTPSource{
						URL:    ts.URL + "/vmlinuz",
						SHA256: sha256Hex(vmlinuzData),
					},
				},
				{
					Path: "images/test-image/initrd",
					HTTP: &v1alpha3.HTTPSource{
						URL:    ts.URL + "/initrd",
						SHA256: sha256Hex(initrdData),
					},
				},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(image).Build()
	cacheDir := t.TempDir()

	reconciler := &ImageReconciler{
		Client:   fc,
		CacheDir: cacheDir,
	}

	result, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-image"},
	})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("unexpected requeue: %v", result.RequeueAfter)
	}

	gotVmlinuz, err := os.ReadFile(cachePath(cacheDir, sha256Hex(vmlinuzData), ""))
	if err != nil {
		t.Fatalf("reading vmlinuz: %v", err)
	}

	if string(gotVmlinuz) != string(vmlinuzData) {
		t.Errorf("vmlinuz content mismatch: got %q", gotVmlinuz)
	}

	gotInitrd, err := os.ReadFile(cachePath(cacheDir, sha256Hex(initrdData), ""))
	if err != nil {
		t.Fatalf("reading initrd: %v", err)
	}

	if string(gotInitrd) != string(initrdData) {
		t.Errorf("initrd content mismatch: got %q", gotInitrd)
	}

	// Re-reconcile should be a no-op (files already cached with correct checksum)
	result, err = reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-image"},
	})
	if err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("unexpected requeue on second reconcile: %v", result.RequeueAfter)
	}
}

func TestImageReconciler_ChecksumMismatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("actual-content"))
	}))
	defer ts.Close()

	image := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-image"},
		Spec: v1alpha3.ImageSpec{
			DHCPBootImageName: "shimx64.efi",
			Files: []v1alpha3.File{
				{
					Path: "images/bad-image/vmlinuz",
					HTTP: &v1alpha3.HTTPSource{
						URL:    ts.URL + "/vmlinuz",
						SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
					},
				},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(image).Build()
	cacheDir := t.TempDir()

	reconciler := &ImageReconciler{
		Client:   fc,
		CacheDir: cacheDir,
	}

	result, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "bad-image"},
	})
	if err != nil {
		t.Fatalf("reconcile should not return error for checksum mismatch: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("should not requeue on checksum mismatch: %v", result.RequeueAfter)
	}

	_, err = os.Stat(cachePath(cacheDir, "0000000000000000000000000000000000000000000000000000000000000000", ""))
	if err == nil {
		t.Error("file should not exist after checksum mismatch")
	}
}

func TestImageReconciler_Deletion(t *testing.T) {
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	reconciler := &ImageReconciler{
		Client:   fc,
		CacheDir: t.TempDir(),
	}

	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "deleted-image"},
	})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
}

func TestHTTPServer_ServeFiles(t *testing.T) {
	vmlinuzData := []byte("test-vmlinuz-binary-data")
	cacheDir := t.TempDir()

	os.MkdirAll(filepath.Join(cacheDir, "sha256"), 0o755)
	os.WriteFile(cachePath(cacheDir, sha256Hex(vmlinuzData), ""), vmlinuzData, 0o644)

	img := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "test-image"},
		Spec: v1alpha3.ImageSpec{
			Files: []v1alpha3.File{
				{
					Path: "images/test-image/vmlinuz",
					HTTP: &v1alpha3.HTTPSource{URL: "https://example.com/vmlinuz", SHA256: sha256Hex(vmlinuzData)},
				},
			},
		},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-serve"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				ImageRef:   v1alpha3.LocalObjectReference{Name: "test-image"},
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:01", IPv4: "10.0.1.50", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, img).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			CacheDir: cacheDir,
			Reader:   fc,
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
	req, _ := http.NewRequest("GET", ts.URL+"/images/test-image/vmlinuz", nil)
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
	req, _ = http.NewRequest("GET", ts.URL+"/images/nonexistent/foo", nil)
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
	resp, err = http.Get(ts.URL + "/images/test-image/vmlinuz")
	if err != nil {
		t.Fatalf("GET vmlinuz (unknown IP): %v", err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown IP status: got %d, want 404", resp.StatusCode)
	}
}

func TestHTTPServer_TemplateRendered(t *testing.T) {
	cacheDir := t.TempDir()

	bootTemplate := `set default=0
menuentry "Install" {
  linux /images/{{ .Image.Name }}/vmlinuz hostname={{ .Machine.Name }} ip={{ (index .Machine.Spec.PXE.DHCPLeases 0).IPv4 }}
}`

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				ImageRef:   v1alpha3.LocalObjectReference{Name: "test-image"},
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f0", IPv4: "10.0.1.10", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
		},
	}

	img := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "test-image"},
		Spec: v1alpha3.ImageSpec{
			DHCPBootImageName: "shimx64.efi",
			Files: []v1alpha3.File{
				{
					Path:     "boot.cfg",
					Template: &v1alpha3.TemplateSource{Content: bootTemplate},
				},
			},
		},
	}

	scheme := newScheme(t)

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, img).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			CacheDir:     cacheDir,
			Reader:       fc,
			ApiserverURL: "https://k8s.example.com",
			ServeURL:     "http://10.0.1.1:8080",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/boot.cfg", nil)
	req.Header.Set("X-Forwarded-For", "10.0.1.10")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /boot.cfg: %v", err)
	}

	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("boot.cfg status: got %d, want 200, body: %s", resp.StatusCode, body)
	}

	bodyStr := string(body)
	if !strings.Contains(bodyStr, "hostname=node-01") {
		t.Errorf("rendered config should contain hostname=node-01, got:\n%s", bodyStr)
	}

	if !strings.Contains(bodyStr, "ip=10.0.1.10") {
		t.Errorf("rendered config should contain ip=10.0.1.10, got:\n%s", bodyStr)
	}

	if !strings.Contains(bodyStr, "/images/test-image/vmlinuz") {
		t.Errorf("rendered config should contain image name, got:\n%s", bodyStr)
	}
}

func TestHTTPServer_TemplateVerbatim(t *testing.T) {
	cacheDir := t.TempDir()
	staticConfig := "network:\n  version: 2\n  ethernets:\n    eth0:\n      dhcp4: false\n"

	img := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "test-image"},
		Spec: v1alpha3.ImageSpec{
			Files: []v1alpha3.File{
				{
					Path:     "configs/test-image/network-config",
					Template: &v1alpha3.TemplateSource{Content: staticConfig},
				},
			},
		},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-verbatim"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				ImageRef:   v1alpha3.LocalObjectReference{Name: "test-image"},
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:02", IPv4: "10.0.1.51", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, img).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			CacheDir: cacheDir,
			Reader:   fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/configs/test-image/network-config", nil)
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
	cacheDir := t.TempDir()
	staticContent := "autoinstall:\n  version: 1\n  identity:\n    hostname: server\n"

	img := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "test-image"},
		Spec: v1alpha3.ImageSpec{
			Files: []v1alpha3.File{
				{
					Path:   "configs/test-image/autoinstall",
					Static: &v1alpha3.StaticSource{Content: staticContent},
				},
			},
		},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-static"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				ImageRef:   v1alpha3.LocalObjectReference{Name: "test-image"},
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:03", IPv4: "10.0.1.52", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, img).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			CacheDir: cacheDir,
			Reader:   fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/configs/test-image/autoinstall", nil)
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

	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type: got %q, want %q", ct, "text/plain")
	}
}

func TestHTTPServer_UnknownSourceIP(t *testing.T) {
	cacheDir := t.TempDir()

	vmlinuzData := []byte("some-binary-data")

	os.MkdirAll(filepath.Join(cacheDir, "sha256"), 0o755)
	os.WriteFile(cachePath(cacheDir, sha256Hex(vmlinuzData), ""), vmlinuzData, 0o644)

	img := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "test-image"},
		Spec: v1alpha3.ImageSpec{
			Files: []v1alpha3.File{
				{
					Path: "images/test-image/vmlinuz",
					HTTP: &v1alpha3.HTTPSource{URL: "https://example.com/vmlinuz", SHA256: sha256Hex(vmlinuzData)},
				},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(img).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			CacheDir: cacheDir,
			Reader:   fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// No node registered -- requests from any IP should get 404
	req, _ := http.NewRequest("GET", ts.URL+"/images/test-image/vmlinuz", nil)
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

func TestTemplateRendering(t *testing.T) {
	tmpl := `Node: {{ .Machine.Name }}, Image: {{ .Image.Name }}, API: {{ .ApiserverURL }}, Serve: {{ .ServeURL }}`

	data := templateData{
		Machine: &v1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		},
		Image: &v1alpha3.Image{
			ObjectMeta: metav1.ObjectMeta{Name: "test-image"},
		},
		ApiserverURL: "https://k8s.example.com",
		ServeURL:     "http://10.0.1.1:8080",
	}

	result, err := renderTemplate(tmpl, data)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}

	expected := "Node: test-node, Image: test-image, API: https://k8s.example.com, Serve: http://10.0.1.1:8080"
	if string(result) != expected {
		t.Errorf("template result: got %q, want %q", result, expected)
	}
}

func TestHTTPServer_Start_Shutdown(t *testing.T) {
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	port := freePort(t)
	srv := &HTTPServer{
		BindAddr: "127.0.0.1",
		Port:     port,
		FileResolver: FileResolver{
			CacheDir: t.TempDir(),
			Reader:   fc,
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

func TestImageReconciler_UpdateFile(t *testing.T) {
	data1 := []byte("version-1")
	data2 := []byte("version-2")

	var serveData []byte

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(serveData)
	}))
	defer ts.Close()

	serveData = data1
	image := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "update-image"},
		Spec: v1alpha3.ImageSpec{
			DHCPBootImageName: "shimx64.efi",
			Files: []v1alpha3.File{{
				Path: "images/update-image/file.bin",
				HTTP: &v1alpha3.HTTPSource{URL: ts.URL + "/file.bin", SHA256: sha256Hex(data1)},
			}},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(image).Build()
	cacheDir := t.TempDir()

	reconciler := &ImageReconciler{Client: fc, CacheDir: cacheDir}

	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "update-image"},
	})
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	got, _ := os.ReadFile(cachePath(cacheDir, sha256Hex(data1), ""))
	if string(got) != string(data1) {
		t.Fatalf("first reconcile: got %q, want %q", got, data1)
	}

	// Update the image spec with new data
	serveData = data2

	var current v1alpha3.Image
	fc.Get(t.Context(), types.NamespacedName{Name: "update-image"}, &current)
	current.Spec.Files[0].HTTP.SHA256 = sha256Hex(data2)
	fc.Update(t.Context(), &current)

	_, err = reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "update-image"},
	})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	got, _ = os.ReadFile(cachePath(cacheDir, sha256Hex(data2), ""))
	if string(got) != string(data2) {
		t.Errorf("second reconcile: got %q, want %q", got, data2)
	}
}

func TestTFTPServer_ResolveFileByPath(t *testing.T) {
	vmlinuzData := []byte("tftp-vmlinuz-data")
	cacheDir := t.TempDir()

	os.MkdirAll(filepath.Join(cacheDir, "sha256"), 0o755)
	os.WriteFile(cachePath(cacheDir, sha256Hex(vmlinuzData), ""), vmlinuzData, 0o644)

	img := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "test-image"},
		Spec: v1alpha3.ImageSpec{
			Files: []v1alpha3.File{{
				Path: "images/test-image/vmlinuz",
				HTTP: &v1alpha3.HTTPSource{URL: "https://example.com/vmlinuz", SHA256: sha256Hex(vmlinuzData)},
			}},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(img).Build()

	srv := &TFTPServer{
		FileResolver: FileResolver{
			CacheDir: cacheDir,
			Reader:   fc,
		},
	}

	resolved, err := srv.ResolveFileByPath(t.Context(), "images/test-image/vmlinuz", nil, "test-image")
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if resolved.DiskPath == "" {
		t.Fatal("expected DiskPath to be set for HTTP-sourced file")
	}

	data, err := os.ReadFile(resolved.DiskPath)
	if err != nil {
		t.Fatalf("reading resolved disk path: %v", err)
	}

	if string(data) != string(vmlinuzData) {
		t.Errorf("data mismatch: got %q", data)
	}

	// Test not found (wrong image)
	_, err = srv.ResolveFileByPath(t.Context(), "images/test-image/vmlinuz", nil, "nonexistent-image")
	if err == nil {
		t.Error("expected error for nonexistent image")
	}

	// Test not found (wrong path)
	_, err = srv.ResolveFileByPath(t.Context(), "images/nonexistent/foo", nil, "test-image")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestTFTPServer_TemplateVerbatim(t *testing.T) {
	cacheDir := t.TempDir()
	staticData := "static-config-data"

	img := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "test-image"},
		Spec: v1alpha3.ImageSpec{
			Files: []v1alpha3.File{{
				Path:     "configs/static",
				Template: &v1alpha3.TemplateSource{Content: staticData},
			}},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(img).Build()

	srv := &TFTPServer{
		FileResolver: FileResolver{
			CacheDir: cacheDir,
			Reader:   fc,
		},
	}

	resolved, err := srv.ResolveFileByPath(t.Context(), "configs/static", nil, "test-image")
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if string(resolved.Data) != staticData {
		t.Errorf("data mismatch: got %q, want %q", resolved.Data, staticData)
	}
}

func TestTFTPServer_StaticFile(t *testing.T) {
	cacheDir := t.TempDir()
	staticData := "static-config-no-template"

	img := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "test-image"},
		Spec: v1alpha3.ImageSpec{
			Files: []v1alpha3.File{{
				Path:   "configs/static",
				Static: &v1alpha3.StaticSource{Content: staticData},
			}},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(img).Build()

	srv := &TFTPServer{
		FileResolver: FileResolver{
			CacheDir: cacheDir,
			Reader:   fc,
		},
	}

	resolved, err := srv.ResolveFileByPath(t.Context(), "configs/static", nil, "test-image")
	if err != nil {
		t.Fatalf("ResolveFileByPath: %v", err)
	}

	if string(resolved.Data) != staticData {
		t.Errorf("data mismatch: got %q, want %q", resolved.Data, staticData)
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
  linux /images/{{ .Image.Name }}/vmlinuz
  initrd /images/{{ .Image.Name }}/initrd
}`
	staticConfig := "autoinstall: true"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vmlinuz":
			w.Write(vmlinuzData)
		case "/initrd":
			w.Write(initrdData)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				ImageRef:   v1alpha3.LocalObjectReference{Name: "e2e-image"},
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:00:11:22", IPv4: "10.0.3.10", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
		},
	}

	img := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-image"},
		Spec: v1alpha3.ImageSpec{
			DHCPBootImageName: "shimx64.efi",
			Files: []v1alpha3.File{
				{
					Path: "images/e2e-image/vmlinuz",
					HTTP: &v1alpha3.HTTPSource{URL: ts.URL + "/vmlinuz", SHA256: sha256Hex(vmlinuzData)},
				},
				{
					Path: "images/e2e-image/initrd",
					HTTP: &v1alpha3.HTTPSource{URL: ts.URL + "/initrd", SHA256: sha256Hex(initrdData)},
				},
				{
					Path:     "boot.cfg",
					Template: &v1alpha3.TemplateSource{Content: bootTemplate},
				},
				{
					Path:   "configs/e2e-image/autoinstall",
					Static: &v1alpha3.StaticSource{Content: staticConfig},
				},
			},
		},
	}

	cacheDir := t.TempDir()

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(img, node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	// Step 1: Reconcile to download HTTP files
	reconciler := &ImageReconciler{Client: fc, CacheDir: cacheDir}

	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "e2e-image"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Verify files are cached
	gotVmlinuz, _ := os.ReadFile(cachePath(cacheDir, sha256Hex(vmlinuzData), ""))
	if string(gotVmlinuz) != string(vmlinuzData) {
		t.Fatalf("vmlinuz mismatch after reconcile")
	}

	// Step 2: Create HTTP server and test serving
	httpSrv := &HTTPServer{
		FileResolver: FileResolver{
			CacheDir:     cacheDir,
			Reader:       fc,
			ApiserverURL: "https://k8s.example.com",
			ServeURL:     "http://10.0.3.1:8080",
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

	// Test vmlinuz via HTTP (with source IP identification)
	req, _ := http.NewRequest("GET", httpTS.URL+"/images/e2e-image/vmlinuz", nil)
	req.Header.Set("X-Forwarded-For", "10.0.3.10")
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != string(vmlinuzData) {
		t.Errorf("HTTP vmlinuz mismatch")
	}

	// Test boot config with IP identification
	req, _ = http.NewRequest("GET", httpTS.URL+"/boot.cfg", nil)
	req.Header.Set("X-Forwarded-For", "10.0.3.10")
	resp, _ = http.DefaultClient.Do(req)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), "Install e2e-node") {
		t.Errorf("boot.cfg should contain node name, got:\n%s", body)
	}

	// Test static template serving (with source IP identification)
	req, _ = http.NewRequest("GET", httpTS.URL+"/configs/e2e-image/autoinstall", nil)
	req.Header.Set("X-Forwarded-For", "10.0.3.10")
	resp, _ = http.DefaultClient.Do(req)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != staticConfig {
		t.Errorf("static config mismatch: got %q", body)
	}

	// Test 404 for file not in this node's image
	req, _ = http.NewRequest("GET", httpTS.URL+"/images/nonexistent/foo", nil)
	req.Header.Set("X-Forwarded-For", "10.0.3.10")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent, got %d", resp.StatusCode)
	}
}

func TestHTTPServer_CrossImageIsolation(t *testing.T) {
	cacheDir := t.TempDir()

	alphaData := []byte("alpha-vmlinuz")
	betaData := []byte("beta-vmlinuz")

	os.MkdirAll(filepath.Join(cacheDir, "sha256"), 0o755)
	os.WriteFile(cachePath(cacheDir, sha256Hex(alphaData), ""), alphaData, 0o644)
	os.WriteFile(cachePath(cacheDir, sha256Hex(betaData), ""), betaData, 0o644)

	alphaImage := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-image"},
		Spec: v1alpha3.ImageSpec{
			Files: []v1alpha3.File{{
				Path: "images/alpha-image/vmlinuz",
				HTTP: &v1alpha3.HTTPSource{URL: "https://example.com/alpha", SHA256: sha256Hex(alphaData)},
			}},
		},
	}
	betaImage := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "beta-image"},
		Spec: v1alpha3.ImageSpec{
			Files: []v1alpha3.File{{
				Path: "images/beta-image/vmlinuz",
				HTTP: &v1alpha3.HTTPSource{URL: "https://example.com/beta", SHA256: sha256Hex(betaData)},
			}},
		},
	}

	alphaNode := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				ImageRef:   v1alpha3.LocalObjectReference{Name: "alpha-image"},
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:aa:aa:aa:aa:aa", IPv4: "10.0.10.1", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
		},
	}
	betaNode := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "beta-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				ImageRef:   v1alpha3.LocalObjectReference{Name: "beta-image"},
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "bb:bb:bb:bb:bb:bb", IPv4: "10.0.10.2", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(alphaNode, betaNode, alphaImage, betaImage).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{
			CacheDir: cacheDir,
			Reader:   fc,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Alpha node can access its own image's files
	req, _ := http.NewRequest("GET", ts.URL+"/images/alpha-image/vmlinuz", nil)
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

	// Alpha node CANNOT access beta's image files
	req, _ = http.NewRequest("GET", ts.URL+"/images/beta-image/vmlinuz", nil)
	req.Header.Set("X-Forwarded-For", "10.0.10.1")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("alpha accessing beta image: got %d, want 404", resp.StatusCode)
	}

	// Beta node can access its own image's files
	req, _ = http.NewRequest("GET", ts.URL+"/images/beta-image/vmlinuz", nil)
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

	// Beta node CANNOT access alpha's image files
	req, _ = http.NewRequest("GET", ts.URL+"/images/alpha-image/vmlinuz", nil)
	req.Header.Set("X-Forwarded-For", "10.0.10.2")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("beta accessing alpha image: got %d, want 404", resp.StatusCode)
	}
}

func TestHTTPServer_503WhenFileNotDownloaded(t *testing.T) {
	cacheDir := t.TempDir()

	img := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-image"},
		Spec: v1alpha3.ImageSpec{
			Files: []v1alpha3.File{{
				Path: "images/pending-image/vmlinuz",
				HTTP: &v1alpha3.HTTPSource{URL: "https://example.com/vmlinuz", SHA256: "abc123"},
			}},
		},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				ImageRef:   v1alpha3.LocalObjectReference{Name: "pending-image"},
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:10", IPv4: "10.0.5.10", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, img).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{CacheDir: cacheDir, Reader: fc},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleFile)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// File exists in image spec but not on disk -- should get 503
	req, _ := http.NewRequest("GET", ts.URL+"/images/pending-image/vmlinuz", nil)
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
	cacheDir := t.TempDir()

	resolver := FileResolver{CacheDir: cacheDir, Reader: nil}

	file := v1alpha3.File{
		Path: "images/test-image/vmlinuz",
		HTTP: &v1alpha3.HTTPSource{URL: "https://example.com/vmlinuz", SHA256: "abc123"},
	}

	_, err := resolver.resolveFile(file, nil, nil)
	if !errors.Is(err, ErrNotYetDownloaded) {
		t.Fatalf("expected ErrNotYetDownloaded, got: %v", err)
	}
}

func TestHTTPServer_DisablePXE(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "pxe-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				ImageRef:   v1alpha3.LocalObjectReference{Name: "test-image"},
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:20", IPv4: "10.0.6.10", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:       fc,
		FileResolver: FileResolver{CacheDir: t.TempDir(), Reader: fc},
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

	// Verify the Machine status was patched so reimage counter matches spec
	var updated v1alpha3.Machine
	if err := fc.Get(t.Context(), types.NamespacedName{Name: "pxe-node"}, &updated); err != nil {
		t.Fatalf("getting updated node: %v", err)
	}

	var specReimage, statusReimage int64
	if updated.Spec.Operations != nil {
		specReimage = updated.Spec.Operations.ReimageCounter
	}

	if updated.Status.Operations != nil {
		statusReimage = updated.Status.Operations.ReimageCounter
	}

	if statusReimage != specReimage {
		t.Errorf("status.operations.reimageCounter (%d) should match spec.operations.reimageCounter (%d)",
			statusReimage, specReimage)
	}

	reimagedCond := findCondition(updated.Status.Conditions, v1alpha3.MachineConditionReimaged)
	if reimagedCond == nil || reimagedCond.Status != metav1.ConditionTrue || reimagedCond.Reason != "Succeeded" {
		t.Fatalf("expected Reimaged=True/Succeeded, got %+v", reimagedCond)
	}

	if reimagedCond.Message != "image=test-image" {
		t.Fatalf("expected Reimaged message 'image=test-image', got %q", reimagedCond.Message)
	}

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

func TestHTTPServer_DisablePXE_UnknownIP(t *testing.T) {
	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:       fc,
		FileResolver: FileResolver{CacheDir: t.TempDir(), Reader: fc},
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

func TestImageReconciler_ConcurrencyLimit(t *testing.T) {
	const (
		maxDownloads = 2
		numFiles     = 10
	)

	var (
		concurrent    atomic.Int32
		maxConcurrent atomic.Int32
	)

	// Pre-generate unique data for each file so they have distinct SHA256s
	fileContents := make([][]byte, numFiles)
	for i := range numFiles {
		fileContents[i] = []byte(fmt.Sprintf("download-data-%d", i))
	}

	// HTTP server that tracks concurrent requests
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := concurrent.Add(1)
		defer concurrent.Add(-1)

		for {
			old := maxConcurrent.Load()
			if cur <= old || maxConcurrent.CompareAndSwap(old, cur) {
				break
			}
		}

		time.Sleep(50 * time.Millisecond) // hold the slot briefly
		// Serve unique content based on request path
		fmt.Fprintf(w, "download-data-%s", strings.TrimPrefix(r.URL.Path, "/file-"))
	}))
	defer ts.Close()

	files := make([]v1alpha3.File, numFiles)
	for i := range numFiles {
		files[i] = v1alpha3.File{
			Path: fmt.Sprintf("images/conc-image/file-%d", i),
			HTTP: &v1alpha3.HTTPSource{URL: fmt.Sprintf("%s/file-%d", ts.URL, i), SHA256: sha256Hex(fileContents[i])},
		}
	}

	image := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "conc-image"},
		Spec:       v1alpha3.ImageSpec{Files: files},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(image).Build()
	cacheDir := t.TempDir()

	reconciler := &ImageReconciler{
		Client:       fc,
		CacheDir:     cacheDir,
		MaxDownloads: maxDownloads,
	}

	result, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "conc-image"},
	})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("unexpected requeue: %v", result.RequeueAfter)
	}

	if got := maxConcurrent.Load(); got > maxDownloads {
		t.Errorf("max concurrent downloads: got %d, want <= %d", got, maxDownloads)
	}

	// Verify all files were downloaded
	for i := range numFiles {
		data, err := os.ReadFile(cachePath(cacheDir, sha256Hex(fileContents[i]), ""))
		if err != nil {
			t.Errorf("file-%d not downloaded: %v", i, err)
		} else if string(data) != string(fileContents[i]) {
			t.Errorf("file-%d content mismatch", i)
		}
	}
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

// createTestQcow2 uses qemu-img to create a small qcow2 file with known
// raw content. Returns the qcow2 file path and the expected raw content.
func createTestQcow2(t *testing.T) (qcow2Path string, rawContent []byte) {
	t.Helper()

	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not available, skipping qcow2 test")
	}

	dir := t.TempDir()

	// Create a small raw image with known content
	rawPath := filepath.Join(dir, "test.raw")
	rawSize := 1024 * 1024 // 1 MiB
	rawContent = make([]byte, rawSize)
	// Write a recognizable pattern
	for i := range rawContent {
		rawContent[i] = byte(i % 251) // prime modulus to avoid trivial patterns
	}

	if err := os.WriteFile(rawPath, rawContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// Convert raw -> qcow2
	qcow2Path = filepath.Join(dir, "test.qcow2")

	cmd := exec.CommandContext(t.Context(), "qemu-img", "convert", "-f", "raw", "-O", "qcow2", rawPath, qcow2Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("qemu-img convert failed: %v\n%s", err, out)
	}

	return qcow2Path, rawContent
}

func TestImageReconciler_ConvertQcow2ToRawGz(t *testing.T) {
	qcow2Path, expectedRaw := createTestQcow2(t)

	qcow2Data, err := os.ReadFile(qcow2Path)
	if err != nil {
		t.Fatal(err)
	}

	qcow2SHA := sha256Hex(qcow2Data)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(qcow2Data)
	}))
	defer ts.Close()

	image := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "convert-image"},
		Spec: v1alpha3.ImageSpec{
			Files: []v1alpha3.File{
				{
					Path: "images/convert-image/disk.img.gz",
					HTTP: &v1alpha3.HTTPSource{
						URL:     ts.URL + "/ubuntu-cloudimg.qcow2",
						SHA256:  qcow2SHA,
						Convert: "UnpackQcow2",
					},
				},
			},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(image).Build()
	cacheDir := t.TempDir()

	reconciler := &ImageReconciler{Client: fc, CacheDir: cacheDir}

	result, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "convert-image"},
	})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("unexpected requeue: %v", result.RequeueAfter)
	}

	// Verify the converted file exists and is valid raw+gzip
	convertedPath := cachePath(cacheDir, qcow2SHA, "UnpackQcow2")

	f, err := os.Open(convertedPath)
	if err != nil {
		t.Fatalf("converted file not found: %v", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("not a valid gzip file: %v", err)
	}
	defer gr.Close()

	gotRaw, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("reading gzipped raw data: %v", err)
	}

	if len(gotRaw) != len(expectedRaw) {
		t.Fatalf("raw size mismatch: got %d, want %d", len(gotRaw), len(expectedRaw))
	}

	if string(gotRaw) != string(expectedRaw) {
		t.Error("raw content mismatch after qcow2 -> raw+gz conversion")
	}

	// Re-reconcile should skip re-download (content-addressed cache check)
	var downloadCount atomic.Int32

	ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloadCount.Add(1)
		w.Write(qcow2Data)
	})

	result, err = reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "convert-image"},
	})
	if err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("unexpected requeue on second reconcile: %v", result.RequeueAfter)
	}

	if downloadCount.Load() != 0 {
		t.Error("second reconcile should not re-download (content-addressed cache hit)")
	}
}

func TestImageReconciler_ConvertUnsupportedMethod(t *testing.T) {
	scheme := newScheme(t)

	image := &v1alpha3.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-convert-image"},
		Spec: v1alpha3.ImageSpec{
			Files: []v1alpha3.File{{
				Path: "images/test/disk.img.gz",
				HTTP: &v1alpha3.HTTPSource{
					URL:     "https://example.com/image.qcow2",
					SHA256:  "0000000000000000000000000000000000000000000000000000000000000000",
					Convert: "SomethingUnsupported",
				},
			}},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(image).Build()
	reconciler := &ImageReconciler{Client: fc, CacheDir: t.TempDir()}

	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "bad-convert-image"},
	})
	if err == nil {
		t.Fatal("expected error for unsupported convert method")
	}

	if !strings.Contains(err.Error(), "unsupported convert value") {
		t.Errorf("error should contain %q, got: %v", "unsupported convert value", err)
	}
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}

	return nil
}
