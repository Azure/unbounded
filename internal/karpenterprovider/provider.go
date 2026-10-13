// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package karpenterprovider

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpenterv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	karpenterresources "sigs.k8s.io/karpenter/pkg/utils/resources"

	karpenterv1alpha1 "github.com/Azure/unbounded/api/karpenter/v1alpha1"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineconfigs"
)

const (
	Name = "unbounded"

	providerIDPrefix = "unbounded://"
)

var _ cloudprovider.CloudProvider = (*Provider)(nil)

type Provider struct {
	client    client.Client
	locker    DomainLocker
	lifecycle Lifecycle
}

func New(kubeClient client.Client, locker DomainLocker, lifecycle Lifecycle) *Provider {
	if locker == nil {
		locker = &MemoryDomainLocker{}
	}

	if lifecycle == nil {
		lifecycle = UnavailableLifecycle{}
	}

	return &Provider{client: kubeClient, locker: locker, lifecycle: lifecycle}
}

func (p *Provider) Name() string {
	return Name
}

func (p *Provider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&karpenterv1alpha1.UnboundedNodeClass{}}
}

func (p *Provider) RepairPolicies() []cloudprovider.RepairPolicy {
	return nil
}

func (p *Provider) Reboot(context.Context, *karpenterv1.NodeClaim, string) error {
	return cloudprovider.NewNodeRebootNotImplementedError()
}

func (p *Provider) GetInstanceTypes(
	ctx context.Context,
	nodePool *karpenterv1.NodePool,
) ([]*cloudprovider.InstanceType, error) {
	nodeClass, err := p.resolveNodeClass(ctx, nodePool.Spec.Template.Spec.NodeClassRef)
	if err != nil {
		return nil, err
	}

	freeByType, err := p.freeCapacity(ctx, nodeClass)
	if err != nil {
		return nil, err
	}

	instanceTypes := make([]*cloudprovider.InstanceType, 0, len(nodeClass.Spec.InstanceTypes))
	for i := range nodeClass.Spec.InstanceTypes {
		instanceTypes = append(instanceTypes, toInstanceType(
			&nodeClass.Spec.InstanceTypes[i],
			freeByType[nodeClass.Spec.InstanceTypes[i].Name],
			nodeClass.Spec.Requirements,
		))
	}

	return instanceTypes, nil
}

func toInstanceType(
	spec *karpenterv1alpha1.InstanceType,
	free int,
	additional []corev1.NodeSelectorRequirement,
) *cloudprovider.InstanceType {
	architecture := spec.Architecture
	if architecture == "" {
		architecture = "amd64"
	}

	operatingSystem := spec.OperatingSystem
	if operatingSystem == "" {
		operatingSystem = string(corev1.Linux)
	}

	capacityType := spec.CapacityType
	if capacityType == "" {
		capacityType = karpenterv1.CapacityTypeOnDemand
	}

	requirements := scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, spec.Name),
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, architecture),
		scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, operatingSystem),
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, spec.Zone),
		scheduling.NewRequirement(karpenterv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, capacityType),
	)
	for _, requirement := range additional {
		requirements.Add(scheduling.NewRequirement(requirement.Key, requirement.Operator, requirement.Values...))
	}

	offeringRequirements := scheduling.NewRequirements(
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, spec.Zone),
		scheduling.NewRequirement(karpenterv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, capacityType),
	)

	return &cloudprovider.InstanceType{
		Name:         spec.Name,
		Requirements: requirements,
		Offerings: cloudprovider.Offerings{{
			Requirements:        offeringRequirements,
			Available:           free > 0,
			ReservationCapacity: free,
			Price:               0,
		}},
		Capacity: spec.Capacity.DeepCopy(),
		Overhead: &cloudprovider.InstanceTypeOverhead{},
	}
}

