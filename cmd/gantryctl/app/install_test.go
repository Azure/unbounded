// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDeleteInstallObjectsRemovesOnlyTrackedResources(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	created := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "created", Namespace: "gantry-system"}}
	preserved := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "preserved", Namespace: "gantry-system"}}
	resourceClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(created, preserved).Build()

	tracked := &unstructured.Unstructured{}
	tracked.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	tracked.SetNamespace(created.Namespace)
	tracked.SetName(created.Name)

	if err := deleteInstallObjects(t.Context(), resourceClient, []*unstructured.Unstructured{tracked}); err != nil {
		t.Fatalf("delete tracked install objects: %v", err)
	}

	var got corev1.ConfigMap
	if err := resourceClient.Get(t.Context(), client.ObjectKeyFromObject(created), &got); !apierrors.IsNotFound(err) {
		t.Fatalf("tracked ConfigMap still exists: %v", err)
	}

	if err := resourceClient.Get(t.Context(), client.ObjectKeyFromObject(preserved), &got); err != nil {
		t.Fatalf("preserved ConfigMap was deleted: %v", err)
	}
}
