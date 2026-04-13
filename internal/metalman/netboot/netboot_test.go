// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/metalman/indexing"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	return s
}

// populateOCICache creates a fake OCI cache directory structure for testing.
// Files are placed under {cacheDir}/oci/{digest}/disk/.
func populateOCICache(cacheDir, digest string, files map[string][]byte) error {
	safe := fmt.Sprintf("sha256_%s", digest)
	diskDir := filepath.Join(cacheDir, "oci", safe, "disk")

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
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
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
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
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
			Cache:    cache,
			Reader:   fc,
			Cluster:  &StaticClusterInfo{Info: ClusterInfo{ApiserverURL: "https://k8s.example.com"}},
			ServeURL: "http://10.0.1.1:8080",
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
		"cloud-init/user-data": []byte(staticConfig),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-verbatim"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:02", IPv4: "10.0.1.51", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
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

	req, _ := http.NewRequest("GET", ts.URL+"/cloud-init/user-data", nil)
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
		"cloud-init/user-data": []byte(staticContent),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-static"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:03", IPv4: "10.0.1.52", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
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

	req, _ := http.NewRequest("GET", ts.URL+"/cloud-init/user-data", nil)
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

func TestUserDataTemplate_WithAgentImage(t *testing.T) {
	userDataTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "host-ubuntu2404", "assets", "user-data.tmpl"))
	if err != nil {
		t.Fatalf("reading user-data.tmpl: %v", err)
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
      "Labels": {
        "node.cloudprovider.kubernetes.io/exclude-from-external-cloud-provider": "true"
      },
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
	}

	result, err := renderTemplate(string(userDataTmpl), data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}

	body := string(result)
	if !strings.Contains(body, `"OCIImage": "ghcr.io/org/rootfs:v1"`) {
		t.Errorf("expected OCIImage in rendered user-data, got:\n%s", body)
	}

	if !strings.Contains(body, `"MachineName": "agent-img-node"`) {
		t.Errorf("expected MachineName in rendered user-data, got:\n%s", body)
	}
}

func TestUserDataTemplate_WithoutAgentImage(t *testing.T) {
	userDataTmpl, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "host-ubuntu2404", "assets", "user-data.tmpl"))
	if err != nil {
		t.Fatalf("reading user-data.tmpl: %v", err)
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
      "Labels": {
        "node.cloudprovider.kubernetes.io/exclude-from-external-cloud-provider": "true"
      },
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
	}

	result, err := renderTemplate(string(userDataTmpl), data)
	if err != nil {
		t.Fatalf("renderTemplate: %v", err)
	}

	body := string(result)
	if strings.Contains(body, "OCIImage") {
		t.Errorf("expected no OCIImage in rendered user-data when AgentImage is empty, got:\n%s", body)
	}

	if !strings.Contains(body, `"MachineName": "no-agent-node"`) {
		t.Errorf("expected MachineName in rendered user-data, got:\n%s", body)
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
	staticConfig := "autoinstall: true"

	cache := setupOCICache(t, "ghcr.io/test/e2e:v1", "e2e123", map[string][]byte{
		"vmlinuz":              vmlinuzData,
		"initrd":               initrdData,
		"grub/grub.cfg.tmpl":   []byte(bootTemplate),
		"cloud-init/user-data": []byte(staticConfig),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/e2e:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:00:11:22", IPv4: "10.0.3.10", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
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
			Cache:    cache,
			Reader:   fc,
			Cluster:  &StaticClusterInfo{Info: ClusterInfo{ApiserverURL: "https://k8s.example.com"}},
			ServeURL: "http://10.0.3.1:8080",
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

	// Test static file serving
	req, _ = http.NewRequest("GET", httpTS.URL+"/cloud-init/user-data", nil)
	req.Header.Set("X-Forwarded-For", "10.0.3.10")
	resp, _ = http.DefaultClient.Do(req)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != staticConfig {
		t.Errorf("static config mismatch: got %q", body)
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
				Image:      "ghcr.io/test/alpha:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:aa:aa:aa:aa:aa", IPv4: "10.0.10.1", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
		},
	}
	betaNode := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "beta-node"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/beta:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "bb:bb:bb:bb:bb:bb", IPv4: "10.0.10.2", SubnetMask: "255.255.255.0"}},
			},
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
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
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
		},
	}

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		FileResolver: FileResolver{Cache: cache, Reader: fc},
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
			Operations: &v1alpha3.OperationsSpec{ReimageCounter: 1},
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
		Client:       fc,
		FileResolver: FileResolver{Cache: cache, Reader: fc},
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

	// Verify the Machine status was patched
	var updated v1alpha3.Machine
	if err := fc.Get(t.Context(), client.ObjectKeyFromObject(node), &updated); err != nil {
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

	if reimagedCond.Message != "image=ghcr.io/test/image:v1" {
		t.Fatalf("expected Reimaged message 'image=ghcr.io/test/image:v1', got %q", reimagedCond.Message)
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
	cache := NewOCICache(t.TempDir())

	scheme := newScheme(t)
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	srv := &HTTPServer{
		Client:       fc,
		FileResolver: FileResolver{Cache: cache, Reader: fc},
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
	metadataContent := "dhcpBootImageName: shimx64.efi\n"

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

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}

	return nil
}