func (p *Provider) Create(ctx context.Context, nodeClaim *karpenterv1.NodeClaim) (*karpenterv1.NodeClaim, error) {
	if nodeClaim.UID == "" {
		return nil, errors.New("NodeClaim UID is required")
	}

	nodeClass, err := p.resolveNodeClass(ctx, nodeClaim.Spec.NodeClassRef)
	if err != nil {
		return nil, err
	}

	ready := nodeClass.StatusConditions().Get(status.ConditionReady)
	if !ready.IsTrue() {
		return nil, cloudprovider.NewNodeClassNotReadyError(fmt.Errorf("%s", ready.Message))
	}

	configVersion, err := machineconfigs.ResolveVersionFromRef(ctx, p.client, &nodeClass.Spec.ConfigurationRef)
	if err != nil {
		return nil, fmt.Errorf("resolve MachineConfiguration: %w", err)
	}

	if configVersion.Spec.Template.Agent == nil || configVersion.Spec.Template.Agent.Image != nodeClass.Spec.ImagePolicy.RequiredImage {
		return nil, cloudprovider.NewNodeClassNotReadyError(errors.New("configured worker image does not match NodeClass requiredImage"))
	}

	instanceSpec, err := selectInstanceType(nodeClass, nodeClaim)
	if err != nil {
		return nil, err
	}

	if owned, err := p.findReservation(ctx, nodeClaim.Name, nodeClaim.UID); err != nil {
		return nil, err
	} else if owned != nil {
		return p.finishCreate(ctx, nodeClaim, nodeClass, instanceSpec, owned)
	}

	machines, err := p.matchingMachines(ctx, nodeClass, instanceSpec)
	if err != nil {
		return nil, err
	}

	for i := range machines {
		machine := &machines[i]
		if !machineEligible(machine) {
			continue
		}

		providerID, err := StableProviderID(machine)
		if err != nil {
			continue
		}

		domain := machine.Labels[machinav1alpha3.MachineCapacityDomainLabelKey]
		if domain == "" {
			continue
		}

		var reserved *machinav1alpha3.Machine

		err = p.locker.WithLock(ctx, domain, func(lockCtx context.Context) error {
			return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				var current machinav1alpha3.Machine
				if err := p.client.Get(lockCtx, types.NamespacedName{Name: machine.Name}, &current); err != nil {
					return err
				}

				if !machineEligible(&current) {
					return nil
				}

				current.Status.Reservation = &machinav1alpha3.MachineReservationStatus{
					NodeClaimName:        nodeClaim.Name,
					NodeClaimUID:         nodeClaim.UID,
					ProviderID:           providerID,
					Domain:               domain,
					NodeClassName:        nodeClass.Name,
					NodeClassGeneration:  nodeClass.Generation,
					InstanceType:         instanceSpec.Name,
					ConfigurationVersion: configVersion.Spec.Version,
					ImageID:              nodeClass.Spec.ImagePolicy.RequiredImage,
					ReservedAt:           metav1.Now(),
				}
				setReservedConditions(&current, nodeClaim.Name)

				if err := p.client.Status().Update(lockCtx, &current); err != nil {
					return err
				}

				reserved = current.DeepCopy()

				return nil
			})
		})
		if err != nil {
			if errors.Is(err, ErrDomainLocked) {
				continue
			}

			return nil, fmt.Errorf("reserve Machine %s: %w", machine.Name, err)
		}

		if reserved == nil {
			continue
		}

		if err := p.configureReservedMachine(ctx, reserved.Name, nodeClass, nodeClaim, providerID); err != nil {
			return nil, err
		}

		return p.finishCreate(ctx, nodeClaim, nodeClass, instanceSpec, reserved)
	}

	return nil, cloudprovider.NewInsufficientCapacityError(errors.New("no available Machine satisfies the NodeClass and NodeClaim requirements"))
}

