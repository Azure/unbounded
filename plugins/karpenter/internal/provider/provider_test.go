// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
	"github.com/Azure/unbounded/plugins/karpenter/internal/nodeclass"
)

const allocationTestUID = "46ed7c08-437e-45be-ae8b-a13f73d17128"

func TestCreatePersistsOneDurableAllocation(t *testing.T) {
	ctx := t.Context()
	provider, store := testProvider(t, testClass())
	claim := testClaim()
	claim.Labels["workload.example.com/tier"] = "batch"
	claim.Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "batch", Effect: corev1.TaintEffectNoSchedule}}
	claim.Spec.Requirements = append(
		claim.Spec.Requirements,
		v1.NodeSelectorRequirementWithMinValues{
			Key:      v1.NodePoolLabelKey,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{"workers"},
		},
		v1.NodeSelectorRequirementWithMinValues{
			Key:      "workload.example.com/tier",
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{"batch"},
		},
	)

	for range 3 {
		_, err := provider.Create(ctx, claim)

		var createError *cloudprovider.CreateError
		if !errors.As(err, &createError) || createError.ConditionReason != reasonAllocationPending {
			t.Fatalf("an allocation without a snapshot must be a retryable pending error, got %v", err)
		}
	}

	list := allocationList()
	must(t, store.List(ctx, list))

	if len(list.Items) != 1 {
		t.Fatalf("expected one durable allocation, got %d", len(list.Items))
	}

	object := &list.Items[0]
	allocation, err := decodeAllocation(object)
	must(t, err)

	if len(object.GetOwnerReferences()) != 0 {
		t.Fatal("NodeClaim garbage collection must not authorize remote release")
	}

	consumer := allocation.Spec.ConsumerRef
	if allocation.Name != "nodeclaim-"+string(claim.UID) ||
		consumer.APIGroup != "karpenter.sh" || consumer.Kind != "NodeClaim" ||
		consumer.Name != claim.Name || consumer.UID != claim.UID ||
		allocation.Spec.ClassRef.Name != "workers" || allocation.Spec.ClassRef.UID != testClass().UID ||
		allocation.Spec.Lifecycle != capacity.Active {
		t.Fatalf("allocation lost its consumer, class, or lifecycle identity: %+v", allocation.Spec)
	}

	// The requirements describe the chosen offering in the controller's
	// terms. The NodePool and custom label requirements name labels only the
	// Node carries, and are not sent.
	for _, requirement := range allocation.Spec.Requirements {
		if requirement.Key == v1.NodePoolLabelKey || requirement.Key == "workload.example.com/tier" ||
			requirement.MinValues != nil {
			t.Fatalf("unexpected requirement sent: %+v", requirement)
		}
	}

	if matched := matchingOfferings(allocation.Spec.Requirements, testClass().Status.Offerings); len(matched) != 1 {
		t.Fatalf("requirements %+v do not select the class's offering", allocation.Spec.Requirements)
	}

	template := allocation.Spec.NodeTemplate
	if template.Labels[v1.NodePoolLabelKey] != "workers" || template.Labels["workload.example.com/tier"] != "batch" {
		t.Fatalf("node template lost the NodeClaim labels: %v", template.Labels)
	}

	if len(template.Taints) != 1 || template.Taints[0].Key != "dedicated" {
		t.Fatalf("node template lost the NodeClaim taints: %v", template.Taints)
	}

	if len(template.StartupTaints) != 1 || !template.StartupTaints[0].MatchTaint(&v1.UnregisteredNoExecuteTaint) {
		t.Fatal("unregistered NoExecute startup taint is required")
	}

	if !allocation.Spec.Resources.Cpu().Equal(resource.MustParse("1")) {
		t.Fatal("allocation lost the requested resources")
	}
}

