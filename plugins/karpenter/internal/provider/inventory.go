// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/awslabs/operatorpkg/status"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
	"github.com/Azure/unbounded/plugins/karpenter/internal/nodeclass"
)

const (
	allocationUIDIndex = "unbounded.allocation.uid"

	// ConsumerIndex indexes allocations by their consumer group and kind.
	ConsumerIndex = "unbounded.allocation.consumer"

	// KarpenterConsumer is the ConsumerIndex value of this plugin's
	// allocations.
	KarpenterConsumer = capacity.ConsumerGroup + "/" + capacity.ConsumerKind

	// DriftClassReplaced reports that the allocation's class is gone or was
	// recreated.
	DriftClassReplaced cloudprovider.DriftReason = "MachineClassReplaced"

	// DriftClassRevision reports that the class resolved a launch
	// configuration other than the one frozen in the allocation.
	DriftClassRevision cloudprovider.DriftReason = "MachineClassDrifted"

	defaultOS = "linux"
)

// RegisterIndexes adds the allocation indexes the provider reads through.
func RegisterIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, AllocationObject(), allocationUIDIndex, func(object client.Object) []string {
		return []string{string(object.GetUID())}
	}); err != nil {
		return fmt.Errorf("index allocation UID: %w", err)
	}

	if err := indexer.IndexField(ctx, AllocationObject(), ConsumerIndex, indexConsumer); err != nil {
		return fmt.Errorf("index allocation consumer: %w", err)
	}

	return nil
}

func indexConsumer(object client.Object) []string {
	allocation, ok := object.(*unstructured.Unstructured)
	if !ok {
		return nil
	}

	group, _, err := unstructured.NestedString(allocation.Object, "spec", "consumerRef", "apiGroup")
	if err != nil {
		return nil
	}

	kind, _, err := unstructured.NestedString(allocation.Object, "spec", "consumerRef", "kind")
	if err != nil || group == "" || kind == "" {
		return nil
	}

	return []string{group + "/" + kind}
}

func (p *Provider) allocationForClaim(ctx context.Context, claim *v1.NodeClaim) (*unstructured.Unstructured, error) {
	name := capacity.AllocationName(claim.UID)

	object := AllocationObject()
	if err := p.cache.Get(ctx, client.ObjectKey{Name: name}, object); err != nil {
		if apierrors.IsNotFound(err) {
			return p.readAllocation(ctx, name)
		}

		return nil, fmt.Errorf("get cached allocation: %w", err)
	}

	return object, nil
}

// readAllocation reads the allocation from the API server. It returns nil
// when the allocation does not exist.
func (p *Provider) readAllocation(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	object := AllocationObject()
	if err := p.reader.Get(ctx, client.ObjectKey{Name: name}, object); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("confirm allocation %s: %w", name, err)
	}

	return object, nil
}

// Get returns the NodeClaim for the allocation with the provider ID. It
// reports not found only when nothing remote can exist: the allocation is
// absent, never got a snapshot, or its release is confirmed. A failed or
// fenced allocation with a snapshot is still returned, because a host may
// exist until release is confirmed.
func (p *Provider) Get(ctx context.Context, providerID string) (*v1.NodeClaim, error) {
	uid, err := allocationUID(providerID)
	if err != nil {
		return nil, err
	}

	list := allocationList()
	if err := p.cache.List(ctx, list, client.MatchingFields{allocationUIDIndex: uid}); err != nil {
		return nil, fmt.Errorf("look up allocation UID: %w", err)
	}

	if len(list.Items) == 0 {
		// A cache miss is not evidence of absence. Confirm using the controller's UID label.
		if err := p.reader.List(
			ctx,
			list,
			client.MatchingLabels{capacity.AllocationLabel: uid},
			client.Limit(2),
		); err != nil {
			return nil, fmt.Errorf("confirm allocation UID lookup: %w", err)
		}
	}

	if len(list.Items) == 0 {
		return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("allocation UID %s is absent", uid))
	}

	if len(list.Items) != 1 || string(list.Items[0].GetUID()) != uid {
		return nil, fmt.Errorf("allocation UID %s has conflicting identity bindings", uid)
	}

	allocation, err := decodeAllocation(&list.Items[0])
	if err != nil {
		return nil, err
	}

	if allocation.Status.Snapshot == nil {
		return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("allocation %s has no snapshot", allocation.Name))
	}

	if confirmedReleased(allocation) {
		return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("allocation %s is released", allocation.Name))
	}

	return inventoryClaim(allocation)
}

