// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/override"
)

const overridesNamespace = "unbounded-system"

func overridesScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()

	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, v1alpha3.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	return scheme
}

func validOverridesDocument() string {
	return `apiVersion: ` + override.APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    extraArgs:
      node: ["--verbose"]
`
}

func overridesConfigMapFor(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       overridesNamespace,
			Name:            override.ConfigMapName,
			ResourceVersion: "12",
		},
		Data: data,
	}
}

func TestOverridesValidateAcceptsAValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overrides.yaml")

	if err := os.WriteFile(path, []byte(validOverridesDocument()), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var out bytes.Buffer
	if err := runOverridesValidateFiles([]string{path}, &out); err != nil {
		t.Fatalf("validate: %v", err)
	}

	if !strings.Contains(out.String(), "ok: 1 entry") {
		t.Fatalf("output = %q, want a success line", out.String())
	}
}

// TestOverridesValidateSaysItDoesNotResolve is the point of scoping this
// command.
//
// Whether a container exists depends on the workload the running operator
// renders, and a plugin built from a different commit would answer from its own
// copy of the manifests. Claiming full validation would be wrong precisely when
// an install is unusual, so the output has to say what it did not check.
func TestOverridesValidateSaysItDoesNotResolve(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overrides.yaml")

	if err := os.WriteFile(path, []byte(validOverridesDocument()), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var out bytes.Buffer
	if err := runOverridesValidateFiles([]string{path}, &out); err != nil {
		t.Fatalf("validate: %v", err)
	}

	for _, want := range []string{"resolved by the operator", "overrides status"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, want it to mention %q", out.String(), want)
		}
	}
}

func TestOverridesValidateRejectsBadFiles(t *testing.T) {
	cases := map[string]string{
		"missing apiVersion": "overrides: []\n",
		"protected path": `apiVersion: ` + override.APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            serviceAccountName: other
`,
		"unknown component": `apiVersion: ` + override.APIVersion + `
overrides:
  - component: nope
    kind: DaemonSet
    extraArgs:
      x: ["--y"]
`,
	}

	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "overrides.yaml")

			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}

			var out bytes.Buffer
			if err := runOverridesValidateFiles([]string{path}, &out); err == nil {
				t.Fatal("expected the document to be rejected")
			}
		})
	}
}

func TestOverridesValidateReportsAbsentConfigMap(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(overridesScheme(t)).Build()

	var out bytes.Buffer
	if err := runOverridesValidateCluster(t.Context(), cl, overridesNamespace, &out); err != nil {
		t.Fatalf("validate: %v", err)
	}

	if !strings.Contains(out.String(), "No overrides ConfigMap found") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestOverridesListShowsEntries(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(overridesScheme(t)).
		WithObjects(overridesConfigMapFor(map[string]string{"overrides.yaml": validOverridesDocument()})).
		Build()

	var out bytes.Buffer
	if err := runOverridesList(t.Context(), cl, overridesNamespace, &out); err != nil {
		t.Fatalf("list: %v", err)
	}

	for _, want := range []string{"overrides.yaml[0]", "net", "DaemonSet", "(all)", "extraArgs(node)", "12"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, want it to contain %q", out.String(), want)
		}
	}
}

// TestOverridesListWarnsAboutUnknownSites covers the inert-but-worth-saying
// case: a Site name that matches nothing is expected before the Site exists,
// and a typo otherwise.
func TestOverridesListWarnsAboutUnknownSites(t *testing.T) {
	document := `apiVersion: ` + override.APIVersion + `
overrides:
  - component: storage
    kind: DaemonSet
    sites: [edge-west, typo-site]
    extraArgs:
      run: ["--x"]
`

	cl := fake.NewClientBuilder().
		WithScheme(overridesScheme(t)).
		WithObjects(
			overridesConfigMapFor(map[string]string{"overrides.yaml": document}),
			&v1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge-west"}},
		).
		Build()

	var out bytes.Buffer
	if err := runOverridesList(t.Context(), cl, overridesNamespace, &out); err != nil {
		t.Fatalf("list: %v", err)
	}

	if !strings.Contains(out.String(), "typo-site") || !strings.Contains(out.String(), "inert") {
		t.Fatalf("output = %q, want a warning naming typo-site", out.String())
	}

	if strings.Contains(out.String(), "edge-west and") {
		t.Fatalf("output = %q, must not warn about a Site that exists", out.String())
	}
}

