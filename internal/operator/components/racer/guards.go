// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"strings"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
)

func decodePolicies(env *component.Env) ([]*unstructured.Unstructured, error) {
	return env.DecodeManifestFiles(manifests.Manifests, []string{"policies.yaml"}, nil)
}

func guardPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return strings.HasPrefix(obj.GetName(), "racer-")
	})
}

// dataplaneBindings are the grants the admission policies restrict.
func dataplaneBindings(env *component.Env) []client.Object {
	return []client.Object{
		&rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: env.Namespace}},
		&rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: dataplaneName}},
	}
}

// planContainment returns a plan that revokes the dataplane grants when any
// admission policy or binding is missing, terminating, or differs from the
// manifest. It reapplies the policies in the same pass; the grants return on
// a later pass that reads every policy intact. With no grant to revoke it
// returns nil, and normal planning orders the policies before the bindings. The executor additionally
// revokes the grants whenever a policy write fails (RevokeOnDependencyFailure).
// Containment never touches workloads or runtime state.
func planContainment(ctx context.Context, env *component.Env, policies []*unstructured.Unstructured) (*component.Plan, error) {
	healthy := true

	for _, guard := range policies {
		var expected, current client.Object

		switch guard.GetKind() {
		case "ValidatingAdmissionPolicy":
			expected, current = &admissionv1.ValidatingAdmissionPolicy{}, &admissionv1.ValidatingAdmissionPolicy{}
		case "ValidatingAdmissionPolicyBinding":
			expected, current = &admissionv1.ValidatingAdmissionPolicyBinding{}, &admissionv1.ValidatingAdmissionPolicyBinding{}
		default:
			return nil, errors.New("unexpected racer admission guard kind " + guard.GetKind())
		}

		if err := env.Scheme.Convert(guard, expected, nil); err != nil {
			return nil, err
		}

		err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(expected), current)

		switch {
		case apierrors.IsNotFound(err):
			healthy = false
		case err != nil:
			return nil, err
		case current.GetDeletionTimestamp() != nil:
			healthy = false
		// Compare specs with API defaults normalized so persisted defaults do
		// not cause repeated revocation.
		case !equality.Semantic.DeepEqual(component.EffectiveAdmissionSpec(expected), component.EffectiveAdmissionSpec(current)):
			healthy = false
		}
	}

	if healthy {
		return nil, nil
	}

	plan := component.NewPlan()

	for _, obj := range dataplaneBindings(env) {
		current, ok := obj.DeepCopyObject().(client.Object)
		if !ok {
			return nil, errors.New("binding copy is not a client object")
		}

		err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(obj), current)
		if apierrors.IsNotFound(err) {
			continue
		}

		if err != nil {
			return nil, err
		}

		// A binding someone else manages is not ours to remove.
		ok, err = adoptable(ctx, env, current)
		if err != nil {
			return nil, err
		}

		if !ok {
			continue
		}

		current.GetObjectKind().SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
		plan.Add(component.Operation{Kind: component.OpDelete, Object: component.ToUnstructured(current), Component: name})
	}

	if plan.Len() == 0 {
		// Nothing is granted, so normal planning may write the policies
		// before the bindings.
		return nil, nil
	}

	for _, guard := range policies {
		op, err := ownedOperation(ctx, env, component.Operation{Kind: component.OpApply, Object: guard.DeepCopy(), Component: name, FailureDomain: "racer-guards"})
		if err != nil {
			return nil, err
		}

		plan.Add(op)
	}

	return plan, nil
}
