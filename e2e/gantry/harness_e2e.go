//go:build e2e

// Package e2e holds the kind-based integration suite for gantry.
//
// All files in this package are guarded by `//go:build e2e` so the
// default `go test ./...` skips them. Run via `make e2e` or
// `go test -tags=e2e ./e2e/...`.
//
// The harness is intentionally CLI-driven (shell-out to kind, kubectl,
// docker) rather than client-go-driven so the e2e module stays
// dependency-free relative to the root module.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

const (
	clusterName       = "gantry-e2e"
	imageTag          = "gantry:e2e"
	namespace         = "unbounded-system"
	dsName            = "gantry"
	e2eRegistry       = "registry.k8s.io"
	e2eRegistryServer = "https://registry.k8s.io"
	e2ePullImage      = "registry.k8s.io/e2e-test-images/agnhost:2.39"
)

// harness bundles the setup/teardown lifecycle for one e2e run. One
// instance is shared across the test functions in this package.
type harness struct {
	t           *testing.T
	repoRoot    string
	artifacts   string
	manifests   string
	keepCluster bool
}

// newHarness resolves the repository root and renders the current Gantry
// templates into a test-local directory.
func newHarness(t *testing.T) *harness {
	t.Helper()

	root := repoRoot(t)

	artifacts := filepath.Join(root, "e2e", ".artifacts")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}

	manifests := t.TempDir()
	if err := render.Render(filepath.Join(root, "deploy", "gantry"), manifests, map[string]string{
		"Namespace": namespace,
		"Image":     imageTag,
	}); err != nil {
		t.Fatalf("render gantry manifests: %v", err)
	}

	return &harness{
		t:           t,
		repoRoot:    root,
		artifacts:   artifacts,
		manifests:   manifests,
		keepCluster: os.Getenv("E2E_KEEP") == "1",
	}
}

// checkPrereqs fails the test fast if any required CLI is missing or
// docker isn't running.
func (h *harness) checkPrereqs() {
	h.t.Helper()

	for _, bin := range []string{"docker", "kind", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			h.t.Skipf("e2e prereq %q missing on PATH; skipping suite", bin)
		}
	}
	// docker info is the canonical "is the engine actually running?" probe.
	if err := h.run(context.Background(), "docker", "info"); err != nil {
		h.t.Skipf("docker engine unreachable (%v); skipping suite", err)
	}
}

// bootCluster creates the kind cluster declared by kind-config.yaml.
// Idempotent: if a cluster with the same name already exists we use it.
func (h *harness) bootCluster(ctx context.Context) {
	h.t.Helper()

	if h.clusterExists(ctx) {
		h.t.Logf("kind cluster %q already exists; reusing", clusterName)
		return
	}

	cfg := filepath.Join(h.repoRoot, "e2e", "gantry", "kind-config.yaml")
	if err := h.run(ctx, "kind", "create", "cluster", "--config", cfg, "--wait", "120s"); err != nil {
		h.t.Fatalf("kind create cluster: %v", err)
	}
}

func (h *harness) clusterExists(ctx context.Context) bool {
	out, err := h.runOut(ctx, "kind", "get", "clusters")
	if err != nil {
		return false
	}

	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == clusterName {
			return true
		}
	}

	return false
}

// buildAndLoadImage builds the gantry container image for the kind
// cluster's host platform and loads it into every kind node.
func (h *harness) buildAndLoadImage(ctx context.Context) {
	h.t.Helper()

	platform := fmt.Sprintf("linux/%s", goArchForDocker(runtime.GOARCH))

	if err := h.run(ctx, "docker", "build", "--platform", platform, "--tag", imageTag,
		"--file", filepath.Join(h.repoRoot, "images", "gantry", "Containerfile"), "."); err != nil {
		h.t.Fatalf("build image: %v", err)
	}

	if err := h.run(ctx, "kind", "load", "docker-image", imageTag, "--name", clusterName); err != nil {
		h.t.Fatalf("kind load: %v", err)
	}
}