func (p *Provider) finishCreate(
	ctx context.Context,
	nodeClaim *karpenterv1.NodeClaim,
	nodeClass *karpenterv1alpha1.UnboundedNodeClass,
	instanceSpec *karpenterv1alpha1.InstanceType,
	machine *machinav1alpha3.Machine,
) (*karpenterv1.NodeClaim, error) {
	ready, reason := machineReady(machine, nodeClass, instanceSpec)
	if !ready {
		if err := p.lifecycle.ProvisionWorker(ctx, machine); err != nil {
			return nil, fmt.Errorf("provision reserved Machine %s: %w", machine.Name, err)
		}

		return nil, fmt.Errorf("reserved Machine %s is not allocation ready: %s", machine.Name, reason)
	}

	return hydrateNodeClaim(nodeClaim, nodeClass, instanceSpec, machine), nil
}

func (p *Provider) configureReservedMachine(
	ctx context.Context,
	machineName string,
	nodeClass *karpenterv1alpha1.UnboundedNodeClass,
	nodeClaim *karpenterv1.NodeClaim,
	providerID string,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var machine machinav1alpha3.Machine
		if err := p.client.Get(ctx, types.NamespacedName{Name: machineName}, &machine); err != nil {
			return err
		}

		if machine.Status.Reservation == nil || machine.Status.Reservation.NodeClaimUID != nodeClaim.UID {
			return errors.New("machine reservation changed before configuration")
		}

		machine.Spec.ConfigurationRef = nodeClass.Spec.ConfigurationRef.DeepCopy()
		if machine.Spec.Kubernetes == nil {
			machine.Spec.Kubernetes = &machinav1alpha3.KubernetesSpec{}
		}

		machine.Spec.Kubernetes.ProviderID = providerID
		if machine.Spec.Kubernetes.NodeLabels == nil {
			machine.Spec.Kubernetes.NodeLabels = map[string]string{}
		}

		maps.Copy(machine.Spec.Kubernetes.NodeLabels, nodeClass.Spec.Labels)

		for key, value := range resolvedLabels(nodeClaim.Spec.Requirements) {
			machine.Spec.Kubernetes.NodeLabels[key] = value
		}

		return p.client.Update(ctx, &machine)
	})
}

func (p *Provider) Get(ctx context.Context, providerID string) (*karpenterv1.NodeClaim, error) {
	machine, err := p.findByProviderID(ctx, providerID)
	if err != nil {
		return nil, err
	}

	if machine == nil || machine.Status.Reservation == nil {
		return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("provider ID %q has no active reservation", providerID))
	}

	return p.reconstruct(ctx, machine)
}

func (p *Provider) List(ctx context.Context) ([]*karpenterv1.NodeClaim, error) {
	var machines machinav1alpha3.MachineList
	if err := p.client.List(ctx, &machines); err != nil {
		return nil, fmt.Errorf("list Machines: %w", err)
	}

	claims := make([]*karpenterv1.NodeClaim, 0)

	for i := range machines.Items {
		if machines.Items[i].Status.Reservation == nil {
			continue
		}

		claim, err := p.reconstruct(ctx, &machines.Items[i])
		if err != nil {
			return nil, err
		}

		claims = append(claims, claim)
	}

	return claims, nil
}

func (p *Provider) reconstruct(ctx context.Context, machine *machinav1alpha3.Machine) (*karpenterv1.NodeClaim, error) {
	reservation := machine.Status.Reservation

	var nodeClass karpenterv1alpha1.UnboundedNodeClass
	if err := p.client.Get(ctx, types.NamespacedName{Name: reservation.NodeClassName}, &nodeClass); err != nil {
		return nil, fmt.Errorf("get NodeClass %s: %w", reservation.NodeClassName, err)
	}

	instanceSpec := findInstanceType(&nodeClass, reservation.InstanceType)
	if instanceSpec == nil {
		return nil, fmt.Errorf("instance type %s no longer exists on NodeClass %s", reservation.InstanceType, nodeClass.Name)
	}

	claim := &karpenterv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: reservation.NodeClaimName, UID: reservation.NodeClaimUID},
		Spec: karpenterv1.NodeClaimSpec{
			NodeClassRef: &karpenterv1.NodeClassReference{
				Group: karpenterv1alpha1.GroupVersion.Group,
				Kind:  karpenterv1alpha1.KindUnboundedNodeClass,
				Name:  nodeClass.Name,
			},
		},
	}

	return hydrateNodeClaim(claim, &nodeClass, instanceSpec, machine), nil
}

