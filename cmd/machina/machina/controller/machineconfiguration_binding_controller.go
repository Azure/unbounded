// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"sort"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// MachineConfigurationBindingReconciler resolves Machine configurationRef
// selection and pins it to a concrete MachineConfigurationVersion. It does not
// copy template fields into Machine spec; configurationRef remains the source
// of truth for later provisioning or repave logic.
type MachineConfigurationBindingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// SetupWithManager sets up the controller with the Manager.
func (r *MachineConfigurationBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("machine-configuration-binding").
		For(&unboundedv1alpha3.Machine{}).
		Watches(
			&unboundedv1alpha3.MachineConfiguration{},
			handler.EnqueueRequestsFromMapFunc(r.findMachinesForConfiguration),
		).
		Watches(
			&unboundedv1alpha3.MachineConfigurationVersion{},
			handler.EnqueueRequestsFromMapFunc(r.findMachinesForConfigurationVersion),
		).
		Complete(r)
}

// Reconcile resolves a Machine's selected MachineConfigurationVersion. Explicit
// configurationRef wins; otherwise matching MachineConfiguration selectors are
// evaluated by priority and name.
func (r *MachineConfigurationBindingReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var machine unboundedv1alpha3.Machine
	if err := r.Get(ctx, req.NamespacedName, &machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	selection, err := r.resolveConfiguration(ctx, &machine)
	if err != nil {
		return ctrl.Result{}, err
	}

	if selection == nil {
		logger.Info("Machine has no matching configuration", "machine", machine.Name)

		return ctrl.Result{}, r.setConfigurationPending(
			ctx,
			&machine,
			metav1.ConditionTrue,
			"Pending",
			"No MachineConfiguration has been assigned or matched",
		)
	}

	updatedSpec := false
	if needsConfigurationRefUpdate(&machine, selection) {
		machine.Spec.ConfigurationRef = &unboundedv1alpha3.MachineConfigurationRef{
			Name:    selection.configurationName,
			Version: ptr.To(selection.version),
		}

		if err := r.Update(ctx, &machine); err != nil {
			return ctrl.Result{}, fmt.Errorf("update Machine configurationRef: %w", err)
		}

		updatedSpec = true
	}

	if updatedSpec {
		if err := r.Get(ctx, client.ObjectKey{Name: machine.Name}, &machine); err != nil {
			return ctrl.Result{}, fmt.Errorf("refetch Machine after configurationRef update: %w", err)
		}
	}

	return ctrl.Result{}, r.setConfigurationPending(
		ctx,
		&machine,
		metav1.ConditionFalse,
		"Resolved",
		fmt.Sprintf("Using MachineConfigurationVersion %s", selection.versionName),
	)
}

type configurationSelection struct {
	configurationName string
	version           int32
	versionName       string
}

func (r *MachineConfigurationBindingReconciler) resolveConfiguration(
	ctx context.Context,
	machine *unboundedv1alpha3.Machine,
) (*configurationSelection, error) {
	if machine.Spec.ConfigurationRef != nil {
		return r.resolveExplicitConfiguration(ctx, machine.Spec.ConfigurationRef)
	}

	mc, err := r.selectConfiguration(ctx, machine)
	if err != nil || mc == nil {
		return nil, err
	}

	return r.resolveLatestVersion(ctx, mc.Name)
}

func (r *MachineConfigurationBindingReconciler) resolveExplicitConfiguration(
	ctx context.Context,
	ref *unboundedv1alpha3.MachineConfigurationRef,
) (*configurationSelection, error) {
	if ref.Version != nil {
		name := unboundedv1alpha3.MachineConfigurationVersionName(ref.Name, *ref.Version)
		var mcv unboundedv1alpha3.MachineConfigurationVersion
		if err := r.Get(ctx, client.ObjectKey{Name: name}, &mcv); err != nil {
			return nil, fmt.Errorf("get MachineConfigurationVersion %s: %w", name, err)
		}

		return &configurationSelection{
			configurationName: ref.Name,
			version:           mcv.Spec.Version,
			versionName:       mcv.Name,
		}, nil
	}

	return r.resolveLatestVersion(ctx, ref.Name)
}

func (r *MachineConfigurationBindingReconciler) selectConfiguration(
	ctx context.Context,
	machine *unboundedv1alpha3.Machine,
) (*unboundedv1alpha3.MachineConfiguration, error) {
	var list unboundedv1alpha3.MachineConfigurationList
	if err := r.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list MachineConfigurations: %w", err)
	}

	var matches []unboundedv1alpha3.MachineConfiguration
	for i := range list.Items {
		mc := &list.Items[i]
		if mc.Spec.MachineSelector == nil {
			continue
		}

		selector, err := metav1.LabelSelectorAsSelector(mc.Spec.MachineSelector)
		if err != nil {
			return nil, fmt.Errorf("parse selector for MachineConfiguration %s: %w", mc.Name, err)
		}

		if selector.Matches(labels.Set(machine.Labels)) {
			matches = append(matches, *mc)
		}
	}

	if len(matches) == 0 {
		return nil, nil
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Spec.Priority != matches[j].Spec.Priority {
			return matches[i].Spec.Priority > matches[j].Spec.Priority
		}

		return matches[i].Name < matches[j].Name
	})

	return &matches[0], nil
}