// applyManifests installs the gantry rollout into the kind cluster,
// rewriting the DaemonSet image to the freshly-loaded e2e tag.
func (h *harness) applyManifests(ctx context.Context) {
	h.t.Helper()
	// `kubectl create ns --dry-run=client` would generate the manifest;
	// the serviceaccount.yaml below contains the namespace declaration
	// so we don't need a separate ns-create call. Documenting here so
	// it doesn't look like an oversight.
	if err := h.run(ctx, "kubectl", "apply", "-f",
		filepath.Join(h.manifests, "serviceaccount.yaml")); err != nil {
		h.t.Fatalf("apply serviceaccount: %v", err)
	}

	if err := h.applyConfigMap(ctx); err != nil {
		h.t.Fatalf("apply configmap: %v", err)
	}
	// NetworkPolicy is intentionally NOT applied by the e2e harness.
	// The hardening manifest at deploy/examples/networkpolicy.yaml
	// is templated - every rule defers CIDR/namespace choices to the
	// operator (see deploy/README.md § Hardening overlays). It is
	// designed for a production cluster with known control-plane,
	// node-CIDR, and namespace values; on kind those values are
	// either dynamic or unknown at apply time, so applying the
	// template as-is would either fail validation (unresolved
	// placeholders) or silently isolate the agent pods from the
	// kind control plane and break every downstream check.
	//
	// A previous revision exposed an opt-in GANTRY_E2E_NETWORKPOLICY
	// env var, but that flag was unusable as shipped: setting it
	// against the template caused the apply to fail, so no
	// caller could meaningfully turn it on. The eleventh review
	// flagged this as dead code. Removed in favor of a future
	// kind-friendly NetworkPolicy template + dedicated hardening
	// variant; tracked as TODO below.
	//
	// TODO(hardening-e2e): generate a kind-specific NetworkPolicy
	// with concrete control-plane and node CIDRs at harness setup
	// time, then add a separate e2e variant (TestE2E_Hardening)
	// that applies it. The production-reference template at
	// deploy/examples/networkpolicy.yaml is unchanged.
	if err := h.applyDaemonSet(ctx); err != nil {
		h.t.Fatalf("apply daemonset: %v", err)
	}
}

func (h *harness) installMirrorHosts(ctx context.Context) {
	h.t.Helper()

	content := fmt.Sprintf(`server = %q

[host."http://127.0.0.1:5000"]
  capabilities = ["pull", "resolve"]
  skip_verify = true
`, e2eRegistryServer)

	for _, node := range h.kindNodes(ctx) {
		path := "/etc/containerd/certs.d/" + e2eRegistry + "/hosts.toml"

		cmd := "mkdir -p " + shellQuote(filepath.Dir(path)) + " && cat > " + shellQuote(path)
		if err := h.runWithInput(ctx, content, "docker", "exec", "-i", node, "sh", "-c", cmd); err != nil {
			h.t.Fatalf("install hosts.toml on %s: %v", node, err)
		}
	}
}

func (h *harness) kindNodes(ctx context.Context) []string {
	h.t.Helper()

	out, err := h.runOut(ctx, "kind", "get", "nodes", "--name", clusterName)
	if err != nil {
		h.t.Fatalf("kind get nodes: %v", err)
	}

	var nodes []string

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			nodes = append(nodes, line)
		}
	}

	if len(nodes) == 0 {
		h.t.Fatal("kind returned no nodes")
	}

	return nodes
}

func (h *harness) workerNodes(ctx context.Context) []string {
	h.t.Helper()

	out, err := h.runOut(ctx, "kubectl", "get", "nodes", "-l", "!node-role.kubernetes.io/control-plane", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		h.t.Fatalf("kubectl get nodes: %v", err)
	}

	var workers []string

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		workers = append(workers, line)
	}

	if len(workers) < 2 {
		h.t.Fatalf("need at least 2 worker nodes for peer-reuse e2e, got %d", len(workers))
	}

	return workers
}

func (h *harness) applyPullPod(ctx context.Context, name, nodeName string) {
	h.t.Helper()

	manifest := strings.Join([]string{
		"apiVersion: v1",
		"kind: Pod",
		"metadata:",
		"  name: " + name,
		"  namespace: default",
		"  labels:",
		"    app.kubernetes.io/name: gantry-e2e-pull",
		"spec:",
		"  restartPolicy: Never",
		"  nodeName: " + nodeName,
		"  tolerations:",
		"    - operator: Exists",
		"  containers:",
		"    - name: agnhost",
		"      image: " + e2ePullImage,
		"      imagePullPolicy: Always",
		"      command: [\"/agnhost\", \"pause\"]",
		"",
	}, "\n")
	if err := h.runWithInput(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("apply pull pod %s: %v", name, err)
	}
}

func (h *harness) waitForPodReady(ctx context.Context, name string) {
	h.t.Helper()
	h.waitForPodReadyTimeout(ctx, name, "180s")
}