func (p *Provider) Delete(ctx context.Context, nodeClaim *karpenterv1.NodeClaim) error {
	machine, err := p.findReservation(ctx, nodeClaim.Name, nodeClaim.UID)
	if err != nil {
		return err
	}

	if machine == nil {
		return cloudprovider.NewNodeClaimNotFoundError(errors.New("machine reservation was released"))
	}

	reservation := machine.Status.Reservation

	domain := reservation.Domain
	if domain == "" {
		return errors.New("reserved Machine has no durable capacity domain")
	}

	return p.locker.WithLock(ctx, domain, func(lockCtx context.Context) error {
		var current machinav1alpha3.Machine
		if err := p.client.Get(lockCtx, types.NamespacedName{Name: machine.Name}, &current); err != nil {
			return err
		}

		if current.Status.Reservation == nil {
			return nil
		}

		if current.Status.Reservation.NodeClaimUID != nodeClaim.UID {
			return fmt.Errorf("machine is reserved by NodeClaim %s", current.Status.Reservation.NodeClaimName)
		}

		stage := current.Status.Reservation.ReleaseStage
		if stage == "" {
			stage = machinav1alpha3.MachineReleaseStageRevokeCredentials
			if err := p.setReleaseStage(lockCtx, current.Name, nodeClaim.UID, stage); err != nil {
				return err
			}
		}

		switch stage {
		case machinav1alpha3.MachineReleaseStageRevokeCredentials:
			if err := p.lifecycle.RevokeCredentials(lockCtx, &current); err != nil {
				return err
			}

			return p.setReleaseStage(lockCtx, current.Name, nodeClaim.UID, machinav1alpha3.MachineReleaseStageResetWorker)
		case machinav1alpha3.MachineReleaseStageResetWorker:
			if err := p.lifecycle.ResetWorker(lockCtx, &current); err != nil {
				return err
			}

			if err := p.clearAllocation(lockCtx, current.Name); err != nil {
				return err
			}

			return p.setReleaseStage(lockCtx, current.Name, nodeClaim.UID, machinav1alpha3.MachineReleaseStageDeleteNode)
		case machinav1alpha3.MachineReleaseStageDeleteNode:
			if err := p.lifecycle.DeleteNode(lockCtx, &current); err != nil {
				return err
			}

			if err := p.clearAllocation(lockCtx, current.Name); err != nil {
				return err
			}

			return p.setReleaseStage(lockCtx, current.Name, nodeClaim.UID, machinav1alpha3.MachineReleaseStageReleaseReservation)
		case machinav1alpha3.MachineReleaseStageReleaseReservation:
			return p.clearReservation(lockCtx, current.Name, nodeClaim.UID)
		default:
			return fmt.Errorf("unknown release stage %q", stage)
		}
	})
}

func (p *Provider) IsDrifted(ctx context.Context, nodeClaim *karpenterv1.NodeClaim) (cloudprovider.DriftReason, error) {
	machine, err := p.findReservation(ctx, nodeClaim.Name, nodeClaim.UID)
	if err != nil {
		return "", err
	}

	if machine == nil {
		return "", cloudprovider.NewNodeClaimNotFoundError(errors.New("machine reservation was released"))
	}

	reservation := machine.Status.Reservation

	var nodeClass karpenterv1alpha1.UnboundedNodeClass
	if err := p.client.Get(ctx, types.NamespacedName{Name: reservation.NodeClassName}, &nodeClass); err != nil {
		if apierrors.IsNotFound(err) {
			return cloudprovider.DriftReason("NodeClassNotFound"), nil
		}

		return "", err
	}

	if nodeClass.Generation != reservation.NodeClassGeneration {
		return cloudprovider.DriftReason("NodeClassChanged"), nil
	}

	configVersion, err := machineconfigs.ResolveVersionFromRef(ctx, p.client, &nodeClass.Spec.ConfigurationRef)
	if err != nil {
		return cloudprovider.DriftReason("ConfigurationUnavailable"), nil
	}

	if configVersion.Spec.Version != reservation.ConfigurationVersion {
		return cloudprovider.DriftReason("ConfigurationChanged"), nil
	}

	if configVersion.Spec.Template.Agent == nil || configVersion.Spec.Template.Agent.Image != reservation.ImageID {
		return cloudprovider.DriftReason("ImageChanged"), nil
	}

	return "", nil
}