func (r *MachineConfigurationBindingReconciler) resolveLatestVersion(
	ctx context.Context,
	configurationName string,
) (*configurationSelection, error) {
	var list unboundedv1alpha3.MachineConfigurationVersionList
	if err := r.List(ctx, &list, client.MatchingLabels{
		unboundedv1alpha3.MCVConfigurationLabelKey: configurationName,
	}); err != nil {
		return nil, fmt.Errorf("list MachineConfigurationVersions for %s: %w", configurationName, err)
	}

	if len(list.Items) == 0 {
		return nil, fmt.Errorf("MachineConfiguration %s has no versions", configurationName)
	}

	latest := list.Items[0]
	for i := 1; i < len(list.Items); i++ {
		if list.Items[i].Spec.Version > latest.Spec.Version {
			latest = list.Items[i]
		}
	}

	return &configurationSelection{
		configurationName: configurationName,
		version:           latest.Spec.Version,
		versionName:       latest.Name,
	}, nil
}

func (r *MachineConfigurationBindingReconciler) setConfigurationPending(
	ctx context.Context,
	machine *unboundedv1alpha3.Machine,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               unboundedv1alpha3.MachineConditionConfigurationPending,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: machine.Generation,
	})

	if status == metav1.ConditionTrue {
		machine.Status.Phase = unboundedv1alpha3.MachinePhasePending
		machine.Status.Message = message
	}

	return r.Status().Update(ctx, machine)
}

func (r *MachineConfigurationBindingReconciler) findMachinesForConfiguration(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	mc, ok := obj.(*unboundedv1alpha3.MachineConfiguration)
	if !ok {
		return nil
	}

	var list unboundedv1alpha3.MachineList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for i := range list.Items {
		machine := &list.Items[i]
		if machine.Spec.ConfigurationRef != nil && machine.Spec.ConfigurationRef.Name == mc.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKey{Name: machine.Name},
			})
			continue
		}

		if machine.Spec.ConfigurationRef != nil || mc.Spec.MachineSelector == nil {
			continue
		}

		selector, err := metav1.LabelSelectorAsSelector(mc.Spec.MachineSelector)
		if err != nil {
			continue
		}

		if selector.Matches(labels.Set(machine.Labels)) {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKey{Name: machine.Name},
			})
		}
	}

	return requests
}

func (r *MachineConfigurationBindingReconciler) findMachinesForConfigurationVersion(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	mcv, ok := obj.(*unboundedv1alpha3.MachineConfigurationVersion)
	if !ok {
		return nil
	}

	configurationName := mcv.Labels[unboundedv1alpha3.MCVConfigurationLabelKey]
	if configurationName == "" {
		return nil
	}

	var mc unboundedv1alpha3.MachineConfiguration
	if err := r.Get(ctx, client.ObjectKey{Name: configurationName}, &mc); err != nil {
		return nil
	}

	return r.findMachinesForConfiguration(ctx, &mc)
}

var _ reconcile.Reconciler = (*MachineConfigurationBindingReconciler)(nil)

func needsConfigurationRefUpdate(
	machine *unboundedv1alpha3.Machine,
	selection *configurationSelection,
) bool {
	ref := machine.Spec.ConfigurationRef
	return ref == nil ||
		ref.Name != selection.configurationName ||
		ref.Version == nil ||
		*ref.Version != selection.version
}