func TestCreateReturnsAtTheSnapshotAndSurvivesRestart(t *testing.T) {
	ctx := t.Context()
	provider, store := testProvider(t, testClass())
	claim := testClaim()
	object := claimAllocation(t, provider, claim)

	// The snapshot is written before any remote effect: Allocated is false.
	setSnapshot(t, store, object)

	restarted, err := New(store, store, store, Options{SnapshotWait: -1})
	must(t, err)

	for _, instance := range []*Provider{provider, restarted} {
		hydrated, err := instance.Create(ctx, claim)
		must(t, err)

		if hydrated.Status.ProviderID != capacity.ProviderIDPrefix+allocationTestUID ||
			hydrated.Status.ImageID != "pinned-host-image" ||
			hydrated.Name != claim.Name || hydrated.UID != claim.UID ||
			hydrated.Labels[v1.CapacityTypeLabelKey] != v1.CapacityTypeOnDemand ||
			hydrated.Labels[corev1.LabelInstanceTypeStable] != "test.medium" ||
			hydrated.Labels[corev1.LabelTopologyZone] != "test-region-1" ||
			!hydrated.Status.Capacity.Cpu().Equal(resource.MustParse("4")) {
			t.Fatalf("Create did not return the frozen allocation identity: %+v", hydrated)
		}
	}

	list := allocationList()
	must(t, store.List(ctx, list))

	if len(list.Items) != 1 {
		t.Fatal("a retry or restart created duplicate capacity")
	}
}

func TestCreateWaitsBrieflyForTheSnapshot(t *testing.T) {
	ctx := t.Context()
	provider, store := testProvider(t, testClass())

	// The allocation exists with its final UID and no snapshot yet.
	pending := claimAllocation(t, provider, testClaim())
	pending.SetUID(allocationTestUID)
	must(t, store.Update(ctx, pending))

	provider.options.SnapshotWait = time.Minute

	var sleeps int

	provider.sleep = func(context.Context, time.Duration) error {
		sleeps++
		if sleeps < 2 {
			return nil
		}

		object, err := provider.readAllocation(ctx, capacity.AllocationName(testClaim().UID))
		must(t, err)
		setSnapshot(t, store, object)

		return nil
	}

	hydrated, err := provider.Create(ctx, testClaim())
	must(t, err)

	if sleeps != 2 || hydrated.Status.ProviderID == "" {
		t.Fatalf("Create must poll until the snapshot appears, slept %d times", sleeps)
	}
}

func TestCreateWaitIsBounded(t *testing.T) {
	provider, _ := testProvider(t, testClass())
	provider.options.SnapshotWait = time.Second
	now := time.Now()
	provider.now = func() time.Time { return now }
	provider.sleep = func(_ context.Context, duration time.Duration) error {
		now = now.Add(duration)
		return nil
	}

	_, err := provider.Create(t.Context(), testClaim())

	var createError *cloudprovider.CreateError
	if !errors.As(err, &createError) || createError.ConditionReason != reasonAllocationPending {
		t.Fatalf("an allocation that stays pending must be reported pending, got %v", err)
	}
}

func TestCreateDiscardsAnAllocationThatFailedBeforeItsSnapshot(t *testing.T) {
	for _, test := range []struct {
		reason           string
		wantInsufficient bool
	}{
		{reason: capacity.FailureInsufficientCapacity, wantInsufficient: true},
		{reason: capacity.FailureQuotaExceeded, wantInsufficient: true},
		{reason: "InvalidRequest"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			ctx := t.Context()
			provider, store := testProvider(t, testClass())
			claim := testClaim()
			object := claimAllocation(t, provider, claim)

			allocation, err := decodeAllocation(object)
			must(t, err)

			allocation.Status.FailureReason = test.reason
			allocation.Status.FailureMessage = "no offering fits"
			writeAllocation(t, store, object, allocation)

			_, err = provider.Create(ctx, claim)
			if err == nil || cloudprovider.IsInsufficientCapacityError(err) != test.wantInsufficient {
				t.Fatalf("unexpected launch error %v", err)
			}

			var createError *cloudprovider.CreateError
			if !test.wantInsufficient && (!errors.As(err, &createError) || createError.ConditionReason != test.reason) {
				t.Fatalf("a terminal failure must carry its reason, got %v", err)
			}

			// A failed allocation can never succeed, so a retry needs a fresh object.
			remaining, err := provider.readAllocation(ctx, capacity.AllocationName(claim.UID))
			must(t, err)

			if remaining != nil {
				t.Fatal("the failed allocation must be removed")
			}

			if _, err := provider.Create(ctx, claim); err == nil || cloudprovider.IsInsufficientCapacityError(err) {
				t.Fatalf("a retry must start a fresh pending allocation, got %v", err)
			}
		})
	}
}

