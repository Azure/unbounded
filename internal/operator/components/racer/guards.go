// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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
// integrity remains trusted. Revoke both grants in a separate pass before any
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

	var (
		missing, terminating bool
		readErr              error
	)

	for _, name := range guardNames {
		for _, obj := range []client.Object{&admissionv1.ValidatingAdmissionPolicy{}, &admissionv1.ValidatingAdmissionPolicyBinding{}} {
			err := env.LiveReader().Get(ctx, client.ObjectKey{Name: name}, obj)
			switch {
			case apierrors.IsNotFound(err):
				missing = true
			case err != nil:
				readErr = errors.Join(readErr, err)
			case obj.GetDeletionTimestamp() != nil:
				missing, terminating = true, true
			}
		}
	}

	if !missing {
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
			// A known missing guard warrants revocation even if a grant cannot
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

	// Only a later live read proving both grants absent permits normal planning.
	// Retained installations do not regrant until caches activate them again.
	return nil, false, nil
}