func (p *Provider) resolveNodeClass(ctx context.Context, ref *karpenterv1.NodeClassReference) (*karpenterv1alpha1.UnboundedNodeClass, error) {
	if ref == nil || ref.Group != karpenterv1alpha1.GroupVersion.Group || ref.Kind != karpenterv1alpha1.KindUnboundedNodeClass {
		return nil, fmt.Errorf("unsupported NodeClass reference")
	}

	var nodeClass karpenterv1alpha1.UnboundedNodeClass
	if err := p.client.Get(ctx, types.NamespacedName{Name: ref.Name}, &nodeClass); err != nil {
		return nil, err
	}

	return &nodeClass, nil
}

func selectInstanceType(
	nodeClass *karpenterv1alpha1.UnboundedNodeClass,
	nodeClaim *karpenterv1.NodeClaim,
) (*karpenterv1alpha1.InstanceType, error) {
	claimRequirements := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	for i := range nodeClass.Spec.InstanceTypes {
		instanceType := toInstanceType(&nodeClass.Spec.InstanceTypes[i], 1, nodeClass.Spec.Requirements)
		if err := instanceType.Requirements.Compatible(claimRequirements); err != nil {
			continue
		}

		if !karpenterresources.Fits(nodeClaim.Spec.Resources.Requests, instanceType.Allocatable()) {
			continue
		}

		return &nodeClass.Spec.InstanceTypes[i], nil
	}

	return nil, cloudprovider.NewInsufficientCapacityError(errors.New("no NodeClass instance type satisfies the NodeClaim"))
}

func (p *Provider) matchingMachines(
	ctx context.Context,
	nodeClass *karpenterv1alpha1.UnboundedNodeClass,
	instanceSpec *karpenterv1alpha1.InstanceType,
) ([]machinav1alpha3.Machine, error) {
	selector, err := metav1.LabelSelectorAsSelector(&nodeClass.Spec.MachineSelector)
	if err != nil {
		return nil, fmt.Errorf("parse Machine selector: %w", err)
	}

	var list machinav1alpha3.MachineList
	if err := p.client.List(ctx, &list); err != nil {
		return nil, err
	}

	matches := make([]machinav1alpha3.Machine, 0)

	for i := range list.Items {
		machine := &list.Items[i]
		if !selector.Matches(labels.Set(machine.Labels)) {
			continue
		}

		if nodeClass.Spec.Site != "" && machine.Labels[machinav1alpha3.MachineSiteLabelKey] != nodeClass.Spec.Site {
			continue
		}

		if nodeClass.Spec.Domain != "" && machine.Labels[machinav1alpha3.MachineCapacityDomainLabelKey] != nodeClass.Spec.Domain {
			continue
		}

		if instanceSpec.GPU != nil &&
			(!strings.EqualFold(machine.Labels[machinav1alpha3.MachineGPUVendorLabelKey], instanceSpec.GPU.Vendor) ||
				!strings.EqualFold(machine.Labels[machinav1alpha3.MachineGPUModelLabelKey], instanceSpec.GPU.Model)) {
			continue
		}

		matches = append(matches, *machine.DeepCopy())
	}

	sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })

	return matches, nil
}

