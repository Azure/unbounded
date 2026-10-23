// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racer/workload"
)

// Names and keys of the retired racer-controller installation. They are read
// only to remove what this operator installed.
const (
	legacyController   = "racer-controller"
	legacyClaim        = "racer-operator-installation"
	legacyMarker       = "racer-installation"
	legacyVersion      = "racer-version"
	legacyCredentials  = "racer-credentials"
	legacyClaimUID     = "unbounded-cloud.io/racer-operator-claim-uid"
	legacyBoundUID     = "racer.unbounded-cloud.io/installation-uid"
	legacyCredentialID = "racer.unbounded-cloud.io/credentials-uid"

	// legacyInstallationUID is the installation stamp the retired operator
	// wrote next to managerAnnotation on every object it applied. A legacy
	// object is removed only when it carries both, so an object that merely
	// carries the manager annotation is never deleted as part of migration.
	legacyInstallationUID = "unbounded-cloud.io/racer-installation-uid"

	legacyConfig = "racer-config"

	// legacyMarkerUIDKey is the claim data key binding the marker UID.
	legacyMarkerUIDKey = "marker_uid"

	// legacyVersionUIDKey is the marker data key a consumed marker uses to
	// record the exact racer-version UID.
	legacyVersionUIDKey = "version_uid"
)

type legacyObject struct {
	obj client.Object
	gvk schema.GroupVersionKind
}

func legacy(obj client.Object, gvk schema.GroupVersionKind) legacyObject {
	return legacyObject{obj: obj, gvk: gvk}
}

// legacyAnchors are the retired operator's installation records. Every object
// that installation applied carries legacyInstallationUID set to the marker
// UID, and the claim and marker bind each other.
type legacyAnchors struct {
	claim, marker *corev1.ConfigMap
	// markerUID is set only when the claim and marker are present and bound.
	markerUID string
}

func readLegacyAnchors(ctx context.Context, env *component.Env) (legacyAnchors, error) {
	var anchors legacyAnchors

	for _, item := range []struct {
		name string
		dst  **corev1.ConfigMap
	}{{legacyClaim, &anchors.claim}, {legacyMarker, &anchors.marker}} {
		cm := &corev1.ConfigMap{}

		err := env.LiveReader().Get(ctx, objectKey(env, item.name), cm)
		if apierrors.IsNotFound(err) {
			continue
		}

		if err != nil {
			return legacyAnchors{}, err
		}

		*item.dst = cm
	}

	claim, marker := anchors.claim, anchors.marker
	if claim != nil && marker != nil && owned(claim) && owned(marker) &&
		marker.Annotations[legacyClaimUID] == string(claim.UID) &&
		claim.Data[legacyMarkerUIDKey] == string(marker.UID) &&
		claim.Data["cluster"] != "" && marker.Data["cluster"] == claim.Data["cluster"] {
		anchors.markerUID = string(marker.UID)
	}

	return anchors, nil
}

// stamped reports whether obj carries the exact stamp of the bound legacy
// installation.
func (a legacyAnchors) stamped(obj client.Object) bool {
	return a.markerUID != "" && owned(obj) && obj.GetAnnotations()[legacyInstallationUID] == a.markerUID
}

