//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer holds the kind-based end-to-end suite for Racer object
// caching: the operator installs Racer from a ClusterCache with an Azure
// origin, and workloads read public blobs through the injected racer-object
// sidecar.
//
// The harness shells out to kind, kubectl, docker, and make so the suite
// exercises the same artifacts and install path an operator would use.
//
// Environment knobs:
//
//	RACER_E2E_SKIP_BUILD=1   reuse images already in the local registry
//	RACER_E2E_SKIP_DATAPLANE_BUILD=1
//	                         rebuild everything except the (slow) Rust dataplane
//	RACER_E2E_KEEP=1         keep the kind cluster and registry after the run
//	RACER_E2E_REGISTRY_PORT  host port of the local registry (default 5001)
//	CI                       when set, missing Azure egress fails instead of skipping
package racer

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	clusterName       = "racer-object-e2e"
	registryContainer = "racer-object-e2e-registry"
	kindNodeImage     = "kindest/node:v1.33.1"
	operatorNamespace = "unbounded-system"
	imageTag          = "e2e"
)

type harness struct {
	t            *testing.T
	repoRoot     string
	workDir      string
	kubeconfig   string
	registryPort string
	keep         bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	root := repoRoot(t)

	work := filepath.Join(root, "tmp", "racer-e2e")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work dir: %v", err)
	}

	port := os.Getenv("RACER_E2E_REGISTRY_PORT")
	if port == "" {
		port = "5001"
	}

	return &harness{
		t:            t,
		repoRoot:     root,
		workDir:      work,
		kubeconfig:   filepath.Join(work, "kubeconfig"),
		registryPort: port,
		keep:         os.Getenv("RACER_E2E_KEEP") == "1",
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// imageRegistry is the component image prefix pushed by the host and pulled by
// kind nodes through the containerd mirror.
func (h *harness) imageRegistry() string {
	return "localhost:" + h.registryPort + "/unbounded"
}

// run executes name with args and fails the test on error.
func (h *harness) run(timeout time.Duration, name string, args ...string) string {
	h.t.Helper()

	out, err := h.exec(timeout, nil, name, args...)
	if err != nil {
		h.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}

	return out
}

func (h *harness) exec(timeout time.Duration, stdin []byte, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.repoRoot
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second

	cmd.Env = append(os.Environ(), "KUBECONFIG="+h.kubeconfig)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	var buf bytes.Buffer

	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	return buf.String(), err
}

// try runs a best-effort command and logs, rather than fails on, errors.
func (h *harness) try(timeout time.Duration, name string, args ...string) {
	if out, err := h.exec(timeout, nil, name, args...); err != nil {
		h.t.Logf("ignoring error from %s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func (h *harness) kubectl(args ...string) string {
	h.t.Helper()
	return h.run(5*time.Minute, "kubectl", args...)
}

func (h *harness) apply(manifest string) {
	h.t.Helper()

	out, err := h.exec(2*time.Minute, []byte(manifest), "kubectl", "apply", "-f", "-")
	if err != nil {
		h.t.Fatalf("kubectl apply: %v\n%s\n---\n%s", err, out, manifest)
	}
}

// requireAzureEgress skips the suite when the public blob is unreachable from
// the host, unless CI is set.
func requireAzureEgress(t *testing.T, url string) {
	t.Helper()

	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Head(url)
	if err == nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Logf("close response body: %v", cerr)
		}

		if resp.StatusCode == http.StatusOK {
			return
		}

		err = fmt.Errorf("HEAD %s: %s", url, resp.Status)
	}

	if os.Getenv("CI") != "" {
		t.Fatalf("Azure blob egress unavailable: %v", err)
	}

	t.Skipf("skipping: Azure blob egress unavailable (set CI=1 to fail instead): %v", err)
}

// setup creates the local registry and kind cluster and registers cleanup.
func (h *harness) setup() {
	h.t.Helper()

	for _, tool := range []string{"docker", "kind", "kubectl", "make", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			h.t.Fatalf("required tool %q not found in PATH", tool)
		}
	}

	h.teardown()

	if !h.keep {
		h.t.Cleanup(h.teardown)
	}

	h.run(2*time.Minute, "docker", "run", "-d", "--name", registryContainer,
		"-p", "127.0.0.1:"+h.registryPort+":5000", "registry:2")

	kindConfig := `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
nodes:
- role: control-plane
- role: worker
`

	cfg := filepath.Join(h.workDir, "kind-config.yaml")
	if err := os.WriteFile(cfg, []byte(kindConfig), 0o644); err != nil {
		h.t.Fatalf("write kind config: %v", err)
	}

	h.run(10*time.Minute, "kind", "create", "cluster", "--name", clusterName,
		"--image", kindNodeImage, "--config", cfg, "--kubeconfig", h.kubeconfig, "--wait", "240s")

	// Connecting may fail if a previous run left the link; that is harmless.
	h.try(time.Minute, "docker", "network", "connect", "kind", registryContainer)

	dir := "/etc/containerd/certs.d/localhost:" + h.registryPort
	hosts := fmt.Sprintf("[host.\"http://%s:5000\"]\n", registryContainer)

	for node := range strings.FieldsSeq(h.run(time.Minute, "kind", "get", "nodes", "--name", clusterName)) {
		h.run(time.Minute, "docker", "exec", node, "mkdir", "-p", dir)

		if out, err := h.exec(time.Minute, []byte(hosts), "docker", "exec", "-i", node, "cp", "/dev/stdin", dir+"/hosts.toml"); err != nil {
			h.t.Fatalf("write hosts.toml on %s: %v\n%s", node, err, out)
		}
	}
}

