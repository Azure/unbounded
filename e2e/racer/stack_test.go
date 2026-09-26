//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	randv2 "math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	namespace = "unbounded-system"
	registry  = "racer-e2e.invalid"
)

// TestOperatorImagePull runs the deployed Go SDK and Rust dataplane together.
// Only a ClusterCache triggers Racer installation; containerd can reach the
// fixture image exclusively through Gantry's Racer-backed mirror.
func TestOperatorImagePull(t *testing.T) {
	for _, tool := range []string{"docker", "kind", "kubectl", "make"} {
		_, err := exec.LookPath(tool)
		require.NoError(t, err, "required e2e tool: %s", tool)
	}

	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "tmp"), 0o755))
	artifacts, err := os.MkdirTemp(filepath.Join(root, "tmp"), "racer-e2e-")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 7*time.Minute)
	t.Cleanup(cancel)
	h := &harness{t: t, ctx: ctx, root: root, artifacts: artifacts, cluster: fmt.Sprintf("racer-e2e-%d", time.Now().UnixNano())}
	h.kubeconfig = filepath.Join(artifacts, "kubeconfig")
	t.Logf("artifacts: %s", artifacts)

	images := []string{"unbounded-operator", "racer-controller", "racer-dataplane", "gantry"}
	for _, component := range images {
		h.run("docker", "image", "inspect", "docker.io/library/"+component+":e2e")
	}

	h.write("kind.yaml", "kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n- role: control-plane\n- role: worker\n")
	t.Cleanup(func() {
		h.diagnostics()

		if os.Getenv("RACER_E2E_KEEP_CLUSTER") != "1" {
			h.command(context.Background(), "kind", "delete", "cluster", "--name", h.cluster)
		}
	})
	h.run("kind", "create", "cluster", "--name", h.cluster, "--image", "kindest/node:v1.33.1", "--config", filepath.Join(artifacts, "kind.yaml"), "--kubeconfig", h.kubeconfig, "--wait", "120s")

	for _, component := range images {
		h.run("kind", "load", "docker-image", "--name", h.cluster, "docker.io/library/"+component+":e2e")
	}

	h.run("make", "unbounded-operator-manifests", "UNBOUNDED_OPERATOR_IMAGE=docker.io/library/unbounded-operator:e2e", "UNBOUNDED_OPERATOR_IMAGE_REGISTRY=docker.io/library", "UNBOUNDED_OPERATOR_REAP_LEGACY_RESOURCES=false")

	manifests, err := filepath.Glob(filepath.Join(root, "deploy/unbounded-operator/rendered/*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, manifests)

	for _, path := range manifests {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		// Sideloaded test images must not be fetched from a public registry.
		data = bytes.ReplaceAll(data, []byte("imagePullPolicy: Always"), []byte("imagePullPolicy: Never"))
		h.apply(string(data))
	}

	h.kubectl("rollout", "status", "deployment/unbounded-operator", "-n", namespace, "--timeout=90s")
	h.kubectl("wait", "--for=condition=Established", "crd/clustercaches.racer.unbounded-cloud.io", "--timeout=60s")
	require.Empty(t, strings.TrimSpace(h.kubectl("get", "deployment/racer-controller", "-n", namespace, "--ignore-not-found", "-o", "name")))
	h.apply("apiVersion: racer.unbounded-cloud.io/v1alpha1\nkind: ClusterCache\nmetadata:\n  name: gantry\n")
	h.waitResource("deployment/racer-controller")
	// Only the elected controller leader reports ready.
	h.kubectl("wait", "deployment/racer-controller", "-n", namespace, "--for=jsonpath={.status.readyReplicas}=1", "--timeout=90s")
	h.waitResource("daemonset/racer-dataplane")
	h.kubectl("rollout", "status", "daemonset/racer-dataplane", "-n", namespace, "--timeout=90s")

	fixture := newImage(t)

	var networks []struct {
		IPAM struct{ Config []struct{ Gateway string } }
	}
	require.NoError(t, json.Unmarshal([]byte(h.run("docker", "network", "inspect", "kind")), &networks))

	var gateway string

	for _, config := range networks[0].IPAM.Config {
		if net.ParseIP(config.Gateway).To4() != nil {
			gateway = config.Gateway
			break
		}
	}

	require.NotEmpty(t, gateway, "kind requires an IPv4 gateway to the fixture")

	origin := h.serve(fixture.handler())
	originURL := "http://" + net.JoinHostPort(gateway, origin)
	h.apply(fmt.Sprintf(gantryManifest, originURL))
	h.kubectl("rollout", "status", "daemonset/gantry-racer-e2e", "-n", namespace, "--timeout=90s")

	racerPod := strings.TrimSpace(h.kubectl("get", "pod", "-n", namespace, "-l", "app.kubernetes.io/name=racer-dataplane", "-o", "jsonpath={.items[0].metadata.name}"))
	racerURL := h.forward(racerPod, "9090", "/readyz")
	gantryPod := strings.TrimSpace(h.kubectl("get", "pod", "-n", namespace, "-l", "app=gantry-racer-e2e", "-o", "jsonpath={.items[0].metadata.name}"))
	mirrorURL, err := url.Parse(h.forward(gantryPod, "5000", "/v2/"))
	require.NoError(t, err)

	proxy := httputil.NewSingleHostReverseProxy(mirrorURL)
	delivered := &observations{digests: make(map[string]bool)}
	proxy.ModifyResponse = func(response *http.Response) error {
		if response.Request.Method == http.MethodGet && response.StatusCode == http.StatusOK {
			if response.Header.Get("Gantry-Mirrored") != "1" {
				return fmt.Errorf("response did not traverse Gantry Racer: %s", response.Request.URL)
			}

			body, err := io.ReadAll(response.Body)
			response.Body.Close()

			if err != nil {
				return err
			}

			delivered.record(digest(body))
			response.Body = io.NopCloser(bytes.NewReader(body))
		}

		return nil
	}
	mirrorPort := h.serve(proxy)
	worker := h.cluster + "-worker"
	hosts := fmt.Sprintf("server = \"http://%s\"\n", net.JoinHostPort(gateway, mirrorPort))
	h.write("hosts.toml", hosts)
	h.run("docker", "exec", worker, "mkdir", "-p", "/etc/containerd/racer-e2e/"+registry)
	h.run("docker", "cp", filepath.Join(artifacts, "hosts.toml"), worker+":/etc/containerd/racer-e2e/"+registry+"/hosts.toml")
	// A new namespace and unique fixture ensure there is no runtime content hit.
	before := h.run("docker", "exec", worker, "ctr", "-n", "racer-e2e", "content", "ls", "-q")
	require.Empty(t, strings.TrimSpace(before))
	h.run("docker", "exec", worker, "ctr", "-n", "racer-e2e", "images", "pull", "--hosts-dir", "/etc/containerd/racer-e2e", registry+"/fixture/image@"+fixture.manifest)

	for id, blob := range fixture.blobs {
		body := h.run("docker", "exec", worker, "ctr", "-n", "racer-e2e", "content", "get", id)
		require.Equal(t, id, digest([]byte(body)), "containerd content digest")
		require.Len(t, body, len(blob.body), "containerd content size")
		require.True(t, delivered.has(id), "pull did not receive %s through Gantry", id)
		require.True(t, fixture.fetched.has(id), "Racer did not request %s from origin", id)
	}

	h.waitHTTP(racerURL + "/readyz")
	t.Logf("containerd pulled and unpacked %s/fixture/image@%s through operator-installed Racer and Gantry (%d objects)", registry, fixture.manifest, len(fixture.blobs))
}

type harness struct {
	t                                    *testing.T
	ctx                                  context.Context
	root, artifacts, cluster, kubeconfig string
	sequence                             int
}

func (h *harness) run(name string, args ...string) string {
	h.t.Helper()
	return h.command(h.ctx, name, args...)
}

func (h *harness) command(parent context.Context, name string, args ...string) string {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.root

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	output, err := cmd.Output()
	if err != nil {
		h.t.Fatalf("%s %v: %v\n%s\n%s", name, args, err, output, stderr.String())
	}

	return string(output)
}

func (h *harness) kubectl(args ...string) string {
	h.t.Helper()
	return h.run("kubectl", append([]string{"--kubeconfig", h.kubeconfig, "--request-timeout=10s"}, args...)...)
}

func (h *harness) write(name, content string) {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(filepath.Join(h.artifacts, name), []byte(content), 0o600))
}

