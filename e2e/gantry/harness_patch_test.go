//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveContainerEngine(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want string
	}{
		{name: "default", want: "docker"},
		{name: "podman", env: "podman", want: "podman"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONTAINER_ENGINE", tc.env)

			if got := resolveContainerEngine(t); got != tc.want {
				t.Fatalf("resolveContainerEngine() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveClusterName(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want string
	}{
		{name: "default", want: defaultCluster},
		{name: "blank", env: "  ", want: defaultCluster},
		{name: "override", env: " gantry-e2e-mine ", want: "gantry-e2e-mine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GANTRY_E2E_CLUSTER", tc.env)

			if got := resolveClusterName(); got != tc.want {
				t.Fatalf("resolveClusterName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHarnessKubeconfigPathStaysInRepoTmp(t *testing.T) {
	got, err := harnessKubeconfigPath("/repo", "gantry-e2e-mine")
	if err != nil {
		t.Fatalf("harnessKubeconfigPath() error = %v", err)
	}

	if want := filepath.Join("/repo", "tmp", "gantry-e2e", "gantry-e2e-mine", "kubeconfig"); got != want {
		t.Fatalf("harnessKubeconfigPath() = %q, want %q", got, want)
	}
}

func TestHarnessKubeconfigPathRejectsUnsafeClusterNames(t *testing.T) {
	for _, name := range []string{
		"",
		".",
		"..",
		"../../../other",
		"a/b",
		`a\b`,
		`..\sibling`,
		"/abs/path",
		"/",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := harnessKubeconfigPath("/repo", name); err == nil {
				t.Fatalf("harnessKubeconfigPath(%q) = %q, want error", name, got)
			}
		})
	}
}

func TestHarnessKubeconfigPathAcceptsKindNames(t *testing.T) {
	for _, name := range []string{"gantry-e2e", "gantry-ci-fix-kcfg", "kind", "a.b", "..x", "x.."} {
		t.Run(name, func(t *testing.T) {
			got, err := harnessKubeconfigPath("/repo", name)
			if err != nil {
				t.Fatalf("harnessKubeconfigPath(%q) error = %v", name, err)
			}

			if want := filepath.Join("/repo", "tmp", "gantry-e2e", name, "kubeconfig"); got != want {
				t.Fatalf("harnessKubeconfigPath(%q) = %q, want %q", name, got, want)
			}
		})
	}
}

func TestClusterCommandOverridesAmbientKubeconfig(t *testing.T) {
	environ := []string{"PATH=/bin", "KUBECONFIG=/home/someone/.kube/config", "HOME=/home/someone"}

	for _, tc := range []struct {
		name     string
		bin      string
		args     []string
		wantArgs []string
	}{
		{
			name: "kubectl pinned to private config and context",
			bin:  "kubectl",
			args: []string{"-n", "ns", "port-forward", "pod/p", "1:2"},
			wantArgs: []string{
				"--kubeconfig", "/repo/tmp/k", "--context", "kind-mine",
				"-n", "ns", "port-forward", "pod/p", "1:2",
			},
		},
		{
			name:     "kubectl by path",
			bin:      "/usr/local/bin/kubectl",
			args:     []string{"apply", "-f", "-"},
			wantArgs: []string{"--kubeconfig", "/repo/tmp/k", "--context", "kind-mine", "apply", "-f", "-"},
		},
		{
			name:     "kind args unchanged",
			bin:      "kind",
			args:     []string{"get", "nodes", "--name", "mine"},
			wantArgs: []string{"get", "nodes", "--name", "mine"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, env := clusterCommand(tc.bin, tc.args, environ, "/repo/tmp/k", "mine")

			if strings.Join(args, " ") != strings.Join(tc.wantArgs, " ") {
				t.Fatalf("args = %q, want %q", args, tc.wantArgs)
			}

			var kubeconfigs []string

			for _, kv := range env {
				if strings.HasPrefix(kv, "KUBECONFIG=") {
					kubeconfigs = append(kubeconfigs, kv)
				}
			}

			if len(kubeconfigs) != 1 || kubeconfigs[0] != "KUBECONFIG=/repo/tmp/k" {
				t.Fatalf("KUBECONFIG entries = %q, want only the private kubeconfig", kubeconfigs)
			}

			if len(env) != len(environ) {
				t.Fatalf("env = %q, want other variables preserved", env)
			}
		})
	}

	if environ[1] != "KUBECONFIG=/home/someone/.kube/config" {
		t.Fatalf("clusterCommand mutated caller environ: %q", environ)
	}
}

// TestPatchDaemonSetForE2E_TargetsGantryContainers is the
// regression test for the twelfth-review finding: the harness's
// previous strings.Replace(..., 1) on the bare "imagePullPolicy:
// IfNotPresent" string patched the FIRST occurrence, which belonged to
// a busybox init container the DaemonSet had at the time, not the
// gantry container. The gantry container kept IfNotPresent, so kubelet
// would have tried to pull the side-loaded :e2e tag from a registry
// that doesn't have it.
//
// This test pins the contract that patchDaemonSetForE2E:
//   - sets the gantry-init and gantry containers, the two that run the
//     gantry image, to use the side-loaded e2e image,
//   - sets both containers' imagePullPolicy to Never,
//   - leaves no IfNotPresent behind, so a container added later fails
//     here until the harness decides how kind obtains its image,
//   - and fails loudly if an anchor stops matching.
func TestPatchDaemonSetForE2E_TargetsGantryContainers(t *testing.T) {
	repoRoot := repoRoot(t)
	manifests := renderGantryChart(t, repoRoot)

	raw, err := os.ReadFile(filepath.Join(manifests, "daemonset.yaml"))
	if err != nil {
		t.Fatalf("read rendered daemonset.yaml: %v", err)
	}

	const e2eTag = "gantry:e2e-test-fixture"

	patched, err := patchDaemonSetForE2E(string(raw), e2eTag)
	if err != nil {
		t.Fatalf("patchDaemonSetForE2E: %v", err)
	}

	// Both gantry-image containers must reference the side-loaded tag
	// with imagePullPolicy=Never.
	if retargeted := strings.Count(patched, "image: "+e2eTag); retargeted != 2 {
		t.Errorf("'image: %s' count = %d, want 2 (gantry-init and gantry)", e2eTag, retargeted)
	}

	if strings.Contains(patched, "image: "+imageTag) {
		t.Errorf("patched manifest still contains the rendered image %q; the swap missed a container", imageTag)
	}

	if neverCount := strings.Count(patched, "imagePullPolicy: Never"); neverCount != 2 {
		t.Errorf("imagePullPolicy=Never count = %d, want exactly 2 (only gantry-init and gantry should be Never)", neverCount)
	}

	if ifNotPresentCount := strings.Count(patched, "imagePullPolicy: IfNotPresent"); ifNotPresentCount != 0 {
		t.Errorf("imagePullPolicy=IfNotPresent count = %d, want 0; a container besides gantry-init and gantry needs the harness to decide how kind obtains its image", ifNotPresentCount)
	}
}

// TestPatchDaemonSetForE2E_FailsLoudWhenAnchorMissing covers the
// other half of the contract: if deploy/daemonset.yaml's gantry
// container line is reformatted in a way that breaks the anchor,
// the helper must fail loudly rather than silently leaving the
// production image in place. The previous "two independent
// strings.Replace calls" code silently no-op'd in that case; this
// helper returns an explicit error so the harness exits before
// kubectl apply.
func TestPatchDaemonSetForE2E_FailsLoudWhenAnchorMissing(t *testing.T) {
	// A manifest that does NOT contain the gantry container line
	// in the expected layout. Any apply on this should error.
	raw := "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: gantry\nspec: {}\n"

	_, err := patchDaemonSetForE2E(raw, "gantry:e2e")
	if err == nil {
		t.Fatalf("patchDaemonSetForE2E returned nil error on a manifest missing the gantry anchor; want a loud error so the harness aborts before kubectl apply")
	}

	if !strings.Contains(err.Error(), "anchor not found") {
		t.Errorf("error message = %q, want it to mention 'anchor not found' so the operator knows to update gantryContainerAnchor", err.Error())
	}
}

// TestPatchConfigMapForE2E_RewritesUpstreamRegistries pins the
// thirteenth-review fix: the e2e harness must not apply the default
// ConfigMap's upstream_registries block as-is. The default ships
// "registry.example.com" (unreachable in kind) plus a commented
// credentials_path that, if ever uncommented, would crashloop the
// agent because the secret volume is optional. patchConfigMapForE2E
// swaps the whole block for a single anonymous-public registry.k8s.io
// entry so the e2e cluster is self-contained.
func TestPatchConfigMapForE2E_RewritesUpstreamRegistries(t *testing.T) {
	// Render the chart so this test catches changes to the production output.
	repoRoot := repoRoot(t)
	manifests := renderGantryChart(t, repoRoot)

	raw, err := os.ReadFile(filepath.Join(manifests, "configmap.yaml"))
	if err != nil {
		t.Fatalf("read rendered configmap.yaml: %v", err)
	}

	patched, err := patchConfigMapForE2E(string(raw))
	if err != nil {
		t.Fatalf("patchConfigMapForE2E: %v", err)
	}

	// The e2e replacement must reference an anonymous public
	// registry - no credentials_path, no placeholder host.
	if !strings.Contains(patched, `name: "registry.k8s.io"`) {
		t.Errorf("patched ConfigMap missing the e2e registry.k8s.io registry entry")
	}

	if !strings.Contains(patched, `endpoint: "https://registry.k8s.io"`) {
		t.Errorf("patched ConfigMap missing the registry.k8s.io endpoint")
	}
	// The original placeholder host must be GONE - leaving it in
	// would make every cache-miss request fail DNS on kind.
	if strings.Contains(patched, "registry.example.com") {
		t.Errorf("patched ConfigMap still contains the production placeholder 'registry.example.com'; the swap did not apply")
	}

	if !strings.Contains(patched, "chair_cluster_size_estimate: 8") {
		t.Error("patched ConfigMap does not use the eight-node kind chair estimate")
	}

	if !strings.Contains(patched, "chair_seed_count: 8") {
		t.Error("patched ConfigMap does not use an eight-chair seed cohort")
	}

	if !strings.Contains(patched, "chair_count: 8") {
		t.Error("patched ConfigMap does not select every eight-node kind agent as a chair")
	}

	// The E2E topology intentionally makes all eight nodes immediately eligible
	// so its fixed eight-chair assertions do not depend on claim pacing.
	if !strings.Contains(patched, "chair_claim_initial_divisor: 1") {
		t.Error("patched ConfigMap does not make every kind node immediately claim-eligible")
	}

	if !strings.Contains(patched, `chair_startup_jitter: "2s"`) {
		t.Error("patched ConfigMap does not shorten the chair startup jitter for kind")
	}
}

// TestPatchConfigMapForE2E_FailsLoudWhenAnchorMissing covers the
// other half of the contract: if deploy/configmap.yaml's
// upstream_registries block is reformatted in a way that breaks the
// anchor, the helper must fail loudly rather than silently leaving
// the production placeholder in place.
func TestPatchConfigMapForE2E_FailsLoudWhenAnchorMissing(t *testing.T) {
	// A minimal ConfigMap that does NOT contain the expected
	// upstream_registries block. Any apply on this should error.
	raw := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: gantry-config\ndata:\n  config.yaml: |\n    mirror_listen: \"0.0.0.0:5000\"\n"

	_, err := patchConfigMapForE2E(raw)
	if err == nil {
		t.Fatalf("patchConfigMapForE2E returned nil error on a manifest missing the upstream_registries anchor; want a loud error so the harness aborts before kubectl apply")
	}

	if !strings.Contains(err.Error(), "anchor not found") {
		t.Errorf("error message = %q, want it to mention 'anchor not found' so the operator knows to update configMapUpstreamRegistriesAnchor", err.Error())
	}
}
