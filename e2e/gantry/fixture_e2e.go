//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Azure/unbounded/e2e/gantry/fixture"
)

const (
	peerOnlyRegistryName  = "peer-only-registry"
	peerOnlyRegistryImage = "docker.io/library/gantry-peer-only-registry:e2e"
	pauseFixtureImage     = "localhost/gantry-multiarch-pause:e2e"
)

func (h *harness) buildFixtureBinary(ctx context.Context, command, arch string) []byte {
	h.t.Helper()

	binary := filepath.Join(h.t.TempDir(), command+"-"+arch)
	if err := h.run(ctx, "env", "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch,
		"go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-buildid=", "-o", binary,
		"./e2e/gantry/fixture/cmd/"+command); err != nil {
		h.t.Fatalf("build %s fixture for %s: %v", command, arch, err)
	}

	body, err := os.ReadFile(binary)
	if err != nil {
		h.t.Fatalf("read fixture binary: %v", err)
	}

	return body
}

func (h *harness) buildPauseFixture(ctx context.Context) *fixture.Image {
	h.t.Helper()

	binaries := map[string][]byte{
		"amd64": h.buildFixtureBinary(ctx, "pause", "amd64"),
		"arm64": h.buildFixtureBinary(ctx, "pause", "arm64"),
	}

	image, err := fixture.NewImage(binaries, "pause")
	if err != nil {
		h.t.Fatalf("build multi-arch image fixture: %v", err)
	}

	return image
}

// deployPeerOnlyRegistry side-loads a scratch HTTPS server. It answers /v2/
// auth probes anonymously and returns 404 for every content request, including
// digests that exist on the seed. It never pulls or serves a public image.
func (h *harness) deployPeerOnlyRegistry(ctx context.Context) (string, []byte) {
	h.t.Helper()

	image, err := fixture.NewImage(map[string][]byte{
		runtime.GOARCH: h.buildFixtureBinary(ctx, "registry", runtime.GOARCH),
	}, "registry")
	if err != nil {
		h.t.Fatalf("build registry fixture image: %v", err)
	}

	archive, err := image.Archive(peerOnlyRegistryImage)
	if err != nil {
		h.t.Fatalf("archive registry fixture: %v", err)
	}

	archivePath := filepath.Join(h.t.TempDir(), "registry.tar")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		h.t.Fatalf("write registry archive: %v", err)
	}

	if err := h.run(ctx, "kind", "load", "image-archive", archivePath, "--name", clusterName); err != nil {
		h.t.Fatalf("load registry fixture: %v", err)
	}

	service := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
---
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    app: %s
  ports:
    - port: 5443
      targetPort: 5443
`, namespace, peerOnlyRegistryName, namespace, peerOnlyRegistryName)
	if err := h.runWithInput(ctx, service, "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("apply registry fixture service: %v", err)
	}

	ip, err := h.runOut(ctx, "kubectl", "-n", namespace, "get", "service", peerOnlyRegistryName,
		"-o", "jsonpath={.spec.clusterIP}")
	if err != nil {
		h.t.Fatalf("get registry fixture address: %v", err)
	}

	ip = strings.TrimSpace(ip)
	if net.ParseIP(ip) == nil {
		h.t.Fatalf("invalid registry fixture IP %q", ip)
	}

	certPEM, keyPEM := newRegistryCertificate(h.t, ip)
	registryHost := net.JoinHostPort(ip, "5443")

	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s-tls
  namespace: %s
type: Opaque
data:
  tls.crt: %s
  tls.key: %s
  ca.crt: %s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
        - name: registry
          image: %s
          imagePullPolicy: Never
          ports:
            - containerPort: 5443
          readinessProbe:
            httpGet:
              scheme: HTTPS
              path: /v2/
              port: 5443
          volumeMounts:
            - name: tls
              mountPath: /tls
              readOnly: true
      volumes:
        - name: tls
          secret:
            secretName: %s-tls
`, peerOnlyRegistryName, namespace,
		base64.StdEncoding.EncodeToString(certPEM), base64.StdEncoding.EncodeToString(keyPEM),
		base64.StdEncoding.EncodeToString(certPEM), peerOnlyRegistryName, namespace,
		peerOnlyRegistryName, peerOnlyRegistryName, peerOnlyRegistryImage, peerOnlyRegistryName)
	if err := h.runWithInput(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("apply registry fixture: %v", err)
	}

	h.t.Cleanup(func() {
		if h.t.Failed() {
			diagnosticCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			h.dumpKubectlArtifact(diagnosticCtx, "-n", namespace, "describe", "deployment/"+peerOnlyRegistryName)
			h.dumpKubectlArtifact(diagnosticCtx, "-n", namespace, "logs", "deployment/"+peerOnlyRegistryName)
		}
	})

	if err := h.run(ctx, "kubectl", "-n", namespace, "rollout", "status",
		"deployment/"+peerOnlyRegistryName, "--timeout=180s"); err != nil {
		h.t.Fatalf("wait for registry fixture: %v", err)
	}

	return registryHost, certPEM
}

