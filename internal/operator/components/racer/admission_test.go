// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Azure/unbounded/internal/operator/component"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

// Kubernetes admits the constructed workloads and persists Node metadata. Racer
// admission is enforced by the shared bootstrap guard and required affinity;
// there is no annotation-mirror admission policy or scheduler in envtest.
func TestSiteWorkloadAdmission(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real workload admission")
	}

	environment := &envtest.Environment{}

	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, policyv1.AddToScheme, rbacv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	const namespace = "custom-system"
	if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}

	for _, obj := range append(sharedResources(namespace), controlDeployment(namespace, component.Config{})) {
		if err := kube.Create(ctx, obj); err != nil {
			t.Fatalf("admit %T %s: %v", obj, obj.GetName(), err)
		}
	}

	for _, tc := range []struct{ name, site string }{
		{"default", "default"},
		{"rack-a", "rack-a"},
		{"dotted-site", "rack.b"},
		{"long-site", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := testSite(tc.site)

			desired := dataplaneDaemonSet(namespace, component.Config{})
			if err := kube.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatalf("admit Site workload: %v", err)
			}

			var actual appsv1.DaemonSet
			if err := kube.Get(ctx, client.ObjectKeyFromObject(desired), &actual); err != nil {
				t.Fatal(err)
			}

			universe := racermeta.UniverseForSite(site.Name)

			pod := actual.Spec.Template.Spec
			if actual.Spec.Selector.MatchLabels[racermeta.UniverseKey] != "" || actual.Spec.Template.Labels[racermeta.UniverseKey] != "" || envValues(pod.InitContainers[0])["POD_UNIVERSE"] != "" {
				t.Fatal("API round-trip changed selector/Pod/bootstrap identity")
			}

			if len(actual.OwnerReferences) != 0 || !reflect.DeepEqual(pod.Affinity, desired.Spec.Template.Spec.Affinity) {
				t.Fatal("API round-trip changed singleton ownership or required affinity")
			}

			if tc.name == "long-site" {
				for _, key := range []string{racermeta.SiteLabelKey, racermeta.DeprecatedSiteLabelKey} {
					node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "long-site-node", Labels: map[string]string{corev1.LabelOSStable: "linux", key: site.Name}}}
					if err := kube.Create(ctx, node, client.DryRunAll); !apierrors.IsInvalid(err) {
						t.Fatalf("long Site cannot be a Node label: %v", err)
					}

					for _, value := range []string{"", "rack-a", universe} {
						node.Labels[key] = value

						admitted := node.DeepCopy()
						if err := kube.Create(ctx, admitted, client.DryRunAll); err != nil {
							t.Fatal(err)
						}

						if matchesNode(t, pod, admitted) != (value != "") {
							t.Fatalf("singleton eligibility disagrees for %s=%q", key, value)
						}
					}
				}
			}
		})
	}

	var ds appsv1.DaemonSet
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: dataplaneName}, &ds); err != nil {
		t.Fatal(err)
	}

	pod := ds.Spec.Template.Spec
	check := func(t *testing.T, node *corev1.Node, want bool) {
		t.Helper()

		var persisted corev1.Node
		if err := kube.Get(ctx, client.ObjectKeyFromObject(node), &persisted); err != nil {
			t.Fatal(err)
		}

		if got := matchesNode(t, pod, &persisted); got != want {
			t.Fatalf("persisted Node affinity match=%v, want %v", got, want)
		}

		if err := racermeta.ValidateBootstrapNode(&persisted, racermeta.NodeUniverse(&persisted)); (err == nil) != want {
			t.Fatalf("persisted Node bootstrap validation=%v, want admitted=%v", err, want)
		}
	}

	for _, tc := range siteAdmissionCases() {
		t.Run(tc.name, func(t *testing.T) {
			tc.labels[corev1.LabelOSStable] = "linux"

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Labels: tc.labels, Annotations: map[string]string{racermeta.UniverseKey: "foreign"}}}
			if err := kube.Create(ctx, node); err != nil {
				t.Fatal(err)
			}

			check(t, node, tc.want)

			// Metadata updates are allowed, but cannot override Site authority.
			node.Annotations[racermeta.UniverseKey] = "moved"
			if err := kube.Update(ctx, node); err != nil {
				t.Fatal(err)
			}

			check(t, node, tc.want)

			node.Labels[racermeta.SiteLabelKey] = "rack-b"
			if err := kube.Update(ctx, node); err != nil {
				t.Fatal(err)
			}

			check(t, node, node.Labels[racermeta.ExcludeLabelKey] != "true")

			node.Labels[racermeta.SiteLabelKey] = "rack-a"

			node.Labels[racermeta.ExcludeLabelKey] = "true"
			if err := kube.Update(ctx, node); err != nil {
				t.Fatal(err)
			}

			check(t, node, false)

			delete(node.Labels, racermeta.ExcludeLabelKey)

			if err := kube.Update(ctx, node); err != nil {
				t.Fatal(err)
			}

			check(t, node, true)
		})
	}

	t.Run("invalid-affinity", func(t *testing.T) {
		invalid := dataplaneDaemonSet(namespace, component.Config{})
		invalid.Name = "invalid-affinity"

		invalid.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Operator = "Invalid"
		if err := kube.Create(ctx, invalid, client.DryRunAll); !apierrors.IsInvalid(err) {
			t.Fatalf("expected API affinity validation failure, got %v", err)
		}
	})

	t.Run("selector-identity-mismatch", func(t *testing.T) {
		invalid := dataplaneDaemonSet(namespace, component.Config{})
		invalid.Name = "invalid-selector"

		invalid.Spec.Template.Labels = map[string]string{racermeta.DataplaneLabelKey: "true", racermeta.UniverseKey: "foreign"}
		if err := kube.Create(ctx, invalid, client.DryRunAll); !apierrors.IsInvalid(err) {
			t.Fatalf("expected API selector validation failure, got %v", err)
		}
	})
}
