// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"
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

	cm, ok := live.(*corev1.ConfigMap)
	if !ok {
		return nil, fmt.Errorf("%s is %T, not a ConfigMap", renamedConfig, live)
	}

	data := map[string]string{}

	for key, value := range cm.Data {
		if rest, found := strings.CutPrefix(key, renamedKeyPrefix); found {
			key = "RACER_" + rest
		}

		data[key] = value
	}

	return data, nil
}

// planRenamedCleanup deletes the operator-owned objects left under the
// pre-rename names. Nothing is deleted until the new DaemonSet is observed
// live and owned by this operator (see replacementLive): a pass runs removals
// before writes, and an override can withhold the new DaemonSet, so a planned
// replacement is no proof that one will exist. The old and new Pods may
// overlap briefly. Only objects that pass validateAdoptable
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
	live, err := replacementLive(ctx, env)
	if err != nil || !live {
		return true, err
	}

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

// replacementLive reports whether the new DaemonSet exists, is not being
// deleted, and is owned by this operator. It deliberately does not wait for
// the rollout to become ready: the old Pods may hold resources, such as host
// ports, that the new Pods need, so waiting on readiness could deadlock. A
// written, owned replacement is enough to keep a workload scheduled.
func replacementLive(ctx context.Context, env *component.Env) (bool, error) {
	live, err := getLegacy(ctx, env, legacy(&appsv1.DaemonSet{ObjectMeta: objectMeta(dataplaneName)}, appsv1.SchemeGroupVersion.WithKind("DaemonSet")))
	if err != nil || live == nil || live.GetDeletionTimestamp() != nil {
		return false, err
	}

	return adoptable(ctx, env, live)
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

// planRenamedContainment revokes the operator-owned pre-rename grants when any
// pre-rename admission policy or binding that restricted them is missing,
// terminating, or no longer managed by this operator. planContainment covers
// only the renamed guards, and planRenamedCleanup waits for a live
// replacement and never runs without a ClusterCache, so without this the old
// identity could keep write access with no admission limits. The pre-rename
// manifests are no longer shipped, so specs cannot be compared; drift is
// detected by presence and ownership only. The old guards are never
// recreated: revoking the grants is enough, and planRenamedCleanup removes
// the guards once the grants are gone. It returns nil when nothing is
// revoked.
func planRenamedContainment(ctx context.Context, env *component.Env) (*component.Plan, error) {
	rbac := rbacv1.SchemeGroupVersion
	grants := []legacyObject{
		legacy(&rbacv1.RoleBinding{ObjectMeta: objectMeta(renamedDataplane)}, rbac.WithKind("RoleBinding")),
		legacy(&rbacv1.ClusterRoleBinding{ObjectMeta: objectMeta(renamedDataplane)}, rbac.WithKind("ClusterRoleBinding")),
	}

	var owned []legacyObject

	for _, grant := range grants {
		live, err := getLegacy(ctx, env, grant)
		if err != nil {
			return nil, err
		}

		if live == nil || live.GetDeletionTimestamp() != nil {
			continue
		}

		ok, err := adoptable(ctx, env, live)
		if err != nil {
			return nil, err
		}

		if ok {
			owned = append(owned, grant)
		}
	}

	if len(owned) == 0 {
		return nil, nil
	}

	healthy := true

	for _, guardName := range renamedPolicies {
		for _, guard := range []legacyObject{
			legacy(&admissionv1.ValidatingAdmissionPolicy{ObjectMeta: objectMeta(guardName)}, admissionv1.SchemeGroupVersion.WithKind("ValidatingAdmissionPolicy")),
			legacy(&admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: objectMeta(guardName)}, admissionv1.SchemeGroupVersion.WithKind("ValidatingAdmissionPolicyBinding")),
		} {
			live, err := getLegacy(ctx, env, guard)
			if err != nil {
				return nil, err
			}

			if live == nil || live.GetDeletionTimestamp() != nil || !isRenamedManaged(live) {
				healthy = false
			}
		}
	}

	if healthy {
		return nil, nil
	}

	plan := component.NewPlan()
	for _, grant := range owned {
		plan.Add(deleteOp(grant.obj, grant.gvk))
	}

	return plan, nil
}