// waitForPodReadyTimeout is waitForPodReady with a custom kubectl wait
// timeout. Tests that exercise cold-start credentialed pulls need a
// generous timeout because containerd's pull-retry backoff stretches
// well past the default 3-minute window when gantry's mirror serves
// 503 retry-after responses while the upstream fetch is in flight.
func (h *harness) waitForPodReadyTimeout(ctx context.Context, name, timeout string) {
	h.t.Helper()

	if err := h.run(ctx, "kubectl", "wait", "--for=condition=Ready", "pod/"+name, "--timeout="+timeout); err != nil {
		h.dumpDiagnostics(ctx)
		h.t.Fatalf("wait for pod %s ready: %v", name, err)
	}
}

func (h *harness) deletePod(ctx context.Context, name string) {
	h.t.Helper()

	// --ignore-not-found already covers the pod being gone, so anything left is
	// a real failure, and the steps that follow assume the pod is not there.
	if err := h.run(ctx, "kubectl", "delete", "pod", name,
		"--ignore-not-found=true", "--wait=true", "--timeout=60s"); err != nil {
		h.t.Fatalf("delete pod %s: %v", name, err)
	}
}

func (h *harness) removePullImageFromNodes(ctx context.Context) {
	h.t.Helper()

	for _, node := range h.kindNodes(ctx) {
		cmd := "crictl rmi " + shellQuote(e2ePullImage) + " >/dev/null 2>&1 || true; " +
			"ctr -n k8s.io images rm " + shellQuote(e2ePullImage) + " >/dev/null 2>&1 || true"
		if err := h.run(ctx, "docker", "exec", node, "sh", "-c", cmd); err != nil {
			h.t.Fatalf("remove cached test image from %s: %v", node, err)
		}
	}
}

func (h *harness) metricSum(ctx context.Context, metric string, filters ...string) float64 {
	h.t.Helper()

	total := 0.0
	for _, pod := range h.gantryPods(ctx) {
		total += h.metricSumOnPod(ctx, pod, metric, filters...)
	}

	return total
}

// metricSumOnPod returns the sum of all `metric{...}` series exposed by
// a single gantry pod whose label set contains all `filters`. Used by
// tests that need to assert "exactly this pod (and not its peers)
// served the request" - gantry's per-node metrics are not aggregated
// upstream, so the sharding is intrinsic.
func (h *harness) metricSumOnPod(ctx context.Context, pod, metric string, filters ...string) float64 {
	h.t.Helper()

	total := 0.0

	metrics := h.fetchPodMetrics(ctx, pod)
	for _, line := range strings.Split(metrics, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if line != metric && !strings.HasPrefix(line, metric+"{") && !strings.HasPrefix(line, metric+" ") {
			continue
		}

		matched := true

		for _, filter := range filters {
			if !strings.Contains(line, filter) {
				matched = false
				break
			}
		}

		if !matched {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err == nil {
			total += value
		}
	}

	return total
}

// pleasePullServedDigestRE matches the digest field emitted by gantry's
// `please_pull served` slog entry. The log line is a JSON object so the
// digest always appears as `"digest":"sha256:..."`.
var pleasePullServedDigestRE = regexp.MustCompile(`"digest":"(sha256:[0-9a-f]{64})"`)

// pleasePullServedDigests parses pod's gantry container log (last
// tailLines) and returns the set of digests that pod logged a
// `please_pull served` entry for. Used by the cold-start e2e to assert
// per-digest HRW exclusivity across pods.
func (h *harness) pleasePullServedDigests(ctx context.Context, pod string, tailLines int) map[string]struct{} {
	h.t.Helper()

	logs, err := h.runOut(ctx, "kubectl", "-n", namespace, "logs", pod, "-c", "gantry",
		fmt.Sprintf("--tail=%d", tailLines))
	if err != nil {
		h.t.Fatalf("read pod %s logs: %v", pod, err)
	}

	out := map[string]struct{}{}

	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "please_pull served") {
			continue
		}

		m := pleasePullServedDigestRE.FindStringSubmatch(line)
		if len(m) >= 2 {
			out[m[1]] = struct{}{}
		}
	}

	return out
}