func TestCreateKeepsAFailedAllocationThatHasASnapshot(t *testing.T) {
	ctx := t.Context()
	provider, store := testProvider(t, testClass())
	claim := testClaim()
	object := claimAllocation(t, provider, claim)

	allocation := setSnapshot(t, store, object)
	allocation.Status.FailureReason = "ProviderFailed"
	writeAllocation(t, store, object, allocation)

	// A host may exist. Karpenter must record the provider ID so it calls
	// Delete; the lifecycle controller then replaces the NodeClaim.
	hydrated, err := provider.Create(ctx, claim)
	must(t, err)

	if hydrated.Status.ProviderID == "" {
		t.Fatal("a failed launch with a snapshot must still be recorded")
	}

	remaining, err := provider.readAllocation(ctx, capacity.AllocationName(claim.UID))
	must(t, err)

	if remaining == nil {
		t.Fatal("an allocation with a snapshot must never be removed by Create")
	}
}

func TestCreateRequiresAReadyClass(t *testing.T) {
	for name, change := range map[string]func(*nodeclass.MachineClass){
		"not ready": func(class *nodeclass.MachineClass) {
			class.Status.Conditions[0].Status = metav1.ConditionFalse
		},
		"unobserved generation": func(class *nodeclass.MachineClass) {
			class.Generation = 2
		},
		"unresolved": func(class *nodeclass.MachineClass) {
			class.Status.ResolvedRevision = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			class := testClass()
			change(class)
			provider, store := testProvider(t, class)

			_, err := provider.Create(t.Context(), testClaim())
			if !cloudprovider.IsNodeClassNotReadyError(err) {
				t.Fatalf("expected a NodeClass-not-ready error, got %v", err)
			}

			list := allocationList()
			must(t, store.List(t.Context(), list))

			if len(list.Items) != 0 {
				t.Fatal("no allocation may be created from an unready class")
			}
		})
	}
}

func TestCreateRefusesRetiringAndForeignAllocations(t *testing.T) {
	for name, change := range map[string]func(*capacity.Allocation){
		"released": func(allocation *capacity.Allocation) {
			allocation.Spec.Lifecycle = capacity.Released
		},
		"fenced": func(allocation *capacity.Allocation) {
			setCondition(allocation, capacity.ConditionFenced, metav1.ConditionTrue, "MachineDeleting")
		},
		"another NodeClaim incarnation": func(allocation *capacity.Allocation) {
			allocation.Spec.ConsumerRef.UID = "00000000-0000-4000-8000-000000000000"
		},
		"another class": func(allocation *capacity.Allocation) {
			allocation.Spec.ClassRef.Name = "other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			provider, store := testProvider(t, testClass())
			claim := testClaim()
			object := claimAllocation(t, provider, claim)

			allocation := setSnapshot(t, store, object)
			change(allocation)
			writeAllocation(t, store, object, allocation)

			if _, err := provider.Create(t.Context(), claim); err == nil {
				t.Fatal("Create must not return a NodeClaim for this allocation")
			}
		})
	}
}

func TestHydrateDefaultsZoneAndOperatingSystem(t *testing.T) {
	provider, store := testProvider(t, testClass())
	claim := testClaim()
	object := claimAllocation(t, provider, claim)

	allocation := setSnapshot(t, store, object)
	allocation.Status.Zone = ""
	delete(allocation.Status.Labels, corev1.LabelTopologyZone)
	delete(allocation.Status.Labels, corev1.LabelOSStable)
	writeAllocation(t, store, object, allocation)

	hydrated, err := provider.Create(t.Context(), claim)
	must(t, err)

	if hydrated.Labels[corev1.LabelTopologyZone] != "test-site" || hydrated.Labels[corev1.LabelOSStable] != "linux" {
		t.Fatalf("zone must default to the Site and the OS to linux, got %v", hydrated.Labels)
	}
}

