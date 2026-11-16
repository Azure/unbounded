// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
	"github.com/Azure/unbounded/plugins/karpenter/internal/provider"
)

const (
	claimUID      = types.UID("55361535-e293-488e-aa38-cef580e84311")
	allocationUID = types.UID("46ed7c08-437e-45be-ae8b-a13f73d17128")
	classUID      = types.UID("1393cb4b-53b7-4ff0-a416-77bc17d07be6")

	allocationFinalizer = "unbounded-cloud.io/machineallocation"
	claimFinalizer      = "karpenter.sh/termination"
)

type marked struct {
	class    types.UID
	offering string
}

type fakeMarker struct {
	marks []marked
}

func (m *fakeMarker) MarkUnavailable(class types.UID, offering string) {
	m.marks = append(m.marks, marked{class: class, offering: offering})
}

type fixture struct {
	t          *testing.T
	store      client.Client
	marker     *fakeMarker
	controller *Controller
}

func newFixture(t *testing.T, objects ...client.Object) *fixture {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	scheme.AddKnownTypes(schema.GroupVersion{Group: "karpenter.sh", Version: "v1"}, &v1.NodeClaim{}, &v1.NodeClaimList{})
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Group: "karpenter.sh", Version: "v1"})

	store := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	marker := &fakeMarker{}

	return &fixture{
		t:      t,
		store:  store,
		marker: marker,
		controller: &Controller{
			Client:   store,
			Reader:   store,
			Recorder: record.NewFakeRecorder(100),
			Marker:   marker,
		},
	}
}

func (f *fixture) reconcile() reconcile.Result {
	f.t.Helper()

	result, err := f.controller.Reconcile(f.t.Context(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: capacity.AllocationName(claimUID)},
	})
	if err != nil {
		f.t.Fatal(err)
	}

	return result
}

// allocation returns the stored allocation, or nil when it is gone.
func (f *fixture) allocation() *capacity.Allocation {
	f.t.Helper()

	object := provider.AllocationObject()

	err := f.store.Get(f.t.Context(), client.ObjectKey{Name: capacity.AllocationName(claimUID)}, object)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		f.t.Fatal(err)
	}

	allocation, err := provider.DecodeAllocation(object)
	if err != nil {
		f.t.Fatal(err)
	}

	return allocation
}

// claim returns the stored NodeClaim, or nil when it is gone.
func (f *fixture) claim() *v1.NodeClaim {
	f.t.Helper()

	claim := new(v1.NodeClaim)

	err := f.store.Get(f.t.Context(), client.ObjectKey{Name: "workload-claim"}, claim)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		f.t.Fatal(err)
	}

	return claim
}

func testClaim(changes ...func(*v1.NodeClaim)) *v1.NodeClaim {
	claim := &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "workload-claim",
			UID:        claimUID,
			Finalizers: []string{claimFinalizer},
		},
		Spec: v1.NodeClaimSpec{
			NodeClassRef: &v1.NodeClassReference{Group: "unbounded-cloud.io", Kind: "MachineClass", Name: "workers"},
		},
	}

	for _, change := range changes {
		change(claim)
	}

	return claim
}

func terminating(claim *v1.NodeClaim) {
	now := metav1.Now()
	claim.DeletionTimestamp = &now
}

// testAllocation is an allocation with a snapshot and the allocation
// controller's finalizer.
func testAllocation(t *testing.T, changes ...func(*capacity.Allocation)) *unstructured.Unstructured {
	t.Helper()

	allocation := &capacity.Allocation{
		TypeMeta: metav1.TypeMeta{
			APIVersion: capacity.AllocationGVK.GroupVersion().String(),
			Kind:       capacity.AllocationGVK.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:       capacity.AllocationName(claimUID),
			UID:        allocationUID,
			Generation: 1,
			Finalizers: []string{allocationFinalizer},
		},
		Spec: capacity.AllocationSpec{
			ConsumerRef: capacity.Consumer{
				APIGroup: capacity.ConsumerGroup,
				Kind:     capacity.ConsumerKind,
				Name:     "workload-claim",
				UID:      claimUID,
			},
			ClassRef:  capacity.Reference{Name: "workers", UID: classUID},
			Lifecycle: capacity.Active,
		},
		Status: capacity.AllocationStatus{
			Snapshot: &capacity.Snapshot{
				ClassUID: classUID,
				Offering: capacity.Offering{Name: "medium-zone-one-ondemand", InstanceType: "test.medium", Zone: "zone-1"},
			},
			ProviderID:   capacity.ProviderIDPrefix + string(allocationUID),
			InstanceType: "test.medium",
			Zone:         "zone-1",
			Labels: map[string]string{
				corev1.LabelInstanceTypeStable: "test.medium",
				corev1.LabelTopologyZone:       "zone-1",
			},
		},
	}

	for _, change := range changes {
		change(allocation)
	}

	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(allocation)
	if err != nil {
		t.Fatal(err)
	}

	return &unstructured.Unstructured{Object: content}
}