func (p *Provider) freeCapacity(
	ctx context.Context,
	nodeClass *karpenterv1alpha1.UnboundedNodeClass,
) (map[string]int, error) {
	result := map[string]int{}

	for i := range nodeClass.Spec.InstanceTypes {
		machines, err := p.matchingMachines(ctx, nodeClass, &nodeClass.Spec.InstanceTypes[i])
		if err != nil {
			return nil, err
		}

		for j := range machines {
			if machineEligible(&machines[j]) {
				result[nodeClass.Spec.InstanceTypes[i].Name]++
			}
		}
	}

	return result, nil
}

func machineEligible(machine *machinav1alpha3.Machine) bool {
	return machine.Status.Reservation == nil &&
		apimeta.IsStatusConditionTrue(machine.Status.Conditions, machinav1alpha3.MachineConditionAvailable) &&
		!apimeta.IsStatusConditionTrue(machine.Status.Conditions, machinav1alpha3.MachineConditionQuarantined)
}

func machineReady(
	machine *machinav1alpha3.Machine,
	nodeClass *karpenterv1alpha1.UnboundedNodeClass,
	instanceSpec *karpenterv1alpha1.InstanceType,
) (bool, string) {
	if machine.Status.Allocation == nil || machine.Status.Allocation.Node == nil {
		return false, "Node observation is missing"
	}

	node := machine.Status.Allocation.Node
	if !node.Ready {
		return false, "Node is not Ready"
	}

	if node.ProviderID != machine.Status.Reservation.ProviderID {
		return false, "Node providerID does not match the Machine reservation"
	}

	if node.Image != nodeClass.Spec.ImagePolicy.RequiredImage {
		return false, "observed image does not match the required image"
	}

	if instanceSpec.GPU == nil {
		return true, ""
	}

	if !nodeClass.Spec.ImagePolicy.RequireNVIDIA {
		return false, "GPU capacity requires an NVIDIA-enabled image policy"
	}

	gpu := machine.Status.Allocation.GPU
	if gpu == nil {
		return false, "GPU observation is missing"
	}

	if !strings.EqualFold(gpu.Vendor, instanceSpec.GPU.Vendor) ||
		!strings.EqualFold(gpu.Model, instanceSpec.GPU.Model) ||
		gpu.Count != instanceSpec.GPU.Count ||
		gpu.HealthyCount != instanceSpec.GPU.Count {
		return false, "GPU identity, total, or allocatable health does not match"
	}

	if instanceSpec.GPU.RequireFabric && !gpu.FabricHealthy {
		return false, "required GPU fabric is unhealthy"
	}

	return true, ""
}

func hydrateNodeClaim(
	nodeClaim *karpenterv1.NodeClaim,
	nodeClass *karpenterv1alpha1.UnboundedNodeClass,
	instanceSpec *karpenterv1alpha1.InstanceType,
	machine *machinav1alpha3.Machine,
) *karpenterv1.NodeClaim {
	out := nodeClaim.DeepCopy()
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}

	maps.Copy(out.Labels, nodeClass.Spec.Labels)
	maps.Copy(out.Labels, resolvedLabels(out.Spec.Requirements))
	out.Labels[corev1.LabelInstanceTypeStable] = instanceSpec.Name
	out.Labels[corev1.LabelArchStable] = defaultString(instanceSpec.Architecture, "amd64")
	out.Labels[corev1.LabelOSStable] = defaultString(instanceSpec.OperatingSystem, string(corev1.Linux))
	out.Labels[corev1.LabelTopologyZone] = instanceSpec.Zone
	out.Labels[karpenterv1.CapacityTypeLabelKey] = defaultString(instanceSpec.CapacityType, karpenterv1.CapacityTypeOnDemand)
	out.Labels[corev1.LabelHostname] = machine.Name
	out.Status.ProviderID = machine.Status.Reservation.ProviderID
	out.Status.ImageID = nodeClass.Spec.ImagePolicy.RequiredImage
	out.Status.Capacity = instanceSpec.Capacity.DeepCopy()

	out.Status.Allocatable = instanceSpec.Capacity.DeepCopy()
	if machine.Status.Allocation != nil && machine.Status.Allocation.Node != nil {
		out.Status.NodeName = machine.Status.Allocation.Node.Name
	}

	return out
}