func (h *harness) waitForMetricIncrease(ctx context.Context, metric string, before float64, filters ...string) float64 {
	h.t.Helper()

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		got := h.metricSum(ctx, metric, filters...)
		if got > before {
			return got
		}

		select {
		case <-ctx.Done():
			h.t.Fatalf("ctx cancelled waiting for metric %s: %v", metric, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}

	h.t.Fatalf("metric %s did not increase above %.0f within 2m", metric, before)

	return before
}

func (h *harness) waitForMetricIncreaseOnPod(ctx context.Context, pod, metric string, before float64, filters ...string) float64 {
	h.t.Helper()

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		got := h.metricSumOnPod(ctx, pod, metric, filters...)
		if got > before {
			return got
		}

		select {
		case <-ctx.Done():
			h.t.Fatalf("context cancelled waiting for metric %s on %s: %v", metric, pod, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}

	h.t.Fatalf("metric %s on %s did not increase above %.0f within 2m", metric, pod, before)

	return before
}

func (h *harness) gantryPods(ctx context.Context) []string {
	h.t.Helper()

	out, err := h.runOut(ctx, "kubectl", "-n", namespace, "get", "pods", "-l", "app.kubernetes.io/name=gantry", "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		h.t.Fatalf("get gantry pods: %v", err)
	}

	var pods []string

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			pods = append(pods, line)
		}
	}

	if len(pods) == 0 {
		h.t.Fatal("no gantry pods found")
	}

	return pods
}

// gantryPodOnNode returns the name of the gantry pod scheduled on
// nodeName. Fails the test if exactly one such pod is not found.
func (h *harness) gantryPodOnNode(ctx context.Context, nodeName string) string {
	h.t.Helper()

	out, err := h.runOut(ctx, "kubectl", "-n", namespace, "get", "pods",
		"-l", "app.kubernetes.io/name=gantry",
		"--field-selector", "spec.nodeName="+nodeName,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		h.t.Fatalf("get gantry pod on %s: %v", nodeName, err)
	}

	var pods []string

	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			pods = append(pods, s)
		}
	}

	if len(pods) != 1 {
		h.t.Fatalf("expected exactly one gantry pod on node %s, got %d", nodeName, len(pods))
	}

	return pods[0]
}

// lastLines returns the last n newline-separated lines of s. Used to
// keep failure logs in test output bounded.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}

	return strings.Join(lines[len(lines)-n:], "\n")
}

func (h *harness) fetchPodMetrics(ctx context.Context, pod string) string {
	h.t.Helper()

	body, err := h.fetchPodPath(ctx, pod, "/metrics")
	if err != nil {
		h.t.Fatalf("metrics from %s: %v", pod, err)
	}

	return body
}

func (h *harness) fetchPodPath(ctx context.Context, pod, path string) (string, error) {
	h.t.Helper()
	port := freeLocalPort(h.t)

	pfCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(pfCtx, "kubectl", "-n", namespace, "port-forward", "pod/"+pod, fmt.Sprintf("%d:9095", port))
	cmd.Dir = h.repoRoot
	cmd.Env = os.Environ()

	var buf bytes.Buffer

	cmd.Stdout = &buf

	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start port-forward for %s: %v", pod, err)
	}

	done := make(chan struct{})

	var waitErr error

	go func() {
		waitErr = cmd.Wait()

		close(done)
	}()

	defer func() {
		cancel()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			// The port-forward is being torn down deliberately and may already
			// have exited, so a failure to signal it says nothing useful.
			_ = cmd.Process.Kill() //nolint:errcheck // best-effort teardown of a process that may be gone
		}
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	deadline := time.Now().Add(20 * time.Second)

	var lastErr error

	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			// Dropping this left req nil, and Do(nil) panics rather than
			// reporting anything about the URL that was wrong.
			return "", fmt.Errorf("build request for %s: %w", url, err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			body, readErr := io.ReadAll(resp.Body)
			closeBody(resp)

			if readErr != nil {
				return "", readErr
			}

			return string(body), nil
		}

		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("status %d", resp.StatusCode)

			closeBody(resp)
		}

		select {
		case <-done:
			return "", fmt.Errorf("port-forward for %s exited early (%v): %s", pod, waitErr, buf.String())
		case <-time.After(500 * time.Millisecond):
		}
	}

	return "", fmt.Errorf("%s from %s unavailable: %v (port-forward logs: %s)", path, pod, lastErr, buf.String())
}

func (h *harness) applyDaemonSet(ctx context.Context) error {
	raw, err := os.ReadFile(filepath.Join(h.manifests, "daemonset.yaml"))
	if err != nil {
		return err
	}

	patched, err := patchDaemonSetForE2E(string(raw), imageTag)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(patched)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout

	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply: %w (stderr: %s)", err, stderr.String())
	}

	return nil
}