func condition(conditionType string, status metav1.ConditionStatus, reason string) func(*capacity.Allocation) {
	return func(allocation *capacity.Allocation) {
		allocation.Status.Conditions = append(allocation.Status.Conditions, metav1.Condition{
			Type:               conditionType,
			Status:             status,
			Reason:             reason,
			ObservedGeneration: allocation.Generation,
			LastTransitionTime: metav1.Now(),
		})
	}
}

func deleting(allocation *capacity.Allocation) {
	now := metav1.Now()
	allocation.DeletionTimestamp = &now
}

func failed(reason string) func(*capacity.Allocation) {
	return func(allocation *capacity.Allocation) {
		allocation.Status.FailureReason = reason
		allocation.Status.FailureMessage = "the provider refused the launch"
	}
}

func TestTerminatingNodeClaimFencesItsAllocation(t *testing.T) {
	f := newFixture(t, testClaim(terminating), testAllocation(t))

	f.reconcile()
	f.reconcile()

	allocation := f.allocation()
	if allocation == nil || allocation.DeletionTimestamp.IsZero() {
		t.Fatal("the allocation must be deleted so the allocation controller fences it")
	}

	// Deleting is the fence, not the release.
	if allocation.Spec.Lifecycle != capacity.Active {
		t.Fatal("fencing must not authorize release; Karpenter's Delete does that after the drain")
	}

	if f.claim() == nil {
		t.Fatal("the NodeClaim belongs to Karpenter")
	}
}

func TestTerminatingNodeClaimRemovesAnAllocationWithoutEffects(t *testing.T) {
	f := newFixture(t, testClaim(terminating), testAllocation(t, func(allocation *capacity.Allocation) {
		allocation.Finalizers = nil
		allocation.Status = capacity.AllocationStatus{}
	}))

	f.reconcile()

	if f.allocation() != nil {
		t.Fatal("an allocation without the finalizer has no effects and disappears when deleted")
	}
}

func TestHealthyAllocationIsLeftAlone(t *testing.T) {
	for name, change := range map[string]func(*capacity.Allocation){
		"launching": condition(capacity.ConditionAllocated, metav1.ConditionFalse, "Launching"),
		"allocated": condition(capacity.ConditionAllocated, metav1.ConditionTrue, "Allocated"),
		"failed before its snapshot": func(allocation *capacity.Allocation) {
			// Create reports this failure; nothing was launched.
			allocation.Status = capacity.AllocationStatus{FailureReason: capacity.FailureInsufficientCapacity}
		},
		"fenced condition false": condition(capacity.ConditionFenced, metav1.ConditionFalse, "Fencing"),
		"provider reports names in another case": func(allocation *capacity.Allocation) {
			condition(capacity.ConditionAllocated, metav1.ConditionTrue, "Allocated")(allocation)
			allocation.Status.InstanceType = "Test.Medium"
			allocation.Status.Zone = "Zone-1"
		},
		"zoneless offering with the Site as its zone label": func(allocation *capacity.Allocation) {
			condition(capacity.ConditionAllocated, metav1.ConditionTrue, "Allocated")(allocation)
			allocation.Status.Snapshot.Offering.Zone = ""
			allocation.Status.Zone = ""
			allocation.Status.Labels[corev1.LabelTopologyZone] = "test-site"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, testClaim(), testAllocation(t, change))

			f.reconcile()

			if f.claim() == nil || !f.claim().DeletionTimestamp.IsZero() {
				t.Fatal("the NodeClaim must be left alone")
			}

			if allocation := f.allocation(); allocation == nil || !allocation.DeletionTimestamp.IsZero() ||
				allocation.Spec.Lifecycle != capacity.Active {
				t.Fatal("the allocation must be left alone")
			}

			if len(f.marker.marks) != 0 {
				t.Fatal("no offering failed")
			}
		})
	}
}

