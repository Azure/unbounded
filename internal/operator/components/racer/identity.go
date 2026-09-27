// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racer/wire"
)

// The permanent operator claim authorizes exactly one marker Create. Its CAS is
// committed before that Create. A crash between them intentionally needs manual
// recovery with a new identity, rather than risking counter reuse after loss of
// established state. Never delete or reset either permanent object to retry.
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
			Data:       map[string]string{"cluster": uuid.NewString(), "state": "reserved"},
		})

		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	if claim.Annotations[managerAnnotation] != component.FieldOwner || claim.UID == "" || claim.ResourceVersion == "" || claim.DeletionTimestamp != nil || !wire.ValidUUID(claim.Data["cluster"]) {
		return nil, fmt.Errorf("invalid Racer operator claim; restore consistent installation state")
	}

	if claim.Data["state"] == "reserved" && !ptr.Deref(claim.Immutable, false) {
		if err := checkNewInstallation(ctx, env); err != nil {
			return nil, err
		}

		claim.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
		committed := claim.DeepCopy()
		committed.Data["state"] = "consumed"
		committed.Immutable = ptr.To(true)
		cas := component.Operation{Kind: component.OpMergePatch, Component: name, Base: component.ToUnstructured(claim), Object: component.ToUnstructured(committed)}
		plan.Add(cas)

		marker := &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: markerName, Namespace: env.Namespace, Annotations: map[string]string{managerAnnotation: component.FieldOwner, claimAnnotation: string(claim.UID)}},
			Data:       map[string]string{"cluster": claim.Data["cluster"], "version_configmap": versionName, "state": "fresh"},
		}
		plan.Add(component.Operation{Kind: component.OpCreateIfAbsent, Component: name, Object: component.ToUnstructured(marker), DependsOn: []component.ObjectRef{cas.Ref()}})

		return nil, nil
	}

	if claim.Data["state"] != "consumed" || !ptr.Deref(claim.Immutable, false) {
		return nil, fmt.Errorf("invalid Racer operator claim state")
	}

	marker := &corev1.ConfigMap{}
	if err := env.LiveReader().Get(ctx, objectKey(env, markerName), marker); err != nil {
		return nil, fmt.Errorf("racer marker unavailable after permanent claim consumption; restore consistent durable state: %w", err)
	}

	if marker.UID == "" || marker.ResourceVersion == "" || marker.DeletionTimestamp != nil || marker.Annotations[managerAnnotation] != component.FieldOwner ||
		marker.Annotations[claimAnnotation] != string(claim.UID) || marker.Data["cluster"] != claim.Data["cluster"] || marker.Data["version_configmap"] != versionName {
		return nil, fmt.Errorf("racer marker does not match the permanent operator claim")
	}

	return marker, nil
}

// Do not adopt standalone resources or create a new identity over evidence of a
// previous installation, even when its operator claim has been lost.
func checkNewInstallation(ctx context.Context, env *component.Env) error {
	// RBAC names are cluster-scoped, so a standalone installation in another
	// namespace is also a conflict. Never repoint its controller binding.
	for _, obj := range []client.Object{&rbacv1.ClusterRole{}, &rbacv1.ClusterRoleBinding{}} {
		err := env.LiveReader().Get(ctx, client.ObjectKey{Name: controllerName}, obj)
		if err == nil {
			return fmt.Errorf("racer cluster RBAC exists without an operator claim; standalone installations are not adopted")
		}

		if !apierrors.IsNotFound(err) {
			return err
		}
	}

	objects := []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: markerName}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configName}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: versionName}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: trustName}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tlsName}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "racer-issuer"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "racer-keyring"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: controllerName}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: controllerName}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane"}},
	}
	for _, obj := range objects {
		err := env.LiveReader().Get(ctx, objectKey(env, obj.GetName()), obj)
		if err == nil {
			return fmt.Errorf("racer resource %s exists without an operator installation; standalone installations are not adopted and lost claims must be restored", obj.GetName())
		}

		if !apierrors.IsNotFound(err) {
			return err
		}
	}

	return nil
}
