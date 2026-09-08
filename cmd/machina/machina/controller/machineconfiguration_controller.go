// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// MachineConfigurationReconciler manages versioned snapshots for
// MachineConfiguration resources.
type MachineConfigurationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager sets up the controller with the Manager.
func (r *MachineConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("machine-configuration").
		For(&unboundedv1alpha3.MachineConfiguration{}).
		Owns(&unboundedv1alpha3.MachineConfigurationVersion{}).
		Complete(r)
}

// Reconcile ensures each MachineConfiguration has a latest
// MachineConfigurationVersion. The latest undeployed version is editable; once
// deployed, further MachineConfiguration spec changes create a new version.
func (r *MachineConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var mc unboundedv1alpha3.MachineConfiguration
	if err := r.Get(ctx, req.NamespacedName, &mc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	versions, err := r.listVersions(ctx, mc.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list MachineConfigurationVersions: %w", err)
	}

	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Spec.Version < versions[j].Spec.Version
	})

	if len(versions) == 0 {
		logger.Info("creating initial MachineConfigurationVersion", "configuration", mc.Name, "version", 1)

		if err := r.createVersion(ctx, &mc, 1); err != nil {
			return ctrl.Result{}, fmt.Errorf("create initial MachineConfigurationVersion: %w", err)
		}

		return ctrl.Result{}, r.updateLatestVersion(ctx, &mc, 1)
	}

	latest := &versions[len(versions)-1]
	if equality.Semantic.DeepEqual(latest.Spec.Template, mc.Spec.Template) {
		return ctrl.Result{}, r.updateLatestVersion(ctx, &mc, latest.Spec.Version)
	}

	if !latest.Status.Deployed {
		logger.Info("updating undeployed MachineConfigurationVersion",
			"configuration", mc.Name,
			"version", latest.Spec.Version,
		)
		latest.Spec.Template = *mc.Spec.Template.DeepCopy()

		if err := r.Update(ctx, latest); err != nil {
			return ctrl.Result{}, fmt.Errorf("update MachineConfigurationVersion %s: %w", latest.Name, err)
		}

		return ctrl.Result{}, r.updateLatestVersion(ctx, &mc, latest.Spec.Version)
	}

	nextVersion := latest.Spec.Version + 1
	if mc.Status.LatestVersion >= nextVersion {
		nextVersion = mc.Status.LatestVersion + 1
	}

	logger.Info("creating new MachineConfigurationVersion",
		"configuration", mc.Name,
		"version", nextVersion,
	)

	if err := r.createVersion(ctx, &mc, nextVersion); err != nil {
		return ctrl.Result{}, fmt.Errorf("create MachineConfigurationVersion %d: %w", nextVersion, err)
	}

	if err := r.cleanupOldVersions(ctx, &mc, versions); err != nil {
		logger.Error(err, "failed to clean up old MachineConfigurationVersions")
	}

	return ctrl.Result{}, r.updateLatestVersion(ctx, &mc, nextVersion)
}

func (r *MachineConfigurationReconciler) listVersions(
	ctx context.Context,
	mcName string,
) ([]unboundedv1alpha3.MachineConfigurationVersion, error) {
	var list unboundedv1alpha3.MachineConfigurationVersionList
	if err := r.List(ctx, &list, client.MatchingLabels{
		unboundedv1alpha3.MCVConfigurationLabelKey: mcName,
	}); err != nil {
		return nil, err
	}

	return list.Items, nil
}

func (r *MachineConfigurationReconciler) createVersion(
	ctx context.Context,
	mc *unboundedv1alpha3.MachineConfiguration,
	version int32,
) error {
	mcv := &unboundedv1alpha3.MachineConfigurationVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: unboundedv1alpha3.MachineConfigurationVersionName(mc.Name, version),
			Labels: map[string]string{
				unboundedv1alpha3.MCVConfigurationLabelKey: mc.Name,
				unboundedv1alpha3.MCVVersionLabelKey:       strconv.Itoa(int(version)),
			},
		},
		Spec: unboundedv1alpha3.MachineConfigurationVersionSpec{
			Version:  version,
			Template: *mc.Spec.Template.DeepCopy(),
		},
	}

	if err := controllerutil.SetControllerReference(mc, mcv, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference: %w", err)
	}

	return r.Create(ctx, mcv)
}

func (r *MachineConfigurationReconciler) updateLatestVersion(
	ctx context.Context,
	mc *unboundedv1alpha3.MachineConfiguration,
	latestVersion int32,
) error {
	if mc.Status.LatestVersion == latestVersion {
		return nil
	}

	mc.Status.LatestVersion = latestVersion

	return r.Status().Update(ctx, mc)
}

func (r *MachineConfigurationReconciler) cleanupOldVersions(
	ctx context.Context,
	mc *unboundedv1alpha3.MachineConfiguration,
	versions []unboundedv1alpha3.MachineConfigurationVersion,
) error {
	// Keep cleanup intentionally conservative for now: only remove old,
	// undeployed versions beyond the default history limit.
	limit := int32(10)
	if mc.Spec.RevisionHistoryLimit != nil {
		limit = *mc.Spec.RevisionHistoryLimit
	}

	if int32(len(versions)) <= limit {
		return nil
	}

	toDelete := int32(len(versions)) - limit
	deleted := int32(0)

	for i := range versions {
		if deleted >= toDelete {
			return nil
		}

		version := &versions[i]
		if version.Status.Deployed || version.Status.DeployedMachines > 0 {
			continue
		}

		if err := r.Delete(ctx, version); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete MachineConfigurationVersion %s: %w", version.Name, err)
		}

		deleted++
	}

	return nil
}
