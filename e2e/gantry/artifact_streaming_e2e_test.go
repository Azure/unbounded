//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	streamingOriginName = "gantry-streaming-origin"
	streamingOriginHost = streamingOriginName + "." + namespace + ".svc.cluster.local"
)

// artifactStreamingOriginRequestURI builds the ACR data-path form OverlayBD
// receives after the registry redirect, doubled slash and encoded query
// included. The origin rejects anything that does not arrive byte-for-byte.
func artifactStreamingOriginRequestURI(digest, sentinel string) string {
	hex := strings.TrimPrefix(digest, "sha256:")

	return "/account//docker/registry/v2/blobs/sha256/" + hex[:2] + "/" + hex + "/data" +
		"?se=2030-01-01T00%3A00%3A00Z&sig=" + sentinel + "&sp=r&sv=2018-03-28"
}

type artifactStreamingFixture struct {
	t            *testing.T
	ctx          context.Context
	harness      *harness
	body         []byte
	digest       string
	originURL    string
	sentinel     string
	requesterPod string
	providerPod  string
	providerNode string
}

func setupArtifactStreaming(t *testing.T) *artifactStreamingFixture {
	t.Helper()

	h := newHarness(t)
	h.checkPrereqs()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)

	h.bootCluster(ctx)
	t.Cleanup(func() {
		teardownCtx, teardownCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer teardownCancel()

		h.teardown(teardownCtx)
	})

	h.buildAndLoadImage(ctx)
	h.applyManifests(ctx)
	h.waitForRollout(ctx)

	// A real SAS value is opaque, so use a sentinel that makes a leak unambiguous.
	const sentinel = "e2eSasValueMustNotLeak"

	body := []byte(fmt.Sprintf("0123456789-artifact-streaming-%d", time.Now().UnixNano()))
	digest := artifactStreamingDigest(body)
	requestURI := artifactStreamingOriginRequestURI(digest, sentinel)

	caPEM, certPEM, keyPEM := generateArtifactStreamingCertificate(t, streamingOriginHost)
	h.installArtifactStreamingOrigin(ctx, body, requestURI, caPEM, certPEM, keyPEM)
	h.enableArtifactStreaming(ctx, caPEM)
	h.waitForRollout(ctx)
	h.checkReadyz(ctx)

	workers := h.workerNodes(ctx)

	return &artifactStreamingFixture{
		t:            t,
		ctx:          ctx,
		harness:      h,
		body:         body,
		digest:       digest,
		originURL:    "https://" + streamingOriginHost + ":8443" + requestURI,
		sentinel:     sentinel,
		requesterPod: h.gantryPodOnNode(ctx, workers[0]),
		providerPod:  h.gantryPodOnNode(ctx, workers[1]),
		providerNode: workers[1],
	}
}

func (f *artifactStreamingFixture) request() {
	f.t.Helper()

	response := f.harness.requestArtifactStreamingRange(f.ctx, f.requesterPod, f.originURL, "bytes=2-5")
	assertArtifactStreamingResponse(f.t, response, len(f.body))
}

func (f *artifactStreamingFixture) streamingSuccesses(source string) float64 {
	f.t.Helper()

	return f.harness.metricSumOnPod(f.ctx, f.requesterPod,
		"gantry_streaming_requests_total", `source="`+source+`"`, `outcome="success"`)
}

func (f *artifactStreamingFixture) waitForStreamingSuccess(source string, before float64) {
	f.t.Helper()

	f.harness.waitForMetricIncreaseOnPod(f.ctx, f.requesterPod,
		"gantry_streaming_requests_total", before, `source="`+source+`"`, `outcome="success"`)
}