// List returns a NodeClaim for every allocation this plugin made that has a
// snapshot and whose release is not confirmed. One allocation it cannot
// describe fully never fails the call or hides the others.
func (p *Provider) List(ctx context.Context) ([]*v1.NodeClaim, error) {
	list := allocationList()
	if err := p.cache.List(ctx, list, client.MatchingFields{ConsumerIndex: KarpenterConsumer}); err != nil {
		return nil, fmt.Errorf("list cached allocations: %w", err)
	}

	claims := make([]*v1.NodeClaim, 0, len(list.Items))

	for index := range list.Items {
		allocation, err := decodeAllocation(&list.Items[index])
		if err != nil {
			return nil, err
		}

		if allocation.Status.Snapshot == nil || confirmedReleased(allocation) {
			continue
		}

		claim, err := inventoryClaim(allocation)
		if err != nil {
			// Not an allocation this plugin made: no NodeClaim has its
			// provider ID, and the lifecycle controller ignores it too.
			log.FromContext(ctx).V(1).Info("ignoring allocation without verified NodeClaim ownership",
				"MachineAllocation", allocation.Name)

			continue
		}

		claims = append(claims, claim)
	}

	return claims, nil
}

// Delete authorizes release of the NodeClaim's allocation and reports not
// found only once release is confirmed or the allocation is gone. Writing
// release intent is not confirmed absence.
func (p *Provider) Delete(ctx context.Context, claim *v1.NodeClaim) error {
	if claim.UID == "" || !supportedClass(claim.Spec.NodeClassRef) {
		return fmt.Errorf("cannot release a NodeClaim without its UID and supported MachineClass")
	}

	object, err := p.readAllocation(ctx, capacity.AllocationName(claim.UID))
	if err != nil {
		return err
	}

	if object == nil {
		return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("allocation is absent"))
	}

	allocation, err := decodeAllocation(object)
	if err != nil {
		return err
	}

	if err := ownsAllocation(claim, allocation); err != nil {
		return err
	}

	if claim.Status.ProviderID != "" && allocation.Status.ProviderID != "" &&
		claim.Status.ProviderID != allocation.Status.ProviderID {
		return fmt.Errorf("NodeClaim and allocation provider IDs do not match")
	}

	if confirmedReleased(allocation) {
		if allocation.DeletionTimestamp.IsZero() {
			if err := p.writer.Delete(ctx, object, client.Preconditions{
				UID: &allocation.UID,
			}); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("remove released allocation: %w", err)
			}
		}

		return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("allocation release is confirmed"))
	}

	return AuthorizeRelease(ctx, p.writer, object)
}

// AuthorizeRelease sets spec.lifecycle to Released on the allocation, unless
// it already is. The write carries the resource version read.
func AuthorizeRelease(ctx context.Context, writer client.Client, object *unstructured.Unstructured) error {
	lifecycle, _, err := unstructured.NestedString(object.Object, "spec", "lifecycle")
	if err != nil {
		return fmt.Errorf("read allocation lifecycle: %w", err)
	}

	if lifecycle == capacity.Released {
		return nil
	}

	before := object.DeepCopy()
	if err := unstructured.SetNestedField(object.Object, capacity.Released, "spec", "lifecycle"); err != nil {
		return fmt.Errorf("set release intent: %w", err)
	}

	if err := writer.Patch(ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("persist release intent: %w", err)
	}

	return nil
}