func siteWithOverrideStatus(name string, status *v1alpha3.OverrideStatus) *v1alpha3.Site {
	return &v1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     v1alpha3.SiteStatus{Overrides: status},
	}
}

func TestOverridesStatusReportsApplied(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(overridesScheme(t)).
		WithObjects(siteWithOverrideStatus("edge", &v1alpha3.OverrideStatus{
			Phase:                   v1alpha3.OverridePhaseApplied,
			ObservedResourceVersion: "12",
			Workloads: []v1alpha3.OverriddenWorkload{{
				Kind: "DaemonSet", Name: "unbounded-net-node",
				DesiredHash: "abc", AppliedHash: "abc",
			}},
		})).
		Build()

	var out bytes.Buffer
	if err := runOverridesStatus(t.Context(), cl, &out); err != nil {
		t.Fatalf("status: %v", err)
	}

	for _, want := range []string{"edge", "Applied", "DaemonSet/unbounded-net-node", "yes"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, want it to contain %q", out.String(), want)
		}
	}
}

// TestOverridesStatusReportsStaleAndDegraded covers the divergence signal the
// per-workload hashes exist to provide.
func TestOverridesStatusReportsStaleAndDegraded(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(overridesScheme(t)).
		WithObjects(siteWithOverrideStatus("edge", &v1alpha3.OverrideStatus{
			Phase:                   v1alpha3.OverridePhaseDegraded,
			ObservedResourceVersion: "12",
			Message:                 "overrides.yaml[0]: patch targets container \"typo\"",
			Workloads: []v1alpha3.OverriddenWorkload{{
				Kind: "DaemonSet", Name: "unbounded-net-node",
				DesiredHash: "want", AppliedHash: "have",
			}},
		})).
		Build()

	var out bytes.Buffer
	if err := runOverridesStatus(t.Context(), cl, &out); err != nil {
		t.Fatalf("status: %v", err)
	}

	for _, want := range []string{"Degraded", "stale", "Degraded Sites:", "leaves the affected workloads"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, want it to contain %q", out.String(), want)
		}
	}
}

// TestOverridesStatusReportsVersionDrift covers the loudest signal: a pinned
// image survives operator upgrades.
func TestOverridesStatusReportsVersionDrift(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(overridesScheme(t)).
		WithObjects(siteWithOverrideStatus("edge", &v1alpha3.OverrideStatus{
			Phase: v1alpha3.OverridePhaseApplied,
			Workloads: []v1alpha3.OverriddenWorkload{{
				Kind: "DaemonSet", Name: "unbounded-net-node",
				DesiredHash: "abc", AppliedHash: "abc",
				VersionDrift: "node=registry.example.com/net:pinned",
			}},
		})).
		Build()

	var out bytes.Buffer
	if err := runOverridesStatus(t.Context(), cl, &out); err != nil {
		t.Fatalf("status: %v", err)
	}

	for _, want := range []string{"Version drift", "not be updated by an operator upgrade", "registry.example.com/net:pinned"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, want it to contain %q", out.String(), want)
		}
	}
}

func TestOverridesStatusWithNoOverrides(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(overridesScheme(t)).
		WithObjects(siteWithOverrideStatus("edge", nil)).
		Build()

	var out bytes.Buffer
	if err := runOverridesStatus(t.Context(), cl, &out); err != nil {
		t.Fatalf("status: %v", err)
	}

	if !strings.Contains(out.String(), "No overrides are in effect") {
		t.Fatalf("output = %q", out.String())
	}
}