func resolvedLabels(requirements []karpenterv1.NodeSelectorRequirementWithMinValues) map[string]string {
	result := map[string]string{}

	for _, requirement := range requirements {
		if requirement.Operator == corev1.NodeSelectorOpIn && len(requirement.Values) == 1 {
			result[requirement.Key] = requirement.Values[0]
		}
	}

	return result
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}

func setReservedConditions(machine *machinav1alpha3.Machine, nodeClaimName string) {
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               machinav1alpha3.MachineConditionReserved,
		Status:             metav1.ConditionTrue,
		Reason:             "NodeClaimReserved",
		Message:            fmt.Sprintf("Reserved by Karpenter NodeClaim %s", nodeClaimName),
		ObservedGeneration: machine.Generation,
	})
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               machinav1alpha3.MachineConditionAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             "Reserved",
		Message:            fmt.Sprintf("Reserved by Karpenter NodeClaim %s", nodeClaimName),
		ObservedGeneration: machine.Generation,
	})
}

func (p *Provider) findReservation(ctx context.Context, name string, uid types.UID) (*machinav1alpha3.Machine, error) {
	var machines machinav1alpha3.MachineList
	if err := p.client.List(ctx, &machines); err != nil {
		return nil, err
	}

	var matches []*machinav1alpha3.Machine

	for i := range machines.Items {
		reservation := machines.Items[i].Status.Reservation
		if reservation != nil && reservation.NodeClaimName == name && reservation.NodeClaimUID == uid {
			matches = append(matches, machines.Items[i].DeepCopy())
		}
	}

	if len(matches) > 1 {
		for _, machine := range matches {
			apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
				Type:               machinav1alpha3.MachineConditionQuarantined,
				Status:             metav1.ConditionTrue,
				Reason:             "DuplicateReservation",
				Message:            fmt.Sprintf("NodeClaim %s is reserved on multiple Machines", name),
				ObservedGeneration: machine.Generation,
			})

			if err := p.client.Status().Update(ctx, machine); err != nil {
				return nil, fmt.Errorf("quarantine duplicate reservation on Machine %s: %w", machine.Name, err)
			}
		}

		return nil, fmt.Errorf("NodeClaim %s is reserved on multiple Machines", name)
	}

	if len(matches) == 0 {
		return nil, nil
	}

	return matches[0], nil
}

func (p *Provider) findByProviderID(ctx context.Context, providerID string) (*machinav1alpha3.Machine, error) {
	var machines machinav1alpha3.MachineList
	if err := p.client.List(ctx, &machines); err != nil {
		return nil, err
	}

	for i := range machines.Items {
		if machines.Items[i].Status.Reservation != nil &&
			machines.Items[i].Status.Reservation.ProviderID == providerID {
			return machines.Items[i].DeepCopy(), nil
		}
	}

	return nil, nil
}

func (p *Provider) setReleaseStage(
	ctx context.Context,
	machineName string,
	uid types.UID,
	stage machinav1alpha3.MachineReservationReleaseStage,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var machine machinav1alpha3.Machine
		if err := p.client.Get(ctx, types.NamespacedName{Name: machineName}, &machine); err != nil {
			return err
		}

		if machine.Status.Reservation == nil || machine.Status.Reservation.NodeClaimUID != uid {
			return errors.New("machine reservation changed during release")
		}

		machine.Status.Reservation.ReleaseStage = stage

		return p.client.Status().Update(ctx, &machine)
	})
}

func (p *Provider) clearAllocation(ctx context.Context, machineName string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var machine machinav1alpha3.Machine
		if err := p.client.Get(ctx, types.NamespacedName{Name: machineName}, &machine); err != nil {
			return err
		}

		machine.Status.Allocation = nil
		apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:               machinav1alpha3.MachineConditionAllocationReady,
			Status:             metav1.ConditionFalse,
			Reason:             "WorkerReleased",
			Message:            "Worker allocation observations were cleared during provider release",
			ObservedGeneration: machine.Generation,
		})

		return p.client.Status().Update(ctx, &machine)
	})
}