// IsDrifted compares the allocation's frozen class identity and revision
// with the class as it is now. An unresolved class is not drift: the answer
// is not known yet.
func (p *Provider) IsDrifted(ctx context.Context, claim *v1.NodeClaim) (cloudprovider.DriftReason, error) {
	if claim.UID == "" || !supportedClass(claim.Spec.NodeClassRef) {
		return "", fmt.Errorf("cannot observe drift without a NodeClaim UID and supported MachineClass")
	}

	object, err := p.allocationForClaim(ctx, claim)
	if err != nil {
		return "", err
	}

	if object == nil {
		return "", fmt.Errorf("allocation is absent; drift cannot be observed")
	}

	allocation, err := decodeAllocation(object)
	if err != nil {
		return "", err
	}

	if err := ownsAllocation(claim, allocation); err != nil {
		return "", err
	}

	snapshot := allocation.Status.Snapshot
	if snapshot == nil {
		return "", nil
	}

	class := new(nodeclass.MachineClass)
	if err := p.cache.Get(ctx, client.ObjectKey{Name: allocation.Spec.ClassRef.Name}, class); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("get MachineClass %s: %w", allocation.Spec.ClassRef.Name, err)
		}

		// Confirm the miss before calling the class replaced.
		if err := p.reader.Get(ctx, client.ObjectKey{Name: allocation.Spec.ClassRef.Name}, class); err != nil {
			if apierrors.IsNotFound(err) {
				return DriftClassReplaced, nil
			}

			return "", fmt.Errorf("confirm MachineClass %s: %w", allocation.Spec.ClassRef.Name, err)
		}
	}

	if class.UID != snapshot.ClassUID {
		return DriftClassReplaced, nil
	}

	if !class.StatusConditions().Get(status.ConditionReady).IsTrue() || class.Status.ResolvedRevision == "" {
		return "", nil
	}

	if class.Status.ResolvedRevision != snapshot.ClassRevision {
		return DriftClassRevision, nil
	}

	return "", nil
}

// ownedByNodeClaim reports an error unless the allocation is one this plugin
// made for a NodeClaim: its consumer is a NodeClaim, it is named from that
// NodeClaim's UID, and it names a class.
func ownedByNodeClaim(allocation *capacity.Allocation) error {
	consumer := allocation.Spec.ConsumerRef
	if consumer.APIGroup != capacity.ConsumerGroup || consumer.Kind != capacity.ConsumerKind ||
		consumer.Name == "" || consumer.UID == "" ||
		allocation.Name != capacity.AllocationName(consumer.UID) ||
		allocation.Spec.ClassRef.Name == "" || allocation.Spec.ClassRef.UID == "" {
		return fmt.Errorf("allocation %s has no verified NodeClaim ownership", allocation.Name)
	}

	if _, err := allocationUID(capacity.ProviderIDPrefix + string(allocation.UID)); err != nil {
		return err
	}

	return nil
}

// hydrate builds the NodeClaim Karpenter records at launch for an allocation
// with a snapshot. It is strict: Karpenter schedules against what it returns,
// so anything missing or contradictory is an error.
func hydrate(allocation *capacity.Allocation) (*v1.NodeClaim, error) {
	if err := ownedByNodeClaim(allocation); err != nil {
		return nil, err
	}

	expectedID := capacity.ProviderIDPrefix + string(allocation.UID)

	observed := allocation.Status
	if observed.Snapshot == nil || observed.ProviderID != expectedID {
		return nil, fmt.Errorf("allocation %s has no snapshot with its own provider ID", allocation.Name)
	}

	capacityList, allocatableList, err := reportedResources(observed.Capacity, observed.Allocatable)
	if err != nil {
		return nil, fmt.Errorf("allocation %s: %w", allocation.Name, err)
	}

	purchaseModel, err := capacityType(observed.CapacityType)
	if err != nil {
		return nil, err
	}

	labels := maps.Clone(observed.Labels)
	if labels == nil {
		labels = make(map[string]string)
	}

	if labels[corev1.LabelInstanceTypeStable] == "" || labels[corev1.LabelArchStable] == "" ||
		labels[capacity.CapacityTypeLabel] != observed.CapacityType {
		return nil, fmt.Errorf("allocation %s has incomplete or conflicting resolved labels", allocation.Name)
	}

	labels[v1.CapacityTypeLabelKey] = purchaseModel
	defaultLabels(labels, observed.Snapshot.SiteName)

	spec, err := restoredSpec(allocation)
	if err != nil {
		return nil, err
	}

	return nodeClaimFor(allocation, labels, spec, capacityList, allocatableList), nil
}

