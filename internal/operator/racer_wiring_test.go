// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
)

func TestRacerAlwaysRegistered(t *testing.T) {
	reg := DefaultRegistry()
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}

	if !reg.Knows("racer") {
		t.Fatal("racer is not registered")
	}

	if len(reg.Site) != 1 || reg.Site[0].Name() != "metalman" {
		t.Fatal("unexpected per-Site components")
	}
}

func TestRacerBootstrapUsesClusterScopedClusterCache(t *testing.T) {
	desired, err := desiredCRDs()
	if err != nil {
		t.Fatal(err)
	}

	crd := desired["clustercaches.racer.unbounded-cloud.io"]
	if crd == nil {
		t.Fatal("ClusterCache CRD is not bootstrapped")
	}

	for _, field := range []struct {
		path []string
		want string
	}{
		{path: []string{"spec", "scope"}, want: "Cluster"},
		{path: []string{"spec", "names", "kind"}, want: "ClusterCache"},
		{path: []string{"spec", "group"}, want: "racer.unbounded-cloud.io"},
	} {
		got, _, err := unstructured.NestedString(crd.Object, field.path...)
		if err != nil || got != field.want {
			t.Fatalf("%v = %q, %v; want %q", field.path, got, err, field.want)
		}
	}

	if desired["clustervolumes.racer.unbounded-cloud.io"] != nil {
		t.Fatal("obsolete ClusterVolume CRD is bootstrapped")
	}
}

func TestRacerBootstrapApplyFailure(t *testing.T) {
	denied := errors.New("ClusterCache CRD apply denied")

	cl := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			if named, ok := obj.(interface{ GetName() string }); ok && named.GetName() == "clustercaches.racer.unbounded-cloud.io" {
				return denied
			}

			return nil
		},
	}).Build()
	if err := BootstrapCRDs(t.Context(), cl); !errors.Is(err, denied) {
		t.Fatalf("bootstrap error = %v", err)
	}
}

func TestRacerReconcilesWithoutSites(t *testing.T) {
	readErr := errors.New("ClusterCache list denied")

	for _, tc := range []struct {
		name       string
		cache      bool
		failList   bool
		failCreate bool
		wantClaim  bool
		wantErr    bool
	}{
		{name: "first cache starts installation", cache: true, wantClaim: true},
		{name: "no cache is inert"},
		{name: "list failure is retryable", failList: true, wantErr: true},
		{name: "claim write failure is retryable", cache: true, failCreate: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newReconcilerTestScheme(t)

			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tc.cache {
				builder.WithObjects(&racerv1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "cache"}})
			}

			cl := builder.WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if tc.failCreate {
						return readErr
					}

					return c.Create(ctx, obj, opts...)
				},
				Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
					return nil
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*racerv1.ClusterCacheList); ok && tc.failList {
						return readErr
					}

					return c.List(ctx, list, opts...)
				},
			}).Build()
			// Use the real registered component, without unrelated cluster services.
			reg := &component.Registry{}

			for _, c := range DefaultRegistry().Cluster {
				if c.Name() == "racer" {
					reg.Cluster = append(reg.Cluster, c)
				}
			}

			r := &SiteReconciler{Client: cl, APIReader: cl, Scheme: scheme, Registry: reg}

			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: component.SingletonRequestName}})
			if errors.Is(err, readErr) != tc.wantErr || (err != nil && !tc.wantErr) {
				t.Fatalf("Reconcile error = %v", err)
			}

			var claims corev1.ConfigMapList
			if err := cl.List(t.Context(), &claims); err != nil {
				t.Fatal(err)
			}

			wantClaims := 0
			if tc.wantClaim {
				wantClaims = 1
			}

			if len(claims.Items) != wantClaims {
				t.Fatalf("claims = %v", claims.Items)
			}

			if tc.wantClaim {
				if claims.Items[0].Name != "racer-operator-installation" || len(claims.Items[0].OwnerReferences) != 0 {
					t.Fatalf("unexpected claim: %+v", claims.Items[0])
				}

				if result.RequeueAfter != 5*time.Second {
					t.Fatalf("requeue = %v", result.RequeueAfter)
				}
			}
		})
	}
}

func TestOperatorRacerRBAC(t *testing.T) {
	role := loadOperatorClusterRole(t)
	for _, verb := range []string{"get", "list", "watch"} {
		if !clusterRoleGrants(role, "racer.unbounded-cloud.io", "clustercaches", verb) {
			t.Fatalf("missing ClusterCache %s", verb)
		}
	}

	for _, resource := range []string{"clustercaches", "clustercaches/status"} {
		for _, verb := range []string{"create", "patch", "update", "delete", "deletecollection"} {
			if clusterRoleGrants(role, "racer.unbounded-cloud.io", resource, verb) {
				t.Fatalf("unexpected %s %s", resource, verb)
			}
		}
	}

	for _, verb := range []string{"get", "list", "watch", "create", "patch", "update"} {
		if !clusterRoleGrants(role, "policy", "poddisruptionbudgets", verb) {
			t.Fatalf("missing PDB %s", verb)
		}
	}
}
