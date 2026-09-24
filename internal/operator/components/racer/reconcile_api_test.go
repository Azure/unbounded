// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Azure/unbounded/internal/operator/component"
)

type countingApplyClient struct {
	client.Client
	writes int
}

func (c *countingApplyClient) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	c.writes++
	return c.Client.Apply(ctx, obj, opts...)
}

// API defaulting and serialization must not invalidate the operator's no-op
// comparison. Fake clients cannot reproduce those transformations.
func TestAPIDefaultedResourcesAreNoOp(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real server-side apply")
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
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	const namespace = "racer-review"
	if err := kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}

	c := &countingApplyClient{Client: kube}
	env := &component.Env{Client: c, Scheme: scheme, Namespace: namespace, Config: component.Config{ImageTag: "v1"}}
	site := testSite("rack-a")

	plan := combinedPlan(t, env, site)
	if result := execute(t, env, plan); result.Err() != nil {
		t.Fatal(result.Err())
	}

	if c.writes != plan.Len() {
		t.Fatalf("initial writes=%d, want %d", c.writes, plan.Len())
	}

	c.writes = 0

	if result := execute(t, env, combinedPlan(t, env, site)); result.Err() != nil {
		t.Fatal(result.Err())
	}

	if c.writes != 0 {
		t.Fatalf("API-defaulted steady state made %d SSA writes", c.writes)
	}

	var ds appsv1.DaemonSet

	key := client.ObjectKey{Namespace: namespace, Name: dataplaneName}
	if err := kube.Get(t.Context(), key, &ds); err != nil {
		t.Fatal(err)
	}

	ds.Spec.Template.Spec.Containers[0].Image = "example.test/drift:v2"
	if err := kube.Update(t.Context(), &ds); err != nil {
		t.Fatal(err)
	}

	if result := execute(t, env, combinedPlan(t, env, site)); result.Err() != nil {
		t.Fatal(result.Err())
	}

	if c.writes != 1 {
		t.Fatalf("drift repair made %d SSA writes, want one", c.writes)
	}

	if err := kube.Get(t.Context(), key, &ds); err != nil {
		t.Fatal(err)
	}

	if ds.Spec.Template.Spec.Containers[0].Image != env.Config.Image(dataplaneName) {
		t.Fatal("unchanged payload hash hid live workload drift")
	}
}
