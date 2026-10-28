// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"strings"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
)

// Names used before the racer-dataplane component was renamed to racer. The
// operator removes the objects it applied under these names; see
// planRenamedCleanup.
const (
	renamedDataplane = "racer-dataplane"
	renamedConfig    = "racer-dataplane-config"

	// renamedKeyPrefix is the ConfigMap key prefix that became "RACER_".
	renamedKeyPrefix = "RACER_DATAPLANE_"
)

// renamedPolicies are the admission policies, each with a binding of the same
// name, that restricted the old ServiceAccount.
var renamedPolicies = []string{
	"racer-dataplane-pod-report",
	"racer-dataplane-lease",
	"racer-dataplane-state",
	"racer-dataplane-webhook-configuration",
	"racer-dataplane-webhook-endpoints",
}

// renamedConfigData returns the settings of an owned pre-rename ConfigMap with
// renamed keys translated, or nil when there is none. It seeds racer-settings
// so administrator tuning survives the rename.
func renamedConfigData(ctx context.Context, env *component.Env) (map[string]string, error) {
	live, err := getLegacy(ctx, env, legacy(&corev1.ConfigMap{ObjectMeta: objectMeta(renamedConfig)}, corev1.SchemeGroupVersion.WithKind("ConfigMap")))
	if err != nil || live == nil {
		return nil, err
	}

	if ok, err := adoptable(ctx, env, live); err != nil || !ok {
		return nil, err
	}

	data := map[string]string{}

	for key, value := range live.(*corev1.ConfigMap).Data {
		if rest, found := strings.CutPrefix(key, renamedKeyPrefix); found {
			key = "RACER_" + rest
		}

		data[key] = value
	}

	return data, nil
}

// planRenamedCleanup deletes the operator-owned objects left under the
// pre-rename names. It runs after the new DaemonSet is planned, so the old
// and new Pods may overlap briefly. Only objects that pass validateAdoptable
// are deleted, each with UID and resourceVersion preconditions; look-alikes
// the operator does not own are left alone. Deletion is ordered over passes:
//
//  1. The DaemonSet, ServiceAccount, and RBAC go first.
//  2. The old ConfigMap goes once racer-settings exists, since it seeds it.
//  3. The old admission policies and bindings go only after the old DaemonSet
//     and grants are observed gone, so the old identity is never left with
//     write access but no admission limits.
//
// It reports whether any pre-rename object is still being removed.
func planRenamedCleanup(ctx context.Context, env *component.Env, plan *component.Plan, settingsExist bool) (bool, error) {
	core, rbac := corev1.SchemeGroupVersion, rbacv1.SchemeGroupVersion
	first := []legacyObject{
		legacy(&appsv1.DaemonSet{ObjectMeta: objectMeta(renamedDataplane)}, appsv1.SchemeGroupVersion.WithKind("DaemonSet")),
		legacy(&rbacv1.RoleBinding{ObjectMeta: objectMeta(renamedDataplane)}, rbac.WithKind("RoleBinding")),
		legacy(&rbacv1.ClusterRoleBinding{ObjectMeta: objectMeta(renamedDataplane)}, rbac.WithKind("ClusterRoleBinding")),
		legacy(&rbacv1.Role{ObjectMeta: objectMeta(renamedDataplane)}, rbac.WithKind("Role")),
		legacy(&rbacv1.ClusterRole{ObjectMeta: objectMeta(renamedDataplane)}, rbac.WithKind("ClusterRole")),
		legacy(&corev1.ServiceAccount{ObjectMeta: objectMeta(renamedDataplane)}, core.WithKind("ServiceAccount")),
	}

	pending, err := planRenamedDeletes(ctx, env, plan, first)
	if err != nil {
		return false, err
	}

	if settingsExist {
		config := []legacyObject{legacy(&corev1.ConfigMap{ObjectMeta: objectMeta(renamedConfig)}, core.WithKind("ConfigMap"))}

		removing, err := planRenamedDeletes(ctx, env, plan, config)
		if err != nil {
			return false, err
		}

		pending = pending || removing
	}

	if pending {
		return true, nil
	}

	var guards []legacyObject

	for _, guardName := range renamedPolicies {
		guards = append(guards,
			legacy(&admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: objectMeta(guardName)}, admissionv1.SchemeGroupVersion.WithKind("ValidatingAdmissionPolicyBinding")),
			legacy(&admissionv1.ValidatingAdmissionPolicy{ObjectMeta: objectMeta(guardName)}, admissionv1.SchemeGroupVersion.WithKind("ValidatingAdmissionPolicy")))
	}

	return planRenamedDeletes(ctx, env, plan, guards)
}

// planRenamedDeletes adds a delete for each candidate the operator owns and
// reports whether any owned candidate still exists, terminating included.
func planRenamedDeletes(ctx context.Context, env *component.Env, plan *component.Plan, candidates []legacyObject) (bool, error) {
	pending := false

	for _, candidate := range candidates {
		live, err := getLegacy(ctx, env, candidate)
		if err != nil {
			return false, err
		}

		if live == nil {
			continue
		}

		if live.GetDeletionTimestamp() != nil {
			// Already going; wait until it is observed gone.
			pending = pending || isRenamedManaged(live)
			continue
		}

		ok, err := adoptable(ctx, env, live)
		if err != nil {
			return false, err
		}

		if ok {
			plan.Add(deleteOp(live, candidate.gvk))

			pending = true
		}
	}

	return pending, nil
}

func isRenamedManaged(obj client.Object) bool {
	return obj.GetAnnotations()[managerAnnotation] == component.FieldOwner || obj.GetLabels()[managerLabel] == component.FieldOwner
}
