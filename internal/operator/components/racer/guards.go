// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
)

var guardNames = []string{"racer-runtime-write-restriction", "racer-node-write-restriction"}

func guardPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		for _, name := range guardNames {
			if obj.GetName() == name {
				return true
			}
		}

		return false
	})
}

// This is eventual containment, not continuous enforcement. Privileged guard
// writers remain trusted. Revoke both grants in a separate pass before any
// repair, even without caches. Keep workloads and durable identity intact;
// removing these bindings contains their API writes without destroying state.
func planGuardContainment(ctx context.Context, env *component.Env) (*component.Plan, bool, error) {
	claim := &corev1.ConfigMap{}
	if err := env.LiveReader().Get(ctx, objectKey(env, claimName), claim); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, err
	}

	if claim.Data["state"] != "consumed" {
		return nil, false, nil
	}

	if _, err := claimedMarker(ctx, env, claim); err != nil {
		return nil, false, err
	}

	guards, err := env.DecodeManifestFiles(manifests.Manifests, []string{"create-restriction.yaml", "node-restriction.yaml"}, nil)
	if err != nil {
		return nil, false, err
	}

	var (
		unhealthy, terminating bool
		readErr                error
	)

	repairs := component.NewPlan()

	for _, guard := range guards {
		var expected, current client.Object

		switch guard.GetKind() {
		case "ValidatingAdmissionPolicy":
			expected, current = &admissionv1.ValidatingAdmissionPolicy{}, &admissionv1.ValidatingAdmissionPolicy{}
		case "ValidatingAdmissionPolicyBinding":
			expected, current = &admissionv1.ValidatingAdmissionPolicyBinding{}, &admissionv1.ValidatingAdmissionPolicyBinding{}
		default:
			return nil, false, errors.New("unexpected admission guard kind")
		}

		if err := env.Scheme.Convert(guard, expected, nil); err != nil {
			return nil, false, err
		}

		err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(expected), current)
		switch {
		case apierrors.IsNotFound(err):
			unhealthy = true
		case err != nil:
			readErr = errors.Join(readErr, err)
		case current.GetDeletionTimestamp() != nil:
			unhealthy, terminating = true, true
		case !equality.Semantic.DeepEqual(effectiveGuardSpec(expected), effectiveGuardSpec(current)):
			unhealthy = true
			base := component.ToUnstructured(current)
			base.SetGroupVersionKind(guard.GroupVersionKind())
			desired := base.DeepCopy()
			desired.Object["spec"] = guard.DeepCopy().Object["spec"]
			repairs.Add(component.Operation{Kind: component.OpMergePatch, Base: base, Object: desired, Component: name})
		}
	}

	if !unhealthy {
		return nil, false, readErr
	}

	bindings := []client.Object{
		&rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: controllerName, Namespace: env.Namespace}},
		&rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: controllerName}},
	}
	exists := false

	for _, obj := range bindings {
		current, ok := obj.DeepCopyObject().(client.Object)
		if !ok {
			return nil, false, errors.New("binding copy is not a client object")
		}

		err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(obj), current)
		if err == nil {
			exists = true
		} else if !apierrors.IsNotFound(err) {
			// A known unhealthy guard warrants revocation even if a grant cannot
			// be read. DeleteIfExists can still succeed independently of Get.
			exists = true
		}
	}

	if exists {
		plan := component.NewPlan()
		for _, obj := range bindings {
			add(plan, component.OpDelete, obj)
		}

		return plan, true, nil
	}

	if readErr != nil {
		return nil, false, readErr
	}

	if terminating {
		return component.NewPlan(), true, nil
	}

	if repairs.Len() > 0 {
		caches := &racerv1.ClusterCacheList{}
		if err := env.LiveReader().List(ctx, caches, client.Limit(1)); err != nil {
			return nil, false, err
		}

		if len(caches.Items) > 0 {
			// Remove added fields too, then verify live integrity on the next pass.
			// Applying an omitted field cannot remove another manager's value.
			return repairs, true, nil
		}
	}

	// Only a later live read proving both grants absent permits normal planning.
	// Retained installations do not regrant until caches activate them again.
	return nil, false, nil
}

// Compare only specs, normalizing API defaults on copies so persisted defaults
// do not cause repeated revocation. Manifest decoding also retargets CEL identity.
func effectiveGuardSpec(obj client.Object) any {
	switch obj := obj.(type) {
	case *admissionv1.ValidatingAdmissionPolicy:
		spec := obj.Spec.DeepCopy()
		if spec.FailurePolicy == nil {
			spec.FailurePolicy = ptr.To(admissionv1.Fail)
		}

		spec.MatchConstraints = effectiveGuardMatch(spec.MatchConstraints)

		return spec
	case *admissionv1.ValidatingAdmissionPolicyBinding:
		spec := obj.Spec.DeepCopy()
		spec.MatchResources = effectiveGuardMatch(spec.MatchResources)

		return spec
	default:
		return nil
	}
}

func effectiveGuardMatch(match *admissionv1.MatchResources) *admissionv1.MatchResources {
	if match == nil {
		match = &admissionv1.MatchResources{}
	}

	if match.MatchPolicy == nil {
		match.MatchPolicy = ptr.To(admissionv1.Equivalent)
	}

	if match.NamespaceSelector == nil {
		match.NamespaceSelector = &metav1.LabelSelector{}
	}

	if match.ObjectSelector == nil {
		match.ObjectSelector = &metav1.LabelSelector{}
	}

	for _, rules := range [][]admissionv1.NamedRuleWithOperations{match.ResourceRules, match.ExcludeResourceRules} {
		for i := range rules {
			if rules[i].Scope == nil {
				rules[i].Scope = ptr.To(admissionv1.AllScopes)
			}
		}
	}

	return match
}