func TestUnusableAllocationDeletesItsNodeClaim(t *testing.T) {
	for _, test := range []struct {
		name     string
		change   func(*capacity.Allocation)
		wantMark bool
	}{
		{name: "insufficient capacity", change: failed(capacity.FailureInsufficientCapacity), wantMark: true},
		{name: "quota exceeded", change: failed(capacity.FailureQuotaExceeded), wantMark: true},
		// Any failed launch is remembered, so a failure that repeats cannot
		// replace the NodeClaim in a tight loop.
		{name: "provider failed", change: failed("ProviderFailed"), wantMark: true},
		{name: "Machine deleted", change: condition(capacity.ConditionFenced, metav1.ConditionTrue, reasonMachineDeleting)},
		{name: "allocation deleted by someone else", change: deleting},
		{name: "released by someone else", change: func(allocation *capacity.Allocation) {
			allocation.Spec.Lifecycle = capacity.Released
		}},
		{name: "launched another instance type", change: func(allocation *capacity.Allocation) {
			condition(capacity.ConditionAllocated, metav1.ConditionTrue, "Allocated")(allocation)
			allocation.Status.InstanceType = "test.large"
		}},
		{name: "launched in another zone", change: func(allocation *capacity.Allocation) {
			condition(capacity.ConditionAllocated, metav1.ConditionTrue, "Allocated")(allocation)
			allocation.Status.Zone = "zone-2"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, testClaim(), testAllocation(t, test.change))

			f.reconcile()

			claim := f.claim()
			if claim == nil || claim.DeletionTimestamp.IsZero() {
				t.Fatal("the NodeClaim must be deleted so Karpenter releases and replaces it")
			}

			// Release stays with Karpenter's Delete, after its drain.
			if allocation := f.allocation(); allocation == nil {
				t.Fatal("the allocation must not be removed here")
			}

			if test.wantMark != (len(f.marker.marks) == 1) {
				t.Fatalf("unexpected unavailable marks: %+v", f.marker.marks)
			}

			if test.wantMark && f.marker.marks[0] != (marked{class: classUID, offering: "medium-zone-one-ondemand"}) {
				t.Fatalf("marked the wrong offering: %+v", f.marker.marks[0])
			}
		})
	}
}

func TestOrphanIsFencedBeforeAnythingElse(t *testing.T) {
	f := newFixture(t, testAllocation(t))

	f.reconcile()

	allocation := f.allocation()
	if allocation == nil || allocation.DeletionTimestamp.IsZero() || allocation.Spec.Lifecycle != capacity.Active {
		t.Fatal("an orphan must first be deleted, which fences it, and not yet released")
	}

	// Not fenced yet: wait, never release.
	if result := f.reconcile(); result.RequeueAfter == 0 {
		t.Fatal("an orphan waiting for its fence must be looked at again")
	}

	if f.allocation().Spec.Lifecycle != capacity.Active {
		t.Fatal("release must wait for the fence, which revokes the bootstrap token")
	}
}

func TestOrphanWithoutEffectsIsRemoved(t *testing.T) {
	f := newFixture(t, testAllocation(t, func(allocation *capacity.Allocation) {
		allocation.Finalizers = nil
		allocation.Status = capacity.AllocationStatus{}
	}))

	f.reconcile()

	if f.allocation() != nil {
		t.Fatal("an orphan with no finalizer has no effects and must be removed")
	}
}

func TestFencedOrphanWithoutANodeIsReleased(t *testing.T) {
	f := newFixture(
		t,
		testAllocation(t, deleting, condition(capacity.ConditionFenced, metav1.ConditionTrue, "AllocationDeleting")),
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "unrelated"},
			Spec:       corev1.NodeSpec{ProviderID: capacity.ProviderIDPrefix + "00000000-0000-4000-8000-000000000000"},
		},
	)

	f.reconcile()

	if f.allocation().Spec.Lifecycle != capacity.Released {
		t.Fatal("a fenced orphan with no Node must have its release authorized")
	}

	// Authorized: the allocation controller finishes it.
	if result := f.reconcile(); result.RequeueAfter != 0 {
		t.Fatal("nothing is left to do once release is authorized")
	}
}

func TestFencedOrphanWithANodeIsHeld(t *testing.T) {
	fenced := condition(capacity.ConditionFenced, metav1.ConditionTrue, "AllocationDeleting")
	recorded := func(allocation *capacity.Allocation) {
		allocation.Status.NodeRef = &capacity.Reference{Name: "ub-node", UID: "node-uid"}
	}

	for name, objects := range map[string][]client.Object{
		"Node recorded on the allocation": {
			testAllocation(t, deleting, fenced, recorded),
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ub-node", UID: "node-uid"}},
		},
		"Node registered with the provider ID": {
			testAllocation(t, deleting, fenced),
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "ub-node"},
				Spec:       corev1.NodeSpec{ProviderID: capacity.ProviderIDPrefix + string(allocationUID)},
			},
		},
		"recorded Node is gone but another has the provider ID": {
			testAllocation(t, deleting, fenced, recorded),
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "renamed"},
				Spec:       corev1.NodeSpec{ProviderID: capacity.ProviderIDPrefix + string(allocationUID)},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, objects...)

			result := f.reconcile()

			if f.allocation().Spec.Lifecycle != capacity.Active {
				t.Fatal("an orphan with a Node must not be released: nothing drained it")
			}

			if result.RequeueAfter == 0 {
				t.Fatal("a held orphan must be looked at again")
			}

			recorder, _ := f.controller.Recorder.(*record.FakeRecorder)
			if len(recorder.Events) != 1 {
				t.Fatal("holding an orphan must be reported")
			}
		})
	}
}