// applyConfigMap loads the rendered deploy/gantry/configmap.yaml, rewrites the
// upstream_registries block so the e2e cluster does NOT depend on
// whatever placeholder ships in the default production ConfigMap,
// and pipes the result into `kubectl apply -f -`. See
// patchConfigMapForE2E for the rationale.
func (h *harness) applyConfigMap(ctx context.Context) error {
	raw, err := os.ReadFile(filepath.Join(h.manifests, "configmap.yaml"))
	if err != nil {
		return err
	}

	patched, err := patchConfigMapForE2E(string(raw))
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(patched)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout

	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply: %w (stderr: %s)", err, stderr.String())
	}

	return nil
}

// patchDaemonSetForE2E rewrites the rendered DaemonSet's gantry
// container - and ONLY the gantry container - to use the
// side-loaded e2e image with imagePullPolicy=Never. The busybox
// initContainer (which also has `imagePullPolicy: IfNotPresent`)
// is left untouched so kind's containerd can pull it from the
// public registry on first boot.
//
// This pure helper exists so the patch logic is unit-testable
// without spinning up a kind cluster or shelling out to kubectl.
// The harness's applyDaemonSet wraps it.
//
// Why this is structural rather than a bare strings.Replace call: the
// DaemonSet has TWO
// `imagePullPolicy: IfNotPresent` entries. A previous revision
// used `strings.Replace(..., 1)` on the policy line alone, which
// patched the busybox initContainer (first occurrence) instead of
// gantry. The result on a fresh kind cluster was the busybox init image
// being set to `Never` - but busybox is not preloaded into kind
// so kubelet hit ErrImageNeverPull and the initContainer never
// started. The eleventh-review fix was to anchor the swap on a
// multi-line pattern that uniquely matches the gantry container,
// then fail loud if the anchor stops matching.
func patchDaemonSetForE2E(raw, imageTag string) (string, error) {
	const anchor = "        - name: gantry\n          image: "

	start := strings.Index(raw, anchor)
	if start < 0 {
		return "", errors.New("patchDaemonSetForE2E: gantry container anchor not found in rendered daemonset.yaml")
	}

	imageStart := start + len(anchor)

	imageEnd := strings.IndexByte(raw[imageStart:], '\n')
	if imageEnd < 0 {
		return "", errors.New("patchDaemonSetForE2E: gantry image line is incomplete")
	}

	imageEnd += imageStart

	const policy = "          imagePullPolicy: IfNotPresent"

	policyStart := imageEnd + 1
	if !strings.HasPrefix(raw[policyStart:], policy) {
		return "", errors.New("patchDaemonSetForE2E: gantry imagePullPolicy anchor not found in rendered daemonset.yaml")
	}

	patched := raw[:imageStart] + imageTag + "\n          imagePullPolicy: Never" + raw[policyStart+len(policy):]

	return patched, nil
}

// configMapUpstreamRegistriesAnchor is the multi-line block in
// deploy/configmap.yaml that lists the operator-facing placeholder
// registries. patchConfigMapForE2E swaps the whole block for a
// single anonymous-public entry so the e2e cluster is self-contained
// - see that helper's doc for why.
const configMapUpstreamRegistriesAnchor = `    upstream_registries:
      - name: "registry.example.com"
        endpoint: "https://registry.example.com"
        # credentials_path: "/etc/gantry/registry/registry.example.com"
      # - name: "ghcr.io"
      #   endpoint: "https://ghcr.io"
      #   credentials_path: "/etc/gantry/registry/ghcr.io"`

// e2eConfigMapUpstreamRegistriesReplacement is what patchConfigMapForE2E
// substitutes in for configMapUpstreamRegistriesAnchor. A single
// anonymous-public registry.k8s.io entry keeps the e2e cluster
// self-contained and matches installMirrorHosts above.
const e2eConfigMapUpstreamRegistriesReplacement = `    upstream_registries:
      - name: "registry.k8s.io"
        endpoint: "https://registry.k8s.io"`