func (h *harness) apply(manifest string) {
	h.t.Helper()
	h.sequence++
	name := fmt.Sprintf("manifest-%d.yaml", h.sequence)
	h.write(name, manifest)
	h.kubectl("apply", "-f", filepath.Join(h.artifacts, name))
}

func (h *harness) waitResource(resource string) {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		return strings.TrimSpace(h.kubectl("get", resource, "-n", namespace, "--ignore-not-found", "-o", "name")) != ""
	}, time.Minute, time.Second, "operator did not create %s", resource)
}

func (h *harness) serve(handler http.Handler) string {
	h.t.Helper()

	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	require.NoError(h.t, err)

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()

	h.t.Cleanup(func() { server.Close() })

	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(h.t, err)

	return port
}

func (h *harness) forward(pod, port, readyPath string) string {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(h.ctx, time.Minute)
	defer cancel()

	address, stop, err := forwardHTTP(ctx, func() *exec.Cmd {
		return exec.Command("kubectl", "--kubeconfig", h.kubeconfig, "-n", namespace, "port-forward", "pod/"+pod, ":"+port)
	}, filepath.Join(h.artifacts, "forward-"+port), readyPath)
	require.NoError(h.t, err, "pod %s port %s not ready", pod, port)
	h.t.Cleanup(stop)

	return address
}

func (h *harness) waitHTTP(endpoint string) {
	h.t.Helper()

	client := &http.Client{Timeout: time.Second}
	require.Eventually(h.t, func() bool {
		response, err := client.Get(endpoint)
		if err != nil {
			return false
		}

		response.Body.Close()

		return response.StatusCode == http.StatusOK
	}, time.Minute, time.Second, "%s not ready", endpoint)
}