// The allocation's nodeRef is never cleared. Once the Node it names is
// deleted, or replaced by another object under the same name, it must stop
// holding the orphan: deleting the Node is what the hold event asks for.
func TestFencedOrphanWithAStaleNodeReferenceIsReleased(t *testing.T) {
	fenced := condition(capacity.ConditionFenced, metav1.ConditionTrue, "AllocationDeleting")
	recorded := func(allocation *capacity.Allocation) {
		allocation.Status.NodeRef = &capacity.Reference{Name: "ub-node", UID: "node-uid"}
	}

	for name, objects := range map[string][]client.Object{
		"recorded Node is gone": {
			testAllocation(t, deleting, fenced, recorded),
		},
		"another Node took the name": {
			testAllocation(t, deleting, fenced, recorded),
			&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "ub-node", UID: "another-uid"},
				Spec:       corev1.NodeSpec{ProviderID: "unbounded:///machineallocations/another"},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, objects...)

			f.reconcile()

			if f.allocation().Spec.Lifecycle != capacity.Released {
				t.Fatal("a Node reference to a Node that no longer exists must not hold the orphan")
			}
		})
	}
}

func TestNodeClaimOfAnotherClassIsLeftAlone(t *testing.T) {
	for name, change := range map[string]func(*v1.NodeClaim){
		"another MachineClass": func(claim *v1.NodeClaim) { claim.Spec.NodeClassRef.Name = "others" },
		"another NodeClass kind": func(claim *v1.NodeClaim) {
			claim.Spec.NodeClassRef.Group = "example.com"
			claim.Spec.NodeClassRef.Kind = "OtherClass"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, testClaim(change), testAllocation(t, failed("ProviderFailed")))

			f.reconcile()

			if f.claim() == nil || !f.claim().DeletionTimestamp.IsZero() {
				t.Fatal("a NodeClaim this plugin does not serve must never be deleted")
			}

			if allocation := f.allocation(); allocation == nil || !allocation.DeletionTimestamp.IsZero() {
				t.Fatal("the allocation must be left alone")
			}
		})
	}
}

func TestRecreatedNodeClaimDoesNotAdoptTheAllocation(t *testing.T) {
	f := newFixture(
		t,
		testClaim(func(claim *v1.NodeClaim) { claim.UID = "00000000-0000-4000-8000-000000000000" }),
		testAllocation(t),
	)

	f.reconcile()

	if allocation := f.allocation(); allocation == nil || allocation.DeletionTimestamp.IsZero() {
		t.Fatal("a NodeClaim with another UID is another consumer; the allocation is an orphan")
	}

	if claim := f.claim(); claim == nil || !claim.DeletionTimestamp.IsZero() {
		t.Fatal("the unrelated NodeClaim must be left alone")
	}
}

func TestAllocationsOfOtherConsumersAreIgnored(t *testing.T) {
	for name, change := range map[string]func(*capacity.Allocation){
		"another consumer kind": func(allocation *capacity.Allocation) {
			allocation.Spec.ConsumerRef.Kind = "MachinePool"
		},
		"not named from its consumer UID": func(allocation *capacity.Allocation) {
			allocation.Spec.ConsumerRef.UID = "00000000-0000-4000-8000-000000000000"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, testAllocation(t, change))

			f.reconcile()

			if allocation := f.allocation(); allocation == nil || !allocation.DeletionTimestamp.IsZero() ||
				allocation.Spec.Lifecycle != capacity.Active {
				t.Fatal("an allocation this plugin did not make must never be touched")
			}
		})
	}
}

func TestNodeClaimEventsMapToTheirAllocation(t *testing.T) {
	requests := allocationForClaim(t.Context(), testClaim())
	if len(requests) != 1 || requests[0].Name != capacity.AllocationName(claimUID) {
		t.Fatalf("unexpected requests %v", requests)
	}

	foreign := testClaim(func(claim *v1.NodeClaim) { claim.Spec.NodeClassRef.Kind = "EC2NodeClass" })
	if len(allocationForClaim(t.Context(), foreign)) != 0 {
		t.Fatal("a NodeClaim of another NodeClass kind is not managed by this plugin")
	}
}

func TestRegisterRequiresItsDependencies(t *testing.T) {
	if err := (&Controller{}).Register(t.Context(), nil); err == nil {
		t.Fatal("an unconfigured controller must not register")
	}
}
