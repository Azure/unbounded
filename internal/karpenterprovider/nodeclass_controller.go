// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package karpenterprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	karpenterv1alpha1 "github.com/Azure/unbounded/api/karpenter/v1alpha1"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineconfigs"
)

type NodeClassReconciler struct {
	client.Client
}

func (r *NodeClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var nodeClass karpenterv1alpha1.UnboundedNodeClass
	if err := r.Get(ctx, req.NamespacedName, &nodeClass); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	stored := nodeClass.DeepCopy()
	if err := r.validate(ctx, &nodeClass); err != nil {
		nodeClass.StatusConditions().SetFalse(status.ConditionReady, "ValidationFailed", err.Error())
	} else {
		nodeClass.StatusConditions().SetTrueWithReason(status.ConditionReady, "Validated", "NodeClass configuration and capacity are valid")
	}

	if err := r.Status().Patch(ctx, &nodeClass, client.MergeFrom(stored)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch UnboundedNodeClass status: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *NodeClassReconciler) validate(ctx context.Context, nodeClass *karpenterv1alpha1.UnboundedNodeClass) error {
	if _, err := metav1.LabelSelectorAsSelector(&nodeClass.Spec.MachineSelector); err != nil {
		return fmt.Errorf("machineSelector: %w", err)
	}

	if nodeClass.Spec.Domain == "" {
		return errors.New("domain is required for durable mutation serialization")
	}

	if strings.TrimSpace(nodeClass.Spec.ImagePolicy.RequiredImage) == "" {
		return errors.New("imagePolicy.requiredImage is required")
	}

	version, err := machineconfigs.ResolveVersionFromRef(ctx, r.Client, &nodeClass.Spec.ConfigurationRef)
	if err != nil {
		return fmt.Errorf("resolve MachineConfiguration: %w", err)
	}

	if version.Spec.Template.Agent == nil || version.Spec.Template.Agent.Image != nodeClass.Spec.ImagePolicy.RequiredImage {
		return errors.New("MachineConfiguration agent image does not match imagePolicy.requiredImage")
	}

	if len(nodeClass.Spec.InstanceTypes) == 0 {
		return errors.New("at least one instance type is required")
	}

	names := map[string]struct{}{}

	for i := range nodeClass.Spec.InstanceTypes {
		instanceType := &nodeClass.Spec.InstanceTypes[i]
		if instanceType.Name == "" || instanceType.Zone == "" {
			return fmt.Errorf("instanceTypes[%d] requires name and zone", i)
		}

		if _, exists := names[instanceType.Name]; exists {
			return fmt.Errorf("instance type %q is duplicated", instanceType.Name)
		}

		names[instanceType.Name] = struct{}{}
		for _, resourceName := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourcePods} {
			if quantity, ok := instanceType.Capacity[resourceName]; !ok || quantity.IsZero() {
				return fmt.Errorf("instance type %q requires positive %s capacity", instanceType.Name, resourceName)
			}
		}

		if instanceType.GPU == nil {
			continue
		}

		if !nodeClass.Spec.ImagePolicy.RequireNVIDIA {
			return fmt.Errorf("instance type %q GPU capacity requires an NVIDIA image policy", instanceType.Name)
		}

		gpuQuantity := instanceType.Capacity[corev1.ResourceName("nvidia.com/gpu")]
		if gpuQuantity.Value() != int64(instanceType.GPU.Count) {
			return fmt.Errorf("instance type %q GPU capacity must equal policy count", instanceType.Name)
		}

		if strings.EqualFold(instanceType.GPU.Model, "GB200") {
			if !strings.EqualFold(instanceType.GPU.Vendor, "NVIDIA") ||
				instanceType.GPU.Count != 4 ||
				!instanceType.GPU.RequireFabric {
				return fmt.Errorf("instance type %q GB200 profile requires NVIDIA image policy, exactly 4 GPUs, and fabric health", instanceType.Name)
			}
		}
	}

	var machines machinav1alpha3.MachineList
	if err := r.List(ctx, &machines, client.MatchingLabels{
		machinav1alpha3.MachineCapacityDomainLabelKey: nodeClass.Spec.Domain,
	}); err != nil {
		return fmt.Errorf("list Machine inventory: %w", err)
	}

	return nil
}

func (r *NodeClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&karpenterv1alpha1.UnboundedNodeClass{}).
		Watches(&machinav1alpha3.MachineConfiguration{}, handler.EnqueueRequestsFromMapFunc(r.nodeClassesForConfiguration)).
		Watches(&machinav1alpha3.MachineConfigurationVersion{}, handler.EnqueueRequestsFromMapFunc(r.nodeClassesForConfiguration)).
		Named("unbounded-nodeclass").
		Complete(r)
}

func (r *NodeClassReconciler) nodeClassesForConfiguration(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var classes karpenterv1alpha1.UnboundedNodeClassList
	if err := r.List(ctx, &classes); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0)

	for i := range classes.Items {
		if classes.Items[i].Spec.ConfigurationRef.Name == configurationName(obj) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: classes.Items[i].Name},
			})
		}
	}

	return requests
}

func configurationName(obj client.Object) string {
	if version, ok := obj.(*machinav1alpha3.MachineConfigurationVersion); ok {
		return version.Labels[machinav1alpha3.MCVConfigurationLabelKey]
	}

	return obj.GetName()
}