// inventoryClaim builds the NodeClaim that Get and List report for an
// allocation with a snapshot. Karpenter reads inventory to learn which
// provider IDs still exist, and deletes a registered NodeClaim whose provider
// ID is missing from it. So only ownership is required here: a description
// that hydrate would reject is reported as far as it is usable, and never
// hides an allocation that may have a host.
func inventoryClaim(allocation *capacity.Allocation) (*v1.NodeClaim, error) {
	if claim, err := hydrate(allocation); err == nil {
		return claim, nil
	}

	if err := ownedByNodeClaim(allocation); err != nil {
		return nil, err
	}

	observed := allocation.Status

	labels := maps.Clone(observed.Labels)
	if labels == nil {
		labels = make(map[string]string)
	}

	if purchaseModel, err := capacityType(observed.CapacityType); err == nil {
		labels[v1.CapacityTypeLabelKey] = purchaseModel
	}

	if observed.Snapshot != nil {
		defaultLabels(labels, observed.Snapshot.SiteName)
	}

	spec, err := restoredSpec(allocation)
	if err != nil {
		spec = v1.NodeClaimSpec{NodeClassRef: classReference(allocation)}
	}

	return nodeClaimFor(allocation, labels, spec, observed.Capacity.DeepCopy(), observed.Allocatable.DeepCopy()), nil
}

func nodeClaimFor(
	allocation *capacity.Allocation,
	labels map[string]string,
	spec v1.NodeClaimSpec,
	capacityList, allocatableList corev1.ResourceList,
) *v1.NodeClaim {
	return &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              allocation.Spec.ConsumerRef.Name,
			UID:               allocation.Spec.ConsumerRef.UID,
			Labels:            labels,
			CreationTimestamp: allocation.CreationTimestamp,
		},
		Spec: spec,
		Status: v1.NodeClaimStatus{
			ProviderID:  capacity.ProviderIDPrefix + string(allocation.UID),
			ImageID:     allocation.Status.Image,
			Capacity:    capacityList,
			Allocatable: allocatableList,
		},
	}
}

func classReference(allocation *capacity.Allocation) *v1.NodeClassReference {
	return &v1.NodeClassReference{
		Group: nodeclass.GroupVersion.Group,
		Kind:  "MachineClass",
		Name:  allocation.Spec.ClassRef.Name,
	}
}

// defaultLabels fills the labels Karpenter needs and an offering may leave
// out. A class has exactly one Site, so the Site name is the failure domain
// of an offering without a zone.
func defaultLabels(labels map[string]string, siteName string) {
	if labels[corev1.LabelTopologyZone] == "" && siteName != "" {
		labels[corev1.LabelTopologyZone] = siteName
	}

	if labels[corev1.LabelOSStable] == "" {
		labels[corev1.LabelOSStable] = defaultOS
	}
}

// reportedResources returns the capacity and allocatable to report for an
// allocation. The capacity is the offering's, published before a host
// exists; once the Node registers, the allocatable is the Node's own. A real
// Node can have more of a resource than its offering advertised, and may
// report resources the offering never mentioned. That is not an error: the
// observed value is the truth, so the reported capacity is raised to it.
// Failing here would fail List for every NodeClaim.
func reportedResources(capacity, allocatable corev1.ResourceList) (corev1.ResourceList, corev1.ResourceList, error) {
	for _, name := range []corev1.ResourceName{
		corev1.ResourceCPU,
		corev1.ResourceMemory,
		corev1.ResourcePods,
	} {
		total := capacity[name]

		available, found := allocatable[name]
		if total.Sign() <= 0 || !found || available.Sign() <= 0 {
			return nil, nil, fmt.Errorf("capacity or allocatable for %s is missing or nonpositive", name)
		}
	}

	reported := capacity.DeepCopy()

	for name, available := range allocatable {
		if available.Sign() < 0 {
			return nil, nil, fmt.Errorf("allocatable %s is negative", name)
		}

		// A missing capacity entry is zero, so an observed zero adds nothing.
		if total := reported[name]; available.Cmp(total) > 0 {
			reported[name] = available.DeepCopy()
		}
	}

	return reported, allocatable.DeepCopy(), nil
}