func TestHydrateRequiresWhatCannotBeGuessed(t *testing.T) {
	for name, change := range map[string]func(*capacity.Allocation){
		"architecture": func(allocation *capacity.Allocation) {
			delete(allocation.Status.Labels, corev1.LabelArchStable)
		},
		"instance type": func(allocation *capacity.Allocation) {
			delete(allocation.Status.Labels, corev1.LabelInstanceTypeStable)
		},
		"its own provider ID": func(allocation *capacity.Allocation) {
			allocation.Status.ProviderID = capacity.ProviderIDPrefix + "00000000-0000-4000-8000-000000000000"
		},
		"capacity": func(allocation *capacity.Allocation) {
			delete(allocation.Status.Capacity, corev1.ResourceCPU)
		},
	} {
		t.Run(name, func(t *testing.T) {
			provider, store := testProvider(t, testClass())
			claim := testClaim()
			object := claimAllocation(t, provider, claim)

			allocation := setSnapshot(t, store, object)
			change(allocation)
			writeAllocation(t, store, object, allocation)

			if _, err := provider.Create(t.Context(), claim); err == nil {
				t.Fatal("an allocation without it must not be hydrated")
			}
		})
	}
}

func TestRebootAndRepairAreNotOffered(t *testing.T) {
	provider, _ := testProvider(t, testClass())

	if !cloudprovider.IsNodeRebootNotImplementedError(provider.Reboot(t.Context(), testClaim(), "operation")) {
		t.Fatal("Reboot must report that it is not implemented")
	}

	if len(provider.RepairPolicies()) != 0 {
		t.Fatal("no repair policy is supported")
	}
}

func testProvider(t *testing.T, class *nodeclass.MachineClass) (*Provider, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	nodeclass.AddToScheme(scheme)

	store := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(class).
		WithIndex(allocationObject(), allocationUIDIndex, func(object client.Object) []string {
			return []string{string(object.GetUID())}
		}).
		WithIndex(allocationObject(), ConsumerIndex, indexConsumer).
		Build()

	provider, err := New(store, store, store, Options{SnapshotWait: -1})
	must(t, err)

	return provider, store
}

func testClaim() *v1.NodeClaim {
	minimum := 1

	return &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "workload-claim",
			UID:  "55361535-e293-488e-aa38-cef580e84311",
			Labels: map[string]string{
				v1.NodePoolLabelKey: "workers",
			},
		},
		Spec: v1.NodeClaimSpec{
			NodeClassRef: &v1.NodeClassReference{
				Group: nodeclass.GroupVersion.Group,
				Kind:  "MachineClass",
				Name:  "workers",
			},
			Requirements: []v1.NodeSelectorRequirementWithMinValues{
				{
					Key:       corev1.LabelInstanceTypeStable,
					Operator:  corev1.NodeSelectorOpIn,
					Values:    []string{"test.medium"},
					MinValues: &minimum,
				},
			},
			Resources: v1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
					corev1.ResourcePods:   resource.MustParse("1"),
				},
			},
		},
	}
}

func testClass() *nodeclass.MachineClass {
	return &nodeclass.MachineClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "workers",
			UID:        "1393cb4b-53b7-4ff0-a416-77bc17d07be6",
			Generation: 1,
		},
		Spec: nodeclass.MachineClassSpec{SiteRef: nodeclass.SiteReference{Name: "test-site"}},
		Status: nodeclass.MachineClassStatus{
			ObservedGeneration: 1,
			ResolvedRevision:   "resolved-content-sha256",
			Offerings:          []capacity.Offering{testOffering()},
			Conditions: []status.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 1,
					Reason:             "InputsResolved",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}
}