func (h *harness) teardown() {
	h.try(5*time.Minute, "kind", "delete", "cluster", "--name", clusterName)
	h.try(time.Minute, "docker", "rm", "-f", registryContainer)
}

// buildImages builds and pushes every operator-managed Racer image plus the
// operator, all tagged with the operator version so component references
// resolve to them.
func (h *harness) buildImages() {
	h.t.Helper()

	type image struct{ target, repo string }

	images := []image{
		{"image-unbounded-operator-local", "unbounded-operator"},
		{"image-racer-controller-local", "racer-controller"},
		{"image-racer-object-local", "racer-object"},
		{"image-racer-dataplane-local", "racer-dataplane"},
	}

	skipAll := os.Getenv("RACER_E2E_SKIP_BUILD") == "1"
	skipDataplane := os.Getenv("RACER_E2E_SKIP_DATAPLANE_BUILD") == "1"

	for _, img := range images {
		ref := h.imageRegistry() + "/" + img.repo + ":" + imageTag

		if !skipAll && (!skipDataplane || img.repo != "racer-dataplane") {
			h.t.Logf("building %s", ref)
			h.run(60*time.Minute, "make", img.target, "CONTAINER_ENGINE=docker", "TRIVY=",
				"VERSION="+imageTag, "CONTAINER_REGISTRY="+h.imageRegistry())
		}

		h.run(10*time.Minute, "docker", "push", ref)
	}
}

// install builds kubectl-unbounded and installs the operator from the local
// registry.
func (h *harness) install() {
	h.t.Helper()

	h.run(15*time.Minute, "make", "kubectl-unbounded-build", "VERSION="+imageTag,
		"CONTAINER_REGISTRY="+h.imageRegistry())
	h.run(10*time.Minute, filepath.Join(h.repoRoot, "bin", "kubectl-unbounded"), "install",
		"--kubeconfig", h.kubeconfig,
		"--image-registry", h.imageRegistry(),
		"--operator-image", "unbounded-operator:"+imageTag,
		"--wait", "--timeout", "5m")
}

// waitFor polls until fn returns true or the deadline passes.
func (h *harness) waitFor(what string, timeout time.Duration, fn func() (bool, string)) {
	h.t.Helper()

	deadline := time.Now().Add(timeout)

	var last string

	for time.Now().Before(deadline) {
		ok, detail := fn()
		if ok {
			return
		}

		last = detail

		time.Sleep(5 * time.Second)
	}

	h.t.Fatalf("timed out after %s waiting for %s: %s", timeout, what, last)
}

// waitExists waits for a namespaced object to exist.
func (h *harness) waitExists(kind, name string, timeout time.Duration) {
	h.t.Helper()
	h.waitFor(kind+"/"+name, timeout, func() (bool, string) {
		out, err := h.exec(time.Minute, nil, "kubectl", "-n", operatorNamespace, "get", kind, name, "-o", "name")
		return err == nil, out
	})
}

func (h *harness) rollout(kind, name string, timeout time.Duration) {
	h.t.Helper()
	h.waitExists(kind, name, timeout)
	h.run(timeout+time.Minute, "kubectl", "-n", operatorNamespace, "rollout", "status",
		kind+"/"+name, "--timeout="+timeout.String())
}

// dumpDiagnostics logs cluster state when the test failed.
func (h *harness) dumpDiagnostics(namespaces ...string) {
	if !h.t.Failed() {
		return
	}

	cmds := [][]string{
		{"get", "nodes", "-o", "wide"},
		{"get", "pods", "-A", "-o", "wide"},
		{"get", "clustercaches", "-o", "yaml"},
		{"get", "events", "-A", "--sort-by=.lastTimestamp"},
		{"-n", operatorNamespace, "get", "ds,deploy,cm", "-o", "wide"},
		{"-n", operatorNamespace, "logs", "deploy/unbounded-operator", "--tail=200"},
		{"-n", operatorNamespace, "logs", "deploy/racer-controller", "--all-containers", "--tail=200"},
		{"-n", operatorNamespace, "logs", "-l", "app.kubernetes.io/name=racer-dataplane", "--all-containers", "--tail=200", "--prefix"},
		{"-n", operatorNamespace, "logs", "-l", "app.kubernetes.io/name=racer-origin", "--all-containers", "--tail=200", "--prefix"},
	}
	for _, ns := range namespaces {
		cmds = append(cmds,
			[]string{"-n", ns, "get", "pods", "-o", "yaml"},
			[]string{"-n", ns, "logs", "-l", "app=fetch", "--all-containers", "--tail=200", "--prefix"})
	}

	for _, args := range cmds {
		out, err := h.exec(time.Minute, nil, "kubectl", args...)
		h.t.Logf("$ kubectl %s (err=%v)\n%s", strings.Join(args, " "), err, out)
	}
}