// TestOverridesStatusReadsOnly is the authority guarantee.
//
// Client-side rendering is rejected for this feature because a plugin and the
// operator are versioned independently: under skew a recomputing CLI would
// report divergence that does not exist, or miss divergence that does. This
// asserts the command only ever reads, so its answer is whatever the operator
// actually did.
func TestOverridesStatusReadsOnly(t *testing.T) {
	var writes []string

	base := fake.NewClientBuilder().
		WithScheme(overridesScheme(t)).
		WithObjects(siteWithOverrideStatus("edge", &v1alpha3.OverrideStatus{
			Phase:     v1alpha3.OverridePhaseApplied,
			Workloads: []v1alpha3.OverriddenWorkload{{Kind: "DaemonSet", Name: "n", DesiredHash: "a", AppliedHash: "a"}},
		})).
		Build()

	cl := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
			writes = append(writes, "create")

			return nil
		},
		Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
			writes = append(writes, "update")

			return nil
		},
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
			writes = append(writes, "patch")

			return nil
		},
		Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
			writes = append(writes, "delete")

			return nil
		},
		Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			writes = append(writes, "apply")

			return nil
		},
	})

	var out bytes.Buffer
	if err := runOverridesStatus(t.Context(), cl, &out); err != nil {
		t.Fatalf("status: %v", err)
	}

	if len(writes) != 0 {
		t.Fatalf("overrides status performed writes %v; it must only read", writes)
	}
}

// TestOverridesValidateAcceptsAConfigMapManifest is a regression test.
//
// A ConfigMap manifest is the shape users actually have: it is what they apply,
// and the shipped example is one, with comments in it telling the reader to
// check it with this command. Doing so failed with three complaints about
// fields missing from an internal Go type, none of which mentioned ConfigMaps.
func TestOverridesValidateAcceptsAConfigMapManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "component-overrides.yaml")

	manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + override.ConfigMapName + `
  namespace: unbounded-system
data:
  resources.yaml: |
` + indentBlock(validOverridesDocument(), "    ") + `
  scheduling.yaml: |
` + indentBlock(validOverridesDocument(), "    ")

	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var out bytes.Buffer
	if err := runOverridesValidateFiles([]string{path}, &out); err != nil {
		t.Fatalf("a ConfigMap manifest must validate: %v", err)
	}

	// Both keys were read, not just the first.
	if !strings.Contains(out.String(), "2 entries") {
		t.Fatalf("output = %q, want both data keys validated", out.String())
	}
}

// TestOverridesValidateShippedExample checks the exact file the repository
// ships, since its own comments instruct the reader to run this command on it.
func TestOverridesValidateShippedExample(t *testing.T) {
	path := filepath.Join("..", "..", "..",
		"deploy", "unbounded-operator", "examples", "component-overrides.example.yaml")

	if _, err := os.Stat(path); err != nil {
		t.Skipf("example not present: %v", err)
	}

	var out bytes.Buffer
	if err := runOverridesValidateFiles([]string{path}, &out); err != nil {
		t.Fatalf("the shipped example must pass the command its own comments recommend: %v", err)
	}
}

// TestOverridesValidateRejectsDuplicateKeys is a regression test for a silent
// false pass.
//
// Files were keyed by base name, so two documents called overrides.yaml in
// different directories overwrote each other. The command validated only the
// last and reported a clean bill of health for a file it had never read. The
// two cannot both become keys of one ConfigMap either way, so this is not a
// case worth supporting silently.
func TestOverridesValidateRejectsDuplicateKeys(t *testing.T) {
	root := t.TempDir()

	paths := make([]string, 0, 2)

	for _, sub := range []string{"a", "b"} {
		dir := filepath.Join(root, sub)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		path := filepath.Join(dir, "overrides.yaml")
		if err := os.WriteFile(path, []byte(validOverridesDocument()), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		paths = append(paths, path)
	}

	var out bytes.Buffer

	err := runOverridesValidateFiles(paths, &out)
	if err == nil {
		t.Fatal("two files sharing a base name must be refused, not silently reduced to one")
	}

	// The message has to name both, or the user cannot tell which to rename.
	for _, path := range paths {
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("error = %q, want it to name %q", err, path)
		}
	}
}

// TestOverridesValidateRejectsMixedDocuments covers a file that is half one
// shape and half the other. Reading only the part that fits would leave the
// rest unvalidated while the command printed ok.
func TestOverridesValidateRejectsMixedDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed.yaml")

	mixed := `apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + override.ConfigMapName + `