func testOffering() capacity.Offering {
	now := metav1.NewTime(time.Now().Add(-time.Minute))
	expires := metav1.NewTime(time.Now().Add(time.Minute))

	return capacity.Offering{
		Name:         "medium-zone-one-ondemand",
		InstanceType: "test.medium",
		Zone:         "test-region-1",
		CapacityType: "OnDemand",
		Availability: "Available",
		Labels: map[string]string{
			capacity.CapacityTypeLabel:     "OnDemand",
			corev1.LabelInstanceTypeStable: "test.medium",
			corev1.LabelTopologyZone:       "test-region-1",
			corev1.LabelTopologyRegion:     "test-region",
			corev1.LabelArchStable:         "amd64",
			corev1.LabelOSStable:           "linux",
		},
		Capacity: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		},
		Overhead: capacity.Overhead{
			KubeReserved: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("100m"),
			},
			SystemReserved: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			EvictionThreshold: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			},
		},
		Price: &capacity.Price{
			Currency:   "USD",
			PerHour:    "0.192",
			Source:     "documented hourly observation excluding disks",
			ObservedAt: &now,
		},
		ObservedAt: &now,
		ValidUntil: &expires,
	}
}

// setSnapshot gives the allocation the status the allocation controller
// writes with the snapshot, before any remote effect.
func setSnapshot(t *testing.T, store client.Client, object *unstructured.Unstructured) *capacity.Allocation {
	t.Helper()

	allocation, err := decodeAllocation(object)
	must(t, err)

	allocation.UID = allocationTestUID
	allocation.Generation = 1

	if allocation.Labels == nil {
		allocation.Labels = map[string]string{}
	}

	allocation.Labels[capacity.AllocationLabel] = allocationTestUID
	offering := testOffering()
	allocatable := offering.Capacity.DeepCopy()
	allocation.Status = capacity.AllocationStatus{
		Snapshot: &capacity.Snapshot{
			ClassUID:      testClass().UID,
			ClassRevision: testClass().Status.ResolvedRevision,
			SiteName:      "test-site",
			Offering:      offering,
		},
		ProviderID:   capacity.ProviderIDPrefix + allocationTestUID,
		Image:        "pinned-host-image",
		InstanceType: offering.InstanceType,
		Zone:         offering.Zone,
		CapacityType: offering.CapacityType,
		Capacity:     offering.Capacity.DeepCopy(),
		Allocatable:  allocatable,
		Labels:       offering.Labels,
		Conditions: []metav1.Condition{
			{
				Type:               capacity.ConditionAllocated,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: 1,
				Reason:             "Launching",
				LastTransitionTime: metav1.Now(),
			},
		},
	}
	writeAllocation(t, store, object, allocation)

	return allocation
}

// setAllocated is setSnapshot followed by a confirmed host.
func setAllocated(t *testing.T, store client.Client, object *unstructured.Unstructured) {
	t.Helper()

	allocation := setSnapshot(t, store, object)
	allocation.Status.ProviderResourceID = "opaque-provider-host-id"
	setCondition(allocation, capacity.ConditionAllocated, metav1.ConditionTrue, "Allocated")
	writeAllocation(t, store, object, allocation)
}

func setCondition(allocation *capacity.Allocation, conditionType string, status metav1.ConditionStatus, reason string) {
	apimeta.SetStatusCondition(&allocation.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: allocation.Generation,
		Reason:             reason,
	})
}

// claimAllocation creates the claim's allocation through Create and returns
// it as stored.
func claimAllocation(t *testing.T, provider *Provider, claim *v1.NodeClaim) *unstructured.Unstructured {
	t.Helper()

	if _, err := provider.Create(t.Context(), claim); err == nil {
		t.Fatal("an allocation without a snapshot must be pending")
	}

	object, err := provider.readAllocation(t.Context(), capacity.AllocationName(claim.UID))
	must(t, err)

	if object == nil {
		t.Fatal("Create did not persist an allocation")
	}

	return object
}

func writeAllocation(t *testing.T, store client.Client, object *unstructured.Unstructured, allocation *capacity.Allocation) {
	t.Helper()

	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(allocation)
	must(t, err)

	object.Object = content
	object.SetGroupVersionKind(capacity.AllocationGVK)
	must(t, store.Update(t.Context(), object))
	allocation.ResourceVersion = object.GetResourceVersion()
}

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
