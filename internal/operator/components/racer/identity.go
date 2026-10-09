// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racer/wire"
)

const (
	operatorInitialization = "initialization_protocol"
	operatorStaged         = "staged-v1"
	operatorMarkerUID      = "marker_uid"
	operatorPending        = "operator-pending"
)

// Stage a non-startable marker before permanently binding its UID. Never reset
// either permanent object. Only staged-v1 claims are supported.
func planIdentity(ctx context.Context, env *component.Env, plan *component.Plan) (*corev1.ConfigMap, error) {
	claim := &corev1.ConfigMap{}

	err := env.LiveReader().Get(ctx, objectKey(env, claimName), claim)
	if apierrors.IsNotFound(err) {
		if err := checkNewInstallation(ctx, env); err != nil {
			return nil, err
		}

		add(plan, component.OpCreateIfAbsent, &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: env.Namespace, Annotations: map[string]string{managerAnnotation: component.FieldOwner}},
			Data:       map[string]string{"cluster": uuid.NewString(), "state": "reserved", operatorInitialization: operatorStaged},
		})

		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	if claim.Annotations[managerAnnotation] != component.FieldOwner || claim.UID == "" || claim.ResourceVersion == "" || claim.DeletionTimestamp != nil || !wire.ValidUUID(claim.Data["cluster"]) {
		return nil, fmt.Errorf("invalid Racer operator claim; restore consistent installation state")
	}

	if claim.Data[operatorInitialization] != operatorStaged {
		return nil, fmt.Errorf("unsupported Racer operator initialization protocol")
	}

	return planStagedIdentity(ctx, env, plan, claim)
}

// Read-only validation cannot consume a claim or recreate a marker.
func claimedMarker(ctx context.Context, env *component.Env, claim *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	if claim.Data[operatorInitialization] != operatorStaged {
		return nil, fmt.Errorf("unsupported Racer operator initialization protocol")
	}

	if claim.Annotations[managerAnnotation] != component.FieldOwner || claim.UID == "" || claim.ResourceVersion == "" || claim.DeletionTimestamp != nil || !wire.ValidUUID(claim.Data["cluster"]) || claim.Data["state"] != "consumed" || !ptr.Deref(claim.Immutable, false) {
		return nil, fmt.Errorf("invalid established Racer operator claim")
	}

	marker := &corev1.ConfigMap{}
	if err := env.LiveReader().Get(ctx, objectKey(env, markerName), marker); err != nil {
		return nil, fmt.Errorf("racer marker unavailable after permanent claim consumption; restore consistent durable state: %w", err)
	}

	if marker.UID == "" || marker.ResourceVersion == "" || marker.DeletionTimestamp != nil || marker.Annotations[managerAnnotation] != component.FieldOwner || marker.Annotations[claimAnnotation] != string(claim.UID) || marker.Data["cluster"] != claim.Data["cluster"] || marker.Data["version_configmap"] != versionName {
		return nil, fmt.Errorf("racer marker does not match the permanent operator claim")
	}

	if claim.Data[operatorMarkerUID] != string(marker.UID) || marker.Data[operatorInitialization] != operatorStaged {
		return nil, fmt.Errorf("racer marker UID does not match the permanent operator claim")
	}

	return marker, nil
}

func checkNewInstallation(ctx context.Context, env *component.Env) error {
	return checkInstallationResources(ctx, env, false)
}

// Existing controller state cannot be adopted or overwritten with a new identity.
// External dataplane workloads are not installation evidence owned by this component.
func checkInstallationResources(ctx context.Context, env *component.Env, stagedMarker bool) error {
	manifests, err := decodeRuntimeManifests(env)
	if err != nil {
		return err
	}

	objects := []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: versionName, Namespace: env.Namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: trustName, Namespace: env.Namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tlsName, Namespace: env.Namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: credentialsName, Namespace: env.Namespace}},
	}
	if !stagedMarker {
		objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: markerName, Namespace: env.Namespace}})
	}

	for _, obj := range manifests {
		objects = append(objects, obj)
	}

	for _, obj := range objects {
		err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(obj), obj)
		if err == nil {
			return fmt.Errorf("racer resource %s exists without an operator installation; standalone installations are not adopted and lost claims must be restored", obj.GetName())
		}

		if !apierrors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

func planStagedIdentity(ctx context.Context, env *component.Env, plan *component.Plan, claim *corev1.ConfigMap) (*corev1.ConfigMap, error) {
	if claim.Data["state"] == "consumed" {
		marker, err := claimedMarker(ctx, env, claim)
		if err != nil {
			return nil, err
		}

		if marker.Data["state"] != operatorPending {
			return marker, nil
		}

		if ptr.Deref(marker.Immutable, false) || marker.Data["version_uid"] != "" {
			return nil, fmt.Errorf("invalid Racer pending marker")
		}
		// CAS prevents promotion of a replacement marker.
		marker.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
		fresh := marker.DeepCopy()
		fresh.Data["state"] = "fresh"
		plan.Add(component.Operation{Kind: component.OpMergePatch, Component: name, Base: component.ToUnstructured(marker), Object: component.ToUnstructured(fresh)})

		return nil, nil
	}

	if claim.Data["state"] != "reserved" || ptr.Deref(claim.Immutable, false) || claim.Data[operatorMarkerUID] != "" {
		return nil, fmt.Errorf("invalid Racer staged operator claim")
	}

	if err := checkInstallationResources(ctx, env, true); err != nil {
		return nil, err
	}

	marker := &corev1.ConfigMap{}

	err := env.LiveReader().Get(ctx, objectKey(env, markerName), marker)
	if apierrors.IsNotFound(err) {
		// A delayed stale Create can leave only a non-startable orphan.
		add(plan, component.OpCreateIfAbsent, &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: markerName, Namespace: env.Namespace, Annotations: map[string]string{managerAnnotation: component.FieldOwner, claimAnnotation: string(claim.UID)}},
			Data:       map[string]string{"cluster": claim.Data["cluster"], "version_configmap": versionName, "state": operatorPending, operatorInitialization: operatorStaged},
		})

		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	if marker.UID == "" || marker.ResourceVersion == "" || marker.DeletionTimestamp != nil || ptr.Deref(marker.Immutable, false) || marker.Annotations[managerAnnotation] != component.FieldOwner || marker.Annotations[claimAnnotation] != string(claim.UID) || marker.Data["cluster"] != claim.Data["cluster"] || marker.Data["version_configmap"] != versionName || marker.Data["state"] != operatorPending || marker.Data[operatorInitialization] != operatorStaged || marker.Data["version_uid"] != "" {
		return nil, fmt.Errorf("invalid Racer staged marker; refusing adoption")
	}

	claim.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
	committed := claim.DeepCopy()
	committed.Data["state"] = "consumed"
	committed.Data[operatorMarkerUID] = string(marker.UID)
	committed.Immutable = ptr.To(true)
	plan.Add(component.Operation{Kind: component.OpMergePatch, Component: name, Base: component.ToUnstructured(claim), Object: component.ToUnstructured(committed)})

	return nil, nil
}