// patchConfigMapForE2E rewrites deploy/configmap.yaml's
// upstream_registries block so the e2e cluster does not depend on
// the production-facing placeholder entries (`registry.example.com`,
// commented-out `ghcr.io`, optionally-mounted credentials secret).
//
// Why this exists: deploy/configmap.yaml is shipped for operators,
// and its default upstream_registries list is by design a placeholder
// that operators are expected to edit before rolling Gantry out. The
// reviewer's case for the e2e harness was the reverse direction:
//
//   - If credentials_path is ever uncommented on the default entry,
//     origin.New eagerly reads the file at startup and crashloops the
//     pod - the registry-creds Secret volume in daemonset.yaml is
//     `optional: true`, so kubelet still starts the pod but the agent
//     dies on the first `os.ReadFile` in newRegistry.
//   - Even with credentials_path commented out, leaving the host
//     `registry.example.com` in the e2e config means every future
//     scenario test that actually pulls an image (cache-hit on second
//     pull, NF5 chaos, eviction headroom) has to special-case the
//     placeholder. Rewriting it once at apply time costs nothing and
//     makes the e2e cluster behave like a real operator deployment
//     targeting registry.k8s.io anonymously.
//
// Returns an error if the anchor stops matching, exactly like
// patchDaemonSetForE2E: silently no-op'ing on a reformatted ConfigMap
// would let the harness ship a default-placeholder e2e cluster that
// the reviewer flagged as fragile.
func patchConfigMapForE2E(raw string) (string, error) {
	patched := strings.Replace(raw, configMapUpstreamRegistriesAnchor, e2eConfigMapUpstreamRegistriesReplacement, 1)
	if patched == raw {
		return "", fmt.Errorf("patchConfigMapForE2E: upstream_registries anchor not found in deploy/configmap.yaml; update configMapUpstreamRegistriesAnchor in harness_e2e.go")
	}

	return patched, nil
}

