// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

// MachineConfigurationReconciler reconciles MachineConfiguration objects.
// It manages the lifecycle of MachineConfigurationVersion children:
//   - Creates v1 when a MachineConfiguration is first created
//   - Updates the latest non-deployed version when the spec changes
//   - Creates a new version when the latest version is already deployed
//   - Cleans up old versions based on revisionHistoryLimit
type MachineConfigurationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager registers the controller with the manager.
func (r *MachineConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha3.MachineConfiguration{}).
		Owns(&v1alpha3.MachineConfigurationVersion{}).
		Complete(r)
}

// Reconcile handles changes to MachineConfiguration objects.
func (r *MachineConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var mc v1alpha3.MachineConfiguration
	if err := r.Get(ctx, req.NamespacedName, &mc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	// List all MachineConfigurationVersions owned by this MC.
	versions, err := r.listVersions(ctx, mc.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list versions: %w", err)
	}

	// Sort by version number ascending.
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Spec.Version < versions[j].Spec.Version
	})

	var latest *v1alpha3.MachineConfigurationVersion
	if len(versions) > 0 {
		latest = &versions[len(versions)-1]
	}

	if latest == nil {
		// No versions exist yet - create v1.
		log.Info("creating initial MachineConfigurationVersion", "version", 1)

		if err := r.createVersion(ctx, &mc, 1); err != nil {
			return ctrl.Result{}, fmt.Errorf("create initial version: %w", err)
		}

		return ctrl.Result{}, r.updateStatus(ctx, &mc, 1, 1)
	}

	// Check if the latest version's template matches the current MC spec.
	if equality.Semantic.DeepEqual(latest.Spec.Template, mc.Spec.Template) {
		// Spec unchanged - just ensure status is correct.
		currentVersion := int32(0)
		if !latest.Status.Deployed {
			currentVersion = latest.Spec.Version
		}

		return ctrl.Result{}, r.updateStatus(ctx, &mc, latest.Spec.Version, currentVersion)
	}

	// Spec changed.
	if !latest.Status.Deployed {
		// Latest version is still editable - update it in place.
		log.Info("updating editable MachineConfigurationVersion",
			"version", latest.Spec.Version)

		latest.Spec.Template = mc.Spec.Template

		if err := r.Update(ctx, latest); err != nil {
			return ctrl.Result{}, fmt.Errorf("update version %d: %w", latest.Spec.Version, err)
		}

		return ctrl.Result{}, r.updateStatus(ctx, &mc, latest.Spec.Version, latest.Spec.Version)
	}

	// Latest version is deployed (immutable) - create a new one.
	nextVersion := latest.Spec.Version + 1
	// Ensure we never reuse a version number.
	if mc.Status.LatestVersion >= nextVersion {
		nextVersion = mc.Status.LatestVersion + 1
	}

	log.Info("creating new MachineConfigurationVersion",
		"version", nextVersion,
		"reason", "latest version is deployed")

	if err := r.createVersion(ctx, &mc, nextVersion); err != nil {
		return ctrl.Result{}, fmt.Errorf("create version %d: %w", nextVersion, err)
	}

	// Clean up old versions beyond the history limit.
	if err := r.cleanupOldVersions(ctx, &mc, versions); err != nil {
		log.Error(err, "failed to cleanup old versions")
		// Non-fatal - continue.
	}

	return ctrl.Result{}, r.updateStatus(ctx, &mc, nextVersion, nextVersion)
}

// listVersions returns all MachineConfigurationVersions for the given
// MachineConfiguration name.
func (r *MachineConfigurationReconciler) listVersions(
	ctx context.Context,
	mcName string,
) ([]v1alpha3.MachineConfigurationVersion, error) {
	var list v1alpha3.MachineConfigurationVersionList
	if err := r.List(ctx, &list, client.MatchingLabels{
		v1alpha3.MCVConfigurationLabelKey: mcName,
	}); err != nil {
		return nil, err
	}

	return list.Items, nil
}

// createVersion creates a new MachineConfigurationVersion with the given
// version number, owned by the MachineConfiguration.
func (r *MachineConfigurationReconciler) createVersion(
	ctx context.Context,
	mc *v1alpha3.MachineConfiguration,
	version int32,
) error {
	mcv := &v1alpha3.MachineConfigurationVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-v%d", mc.Name, version),
			Labels: map[string]string{
				v1alpha3.MCVConfigurationLabelKey: mc.Name,
				v1alpha3.MCVVersionLabelKey:       strconv.Itoa(int(version)),
			},
		},
		Spec: v1alpha3.MachineConfigurationVersionSpec{
			Version:  version,
			Template: *mc.Spec.Template.DeepCopy(),
		},
	}

	if err := controllerutil.SetControllerReference(mc, mcv, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference: %w", err)
	}

	return r.Create(ctx, mcv)
}

// updateStatus updates the MachineConfiguration's status fields.
func (r *MachineConfigurationReconciler) updateStatus(
	ctx context.Context,
	mc *v1alpha3.MachineConfiguration,
	latestVersion int32,
	currentVersion int32,
) error {
	mc.Status.LatestVersion = latestVersion
	mc.Status.CurrentVersion = currentVersion

	return r.Status().Update(ctx, mc)
}

// cleanupOldVersions removes non-deployed, non-referenced versions
// beyond the revisionHistoryLimit.
func (r *MachineConfigurationReconciler) cleanupOldVersions(
	ctx context.Context,
	mc *v1alpha3.MachineConfiguration,
	versions []v1alpha3.MachineConfigurationVersion,
) error {
	limit := int32(10)
	if mc.Spec.RevisionHistoryLimit != nil {
		limit = *mc.Spec.RevisionHistoryLimit
	}

	if int32(len(versions)) <= limit {
		return nil
	}

	// Candidates for deletion: old versions that are not deployed and
	// have no machines referencing them.
	var candidates []v1alpha3.MachineConfigurationVersion

	for i := range versions {
		v := &versions[i]
		// Never delete the most recent version.
		if i == len(versions)-1 {
			continue
		}
		// Never delete deployed versions that still have machines.
		if v.Status.Deployed && v.Status.DeployedMachines > 0 {
			continue
		}

		candidates = append(candidates, *v)
	}

	// Delete the oldest candidates first until we are within the limit.
	toDelete := int32(len(versions)) - limit
	if toDelete <= 0 {
		return nil
	}

	deleted := int32(0)

	for i := range candidates {
		if deleted >= toDelete {
			break
		}

		if err := r.Delete(ctx, &candidates[i]); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete version %s: %w", candidates[i].Name, err)
			}
		}

		deleted++
	}

	return nil
}
