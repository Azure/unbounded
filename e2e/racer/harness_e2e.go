//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer holds the kind-based end-to-end suite for Racer: the operator
// installs Racer from ClusterCaches, workloads read public Azure blobs through
// the injected racer-object sidecar, and Gantry serves registry content
// through its Racer backend.
//
// The harness shells out to kind, kubectl, docker, and make so the suite
// exercises the same artifacts and install path an operator would use. Every
// test shares one kind cluster, created by the first test that needs it and
// deleted after the last.
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
	"sync"
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

// shared is the cluster every test uses. sharedTried and sharedRunning record
// whether setup was attempted and whether it finished, so later tests fail
// fast instead of retrying a broken setup.
var (
	sharedMu      sync.Mutex
	shared        *harness
	sharedTried   bool
	sharedRunning bool
)

// TestMain deletes the shared cluster after every test has run.
func TestMain(m *testing.M) {
	code := m.Run()

	sharedMu.Lock()
	h, tried := shared, sharedTried
	sharedMu.Unlock()

	if tried && h != nil && !h.keep {
		h.forTest(nil).teardown()
	}

	os.Exit(code)
}

// cluster returns a harness bound to t for the shared cluster, creating the
// cluster, building and pushing images, and installing the operator on first
// use.
func cluster(t *testing.T) *harness {
	t.Helper()

	sharedMu.Lock()
	defer sharedMu.Unlock()

	if sharedRunning {
		return shared.forTest(t)
	}

	if sharedTried {
		t.Fatal("shared kind cluster setup failed in an earlier test")
	}

	sharedTried = true
	shared = newHarness(t)
	shared.setup()
	shared.buildImages()
	shared.install()

	sharedRunning = true

	return shared.forTest(t)
}

// forTest returns a copy of h that reports to t. A nil t logs to stderr.
func (h *harness) forTest(t *testing.T) *harness {
	c := *h
	c.t = t

	return &c
}

func (h *harness) logf(format string, args ...any) {
	if h.t == nil {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
		return
	}

	h.t.Helper()
	h.t.Logf(format, args...)
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

	// Keep stderr out of successful output: callers parse stdout, and tools
	// such as kind print notices on stderr (for example when
	// KIND_EXPERIMENTAL_PROVIDER is set).
	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String() + stderr.String(), err
	}

	return stdout.String(), nil
}

// try runs a best-effort command and logs, rather than fails on, errors.
func (h *harness) try(timeout time.Duration, name string, args ...string) {
	if out, err := h.exec(timeout, nil, name, args...); err != nil {
		h.logf("ignoring error from %s %s: %v\n%s", name, strings.Join(args, " "), err, out)
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

// setup creates the local registry and kind cluster.
func (h *harness) setup() {
	h.t.Helper()

	for _, tool := range []string{"docker", "kind", "kubectl", "make", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			h.t.Fatalf("required tool %q not found in PATH", tool)
		}
	}

	// Start clean; TestMain deletes the cluster after the run.
	h.teardown()

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

// buildImages builds and pushes every operator-managed Racer image, Gantry,
// and the operator, all tagged with the operator version so component references
// resolve to them.
func (h *harness) buildImages() {
	h.t.Helper()

	type image struct{ target, repo string }

	images := []image{
		{"image-unbounded-operator-local", "unbounded-operator"},
		{"image-racer-controller-local", "racer-controller"},
		{"image-racer-object-local", "racer-object"},
		{"image-racer-dataplane-local", "racer-dataplane"},
		{"image-gantry-local", "gantry"},
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

// installRacer waits for the operator to install Racer after a ClusterCache
// is created, caps the dataplane threads for kind, and waits for the Racer
// workloads to roll out. It is safe to call more than once.
func (h *harness) installRacer() {
	h.t.Helper()

	// Automatic sizing scales with host cores. The operator seeds this
	// ConfigMap once and preserves administrator edits, rolling the DaemonSet
	// on change.
	h.waitExists("configmap", "racer-dataplane-config", 5*time.Minute)
	h.kubectl("-n", operatorNamespace, "patch", "configmap", "racer-dataplane-config", "--type=merge",
		"-p", `{"data":{"RACER_MAX_THREADS":"2"}}`)

	h.rollout("deployment", "racer-controller", 5*time.Minute)
	h.rollout("daemonset", "racer-dataplane", 10*time.Minute)
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

// dumpDiagnostics logs cluster state when the test failed and writes it to
// e2e/.artifacts so CI can upload it.
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
		{"get", "sites", "-o", "yaml"},
		{"-n", operatorNamespace, "get", "ds/gantry", "cm/gantry-config", "-o", "yaml"},
		{"-n", operatorNamespace, "logs", "-l", "app.kubernetes.io/name=gantry", "--all-containers", "--tail=200", "--prefix"},
	}
	for _, ns := range namespaces {
		cmds = append(cmds,
			[]string{"-n", ns, "get", "pods", "-o", "yaml"},
			[]string{"-n", ns, "logs", "-l", "app=fetch", "--all-containers", "--tail=200", "--prefix"})
	}

	var report strings.Builder

	for _, args := range cmds {
		out, err := h.exec(time.Minute, nil, "kubectl", args...)
		entry := fmt.Sprintf("$ kubectl %s (err=%v)\n%s", strings.Join(args, " "), err, out)
		h.t.Log(entry)
		report.WriteString(entry + "\n")
	}

	dir := filepath.Join(h.repoRoot, "e2e", ".artifacts", "racer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Logf("diagnostics: mkdir %s: %v", dir, err)
		return
	}

	dst := filepath.Join(dir, strings.ReplaceAll(h.t.Name(), "/", "_")+".log")
	//#nosec G306 -- test artifact
	if err := os.WriteFile(dst, []byte(report.String()), 0o644); err != nil {
		h.t.Logf("diagnostics: could not write %s: %v", dst, err)
		return
	}

	h.t.Logf("diagnostics: wrote %s", dst)
}