// waitForRollout polls until the DaemonSet reports all desired pods
// ready, or the context fires.
//
// The poll cadence is 5 s - explicitly chosen to honor the
// "never sleep > 5s in one call" project preference: the loop sleeps
// in 5 s steps so the test stays interruptible by Ctrl-C / ctx.
func (h *harness) waitForRollout(ctx context.Context) {
	h.t.Helper()

	deadline := time.Now().Add(5 * time.Minute)
	for {
		if time.Now().After(deadline) {
			h.dumpDiagnostics(ctx)
			h.t.Fatalf("daemonset %s/%s did not roll out within 5m", namespace, dsName)
		}

		out, err := h.runOut(ctx, "kubectl", "-n", namespace, "rollout", "status",
			"ds/"+dsName, "--timeout=5s")
		if err == nil && strings.Contains(out, "rolled out") {
			return
		}

		select {
		case <-ctx.Done():
			h.t.Fatalf("ctx cancelled while waiting for rollout: %v", ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

// checkReadyz reaches one daemon pod's /readyz endpoint through
// kubectl port-forward. The Gantry image is distroless, so kubectl exec
// with wget/curl is not available inside the container.
func (h *harness) checkReadyz(ctx context.Context) {
	h.t.Helper()
	name := h.gantryPods(ctx)[0]

	out, err := h.fetchPodPath(ctx, name, "/readyz")
	if err != nil {
		h.t.Fatalf("/readyz probe on pod %q: %v", name, err)
	}

	if !strings.Contains(out, "ok") && !strings.Contains(out, "ready") {
		h.t.Fatalf("/readyz returned %q; expected 'ok' or 'ready'", out)
	}
}

// teardown deletes the kind cluster. Skipped when E2E_KEEP=1.
func (h *harness) teardown(ctx context.Context) {
	if h.keepCluster {
		h.t.Logf("E2E_KEEP=1 - leaving cluster %q running", clusterName)
		return
	}

	if err := h.run(ctx, "kind", "delete", "cluster", "--name", clusterName); err != nil {
		h.t.Logf("kind delete cluster: %v", err)
	}
}

// dumpDiagnostics writes pod logs + describe output into the
// artifacts dir on failure.
func (h *harness) dumpDiagnostics(ctx context.Context) {
	h.t.Helper()

	for _, args := range [][]string{
		{"-n", namespace, "get", "pods", "-o", "wide"},
		{"-n", namespace, "describe", "ds/" + dsName},
		{"-n", namespace, "logs", "ds/" + dsName, "--all-containers", "--tail=200"},
		{"get", "pods", "-A", "-o", "wide"},
	} {
		out, runErr := h.runOut(ctx, "kubectl", args...)
		if runErr != nil {
			// Partial output is still worth keeping, so the error joins the
			// artifact rather than discarding what the command did produce.
			out += "\n\ncommand failed: " + runErr.Error() + "\n"
		}

		// Sanitize args for the filename - kubectl args can contain
		// '/' (e.g. ds/gantry, pod/foo) which os.WriteFile would
		// silently fail on because the parent dir does not exist.
		safe := strings.ReplaceAll(strings.Join(args, "_"), "/", "_")
		dst := filepath.Join(h.artifacts, fmt.Sprintf("%s_%s.log", h.t.Name(), safe))
		//#nosec G306 -- test artifact
		if err := os.WriteFile(dst, []byte(out), 0o644); err != nil {
			// The comment above exists because this silently failed once
			// already; saying so beats writing nothing and reporting success.
			h.t.Logf("diagnostics: could not write %s: %v", dst, err)

			continue
		}

		h.t.Logf("diagnostics: wrote %s", dst)
	}
}

// evictImageFromNode simulates kubelet eviction by deleting the
// given image from the node's containerd store. If image is empty,
// evicts e2ePullImage for backwards compatibility with existing
// smoke-test callers.
//
// We also drop any gantry-managed content leases on the node so
// containerd's GC actually collects the blobs. Without this the
// image bytes survive `crictl rmi` (because gantry pins them for the
// configured lease TTL) and a subsequent kubelet pull short-circuits
// at containerd's HEAD-then-mount fast path - never reaching the
// mirror - which would silently bypass the credential code path
// under test.
func (h *harness) evictImageFromNode(ctx context.Context, nodeName string, image ...string) {
	h.t.Helper()

	target := e2ePullImage
	if len(image) > 0 && image[0] != "" {
		target = image[0]
	}

	cmd := "crictl rmi " + shellQuote(target) + " >/dev/null 2>&1 || true; " +
		"ctr -n k8s.io images rm " + shellQuote(target) + " >/dev/null 2>&1 || true; " +
		// Drop all gantry-prefixed leases (lease IDs start with
		// 'gantry-sha256:'). Without this, content survives image
		// removal until the gantry lease TTL elapses.
		"for L in $(ctr -n k8s.io leases ls -q 2>/dev/null | grep '^gantry-'); do " +
		"ctr -n k8s.io leases rm \"$L\" >/dev/null 2>&1 || true; done; " +
		// Force a content GC sweep so blobs no longer referenced by
		// any image or lease are reclaimed before the next pull.
		"ctr -n k8s.io content prune references >/dev/null 2>&1 || true"
	if err := h.run(ctx, "docker", "exec", nodeName, "sh", "-c", cmd); err != nil {
		h.t.Fatalf("evict image from %s: %v", nodeName, err)
	}
}

// verifyContainerdSocketAccess confirms that a gantry pod can access
// the node's containerd socket with proper read/write permissions.
// The gantry image is distroless (no shell, no stat), so we probe
// indirectly through the gantry agent itself: on successful socket
// connect it logs "connected to containerd" at startup, and the
// /readyz HTTP endpoint only flips to OK once the containerd-backed
// content store is wired up. We check both signals here.
func (h *harness) verifyContainerdSocketAccess(ctx context.Context, pod string) {
	h.t.Helper()

	logs, err := h.runOut(ctx, "kubectl", "-n", namespace, "logs", pod, "-c", "gantry", "--tail=200")
	if err != nil {
		h.dumpDiagnostics(ctx)
		h.t.Fatalf("read pod %s logs: %v", pod, err)
	}

	if !strings.Contains(logs, "connected to containerd") {
		h.dumpDiagnostics(ctx)
		h.t.Fatalf("pod %s did not log 'connected to containerd' - socket access likely broken", pod)
	}

	body, err := h.fetchPodPath(ctx, pod, "/readyz")
	if err != nil {
		h.dumpDiagnostics(ctx)
		h.t.Fatalf("pod %s /readyz: %v", pod, err)
	}

	if b := strings.ToLower(strings.TrimSpace(body)); b != "ready" && b != "ok" {
		h.dumpDiagnostics(ctx)
		h.t.Fatalf("pod %s /readyz body = %q, want 'ready' or 'ok' (socket-backed content store not wired)", pod, body)
	}
}

func (h *harness) podUID(ctx context.Context, pod string) string {
	h.t.Helper()

	out, err := h.runOut(ctx, "kubectl", "-n", namespace, "get", "pod", pod, "-o", "jsonpath={.metadata.uid}")
	if err != nil {
		h.t.Fatalf("get pod %s UID: %v", pod, err)
	}

	return strings.TrimSpace(out)
}

func (h *harness) podRestartCount(ctx context.Context, pod string) int {
	h.t.Helper()

	out, err := h.runOut(ctx, "kubectl", "-n", namespace, "get", "pod", pod,
		"-o", "jsonpath={.status.containerStatuses[?(@.name==\"gantry\")].restartCount}")
	if err != nil {
		h.t.Fatalf("get pod %s restart count: %v", pod, err)
	}

	count, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		h.t.Fatalf("parse pod %s restart count %q: %v", pod, out, err)
	}

	return count
}

func (h *harness) containerdSocketID(ctx context.Context, node string) string {
	h.t.Helper()

	out, err := h.runOut(ctx, "docker", "exec", node, "stat", "-Lc", "%d:%i", "/run/containerd/containerd.sock")
	if err != nil {
		h.t.Fatalf("stat containerd socket on %s: %v", node, err)
	}

	return strings.TrimSpace(out)
}

func (h *harness) restartContainerd(ctx context.Context, node string) {
	h.t.Helper()

	if err := h.run(ctx, "docker", "exec", node, "systemctl", "restart", "containerd"); err != nil {
		h.t.Fatalf("restart containerd on %s: %v", node, err)
	}
}

func (h *harness) waitForContainerdSocketReplacement(ctx context.Context, node, oldID string) {
	h.t.Helper()

	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		out, err := h.runOut(ctx, "docker", "exec", node, "stat", "-Lc", "%d:%i", "/run/containerd/containerd.sock")
		if err == nil {
			if id := strings.TrimSpace(out); id != "" && id != oldID {
				return
			}
		}

		select {
		case <-ctx.Done():
			h.t.Fatalf("context cancelled waiting for containerd socket replacement: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}

	h.t.Fatalf("containerd socket on %s retained inode %s after restart", node, oldID)
}

func (h *harness) waitForPodReadyByName(ctx context.Context, pod string) {
	h.t.Helper()

	if err := h.run(ctx, "kubectl", "-n", namespace, "wait", "--for=condition=Ready", "pod/"+pod, "--timeout=180s"); err != nil {
		h.dumpDiagnostics(ctx)
		h.t.Fatalf("wait for gantry pod %s to recover readiness: %v", pod, err)
	}
}

// run executes cmd and pipes stdout+stderr to the test log.
// Inherits the parent process environment; per-call env overrides are
// not currently needed.
func (h *harness) run(ctx context.Context, name string, args ...string) error {
	h.t.Helper()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.repoRoot
	cmd.Env = os.Environ()

	var buf bytes.Buffer

	cmd.Stdout = &buf

	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, buf.String())
	}

	return nil
}

func (h *harness) runWithInput(ctx context.Context, input, name string, args ...string) error {
	h.t.Helper()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.repoRoot
	cmd.Env = os.Environ()
	cmd.Stdin = strings.NewReader(input)

	var buf bytes.Buffer

	cmd.Stdout = &buf

	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, buf.String())
	}

	return nil
}

// runOut is run() but returns stdout for callers that need to parse it.
// stderr is still surfaced on error.
func (h *harness) runOut(ctx context.Context, name string, args ...string) (string, error) {
	h.t.Helper()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.repoRoot
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout

	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, stderr.String())
	}

	return stdout.String(), nil
}

