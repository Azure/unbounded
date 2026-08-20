//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPatchDaemonSetForE2E_TargetsGantryContainerOnly is the
// regression test for the twelfth-review finding: the harness's
// previous strings.Replace(..., 1) on the bare "imagePullPolicy:
// IfNotPresent" string patched the FIRST occurrence (the busybox
// initContainer), not the gantry container.
//
// On a fresh kind cluster the busybox init image is NOT preloaded, so setting
// it to imagePullPolicy=Never caused ErrImageNeverPull and the
// initContainer never started. Meanwhile the gantry container kept
// imagePullPolicy=IfNotPresent, so kubelet would have tried to pull
// the side-loaded :e2e tag from a registry that doesn't have it.
//
// This test pins the contract that patchDaemonSetForE2E:
//   - sets the gantry container to use the side-loaded e2e image,
//   - sets the gantry container's imagePullPolicy to Never,
//   - leaves the busybox initContainer's image and imagePullPolicy
//     UNCHANGED so kind's containerd can pull busybox normally,
//   - and fails loudly if the anchor stops matching.
func TestPatchDaemonSetForE2E_TargetsGantryContainerOnly(t *testing.T) {
	repoRoot := repoRoot(t)

	raw, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "gantry", "daemonset.yaml.tmpl"))
	if err != nil {
		t.Fatalf("read daemonset.yaml.tmpl: %v", err)
	}

	raw = []byte(strings.ReplaceAll(string(raw), "{{ .Image }}", "gantry:rendered-fixture"))

	const e2eTag = "gantry:e2e-test-fixture"

	patched, err := patchDaemonSetForE2E(string(raw), e2eTag)
	if err != nil {
		t.Fatalf("patchDaemonSetForE2E: %v", err)
	}

	// Gantry container must reference the side-loaded tag with
	// imagePullPolicy=Never.
	if !strings.Contains(patched, "image: "+e2eTag) {
		t.Errorf("patched manifest missing 'image: %s'; gantry container was not retargeted", e2eTag)
	}

	if strings.Contains(patched, "image: gantry:rendered-fixture") {
		t.Errorf("patched manifest still contains the original gantry image reference; the swap did not apply to the gantry container")
	}

	// Busybox initContainer must be UNCHANGED. The reviewer's
	// concrete case: busybox is not preloaded into kind, so it
	// must keep imagePullPolicy=IfNotPresent (kind's containerd
	// pulls it from the public registry on first boot).
	const busyboxImage = "image: mcr.microsoft.com/cbl-mariner/busybox:2.0"
	if !strings.Contains(patched, busyboxImage) {
		t.Errorf("patched manifest is missing the busybox initContainer image; the helper accidentally rewrote it")
	}

	// Count imagePullPolicy occurrences: there must be exactly
	// one "Never" (the gantry container) and one "IfNotPresent"
	// (the busybox initContainer). Anything else means the
	// swap leaked across containers.
	neverCount := strings.Count(patched, "imagePullPolicy: Never")
	ifNotPresentCount := strings.Count(patched, "imagePullPolicy: IfNotPresent")

	if neverCount != 1 {
		t.Errorf("imagePullPolicy=Never count = %d, want exactly 1 (only the gantry container should be Never)", neverCount)
	}

	if ifNotPresentCount != 1 {
		t.Errorf("imagePullPolicy=IfNotPresent count = %d, want exactly 1 (the busybox initContainer must keep IfNotPresent so kind can pull it)", ifNotPresentCount)
	}

	// Specifically: the line that follows the busybox image
	// must still be `imagePullPolicy: IfNotPresent`. This is the
	// load-bearing assertion - it's the exact bug class the
	// twelfth review flagged.
	busyboxIdx := strings.Index(patched, busyboxImage)
	if busyboxIdx < 0 {
		t.Fatalf("patched: busybox not found")
	}

	after := patched[busyboxIdx:]
	// Find the next imagePullPolicy line after the busybox image
	// line.
	policyIdx := strings.Index(after, "imagePullPolicy:")
	if policyIdx < 0 {
		t.Fatalf("patched: imagePullPolicy not found after busybox")
	}
	// Read the policy value (up to end of line).
	policyLine := after[policyIdx:]
	if nl := strings.IndexByte(policyLine, '\n'); nl >= 0 {
		policyLine = policyLine[:nl]
	}

	if !strings.Contains(policyLine, "IfNotPresent") {
		t.Errorf("busybox initContainer policy line = %q, want it to retain 'IfNotPresent' (the previous bug set busybox to Never which fails on kind because busybox is not preloaded)", policyLine)
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
	// Locate the real deploy/gantry/configmap template so this test also
	// catches "someone reformatted the upstream_registries block"
	// regressions in the production manifest.
	repoRoot := repoRoot(t)

	raw, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "gantry", "configmap.yaml.tmpl"))
	if err != nil {
		t.Fatalf("read configmap.yaml.tmpl: %v", err)
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
	// The commented-out ghcr.io alternative entry must also be
	// gone - the whole upstream_registries block is replaced.
	if strings.Contains(patched, `name: "ghcr.io"`) {
		t.Errorf("patched ConfigMap still contains the commented ghcr.io alternative; the swap did not replace the whole block")
	}
	// The two operative credentials_path references that lived
	// inside the original upstream_registries block must be GONE.
	// The doc-prose mention near the top of the block (which uses
	// backticks to describe the field as a concept for operators)
	// is unaffected by the patch and intentionally left in place
	// so the patched ConfigMap is still self-documenting.
	if strings.Contains(patched, `# credentials_path: "/etc/gantry/registry/registry.example.com"`) {
		t.Errorf("patched ConfigMap still contains the commented credentials_path for registry.example.com; the upstream_registries swap did not remove the whole entry")
	}

	if strings.Contains(patched, `credentials_path: "/etc/gantry/registry/ghcr.io"`) {
		t.Errorf("patched ConfigMap still contains the credentials_path for ghcr.io; the upstream_registries swap did not remove the whole alternative entry")
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