func (h *harness) diagnostics() {
	// Best effort, with independent deadlines so failures retain their evidence.
	commands := [][]string{{"get", "pods", "-A", "-o", "wide"}, {"get", "events", "-A", "--sort-by=.lastTimestamp"}}
	for _, app := range []string{"unbounded-operator", "racer-dataplane"} {
		commands = append(commands, []string{"logs", "-n", namespace, "-l", "app.kubernetes.io/name=" + app, "--all-containers", "--prefix", "--tail=300"})
	}

	commands = append(commands, []string{"logs", "-n", namespace, "deployment/racer-controller", "--all-containers", "--prefix", "--tail=300"})

	commands = append(commands, []string{"logs", "-n", namespace, "-l", "app=gantry-racer-e2e", "--all-containers", "--prefix", "--tail=300"})
	for i, args := range commands {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		data, _ := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", h.kubeconfig}, args...)...).CombinedOutput()

		cancel()
		h.write(fmt.Sprintf("diagnostic-%d.log", i), string(data))
	}
}

type observations struct {
	sync.Mutex
	digests map[string]bool
}

func (o *observations) record(id string)   { o.Lock(); defer o.Unlock(); o.digests[id] = true }
func (o *observations) has(id string) bool { o.Lock(); defer o.Unlock(); return o.digests[id] }

type (
	blob struct {
		body      []byte
		mediaType string
	}
	image struct {
		manifest string
		blobs    map[string]blob
		fetched  observations
	}
)

func digest(body []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(body)) }

func newImage(t *testing.T) *image {
	t.Helper()
	// Incompressible content slides beyond the two-page acquisition window.
	payload := make([]byte, 65<<20)

	var seed [32]byte

	_, err := rand.Read(seed[:])
	require.NoError(t, err)

	random := randv2.NewChaCha8(seed)
	_, err = random.Read(payload)
	require.NoError(t, err)

	var layer bytes.Buffer

	compressed := gzip.NewWriter(&layer)
	diffHash := sha256.New()
	archive := tar.NewWriter(io.MultiWriter(compressed, diffHash))
	require.NoError(t, archive.WriteHeader(&tar.Header{Name: "payload", Mode: 0o644, Size: int64(len(payload))}))
	_, err = archive.Write(payload)
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, compressed.Close())
	require.Greater(t, layer.Len(), 64<<20)

	config, err := json.Marshal(map[string]any{"architecture": runtime.GOARCH, "os": "linux", "rootfs": map[string]any{"type": "layers", "diff_ids": []string{fmt.Sprintf("sha256:%x", diffHash.Sum(nil))}}})
	require.NoError(t, err)

	result := &image{blobs: make(map[string]blob), fetched: observations{digests: make(map[string]bool)}}
	descriptor := func(body []byte, mediaType string) map[string]any {
		id := digest(body)
		result.blobs[id] = blob{body: body, mediaType: mediaType}

		return map[string]any{"mediaType": mediaType, "digest": id, "size": len(body)}
	}
	manifest, err := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": descriptor(config, "application/vnd.oci.image.config.v1+json"), "layers": []any{descriptor(layer.Bytes(), "application/vnd.oci.image.layer.v1.tar+gzip")}})
	require.NoError(t, err)

	result.manifest = digest(manifest)
	descriptor(manifest, "application/vnd.oci.image.manifest.v1+json")

	return result
}

func (image *image) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}

		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

		value, ok := image.blobs[id]
		if !ok {
			http.NotFound(w, r)
			return
		}

		if r.Method == http.MethodGet {
			image.fetched.record(id)
		}

		w.Header().Set("Content-Type", value.mediaType)
		w.Header().Set("Docker-Content-Digest", id)
		http.ServeContent(w, r, id, time.Time{}, bytes.NewReader(value.body))
	})
}

// Same-UID sockets connect the two real agents without legacy storage mounts.
const gantryManifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: gantry-racer-e2e
  namespace: unbounded-system
data:
  config.yaml: |
    mirror_listen: 0.0.0.0:5000
    mirror_bind_allow_non_loopback: true
    upstream_registries:
      - name: racer-e2e.invalid
        endpoint: %s
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: gantry-racer-e2e
  namespace: unbounded-system
spec:
  selector:
    matchLabels: {app: gantry-racer-e2e}
  template:
    metadata:
      labels: {app: gantry-racer-e2e}
    spec:
      containers:
        - name: gantry
          image: docker.io/library/gantry:e2e
          imagePullPolicy: Never
          args: [agent, --config=/etc/gantry/config.yaml]
          env:
            - {name: GANTRY_RACER_ENABLED, value: "true"}
          securityContext:
            runAsUser: 0
            runAsGroup: 0
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
          readinessProbe:
            httpGet: {path: /readyz, port: 9095}
            periodSeconds: 1
          volumeMounts:
            - {name: sockets, mountPath: /run/racer}
            - {name: config, mountPath: /etc/gantry, readOnly: true}
      volumes:
        - name: sockets
          hostPath: {path: /run/racer, type: Directory}
        - name: config
          configMap: {name: gantry-racer-e2e}
`
