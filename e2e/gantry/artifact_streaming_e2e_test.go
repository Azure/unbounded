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

func TestE2E_ArtifactStreamingOriginThenPeer(t *testing.T) {
	h := newHarness(t)
	h.checkPrereqs()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	h.bootCluster(ctx)
	t.Cleanup(func() {
		teardownCtx, teardownCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer teardownCancel()

		h.teardown(teardownCtx)
	})

	h.buildAndLoadImage(ctx)
	h.applyManifests(ctx)
	h.waitForRollout(ctx)

	body := []byte(fmt.Sprintf("0123456789-artifact-streaming-%d", time.Now().UnixNano()))
	digest := artifactStreamingDigest(body)
	caPEM, certPEM, keyPEM := generateArtifactStreamingCertificate(t, streamingOriginHost)
	h.installArtifactStreamingOrigin(ctx, body, caPEM, certPEM, keyPEM)
	h.enableArtifactStreaming(ctx, caPEM)
	h.waitForRollout(ctx)
	h.checkReadyz(ctx)

	workers := h.workerNodes(ctx)
	requesterPod := h.gantryPodOnNode(ctx, workers[0])
	providerPod := h.gantryPodOnNode(ctx, workers[1])
	originURL := "https://" + streamingOriginHost + ":8443/blob?d=" + digest + "&sig=redacted"

	originBefore := h.metricSumOnPod(ctx, requesterPod, "gantry_streaming_requests_total", `source="origin"`, `outcome="success"`)
	response := h.requestArtifactStreamingRange(ctx, requesterPod, originURL, "bytes=2-5")
	assertArtifactStreamingResponse(t, response, len(body))
	h.waitForMetricIncreaseOnPod(ctx, requesterPod, "gantry_streaming_requests_total", originBefore, `source="origin"`, `outcome="success"`)

	advertiseBefore := h.metricSumOnPod(ctx, providerPod, "gantry_advertise_total")
	h.ingestArtifactStreamingBlob(ctx, workers[1], digest, body)
	h.waitForMetricIncreaseOnPod(ctx, providerPod, "gantry_advertise_total", advertiseBefore)

	peerBefore := h.metricSumOnPod(ctx, requesterPod, "gantry_streaming_requests_total", `source="peer"`, `outcome="success"`)
	deadline := time.Now().Add(2 * time.Minute)

	for {
		response = h.requestArtifactStreamingRange(ctx, requesterPod, originURL, "bytes=2-5")
		assertArtifactStreamingResponse(t, response, len(body))

		if h.metricSumOnPod(ctx, requesterPod, "gantry_streaming_requests_total", `source="peer"`, `outcome="success"`) > peerBefore {
			break
		}

		if time.Now().After(deadline) {
			h.dumpDiagnostics(ctx)
			t.Fatal("artifact streaming did not transition from signed origin to complete peer within 2m")
		}

		select {
		case <-ctx.Done():
			t.Fatalf("context canceled waiting for peer range service: %v", ctx.Err())
		case <-time.After(2 * time.Second):
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

func (h *harness) installArtifactStreamingOrigin(ctx context.Context, body, caPEM, certPEM, keyPEM []byte) {
	h.t.Helper()

	manifest := artifactStreamingOriginManifest(body, caPEM, certPEM, keyPEM)

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

func artifactStreamingOriginManifest(body, caPEM, certPEM, keyPEM []byte) string {
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
        location / { root /usr/share/nginx/html; }
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
		base64.StdEncoding.EncodeToString(keyPEM))
}

func TestArtifactStreamingOriginManifestIsValidYAML(t *testing.T) {
	t.Parallel()

	manifest := artifactStreamingOriginManifest(
		[]byte("body"),
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