data:
  a.yaml: |
` + indentBlock(validOverridesDocument(), "    ") + `
---
` + validOverridesDocument()

	if err := os.WriteFile(path, []byte(mixed), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out bytes.Buffer
	if err := runOverridesValidateFiles([]string{path}, &out); err == nil {
		t.Fatal("a file mixing both shapes must be refused rather than half-read")
	}
}

// TestOverridesValidateStillAcceptsBareDocuments confirms the ConfigMap
// unwrapping did not cost the simpler shape.
func TestOverridesValidateStillAcceptsBareDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overrides.yaml")
	if err := os.WriteFile(path, []byte(validOverridesDocument()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out bytes.Buffer
	if err := runOverridesValidateFiles([]string{path}, &out); err != nil {
		t.Fatalf("a bare document must still validate: %v", err)
	}
}

// indentBlock indents every line of a block so it can be embedded in a YAML
// literal scalar.
func indentBlock(block, indent string) string {
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = indent + line
		}
	}

	return strings.Join(lines, "\n")
}

// TestOverridesCommandsAcceptKubeconfig is a regression test.
//
// The CLI reference documents --kubeconfig as accepted by every command that
// talks to a cluster, and every other such command in this plugin registers it.
// The overrides commands did not, and built their client through
// ctrl.GetConfigOrDie, which reads a flag registered on the standard library's
// flag.CommandLine that cobra never parses. So the documented flag was rejected
// outright and only the ambient KUBECONFIG was ever honoured.
func TestOverridesCommandsAcceptKubeconfig(t *testing.T) {
	for _, name := range []string{"list", "status", "validate"} {
		t.Run(name, func(t *testing.T) {
			var found bool

			for _, cmd := range overridesCommandGroup().Commands() {
				if cmd.Name() != name {
					continue
				}

				found = true

				if cmd.Flags().Lookup("kubeconfig") == nil {
					t.Fatalf("overrides %s does not accept --kubeconfig, which the CLI reference documents", name)
				}
			}

			if !found {
				t.Fatalf("overrides %s command not found", name)
			}
		})
	}
}

// TestOverridesValidateRejectsDuplicateKeysWithinOneFile is a regression test.
//
// A file may hold several ConfigMap documents. Their data maps were merged
// last-write-wins, so two documents naming the same key silently collapsed to
// the later one and the earlier document was never validated, while the command
// printed ok. The cross-file check cannot catch this: by the time it runs the
// duplicate no longer exists.
func TestOverridesValidateRejectsDuplicateKeysWithinOneFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "two-configmaps.yaml")

	manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + override.ConfigMapName + `
data:
  overrides.yaml: |
` + indentBlock(validOverridesDocument(), "    ") + `
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + override.ConfigMapName + `
data:
  overrides.yaml: |
` + indentBlock(validOverridesDocument(), "    ")

	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var out bytes.Buffer

	err := runOverridesValidateFiles([]string{path}, &out)
	if err == nil {
		t.Fatal("two documents supplying the same key must be refused, not silently reduced to one")
	}

	if !strings.Contains(err.Error(), "overrides.yaml") {
		t.Fatalf("error = %q, want it to name the colliding key", err)
	}
}

// TestOverridesValidateAcceptsDistinctKeysAcrossDocuments keeps multi-document
// files usable, which is the shape kubectl produces for a whole directory.
func TestOverridesValidateAcceptsDistinctKeysAcrossDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "two-configmaps.yaml")

	manifest := `apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + override.ConfigMapName + `
data:
  resources.yaml: |
` + indentBlock(validOverridesDocument(), "    ") + `
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ` + override.ConfigMapName + `
data:
  scheduling.yaml: |
` + indentBlock(validOverridesDocument(), "    ")

	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var out bytes.Buffer
	if err := runOverridesValidateFiles([]string{path}, &out); err != nil {
		t.Fatalf("distinct keys across documents must be accepted: %v", err)
	}

	if !strings.Contains(out.String(), "2 entries") {
		t.Fatalf("output = %q, want both documents validated", out.String())
	}
}