// goArchForDocker maps Go's GOARCH to Docker's platform flag form.
func goArchForDocker(goarch string) string {
	switch goarch {
	case "amd64", "arm64":
		return goarch
	default:
		return goarch
	}
}

func freeLocalPort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate local port: %v", err)
	}

	defer func() {
		if err := ln.Close(); err != nil {
			t.Logf("close probe listener: %v", err)
		}
	}()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T, want *net.TCPAddr", ln.Addr())
	}

	return addr.Port
}

// closeBody discards a response body's close error, which is never actionable
// on a path that has already read what it needs.
func closeBody(resp *http.Response) {
	_ = resp.Body.Close() //nolint:errcheck // nothing can be done about a failed close after reading
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func repoRoot(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err == nil {
		if root, ok := findRepoRoot(wd); ok {
			return root
		}
	}

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	if root, ok := findRepoRoot(filepath.Dir(file)); ok {
		return root
	}

	t.Fatalf("reached filesystem root without finding go.mod")

	return ""
}

func findRepoRoot(start string) (string, bool) {
	dir := start

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}

		dir = parent
	}
}

// guardAssumptions panics if the test environment violates contracts
// the harness depends on. Used by TestMain in smoke_e2e_test.go.
func guardAssumptions() error {
	if runtime.GOOS == "windows" {
		return errors.New("e2e suite is unsupported on windows; run from linux or darwin")
	}

	return nil
}
