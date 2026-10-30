// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
)

func kubeProxyImageDocument(image string) string {
	return "apiVersion: " + override.APIVersion + "\nimages:\n  kubeProxy:\n    image: " + image + "\n"
}

// TestKubeProxyImageOverrideReachesTheNetController holds images.kubeProxy
// against the net controller Deployment the operator actually renders, so a
// renamed container fails here rather than on a cluster.
//
// It relies on `make test` rendering the manifests first.
func TestKubeProxyImageOverrideReachesTheNetController(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	sites := []unboundedv1alpha3.Site{{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}}
	env := &component.Env{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme, Namespace: component.DefaultNamespace}

	_, _, plan := planComponents(t.Context(), env, DefaultRegistry(), sites, []*unboundedv1alpha3.Site{&sites[0]})

	entries, problems, err := override.Parse(map[string]string{"kube-proxy.yaml": kubeProxyImageDocument("myreg.example/kube-proxy")})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if err := override.ProblemsError(append(problems, override.Validate(entries)...)); err != nil {
		t.Fatalf("document rejected: %v", err)
	}

	report := override.Apply(plan, entries, siteNames(sites))
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	var deployment *unstructured.Unstructured

	for _, op := range plan.Operations {
		if op.Component == "net" && op.Object.GetKind() == "Deployment" && op.Object.GetName() == "unbounded-net-controller" {
			deployment = op.Object
		}
	}

	if deployment == nil {
		t.Fatal("the net controller Deployment was not planned")
	}

	containers, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil {
		t.Fatalf("read containers: %v", err)
	}

	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok || container["name"] != "controller" {
			continue
		}

		args, _ := container["args"].([]any) //nolint:errcheck // absent means no args
		if len(args) == 0 || args[len(args)-1] != "--managed-kube-proxy-image=myreg.example/kube-proxy" {
			t.Fatalf("controller args = %v, want the kube-proxy image flag last", args)
		}

		return
	}

	t.Fatalf("the net controller Deployment has no controller container: %v", containers)
}

// TestInvalidKubeProxyImageWithholdsOnlyTheNetController pins the blast radius
// of a bad images.kubeProxy: it could only have changed the net controller
// Deployment, so nothing else waits for it to be fixed.
func TestInvalidKubeProxyImageWithholdsOnlyTheNetController(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	env := &component.Env{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(overridesConfigMap(map[string]string{
			"kube-proxy.yaml": kubeProxyImageDocument("myreg.example/Kube-Proxy"),
		})).Build(),
		Namespace: component.DefaultNamespace,
	}

	snapshot := loadOverrides(t.Context(), env)
	if !snapshot.rejected() {
		t.Fatal("a malformed kube-proxy image must be rejected")
	}

	plan := component.NewPlan()
	plan.Add(
		component.Operation{
			Kind: component.OpApply, Object: unstructuredOf("apps/v1", "Deployment", "unbounded-net-controller"),
			Component: "net", Overridable: true,
		},
		component.Operation{
			Kind: component.OpApply, Object: unstructuredOf("apps/v1", "DaemonSet", "unbounded-net-node"),
			Component: "net", Overridable: true,
		},
		component.Operation{
			Kind: component.OpApply, Object: unstructuredOf("apps/v1", "Deployment", "machina-controller"),
			Component: "machina", Overridable: true,
		},
	)

	withheld := dropOverridableOperations(plan, snapshot.quarantine())
	if len(withheld) != 1 || withheld[0].Ref.Name != "unbounded-net-controller" {
		t.Fatalf("withheld = %+v, want only the net controller Deployment", withheld)
	}

	if !strings.Contains(withheld[0].Err.Error(), "kube-proxy.yaml:images.kubeProxy") {
		t.Fatalf("withheld because %v, want it attributed to images.kubeProxy", withheld[0].Err)
	}

	if len(plan.Operations) != 2 {
		t.Fatalf("remaining = %d operations, want the other workloads to keep reconciling", len(plan.Operations))
	}
}