func (p *Provider) clearReservation(ctx context.Context, machineName string, uid types.UID) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var machine machinav1alpha3.Machine
		if err := p.client.Get(ctx, types.NamespacedName{Name: machineName}, &machine); err != nil {
			return client.IgnoreNotFound(err)
		}

		if machine.Status.Reservation == nil {
			return nil
		}

		if machine.Status.Reservation.NodeClaimUID != uid {
			return fmt.Errorf("refusing to clear reservation owned by NodeClaim %s", machine.Status.Reservation.NodeClaimName)
		}

		nodeClaimName := machine.Status.Reservation.NodeClaimName
		machine.Status.Reservation = nil
		apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:               machinav1alpha3.MachineConditionReserved,
			Status:             metav1.ConditionFalse,
			Reason:             "Released",
			Message:            fmt.Sprintf("NodeClaim %s completed all provider release barriers", nodeClaimName),
			ObservedGeneration: machine.Generation,
		})
		available := !apimeta.IsStatusConditionTrue(machine.Status.Conditions, machinav1alpha3.MachineConditionQuarantined)
		apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:               machinav1alpha3.MachineConditionAvailable,
			Status:             metav1.ConditionStatus(map[bool]string{true: "True", false: "False"}[available]),
			Reason:             map[bool]string{true: "Released", false: "Quarantined"}[available],
			Message:            map[bool]string{true: "Machine inventory is available", false: "Machine remains quarantined"}[available],
			ObservedGeneration: machine.Generation,
		})

		return p.client.Status().Update(ctx, &machine)
	})
}

func findInstanceType(
	nodeClass *karpenterv1alpha1.UnboundedNodeClass,
	name string,
) *karpenterv1alpha1.InstanceType {
	for i := range nodeClass.Spec.InstanceTypes {
		if nodeClass.Spec.InstanceTypes[i].Name == name {
			return &nodeClass.Spec.InstanceTypes[i]
		}
	}

	return nil
}

// StableProviderID derives identity only from immutable host ownership fields.
// DHCP addresses, SSH endpoints, and mutable Rack-SCM server IDs are excluded.
func StableProviderID(machine *machinav1alpha3.Machine) (string, error) {
	var identity string

	switch {
	case machine.Spec.Host != nil && machine.Spec.Host.Azure != nil:
		identity = "azure:" + strings.ToLower(machine.Spec.Host.Azure.ResourceID)
	case machine.Spec.Host != nil && machine.Spec.Host.External != nil && machine.Spec.Host.External.MachineRef != nil:
		ref := machine.Spec.Host.External.MachineRef
		identity = strings.Join([]string{"external", machine.Spec.Host.External.Provider, ref.APIGroup, ref.Kind, ref.Name}, ":")
	case machine.Spec.Host != nil && machine.Spec.Host.External != nil && machine.Spec.Host.External.ProviderID != "":
		identity = "external:" + machine.Spec.Host.External.Provider + ":" + machine.Spec.Host.External.ProviderID
	case machine.Spec.ProviderID != "":
		identity = "legacy:" + machine.Spec.Provider + ":" + machine.Spec.ProviderID
	default:
		netboot := machine.Spec.Netboot()
		if netboot != nil {
			macs := make([]string, 0, len(netboot.DHCPLeases))
			for _, lease := range netboot.DHCPLeases {
				if lease.MAC != "" {
					macs = append(macs, strings.ToLower(lease.MAC))
				}
			}

			sort.Strings(macs)

			if len(macs) > 0 {
				identity = "netboot-mac:" + strings.Join(macs, ",")
			}
		}
	}

	if identity == "" {
		return "", errors.New("machine has no immutable host ownership identity")
	}

	sum := sha256.Sum256([]byte(identity))

	return fmt.Sprintf("%s%x", providerIDPrefix, sum[:]), nil
}
