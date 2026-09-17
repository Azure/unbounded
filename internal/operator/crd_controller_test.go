// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCRDReconcilerIgnoresUnownedCRD(t *testing.T) {
	applies := 0
	r := &CRDReconciler{
		Client: fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				applies++

				return nil
			},
		}).Build(),
		desired: map[string]*unstructured.Unstructured{},
	}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "foreign.example.com"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if applies != 0 {
		t.Fatalf("applies = %d, want 0", applies)
	}
}

func TestCRDReconcilerAppliesOwnedCRD(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add CRD scheme: %v", err)
	}

	desired, err := desiredCRDs()
	if err != nil {
		t.Fatalf("desiredCRDs: %v", err)
	}

	name := RequiredCRDNames[0]
	applies := 0
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &CRDReconciler{
		Client: interceptor.NewClient(base, interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, cfg runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applies++

				named, ok := cfg.(interface{ GetName() string })
				if !ok {
					t.Fatalf("apply configuration has type %T, want named object", cfg)
				}

				if named.GetName() != name {
					t.Fatalf("applied CRD = %q, want %q", named.GetName(), name)
				}

				return nil
			},
		}),
		desired: desired,
	}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if applies != 1 {
		t.Fatalf("applies = %d, want 1", applies)
	}
}

func TestCRDReconcilerSkipsMatchingCRD(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add CRD scheme: %v", err)
	}

	desired, err := desiredCRDs()
	if err != nil {
		t.Fatalf("desiredCRDs: %v", err)
	}

	name := RequiredCRDNames[0]

	var current apiextensionsv1.CustomResourceDefinition
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(desired[name].Object, &current); err != nil {
		t.Fatalf("convert desired CRD: %v", err)
	}

	applies := 0
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&current).Build()
	r := &CRDReconciler{
		Client: interceptor.NewClient(base, interceptor.Funcs{
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				applies++

				return nil
			},
		}),
		desired: desired,
	}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if applies != 0 {
		t.Fatalf("applies = %d, want 0 for matching CRD", applies)
	}
}