// patchPeerOnlyRegistryConfig retains all normal kind tuning but replaces the
// entire upstream list, so neither auth discovery nor origin fallback can
// contact registry.k8s.io.
func patchPeerOnlyRegistryConfig(raw, registryHost string) (string, error) {
	if _, _, err := net.SplitHostPort(registryHost); err != nil {
		return "", fmt.Errorf("invalid fixture registry address: %w", err)
	}

	patched, err := patchConfigMapForE2E(raw)
	if err != nil {
		return "", err
	}

	if strings.Count(patched, e2eConfigMapUpstreamRegistriesReplacement) != 1 {
		return "", fmt.Errorf("fixture upstream registry anchor not found exactly once")
	}

	replacement := fmt.Sprintf("    upstream_registries:\n      - name: %q\n        endpoint: %q",
		registryHost, "https://"+registryHost)

	return strings.Replace(patched, e2eConfigMapUpstreamRegistriesReplacement, replacement, 1), nil
}

func (h *harness) configurePeerOnlyRegistry(ctx context.Context, registryHost string) {
	h.t.Helper()

	raw, err := os.ReadFile(filepath.Join(h.manifests, "configmap.yaml"))
	if err != nil {
		h.t.Fatalf("read registry fixture config: %v", err)
	}

	patched, err := patchPeerOnlyRegistryConfig(string(raw), registryHost)
	if err != nil {
		h.t.Fatalf("patch registry fixture config: %v", err)
	}

	if err := h.runWithInput(ctx, patched, "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("apply registry fixture config: %v", err)
	}

	h.configureGantryRegistryCA(ctx, peerOnlyRegistryName)
}

func (h *harness) installPeerOnlyMirrorHosts(ctx context.Context, registryHost string, certPEM []byte) {
	h.t.Helper()

	dir := "/etc/containerd/certs.d/" + registryHost

	content := fmt.Sprintf(`server = %q

[host."http://127.0.0.1:5000"]
  capabilities = ["pull", "resolve"]

[host.%q]
  capabilities = ["pull", "resolve"]
  ca = %q
`, "https://"+registryHost, "https://"+registryHost, dir+"/ca.crt")
	for _, node := range h.kindNodes(ctx) {
		cmd := "mkdir -p " + shellQuote(dir) + " && cat > " + shellQuote(dir+"/ca.crt")
		if err := h.runWithInput(ctx, string(certPEM), h.containerEngine, "exec", "-i", node, "sh", "-c", cmd); err != nil {
			h.t.Fatalf("install fixture CA on %s: %v", node, err)
		}

		if err := h.runWithInput(ctx, content, h.containerEngine, "exec", "-i", node, "sh", "-c",
			"cat > "+shellQuote(dir+"/hosts.toml")); err != nil {
			h.t.Fatalf("install fixture hosts on %s: %v", node, err)
		}
	}
}

// purgePauseFixture removes even GC-root-labeled content left by E2E_KEEP.
// The registry executable uses a different layer path, so this cannot warm or
// delete the workload graph through the side-loaded registry image.
func (h *harness) purgePauseFixture(ctx context.Context, image *fixture.Image, peerOnlyRef string) {
	h.t.Helper()

	for _, node := range h.kindNodes(ctx) {
		h.evictImageFromNode(ctx, node, pauseFixtureImage)
		h.evictImageFromNode(ctx, node, peerOnlyRef)

		commands := []string{"set -eu"}
		for _, digest := range image.Digests() {
			commands = append(commands,
				"ctr -n k8s.io content rm "+shellQuote(digest.String())+" >/dev/null 2>&1 || true",
				"if ctr -n k8s.io content get "+shellQuote(digest.String())+" >/dev/null 2>&1; then "+
					"echo 'fixture content survived eviction' >&2; exit 1; fi",
			)
		}

		if err := h.run(ctx, h.containerEngine, "exec", node, "sh", "-c", strings.Join(commands, "; ")); err != nil {
			h.t.Fatalf("purge fixture content on %s: %v", node, err)
		}
	}
}