func allocationUID(providerID string) (string, error) {
	raw := strings.TrimPrefix(providerID, capacity.ProviderIDPrefix)

	parsed, err := uuid.Parse(raw)
	if !strings.HasPrefix(providerID, capacity.ProviderIDPrefix) || err != nil || parsed.String() != raw {
		return "", fmt.Errorf("invalid Unbounded allocation provider ID")
	}

	return raw, nil
}

func conditionTrue(allocation *capacity.Allocation, name string) bool {
	return apimeta.IsStatusConditionTrue(allocation.Status.Conditions, name)
}

func confirmedReleased(allocation *capacity.Allocation) bool {
	condition := apimeta.FindStatusCondition(allocation.Status.Conditions, capacity.ConditionReleased)

	return condition != nil &&
		condition.Status == metav1.ConditionTrue &&
		condition.ObservedGeneration == allocation.Generation
}

func decodeAllocation(object *unstructured.Unstructured) (*capacity.Allocation, error) {
	return DecodeAllocation(object)
}

// DecodeAllocation converts an unstructured allocation to its projection.
func DecodeAllocation(object *unstructured.Unstructured) (*capacity.Allocation, error) {
	allocation := new(capacity.Allocation)
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, allocation); err != nil {
		return nil, fmt.Errorf("decode allocation %s: %w", object.GetName(), err)
	}

	return allocation, nil
}

// AllocationObject returns an empty unstructured MachineAllocation.
func AllocationObject() *unstructured.Unstructured {
	object := new(unstructured.Unstructured)
	object.SetGroupVersionKind(capacity.AllocationGVK)

	return object
}

func allocationObject() *unstructured.Unstructured {
	return AllocationObject()
}

func allocationList() *unstructured.UnstructuredList {
	list := new(unstructured.UnstructuredList)
	list.SetGroupVersionKind(capacity.AllocationGVK.GroupVersion().WithKind("MachineAllocationList"))

	return list
}

var _ cloudprovider.CloudProvider = (*Provider)(nil)

func restoredSpec(allocation *capacity.Allocation) (v1.NodeClaimSpec, error) {
	requirements := make([]v1.NodeSelectorRequirementWithMinValues, 0, len(allocation.Spec.Requirements))

	for _, requirement := range allocation.Spec.Requirements {
		var minValues *int

		if requirement.MinValues != nil {
			value := int(*requirement.MinValues)
			minValues = &value
		}

		key := requirement.Key
		values := append([]string(nil), requirement.Values...)

		if key == capacity.CapacityTypeLabel {
			key = v1.CapacityTypeLabelKey

			for index, value := range values {
				model, err := capacityType(value)
				if err != nil {
					return v1.NodeClaimSpec{}, err
				}

				values[index] = model
			}
		}

		requirements = append(requirements, v1.NodeSelectorRequirementWithMinValues{
			Key:       key,
			Operator:  requirement.Operator,
			Values:    values,
			MinValues: minValues,
		})
	}

	return v1.NodeClaimSpec{
		NodeClassRef: classReference(allocation),
		Requirements: requirements,
		Resources: v1.ResourceRequirements{
			Requests: allocation.Spec.Resources.DeepCopy(),
		},
		Taints:        allocation.Spec.NodeTemplate.Taints,
		StartupTaints: allocation.Spec.NodeTemplate.StartupTaints,
	}, nil
}