// validateAdoptable is validateOwner plus the legacy stamp check: an object
// stamped by a retired installation is adopted only when its stamp is the UID
// of the bound marker.
func validateAdoptable(ctx context.Context, env *component.Env, obj client.Object) error {
	if err := validateOwner(obj); err != nil {
		return err
	}

	if obj.GetAnnotations()[legacyInstallationUID] == "" {
		return nil
	}

	anchors, err := readLegacyAnchors(ctx, env)
	if err != nil {
		return err
	}

	if !anchors.stamped(obj) {
		return fmt.Errorf("racer resource %s %s carries an installation stamp not bound to the racer-installation marker and is %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), errNotOwned)
	}

	return nil
}

// adoptable reports whether obj may be treated as operator owned.
func adoptable(ctx context.Context, env *component.Env, obj client.Object) (bool, error) {
	err := validateAdoptable(ctx, env, obj)
	if errors.Is(err, errNotOwned) {
		return false, nil
	}

	return err == nil, err
}

// planLegacy removes the retired racer-controller installation in ordered,
// resumable passes so the old and new control planes never run together:
//
//  1. Delete the controller Deployment, only if it carries the bound stamp.
//  2. Wait until no controller Pod is left; done is false until then.
//  3. Delete the stamped controller objects. Objects without the bound stamp,
//     or any object when the anchors are missing or inconsistent, are kept.
//  4. Once no stamped object remains (adopted objects are restamped by the
//     install that runs in the same passes), delete the credentials, then the
//     version record, then the marker, then the claim, each only after the
//     previous one is observed gone.
//
// Every delete carries UID and resourceVersion preconditions.
func planLegacy(ctx context.Context, env *component.Env) (plan *component.Plan, done bool, err error) {
	plan = component.NewPlan()

	anchors, err := readLegacyAnchors(ctx, env)
	if err != nil {
		return nil, false, err
	}

	deployment := &appsv1.Deployment{}

	err = env.LiveReader().Get(ctx, objectKey(env, legacyController), deployment)

	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, false, err
	case !anchors.stamped(deployment):
		return nil, false, fmt.Errorf("deployment %s is %w: it lacks the stamp of a consistent racer-installation marker and claim; remove it before Racer runs its control plane in the dataplane", legacyController, errNotOwned)
	default:
		if err := checkLegacyConfig(ctx, env); err != nil {
			return nil, false, err
		}

		plan.Add(deleteOp(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment")))

		return plan, false, nil
	}

	pods := &corev1.PodList{}
	if err := env.LiveReader().List(ctx, pods, client.InNamespace(env.Namespace), client.MatchingLabels{"app": legacyController}); err != nil {
		return nil, false, err
	}

	if len(pods.Items) != 0 {
		return plan, false, nil
	}

	if anchors.markerUID == "" {
		// Unproven: keep every object, except a claim whose marker is gone
		// after a completed cleanup.
		if err := planOrphanClaim(ctx, env, plan, anchors); err != nil {
			return nil, false, err
		}

		return plan, true, nil
	}

	pending := false

	for _, candidate := range legacyCandidates() {
		live, err := getLegacy(ctx, env, candidate)
		if err != nil {
			return nil, false, err
		}

		if live != nil && anchors.stamped(live) {
			plan.Add(deleteOp(live, candidate.gvk))

			pending = true
		}
	}

	restamp, err := planLegacyRestamp(ctx, env, plan, anchors)
	if err != nil {
		return nil, false, err
	}

	if pending || restamp {
		return plan, true, nil
	}

	if err := planLegacyIdentity(ctx, env, plan, anchors); err != nil {
		return nil, false, err
	}

	return plan, true, nil
}

func legacyCandidates() []legacyObject {
	core := corev1.SchemeGroupVersion
	candidates := []legacyObject{
		legacy(&corev1.Service{}, core.WithKind("Service")),
		legacy(&policyv1.PodDisruptionBudget{}, policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget")),
		legacy(&rbacv1.RoleBinding{}, rbacv1.SchemeGroupVersion.WithKind("RoleBinding")),
		legacy(&rbacv1.ClusterRoleBinding{}, rbacv1.SchemeGroupVersion.WithKind("ClusterRoleBinding")),
		legacy(&rbacv1.Role{}, rbacv1.SchemeGroupVersion.WithKind("Role")),
		legacy(&rbacv1.ClusterRole{}, rbacv1.SchemeGroupVersion.WithKind("ClusterRole")),
		legacy(&corev1.ServiceAccount{}, core.WithKind("ServiceAccount")),
		legacy(&corev1.Secret{}, core.WithKind("Secret")),
	}

	for i := range candidates {
		candidates[i].obj.SetName(legacyController)
	}

	candidates[len(candidates)-1].obj.SetName("racer-controller-tls")

	for _, configName := range []string{legacyConfig, "racer-bootstrap-trust"} {
		cm := &corev1.ConfigMap{}
		cm.SetName(configName)
		candidates = append(candidates, legacy(cm, core.WithKind("ConfigMap")))
	}

	for _, guardName := range []string{"racer-node-write-restriction", "racer-runtime-write-restriction"} {
		policy := &admissionv1.ValidatingAdmissionPolicy{}
		policy.SetName(guardName)

		binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
		binding.SetName(guardName)
		candidates = append(candidates,
			legacy(policy, admissionv1.SchemeGroupVersion.WithKind("ValidatingAdmissionPolicy")),
			legacy(binding, admissionv1.SchemeGroupVersion.WithKind("ValidatingAdmissionPolicyBinding")))
	}

	return candidates
}

// planLegacyRestamp reports whether an object the new installation adopts
// still carries the legacy stamp. Install planning restamps applied objects;
// the administrator ConfigMap is never applied, so its stamp is dropped here
// with an optimistic lock. Anchors stay until no stamp is left.
func planLegacyRestamp(ctx context.Context, env *component.Env, plan *component.Plan, anchors legacyAnchors) (bool, error) {
	core, rbac := corev1.SchemeGroupVersion, rbacv1.SchemeGroupVersion
	named := []legacyObject{
		legacy(&appsv1.DaemonSet{ObjectMeta: objectMeta(dataplaneName)}, appsv1.SchemeGroupVersion.WithKind("DaemonSet")),
		legacy(&corev1.ServiceAccount{ObjectMeta: objectMeta(dataplaneName)}, core.WithKind("ServiceAccount")),
		legacy(&rbacv1.Role{ObjectMeta: objectMeta(dataplaneName)}, rbac.WithKind("Role")),
		legacy(&rbacv1.RoleBinding{ObjectMeta: objectMeta(dataplaneName)}, rbac.WithKind("RoleBinding")),
		legacy(&rbacv1.ClusterRole{ObjectMeta: objectMeta(dataplaneName)}, rbac.WithKind("ClusterRole")),
		legacy(&rbacv1.ClusterRoleBinding{ObjectMeta: objectMeta(dataplaneName)}, rbac.WithKind("ClusterRoleBinding")),
		legacy(&admissionv1.MutatingWebhookConfiguration{ObjectMeta: objectMeta(webhookConfigName)}, admissionv1.SchemeGroupVersion.WithKind("MutatingWebhookConfiguration")),
	}

	remaining := false

	for _, candidate := range named {
		live, err := getLegacy(ctx, env, candidate)
		if err != nil {
			return false, err
		}

		if live != nil && live.GetAnnotations()[legacyInstallationUID] != "" {
			remaining = true
		}
	}

	opts := []client.ListOption{client.InNamespace(env.Namespace), client.MatchingLabels{"app.kubernetes.io/component": originAppName}}

	daemonSets := &appsv1.DaemonSetList{}
	if err := env.LiveReader().List(ctx, daemonSets, opts...); err != nil {
		return false, err
	}

	accounts := &corev1.ServiceAccountList{}
	if err := env.LiveReader().List(ctx, accounts, opts...); err != nil {
		return false, err
	}

	for i := range daemonSets.Items {
		remaining = remaining || daemonSets.Items[i].Annotations[legacyInstallationUID] != ""
	}

	for i := range accounts.Items {
		remaining = remaining || accounts.Items[i].Annotations[legacyInstallationUID] != ""
	}

	config := &corev1.ConfigMap{}

	err := env.LiveReader().Get(ctx, objectKey(env, dataplaneConfigName), config)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}

	if err == nil && config.Annotations[legacyInstallationUID] != "" {
		remaining = true

		if anchors.stamped(config) {
			config.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
			restamped := config.DeepCopy()
			delete(restamped.Annotations, legacyInstallationUID)

			plan.Add(component.Operation{Kind: component.OpMergePatch, Component: name, Base: component.ToUnstructured(config), Object: component.ToUnstructured(restamped)})
		}
	}

	return remaining, nil
}

func getLegacy(ctx context.Context, env *component.Env, candidate legacyObject) (client.Object, error) {
	if !isClusterScoped(candidate.gvk.Kind) {
		candidate.obj.SetNamespace(env.Namespace)
	}

	err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(candidate.obj), candidate.obj)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	candidate.obj.GetObjectKind().SetGroupVersionKind(candidate.gvk)

	return candidate.obj, nil
}

func isClusterScoped(kind string) bool {
	switch kind {
	case "ClusterRole", "ClusterRoleBinding", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "MutatingWebhookConfiguration":
		return true
	}

	return false
}

func deleteOp(obj client.Object, gvk schema.GroupVersionKind) component.Operation {
	obj.GetObjectKind().SetGroupVersionKind(gvk)

	// The UID and resourceVersion preconditions keep a replacement safe.
	return component.Operation{Kind: component.OpDelete, Object: component.ToUnstructured(obj), Component: name}
}

// planLegacyIdentity removes the bound installation records one stage per
// pass: credentials, version, marker, then claim. The version and credentials
// were written by the retired controller and are removed only when the
// marker binds them. A consumed marker records the exact version UID in
// data.version_uid; a pending marker records none, so only the version's own
// binding annotation proves it. A terminating version is waited out, and a
// version that contradicts the marker's record keeps the marker in place.
func planLegacyIdentity(ctx context.Context, env *component.Env, plan *component.Plan, anchors legacyAnchors) error {
	configMapGVK := corev1.SchemeGroupVersion.WithKind("ConfigMap")
	secretGVK := corev1.SchemeGroupVersion.WithKind("Secret")

	version, err := getLegacy(ctx, env, legacy(&corev1.ConfigMap{ObjectMeta: objectMeta(legacyVersion)}, configMapGVK))
	if err != nil {
		return err
	}

	recordedVersion := anchors.marker.Data[legacyVersionUIDKey]

	if version != nil {
		boundByAnnotation := version.GetAnnotations()[legacyBoundUID] == anchors.markerUID

		switch {
		case recordedVersion != "" && (string(version.GetUID()) != recordedVersion || !boundByAnnotation):
			return fmt.Errorf("ConfigMap %s does not match the version UID %s recorded by marker %s; restore it or remove both by hand", legacyVersion, recordedVersion, legacyMarker)
		case !boundByAnnotation:
			// Not bound to this installation: keep it and continue.
		case version.GetDeletionTimestamp() != nil:
			// Wait until the bound version is observed gone.
			return nil
		default:
			if credentialUID := version.GetAnnotations()[legacyCredentialID]; credentialUID != "" {
				credentials, err := getLegacy(ctx, env, legacy(&corev1.Secret{ObjectMeta: objectMeta(legacyCredentials)}, secretGVK))
				if err != nil {
					return err
				}

				if credentials != nil && string(credentials.GetUID()) == credentialUID {
					if credentials.GetDeletionTimestamp() == nil {
						plan.Add(deleteOp(credentials, secretGVK))
					}

					return nil
				}
			}

			plan.Add(deleteOp(version, configMapGVK))

			return nil
		}
	}

	plan.Add(deleteOp(anchors.marker, configMapGVK))

	return nil
}

// planOrphanClaim removes the claim left when cleanup deleted the marker and
// stopped before the claim. It only acts when the claim's marker is gone and
// no object carries that marker's stamp.
func planOrphanClaim(ctx context.Context, env *component.Env, plan *component.Plan, anchors legacyAnchors) error {
	claim := anchors.claim
	if claim == nil || anchors.marker != nil || !owned(claim) || claim.Data[legacyMarkerUIDKey] == "" {
		return nil
	}

	probe := legacyAnchors{markerUID: claim.Data[legacyMarkerUIDKey]}

	remaining, err := planLegacyRestamp(ctx, env, component.NewPlan(), probe)
	if err != nil || remaining {
		return err
	}

	for _, candidate := range legacyCandidates() {
		live, err := getLegacy(ctx, env, candidate)
		if err != nil {
			return err
		}

		if live != nil && live.GetAnnotations()[legacyInstallationUID] == probe.markerUID {
			return nil
		}
	}

	plan.Add(deleteOp(claim, corev1.SchemeGroupVersion.WithKind("ConfigMap")))

	return nil
}

// checkLegacyConfig refuses migration while racer-config holds workload
// settings the new installation no longer reads. The retired controller used
// them to render the dataplane; dropping them silently would move a running
// cluster to another peer port or off the host network.
func checkLegacyConfig(ctx context.Context, env *component.Env) error {
	config := &corev1.ConfigMap{}

	err := env.LiveReader().Get(ctx, objectKey(env, legacyConfig), config)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return err
	}

	var unsupported []string

	if value, ok := config.Data["RACER_PEER_PORT"]; ok && strings.TrimSpace(value) != strconv.Itoa(int(workload.DefaultConfig("", "", "").PeerPort)) {
		unsupported = append(unsupported, "RACER_PEER_PORT (the peer port is fixed at 8082)")
	}

	if value := strings.TrimSpace(config.Data["RACER_DIAGNOSTICS_PORT"]); value != "" {
		unsupported = append(unsupported, "RACER_DIAGNOSTICS_PORT (the diagnostics port is fixed at 9090)")
	}

	if value := strings.TrimSpace(config.Data["RACER_HOST_NETWORK"]); value != "" {
		if enabled, err := strconv.ParseBool(value); err != nil || enabled {
			unsupported = append(unsupported, "RACER_HOST_NETWORK (set spec.template.spec.hostNetwork through a racer-dataplane DaemonSet override)")
		}
	}

	if strings.TrimSpace(config.Data["RACER_POD_NETWORK_NODES"]) != "" {
		unsupported = append(unsupported, "RACER_POD_NETWORK_NODES")
	}

	if len(unsupported) == 0 {
		return nil
	}

	return fmt.Errorf("ConfigMap %s sets workload settings the Racer dataplane installation no longer supports: %s; remove them before the retired racer-controller is replaced", legacyConfig, strings.Join(unsupported, ", "))
}