// serveFromPeer publishes the blob on the provider node and drives ranges until
// the requester resolves one from the advertised peer.
func (f *artifactStreamingFixture) serveFromPeer() {
	f.t.Helper()

	advertiseBefore := f.harness.metricSumOnPod(f.ctx, f.providerPod, "gantry_advertise_total")
	f.harness.ingestArtifactStreamingBlob(f.ctx, f.providerNode, f.digest, f.body)
	f.harness.waitForMetricIncreaseOnPod(f.ctx, f.providerPod, "gantry_advertise_total", advertiseBefore)

	peerBefore := f.streamingSuccesses("peer")
	deadline := time.Now().Add(2 * time.Minute)

	for {
		f.request()

		if f.streamingSuccesses("peer") > peerBefore {
			return
		}

		if time.Now().After(deadline) {
			f.harness.dumpDiagnostics(f.ctx)
			f.t.Fatal("artifact streaming did not transition from signed origin to complete peer within 2m")
		}

		select {
		case <-f.ctx.Done():
			f.t.Fatalf("context canceled waiting for peer range service: %v", f.ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func TestE2E_ArtifactStreamingOriginThenPeer(t *testing.T) {
	f := setupArtifactStreaming(t)

	originBefore := f.streamingSuccesses("origin")
	f.request()
	f.waitForStreamingSuccess("origin", originBefore)

	f.serveFromPeer()

	f.harness.assertNoSignedQueryLeak(f.ctx, f.sentinel)
}

// TestE2E_ArtifactStreamingStaleProviderFallsBackToOrigin covers the provider
// record outliving the blob: libp2p has no protocol-level withdraw, so a peer
// that can no longer serve must be failed over inside the same request.
func TestE2E_ArtifactStreamingStaleProviderFallsBackToOrigin(t *testing.T) {
	f := setupArtifactStreaming(t)

	f.serveFromPeer()
	f.harness.removeArtifactStreamingBlob(f.ctx, f.providerNode, f.digest)

	originBefore := f.streamingSuccesses("origin")
	peerBefore := f.streamingSuccesses("peer")

	f.request()
	f.waitForStreamingSuccess("origin", originBefore)

	if peerAfter := f.streamingSuccesses("peer"); peerAfter != peerBefore {
		t.Fatalf("peer served %.0f ranges after its blob was removed", peerAfter-peerBefore)
	}
}

// assertNoSignedQueryLeak pins that a signed origin credential never reaches
// anything an operator or scrape can read back.
func (h *harness) assertNoSignedQueryLeak(ctx context.Context, sentinel string) {
	h.t.Helper()

	for _, pod := range h.gantryPods(ctx) {
		logs, err := h.runOut(ctx, "kubectl", "-n", namespace, "logs", pod, "-c", "gantry", "--tail=-1")
		if err != nil {
			h.t.Fatalf("read %s logs: %v", pod, err)
		}

		if strings.Contains(logs, sentinel) {
			h.t.Fatalf("signed origin query leaked into %s logs", pod)
		}

		if strings.Contains(h.fetchPodMetrics(ctx, pod), sentinel) {
			h.t.Fatalf("signed origin query leaked into %s metrics", pod)
		}
	}
}

type artifactStreamingResponse struct {
	status       int
	body         string
	contentRange string
}

func assertArtifactStreamingResponse(t *testing.T, response artifactStreamingResponse, total int) {
	t.Helper()

	wantContentRange := fmt.Sprintf("bytes 2-5/%d", total)
	if response.status != http.StatusPartialContent || response.body != "2345" || response.contentRange != wantContentRange {
		t.Fatalf("streaming response = status %d, body %q, Content-Range %q; want 206, 2345, %s", response.status, response.body, response.contentRange, wantContentRange)
	}
}

func artifactStreamingDigest(body []byte) string {
	sum := sha256.Sum256(body)

	return "sha256:" + hex.EncodeToString(sum[:])
}

func generateArtifactStreamingCertificate(t *testing.T, host string) ([]byte, []byte, []byte) {
	t.Helper()

	now := time.Now()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Gantry streaming E2E CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
}

func (h *harness) installArtifactStreamingOrigin(ctx context.Context, body []byte, expectedRequestURI string, caPEM, certPEM, keyPEM []byte) {
	h.t.Helper()

	manifest := artifactStreamingOriginManifest(body, expectedRequestURI, caPEM, certPEM, keyPEM)

	if err := h.runWithInput(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("apply artifact streaming origin: %v", err)
	}

	if err := h.run(ctx, "kubectl", "-n", namespace, "rollout", "restart", "deployment/"+streamingOriginName); err != nil {
		h.t.Fatalf("restart artifact streaming origin: %v", err)
	}

	if err := h.run(ctx, "kubectl", "-n", namespace, "rollout", "status", "deployment/"+streamingOriginName, "--timeout=2m"); err != nil {
		h.t.Fatalf("wait for artifact streaming origin: %v", err)
	}
}

func artifactStreamingOriginManifest(body []byte, expectedRequestURI string, caPEM, certPEM, keyPEM []byte) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s-ca
  namespace: %[2]s
binaryData:
  ca.crt: %[3]s
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[1]s-content
  namespace: %[2]s
binaryData:
  blob: %[4]s
data:
  nginx.conf: |
    worker_processes 1;
    events { worker_connections 128; }
    http {
      access_log /dev/stdout;
      error_log /dev/stderr;
      server {
        listen 8443 ssl;
        ssl_certificate /tls/tls.crt;
        ssl_certificate_key /tls/tls.key;
        location / {
          if ($request_uri = "%[7]s") { rewrite ^ /blob last; }
          return 421;
        }
        location = /blob { root /usr/share/nginx/html; }
      }
    }
---
apiVersion: v1
kind: Secret
metadata:
  name: %[1]s-tls
  namespace: %[2]s
type: kubernetes.io/tls
data:
  tls.crt: %[5]s
  tls.key: %[6]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  replicas: 1
  selector:
    matchLabels: {app: %[1]s}
  template:
    metadata:
      labels: {app: %[1]s}
    spec:
      containers:
        - name: origin
          image: registry.k8s.io/nginx-slim:0.27
          ports:
            - {name: https, containerPort: 8443}
          readinessProbe:
            tcpSocket: {port: https}
          volumeMounts:
            - {name: config, mountPath: /etc/nginx/nginx.conf, subPath: nginx.conf, readOnly: true}
            - {name: config, mountPath: /usr/share/nginx/html/blob, subPath: blob, readOnly: true}
            - {name: tls, mountPath: /tls, readOnly: true}
      volumes:
        - name: config
          configMap: {name: %[1]s-content}
        - name: tls
          secret: {secretName: %[1]s-tls}
---
apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  selector: {app: %[1]s}
  ports:
    - {name: https, port: 8443, targetPort: https}
`, streamingOriginName, namespace,
		base64.StdEncoding.EncodeToString(caPEM),
		base64.StdEncoding.EncodeToString(body),
		base64.StdEncoding.EncodeToString(certPEM),
		base64.StdEncoding.EncodeToString(keyPEM),
		expectedRequestURI)
}

func TestArtifactStreamingOriginManifestIsValidYAML(t *testing.T) {
	t.Parallel()

	manifest := artifactStreamingOriginManifest(
		[]byte("body"),
		"/account//docker/registry/v2/blobs/sha256/ab/"+strings.Repeat("a", 64)+"/data?sig=x",
		[]byte("ca"),
		[]byte("certificate"),
		[]byte("key"),
	)
	if strings.ContainsRune(manifest, '\t') {
		t.Fatal("origin manifest contains a tab")
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	documents := 0

	for {
		var document map[string]any
		if err := decoder.Decode(&document); err != nil {
			if err == io.EOF {
				break
			}

			t.Fatalf("decode document %d: %v", documents+1, err)
		}

		if len(document) == 0 {
			continue
		}

		documents++
	}

	if documents != 5 {
		t.Fatalf("decoded documents = %d, want 5", documents)
	}
}

func (h *harness) enableArtifactStreaming(ctx context.Context, caPEM []byte) {
	h.t.Helper()

	raw, err := os.ReadFile(filepath.Join(h.manifests, "configmap.yaml"))
	if err != nil {
		h.t.Fatal(err)
	}

	patched, err := patchConfigMapForE2E(string(raw))
	if err != nil {
		h.t.Fatal(err)
	}

	patched, err = patchConfigMapForArtifactStreamingE2E(patched)
	if err != nil {
		h.t.Fatal(err)
	}

	if err := h.runWithInput(ctx, patched, "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("enable artifact streaming config: %v", err)
	}

	patch := map[string]any{"spec": map[string]any{"template": map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{
			"gantry.unbounded-cloud.io/streaming-origin-ca": artifactStreamingDigest(caPEM),
		}},
		"spec": map[string]any{
			"containers": []any{map[string]any{
				"name":         "gantry",
				"env":          []any{map[string]any{"name": "SSL_CERT_FILE", "value": "/etc/gantry-streaming-ca/ca.crt"}},
				"volumeMounts": []any{map[string]any{"name": "streaming-origin-ca", "mountPath": "/etc/gantry-streaming-ca", "readOnly": true}},
			}},
			"volumes": []any{map[string]any{"name": "streaming-origin-ca", "configMap": map[string]any{"name": streamingOriginName + "-ca"}}},
		},
	}}}

	patchJSON, err := json.Marshal(patch)
	if err != nil {
		h.t.Fatal(err)
	}

	if err := h.run(ctx, "kubectl", "-n", namespace, "patch", "daemonset", dsName, "--type=strategic", "-p", string(patchJSON)); err != nil {
		h.t.Fatalf("mount artifact streaming origin CA: %v", err)
	}
}

func patchConfigMapForArtifactStreamingE2E(raw string) (string, error) {
	const (
		enabledFrom = "    artifact_streaming_enabled: false"
		enabledTo   = "    artifact_streaming_enabled: true"
	)

	if strings.Count(raw, enabledFrom) != 1 {
		return "", fmt.Errorf("patchConfigMapForArtifactStreamingE2E: enabled anchor found %d times", strings.Count(raw, enabledFrom))
	}

	const hostsAnchor = "    artifact_streaming_allowed_host_suffixes:\n"
	if strings.Count(raw, hostsAnchor) != 1 {
		return "", fmt.Errorf("patchConfigMapForArtifactStreamingE2E: allowed hosts anchor found %d times", strings.Count(raw, hostsAnchor))
	}

	patched := strings.Replace(raw, enabledFrom, enabledTo, 1)
	patched = strings.Replace(patched, hostsAnchor, hostsAnchor+"      - "+streamingOriginHost+"\n", 1)

	const (
		reconcileFrom = `    advertise_reconcile_interval: "1m"`
		reconcileTo   = `    advertise_reconcile_interval: "2s"`
	)

	if strings.Count(patched, reconcileFrom) != 1 {
		return "", fmt.Errorf("patchConfigMapForArtifactStreamingE2E: advertise reconcile anchor found %d times", strings.Count(patched, reconcileFrom))
	}

	patched = strings.Replace(patched, reconcileFrom, reconcileTo, 1)

	return patched, nil
}

func (h *harness) ingestArtifactStreamingBlob(ctx context.Context, node, digest string, body []byte) {
	h.t.Helper()

	ref := "gantry-e2e-streaming-" + strings.TrimPrefix(digest, "sha256:")[:16]
	if err := h.runWithInput(ctx, string(body), h.containerEngine, "exec", "-i", node,
		"ctr", "-n", "k8s.io", "content", "ingest",
		"--expected-digest", digest, "--expected-size", fmt.Sprint(len(body)), ref); err != nil {
		h.t.Fatalf("ingest artifact streaming blob on %s: %v", node, err)
	}

	if err := h.run(ctx, h.containerEngine, "exec", node,
		"ctr", "-n", "k8s.io", "content", "label", digest,
		"containerd.io/gc.root="+time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		h.t.Fatalf("pin artifact streaming blob on %s: %v", node, err)
	}

	if err := h.run(ctx, h.containerEngine, "exec", node,
		"ctr", "-n", "k8s.io", "content", "get", digest); err != nil {
		h.t.Fatalf("verify artifact streaming blob on %s: %v", node, err)
	}

	h.t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := h.run(cleanupCtx, h.containerEngine, "exec", node,
			"ctr", "-n", "k8s.io", "content", "rm", digest); err != nil {
			h.t.Logf("remove artifact streaming blob from %s: %v", node, err)
		}
	})
}

func (h *harness) removeArtifactStreamingBlob(ctx context.Context, node, digest string) {
	h.t.Helper()

	if err := h.run(ctx, h.containerEngine, "exec", node,
		"ctr", "-n", "k8s.io", "content", "rm", digest); err != nil {
		h.t.Fatalf("remove artifact streaming blob from %s: %v", node, err)
	}
}

func (h *harness) requestArtifactStreamingRange(ctx context.Context, pod, originURL, requestedRange string) artifactStreamingResponse {
	h.t.Helper()

	port := freeLocalPort(h.t)

	forwardCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(forwardCtx, "kubectl", "-n", namespace, "port-forward", "pod/"+pod, fmt.Sprintf("%d:5000", port))
	cmd.Dir = h.repoRoot
	cmd.Env = os.Environ()

	var output bytes.Buffer

	cmd.Stdout = &output

	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start artifact streaming port-forward: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	defer func() {
		cancel()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill() //nolint:errcheck // best-effort teardown
		}
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d/blobs/%s", port, originURL)
	deadline := time.Now().Add(20 * time.Second)

	var lastErr error

	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			h.t.Fatalf("build artifact streaming request: %v", err)
		}

		req.Header.Set("Range", requestedRange)

		response, requestErr := http.DefaultClient.Do(req)
		if requestErr == nil {
			responseBody, readErr := io.ReadAll(response.Body)
			closeBody(response)

			if readErr != nil {
				h.t.Fatalf("read artifact streaming response: %v", readErr)
			}

			return artifactStreamingResponse{status: response.StatusCode, body: string(responseBody), contentRange: response.Header.Get("Content-Range")}
		}

		lastErr = requestErr

		select {
		case waitErr := <-done:
			h.t.Fatalf("artifact streaming port-forward exited early (%v): %s", waitErr, output.String())
		case <-time.After(500 * time.Millisecond):
		}
	}

	h.t.Fatalf("artifact streaming request unavailable: %v (port-forward logs: %s)", lastErr, output.String())

	return artifactStreamingResponse{}
}
