// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NamespaceOperation returns the single operation that reconciles the namespace
// the operator installs components into.
//
// Every component's embedded manifests carry a Namespace, because each set is
// also meant to be installable on its own with kubectl. Reconciling them all
// meant applying the same object once per component and, for per-Site
// components, once per Site. That was not merely redundant: the manifests do
// not agree on the labels, and they are applied under a single field owner, so
// whichever component ran last won and the label changed on every pass.
//
// Server-side apply makes one owner the right answer rather than a convenience.
// A field owner that stops declaring a field it previously owned removes that
// field, so two owners alternating over one object is a write loop, not a race
// that eventually settles.
//
// The operator's own manifests already create this namespace, since the
// operator has to run somewhere, so in practice this operation adopts an
// existing object rather than creating one. It is still planned every pass so
// the namespace is recreated if it is deleted out from under a running
// operator.
func NamespaceOperation(namespace string) Operation {
	ns := &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: NamespaceKind},
		ObjectMeta: metav1.ObjectMeta{
			Name:   namespace,
			Labels: map[string]string{"app.kubernetes.io/name": NamespaceOwner},
		},
	}

	return Operation{
		Kind:      OpApply,
		Object:    ToUnstructured(ns),
		Component: NamespaceOwner,
	}
}

// NamespaceOwner names the operator as the component that owns the namespace.
// It is not a registered component, so it never appears as a Site condition;
// it exists to attribute the operation in plan summaries and execution results.
const NamespaceOwner = "unbounded-operator"
