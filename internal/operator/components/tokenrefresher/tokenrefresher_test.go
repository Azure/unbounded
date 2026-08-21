// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package tokenrefresher

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
)

func boolPtr(value bool) *bool { return &value }

func site(name string, enabled *bool) unboundedv1alpha3.Site {
	s := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if enabled != nil {
		s.Spec.Components.TokenRefresher = &unboundedv1alpha3.TokenRefresherComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: enabled},
		}
	}

	return s
}

func testEnv(t *testing.T) *component.Env {
	t.Helper()

	scheme := runtime.NewScheme()

	return &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(scheme).Build(),
		Scheme:    scheme,
		Namespace: component.DefaultNamespace,
		Config:    component.Config{ImageRegistry: "ghcr.io/azure", ImageTag: "v1.2.3"},
	}
}

func TestEnabledForDefaultsOnForNonClusterSites(t *testing.T) {
	for _, tc := range []struct {
		name string
		site unboundedv1alpha3.Site
		want bool
	}{
		{name: "omitted", site: site("edge", nil), want: true},
		{name: "enabled", site: site("edge", boolPtr(true)), want: true},
		{name: "disabled", site: site("edge", boolPtr(false)), want: false},
		{name: "cluster omitted", site: site(clusterSiteName, nil), want: false},
		{name: "cluster explicitly enabled", site: site(clusterSiteName, boolPtr(true)), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EnabledFor(&tc.site); got != tc.want {
				t.Fatalf("EnabledFor = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestPlanAppliesSingletonWhenAnyEligibleSiteEnablesIt(t *testing.T) {
	plan, result, err := (Component{}).Plan(t.Context(), testEnv(t), []unboundedv1alpha3.Site{
		site("disabled", boolPtr(false)), site("edge", nil), site(clusterSiteName, nil),
	})
	if err != nil || !result.Ready {
		t.Fatalf("Plan = result %+v, err %v", result, err)
	}

	wantRefs := []string{
		"ServiceAccount/unbounded-system/token-refresher",
		"ClusterRole/token-refresher",
		"ClusterRoleBinding/token-refresher",
		"Role/kube-system/token-refresher",
		"RoleBinding/kube-system/token-refresher",
		"Role/unbounded-system/token-refresher",
		"RoleBinding/unbounded-system/token-refresher",
		"ConfigMap/unbounded-system/token-refresher",
		"Deployment/unbounded-system/token-refresher",
	}

	var deploymentFound bool

	for _, op := range plan.Operations {
		if !slices.Contains(wantRefs, op.Ref().String()) {
			t.Errorf("unexpected operation %s", op.Ref())
		}

		if op.Object.GetKind() == "Deployment" {
			deploymentFound = true

			if !op.Overridable {
				t.Error("Deployment is not overridable")
			}

			containers, _, _ := unstructured.NestedSlice(op.Object.Object, "spec", "template", "spec", "containers")

			container := containers[0].(map[string]any)
			if container["image"] != "ghcr.io/azure/token-refresher:v1.2.3" {
				t.Errorf("Deployment image = %v", container["image"])
			}

			annotations, _, _ := unstructured.NestedStringMap(op.Object.Object, "spec", "template", "metadata", "annotations")
			if annotations[configHashAnnotation] == "" {
				t.Error("Deployment has no config hash annotation")
			}
		}
	}

	if plan.Len() != len(wantRefs) || !deploymentFound {
		t.Fatalf("planned %d operations, want %d; deployment found=%t\n%s", plan.Len(), len(wantRefs), deploymentFound, plan.Summary())
	}
}

func TestPlanDeletesOwnedResourcesWhenDisabled(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sites []unboundedv1alpha3.Site
	}{
		{name: "no sites"},
		{name: "cluster and opted-out edge", sites: []unboundedv1alpha3.Site{
			site(clusterSiteName, boolPtr(true)), site("edge", boolPtr(false)),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, result, err := (Component{}).Plan(t.Context(), testEnv(t), tc.sites)
			if err != nil || result.Reason != component.ReasonDisabled {
				t.Fatalf("Plan = result %+v, err %v", result, err)
			}

			if plan.Len() != 9 {
				t.Fatalf("cleanup planned %d operations, want 9\n%s", plan.Len(), plan.Summary())
			}

			for _, op := range plan.Operations {
				if op.Kind != component.OpDelete || op.Object.GetName() != name {
					t.Errorf("cleanup operation = %s %s", op.Kind, op.Ref())
				}
			}
		})
	}
}
