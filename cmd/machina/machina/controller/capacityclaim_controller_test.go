// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const testNVIDIAImage = "ghcr.io/azure/agent-ubuntu2404-nvidia:v20260619"

func TestCapacityClaimAllocatesEligibleMachine(t *testing.T) {
	claim := testCapacityClaim("claim-a")
	machine := testMachine("gb200-1", true, false)
	reconciler := newCapacityTestReconciler(t, claim, machine, testConfigurationVersion())

	reconcileTwice(t, reconciler, claim.Name)

	var gotClaim unboundedv1alpha3.CapacityClaim
	if err := reconciler.Get(context.Background(), client.ObjectKey{Name: claim.Name}, &gotClaim); err != nil {
		t.Fatalf("get claim: %v", err)
	}

	if gotClaim.Status.Machine == nil || gotClaim.Status.Machine.Name != machine.Name {
		t.Fatalf("bound Machine = %#v, want %s", gotClaim.Status.Machine, machine.Name)
	}

	var gotMachine unboundedv1alpha3.Machine
	if err := reconciler.Get(context.Background(), client.ObjectKey{Name: machine.Name}, &gotMachine); err != nil {
		t.Fatalf("get Machine: %v", err)
	}

	if gotMachine.Status.Reservation == nil || gotMachine.Status.Reservation.ClaimUID != claim.UID {
		t.Fatalf("reservation = %#v, want claim UID %s", gotMachine.Status.Reservation, claim.UID)
	}

	if gotMachine.Spec.ConfigurationRef == nil || gotMachine.Spec.ConfigurationRef.Name != "hillsboro-gb200" {
		t.Fatalf("configurationRef = %#v", gotMachine.Spec.ConfigurationRef)
	}
}

func TestCapacityClaimReservesThenProvisionsBeforeReady(t *testing.T) {
	claim := testCapacityClaim("claim-scale")
	machine := testMachine("gb200-1", false, false)
	machine.Status.Allocation = nil
	reconciler := newCapacityTestReconciler(t, claim, machine, testConfigurationVersion())
	lifecycle := &recordingCapacityLifecycle{}
	reconciler.Lifecycle = lifecycle

	reconcileTwice(t, reconciler, claim.Name)

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: claim.Name},
	}); err != nil {
		t.Fatalf("provision reconcile: %v", err)
	}

	if lifecycle.provisionCalls != 1 {
		t.Fatalf("provision calls = %d, want 1", lifecycle.provisionCalls)
	}

	var reserved unboundedv1alpha3.Machine
	if err := reconciler.Get(context.Background(), client.ObjectKey{Name: machine.Name}, &reserved); err != nil {
		t.Fatalf("get reserved Machine: %v", err)
	}

	if reserved.Status.Reservation == nil {
		t.Fatal("Machine was not reserved before provisioning")
	}

	reserved.Status.Allocation = testMachine(machine.Name, true, false).Status.Allocation
	if err := reconciler.Status().Update(context.Background(), &reserved); err != nil {
		t.Fatalf("publish allocation observations: %v", err)
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: claim.Name},
	}); err != nil {
		t.Fatalf("ready reconcile: %v", err)
	}

	var got unboundedv1alpha3.CapacityClaim
	if err := reconciler.Get(context.Background(), client.ObjectKey{Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}

	if got.Status.Phase != unboundedv1alpha3.CapacityClaimPhaseReady {
		t.Fatalf("phase = %s, want Ready", got.Status.Phase)
	}
}

func TestCapacityClaimRecoversReservationAfterRestart(t *testing.T) {
	claim := testCapacityClaim("claim-restart")
	claim.Finalizers = []string{unboundedv1alpha3.CapacityClaimFinalizer}
	machine := testMachine("gb200-2", false, false)
	machine.Status.Reservation = &unboundedv1alpha3.MachineReservationStatus{
		ClaimName: claim.Name,
		ClaimUID:  claim.UID,
		Domain:    "hillsboro",
	}

	reconciler := newCapacityTestReconciler(t, claim, machine, testConfigurationVersion())

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: claim.Name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got unboundedv1alpha3.CapacityClaim
	if err := reconciler.Get(context.Background(), client.ObjectKey{Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}

	if got.Status.Machine == nil || got.Status.Machine.Name != machine.Name {
		t.Fatalf("recovered binding = %#v, want %s", got.Status.Machine, machine.Name)
	}
}

func TestCapacityClaimQuarantinesDuplicateReservationsAfterRestart(t *testing.T) {
	claim := testCapacityClaim("claim-duplicate-restart")
	claim.Finalizers = []string{unboundedv1alpha3.CapacityClaimFinalizer}
	first := testMachine("gb200-1", false, false)
	second := testMachine("gb200-2", false, false)

	for _, machine := range []*unboundedv1alpha3.Machine{first, second} {
		machine.Status.Reservation = &unboundedv1alpha3.MachineReservationStatus{
			ClaimName: claim.Name,
			ClaimUID:  claim.UID,
			Domain:    "hillsboro",
		}
	}

	reconciler := newCapacityTestReconciler(t, claim, first, second, testConfigurationVersion())
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: claim.Name},
	}); err == nil {
		t.Fatal("duplicate reservation recovery succeeded, want fail-closed error")
	}

	for _, name := range []string{first.Name, second.Name} {
		var got unboundedv1alpha3.Machine
		if err := reconciler.Get(context.Background(), client.ObjectKey{Name: name}, &got); err != nil {
			t.Fatalf("get Machine %s: %v", name, err)
		}

		if got.Status.Reservation == nil {
			t.Fatalf("Machine %s reservation was cleared without release barriers", name)
		}

		if !apimeta.IsStatusConditionTrue(got.Status.Conditions, unboundedv1alpha3.MachineConditionQuarantined) {
			t.Fatalf("Machine %s was not quarantined", name)
		}
	}
}

func TestDeletingClaimRecoversReservationBeforeRelease(t *testing.T) {
	claim := testCapacityClaim("claim-delete-recovery")
	now := metav1.Now()
	claim.Finalizers = []string{unboundedv1alpha3.CapacityClaimFinalizer}
	claim.DeletionTimestamp = &now
	machine := testMachine("gb200-2", false, false)
	machine.Status.Reservation = &unboundedv1alpha3.MachineReservationStatus{
		ClaimName: claim.Name,
		ClaimUID:  claim.UID,
		Domain:    "hillsboro",
	}
	reconciler := newCapacityTestReconciler(t, claim, machine, testConfigurationVersion())

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: claim.Name},
	}); err != nil {
		t.Fatalf("reconcile deleting claim: %v", err)
	}

	var got unboundedv1alpha3.CapacityClaim
	if err := reconciler.Get(context.Background(), client.ObjectKey{Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}

	if got.Status.Machine == nil || got.Status.Machine.Name != machine.Name {
		t.Fatalf("recovered binding = %#v, want %s", got.Status.Machine, machine.Name)
	}

	if !containsString(got.Finalizers, unboundedv1alpha3.CapacityClaimFinalizer) {
		t.Fatal("finalizer removed before reservation release")
	}
}

func TestCapacityClaimRejectsDuplicateBindingDeterministically(t *testing.T) {
	machine := testMachine("gb200-1", false, false)
	winner := testCapacityClaim("a-winner")
	loser := testCapacityClaim("z-loser")
	winner.Finalizers = []string{unboundedv1alpha3.CapacityClaimFinalizer}
	loser.Finalizers = []string{unboundedv1alpha3.CapacityClaimFinalizer}
	winner.Status.Machine = &unboundedv1alpha3.BoundMachineReference{Name: machine.Name, UID: machine.UID}
	loser.Status.Machine = &unboundedv1alpha3.BoundMachineReference{Name: machine.Name, UID: machine.UID}
	reconciler := newCapacityTestReconciler(t, winner, loser, machine, testConfigurationVersion())

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: loser.Name}}); err != nil {
		t.Fatalf("reconcile loser: %v", err)
	}

	var got unboundedv1alpha3.CapacityClaim
	if err := reconciler.Get(context.Background(), client.ObjectKey{Name: loser.Name}, &got); err != nil {
		t.Fatalf("get loser: %v", err)
	}

	if got.Status.Phase != unboundedv1alpha3.CapacityClaimPhaseFailed || got.Status.Machine != nil {
		t.Fatalf("loser status = %#v, want failed and unbound", got.Status)
	}
}

func TestCapacityClaimSkipsQuarantinedAndBlockedGB200(t *testing.T) {
	claim := testCapacityClaim("claim-safe")
	gb2003 := testMachine("gb200-3", true, true)
	gb2004 := testMachine("gb200-4", false, false)
	apimeta.SetStatusCondition(&gb2004.Status.Conditions, metav1.Condition{
		Type:   unboundedv1alpha3.MachineConditionAvailable,
		Status: metav1.ConditionFalse,
		Reason: "InventoryBlocked",
	})
	reconciler := newCapacityTestReconciler(t, claim, gb2003, gb2004, testConfigurationVersion())

	reconcileTwice(t, reconciler, claim.Name)

	var got unboundedv1alpha3.CapacityClaim
	if err := reconciler.Get(context.Background(), client.ObjectKey{Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}

	if got.Status.Machine != nil {
		t.Fatalf("claim bound unsafe Machine: %#v", got.Status.Machine)
	}

	if got.Status.Phase != unboundedv1alpha3.CapacityClaimPhasePending {
		t.Fatalf("phase = %s, want Pending", got.Status.Phase)
	}
}

func TestCapacityClaimGPUReadinessFailsClosed(t *testing.T) {
	claim := testCapacityClaim("claim-gpu")
	claim.Finalizers = []string{unboundedv1alpha3.CapacityClaimFinalizer}
	machine := testMachine("gb200-1", false, false)
	machine.Status.Reservation = &unboundedv1alpha3.MachineReservationStatus{
		ClaimName: claim.Name, ClaimUID: claim.UID, Domain: "hillsboro",
	}

	machine.Status.Allocation.GPU.HealthyCount = 3
	claim.Status.Machine = &unboundedv1alpha3.BoundMachineReference{Name: machine.Name, UID: machine.UID}
	reconciler := newCapacityTestReconciler(t, claim, machine, testConfigurationVersion())

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: claim.Name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got unboundedv1alpha3.CapacityClaim
	if err := reconciler.Get(context.Background(), client.ObjectKey{Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}

	if got.Status.Phase != unboundedv1alpha3.CapacityClaimPhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}

	var gotMachine unboundedv1alpha3.Machine
	if err := reconciler.Get(context.Background(), client.ObjectKey{Name: machine.Name}, &gotMachine); err != nil {
		t.Fatalf("get Machine: %v", err)
	}

	if !apimeta.IsStatusConditionTrue(gotMachine.Status.Conditions, unboundedv1alpha3.MachineConditionQuarantined) {
		t.Fatal("GPU-unhealthy Machine was not quarantined")
	}
}

func TestCapacityClaimRequiresReadyNodeInternalIP(t *testing.T) {
	claim := testCapacityClaim("claim-node-ip")
	claim.Finalizers = []string{unboundedv1alpha3.CapacityClaimFinalizer}
	machine := testMachine("gb200-1", true, false)
	machine.Status.Reservation = &unboundedv1alpha3.MachineReservationStatus{
		ClaimName: claim.Name, ClaimUID: claim.UID, Domain: "hillsboro",
	}
	machine.Status.Allocation.Node.InternalIP = ""
	claim.Status.Machine = &unboundedv1alpha3.BoundMachineReference{Name: machine.Name, UID: machine.UID}
	reconciler := newCapacityTestReconciler(t, claim, machine, testConfigurationVersion())

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: claim.Name},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got unboundedv1alpha3.CapacityClaim
	if err := reconciler.Get(context.Background(), client.ObjectKey{Name: claim.Name}, &got); err != nil {
		t.Fatalf("get claim: %v", err)
	}

	if got.Status.Phase != unboundedv1alpha3.CapacityClaimPhaseProvisioning {
		t.Fatalf("phase = %s, want Provisioning", got.Status.Phase)
	}
}

func TestNVIDIAPolicyRequiresExactTrustedImage(t *testing.T) {
	claim := testCapacityClaim("claim-image")
	claim.Spec.ImagePolicy.RequiredImage = ""

	if err := validateCapacityClaim(claim); err == nil {
		t.Fatal("RequireNVIDIA without requiredImage passed validation")
	}

	claim.Spec.ImagePolicy.RequiredImage = "example.com/worker:trusted"
	if err := validateConfiguredImage(claim, "example.com/nvidia-looking:latest"); err == nil {
		t.Fatal("substring-matching image passed exact validation")
	}

	if err := validateConfiguredImage(claim, "example.com/worker:trusted"); err != nil {
		t.Fatalf("exact trusted image rejected: %v", err)
	}
}

func TestNodeObservationTreatsDHCPAddressAsMutableState(t *testing.T) {
	machine := testMachine("gb200-2", false, false)
	machine.Labels["unbounded-cloud.io/serial"] = "serial-2426"
	machine.Labels["unbounded-cloud.io/mac"] = "02:00:00:00:00:02"
	machine.Status.Agent = &unboundedv1alpha3.AgentStatus{Image: testNVIDIAImage}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: machine.Name,
			Labels: map[string]string{
				unboundedv1alpha3.MachineGPUVendorLabelKey:        "NVIDIA",
				unboundedv1alpha3.MachineGPUModelLabelKey:         "GB200",
				unboundedv1alpha3.MachineGPUFabricHealthyLabelKey: "true",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.10"}},
			Capacity: corev1.ResourceList{
				corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("4"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("4"),
			},
		},
	}

	observeNodeAllocation(machine, node)
	node.Status.Addresses[0].Address = "192.0.2.11"
	observeNodeAllocation(machine, node)

	if got := machine.Status.Allocation.Node.InternalIP; got != "192.0.2.11" {
		t.Fatalf("observed internal IP = %q", got)
	}

	if machine.Labels["unbounded-cloud.io/serial"] != "serial-2426" ||
		machine.Labels["unbounded-cloud.io/mac"] != "02:00:00:00:00:02" {
		t.Fatalf("immutable identity labels changed: %#v", machine.Labels)
	}
}

func TestCapacityClaimReleaseStagesAreOrderedAndBarriered(t *testing.T) {
	claim := testCapacityClaim("claim-release")
	claim.Finalizers = []string{unboundedv1alpha3.CapacityClaimFinalizer}
	machine := testMachine("gb200-1", false, false)
	machine.Status.Reservation = &unboundedv1alpha3.MachineReservationStatus{
		ClaimName: claim.Name, ClaimUID: claim.UID, Domain: "hillsboro",
	}
	claim.Status.Machine = &unboundedv1alpha3.BoundMachineReference{Name: machine.Name, UID: machine.UID}
	lifecycle := &recordingCapacityLifecycle{failAt: unboundedv1alpha3.CapacityReleaseStageResetWorker}
	reconciler := newCapacityTestReconciler(t, claim, machine, testConfigurationVersion())
	reconciler.Lifecycle = lifecycle

	ctx := context.Background()

	for range 3 {
		var current unboundedv1alpha3.CapacityClaim
		if err := reconciler.Get(ctx, client.ObjectKey{Name: claim.Name}, &current); err != nil {
			t.Fatalf("get claim: %v", err)
		}

		_, err := reconciler.reconcileRelease(ctx, &current)
		if errors.Is(err, errReleaseBarrier) {
			break
		}

		if err != nil {
			t.Fatalf("release reconcile: %v", err)
		}
	}

	want := []unboundedv1alpha3.CapacityReleaseStage{
		unboundedv1alpha3.CapacityReleaseStageCordonDrain,
		unboundedv1alpha3.CapacityReleaseStageRevokeCredentials,
		unboundedv1alpha3.CapacityReleaseStageResetWorker,
	}
	if len(lifecycle.calls) != len(want) {
		t.Fatalf("release calls = %v, want %v", lifecycle.calls, want)
	}

	for i := range want {
		if lifecycle.calls[i] != want[i] {
			t.Fatalf("release calls = %v, want %v", lifecycle.calls, want)
		}
	}

	var gotMachine unboundedv1alpha3.Machine
	if err := reconciler.Get(ctx, client.ObjectKey{Name: machine.Name}, &gotMachine); err != nil {
		t.Fatalf("get Machine: %v", err)
	}

	if gotMachine.Status.Reservation == nil {
		t.Fatal("reservation cleared before all release barriers completed")
	}
}

func TestCapacityClaimReleaseCompletesBeforeReservationClears(t *testing.T) {
	claim := testCapacityClaim("claim-release-complete")
	claim.Finalizers = []string{unboundedv1alpha3.CapacityClaimFinalizer}
	machine := testMachine("gb200-1", true, false)
	machine.Status.Reservation = &unboundedv1alpha3.MachineReservationStatus{
		ClaimName: claim.Name, ClaimUID: claim.UID, Domain: "hillsboro",
	}
	claim.Status.Machine = &unboundedv1alpha3.BoundMachineReference{Name: machine.Name, UID: machine.UID}
	lifecycle := &recordingCapacityLifecycle{}
	reconciler := newCapacityTestReconciler(t, claim, machine, testConfigurationVersion())
	reconciler.Lifecycle = lifecycle

	ctx := context.Background()

	for range 6 {
		var current unboundedv1alpha3.CapacityClaim
		if err := reconciler.Get(ctx, client.ObjectKey{Name: claim.Name}, &current); err != nil {
			t.Fatalf("get claim: %v", err)
		}

		if _, err := reconciler.reconcileRelease(ctx, &current); err != nil {
			t.Fatalf("release reconcile: %v", err)
		}
	}

	var gotMachine unboundedv1alpha3.Machine
	if err := reconciler.Get(ctx, client.ObjectKey{Name: machine.Name}, &gotMachine); err != nil {
		t.Fatalf("get Machine: %v", err)
	}

	if gotMachine.Status.Reservation != nil {
		t.Fatalf("reservation remains after completed barriers: %#v", gotMachine.Status.Reservation)
	}

	if gotMachine.Status.Allocation != nil {
		t.Fatalf("stale allocation remains after reset/delete: %#v", gotMachine.Status.Allocation)
	}

	if apimeta.IsStatusConditionTrue(gotMachine.Status.Conditions, unboundedv1alpha3.MachineConditionAllocationReady) {
		t.Fatal("AllocationReady remained true after reset/delete")
	}

	available := apimeta.FindStatusCondition(gotMachine.Status.Conditions, unboundedv1alpha3.MachineConditionAvailable)
	if available == nil || available.Status != metav1.ConditionTrue {
		t.Fatalf("Available condition = %#v, want True", available)
	}

	var gotClaim unboundedv1alpha3.CapacityClaim
	if err := reconciler.Get(ctx, client.ObjectKey{Name: claim.Name}, &gotClaim); err != nil {
		t.Fatalf("get claim: %v", err)
	}

	if containsString(gotClaim.Finalizers, unboundedv1alpha3.CapacityClaimFinalizer) {
		t.Fatal("CapacityClaim finalizer remains after completed release")
	}
}

func TestMemoryDomainLockerReportsContention(t *testing.T) {
	locker := &MemoryDomainLocker{}
	entered := make(chan struct{})
	release := make(chan struct{})

	go func() {
		_ = locker.WithLock(context.Background(), "hillsboro", func(context.Context) error {
			close(entered)
			<-release

			return nil
		})
	}()

	<-entered

	err := locker.WithLock(context.Background(), "hillsboro", func(context.Context) error {
		t.Fatal("contended lock executed callback")

		return nil
	})

	close(release)

	if !errors.Is(err, ErrDomainLocked) {
		t.Fatalf("lock error = %v, want ErrDomainLocked", err)
	}
}

func TestLeaseExpiration(t *testing.T) {
	holder := "holder-a"
	other := "holder-b"
	duration := int32(10)
	renew := metav1.NewMicroTime(time.Unix(100, 0))
	lease := &coordinationv1.Lease{
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &other,
			LeaseDurationSeconds: &duration,
			RenewTime:            &renew,
		},
	}

	if leaseAvailable(lease, holder, time.Unix(105, 0)) {
		t.Fatal("unexpired Lease reported available")
	}

	if !leaseAvailable(lease, holder, time.Unix(111, 0)) {
		t.Fatal("expired Lease reported unavailable")
	}

	if got := capacityDomainLeaseName("Hillsboro/Rack A"); got != "capacityclaim-hillsboro-rack-a" {
		t.Fatalf("lease name = %q", got)
	}
}

func TestKubernetesDomainLockerHonorsContentionAndExpiration(t *testing.T) {
	testScheme := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(testScheme); err != nil {
		t.Fatalf("add coordination scheme: %v", err)
	}

	other := "other-holder"
	duration := int32(10)
	renew := metav1.NewMicroTime(time.Unix(100, 0))
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      capacityDomainLeaseName("hillsboro"),
			Namespace: "unbounded-system",
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &other,
			LeaseDurationSeconds: &duration,
			RenewTime:            &renew,
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(lease).Build()
	locker := &KubernetesDomainLocker{
		Client:         fakeClient,
		Namespace:      "unbounded-system",
		HolderIdentity: "this-holder",
		LeaseDuration:  30 * time.Second,
		Now:            func() time.Time { return time.Unix(105, 0) },
	}

	called := false

	err := locker.WithLock(context.Background(), "hillsboro", func(context.Context) error {
		called = true

		return nil
	})
	if !errors.Is(err, ErrDomainLocked) {
		t.Fatalf("unexpired lock error = %v, want ErrDomainLocked", err)
	}

	if called {
		t.Fatal("callback ran while another holder owned an unexpired Lease")
	}

	locker.Now = func() time.Time { return time.Unix(111, 0) }
	if err := locker.WithLock(context.Background(), "hillsboro", func(context.Context) error {
		called = true

		return nil
	}); err != nil {
		t.Fatalf("take expired Lease: %v", err)
	}

	if !called {
		t.Fatal("callback did not run after Lease expiration")
	}
}

func TestKubernetesDomainLockerSerializesSameProcessCalls(t *testing.T) {
	testScheme := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(testScheme); err != nil {
		t.Fatalf("add coordination scheme: %v", err)
	}

	fakeClient := fake.NewClientBuilder().WithScheme(testScheme).Build()
	locker := &KubernetesDomainLocker{
		Client:         fakeClient,
		Namespace:      "unbounded-system",
		HolderIdentity: "same-process-holder",
		LeaseDuration:  30 * time.Second,
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- locker.WithLock(context.Background(), "hillsboro", func(context.Context) error {
			close(entered)
			<-release

			return nil
		})
	}()

	<-entered

	overlapped := false
	err := locker.WithLock(context.Background(), "hillsboro", func(context.Context) error {
		overlapped = true

		return nil
	})

	close(release)

	if !errors.Is(err, ErrDomainLocked) {
		t.Fatalf("second lock error = %v, want ErrDomainLocked", err)
	}

	if overlapped {
		t.Fatal("same-process Kubernetes Lease callbacks overlapped")
	}

	if err := <-done; err != nil {
		t.Fatalf("first lock: %v", err)
	}
}

func TestConfigurationEventsMapAffectedClaims(t *testing.T) {
	version := int32(2)
	latest := testCapacityClaim("latest")
	latest.Spec.ConfigurationRef.Version = nil
	pinned := testCapacityClaim("pinned")
	pinned.Spec.ConfigurationRef.Version = &version
	other := testCapacityClaim("other")
	other.Spec.ConfigurationRef.Name = "other-config"
	reconciler := newCapacityTestReconciler(t, latest, pinned, other)

	requests := reconciler.findClaimsForConfiguration(context.Background(), &unboundedv1alpha3.MachineConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "hillsboro-gb200"},
	})
	if len(requests) != 2 {
		t.Fatalf("MachineConfiguration requests = %v, want 2", requests)
	}

	requests = reconciler.findClaimsForConfigurationVersion(context.Background(), &unboundedv1alpha3.MachineConfigurationVersion{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			unboundedv1alpha3.MCVConfigurationLabelKey: "hillsboro-gb200",
		}},
		Spec: unboundedv1alpha3.MachineConfigurationVersionSpec{Version: 1},
	})
	if len(requests) != 1 || requests[0].Name != latest.Name {
		t.Fatalf("MachineConfigurationVersion requests = %v, want latest claim", requests)
	}
}

var errReleaseBarrier = errors.New("release barrier")

type recordingCapacityLifecycle struct {
	calls          []unboundedv1alpha3.CapacityReleaseStage
	failAt         unboundedv1alpha3.CapacityReleaseStage
	provisionCalls int
	provisionErr   error
}

func (l *recordingCapacityLifecycle) record(stage unboundedv1alpha3.CapacityReleaseStage) error {
	l.calls = append(l.calls, stage)
	if stage == l.failAt {
		return errReleaseBarrier
	}

	return nil
}

func (l *recordingCapacityLifecycle) ProvisionWorker(context.Context, *unboundedv1alpha3.Machine) error {
	l.provisionCalls++

	return l.provisionErr
}

func (l *recordingCapacityLifecycle) CordonDrain(context.Context, *unboundedv1alpha3.Machine) error {
	return l.record(unboundedv1alpha3.CapacityReleaseStageCordonDrain)
}

func (l *recordingCapacityLifecycle) RevokeCredentials(context.Context, *unboundedv1alpha3.Machine) error {
	return l.record(unboundedv1alpha3.CapacityReleaseStageRevokeCredentials)
}

func (l *recordingCapacityLifecycle) ResetWorker(context.Context, *unboundedv1alpha3.Machine) error {
	return l.record(unboundedv1alpha3.CapacityReleaseStageResetWorker)
}

func (l *recordingCapacityLifecycle) DeleteNode(context.Context, *unboundedv1alpha3.Machine) error {
	return l.record(unboundedv1alpha3.CapacityReleaseStageDeleteNode)
}

func newCapacityTestReconciler(t *testing.T, objects ...client.Object) *CapacityClaimReconciler {
	t.Helper()

	testScheme := runtime.NewScheme()
	if err := unboundedv1alpha3.AddToScheme(testScheme); err != nil {
		t.Fatalf("add Machina scheme: %v", err)
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&unboundedv1alpha3.CapacityClaim{}, &unboundedv1alpha3.Machine{}).
		WithObjects(objects...).
		Build()

	return &CapacityClaimReconciler{
		Client:    fakeClient,
		Scheme:    testScheme,
		Locker:    &MemoryDomainLocker{},
		Lifecycle: &recordingCapacityLifecycle{},
	}
}

func reconcileTwice(t *testing.T, reconciler *CapacityClaimReconciler, name string) {
	t.Helper()

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
	for range 2 {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
	}
}

func testCapacityClaim(name string) *unboundedv1alpha3.CapacityClaim {
	version := int32(1)

	return &unboundedv1alpha3.CapacityClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid")},
		Spec: unboundedv1alpha3.CapacityClaimSpec{
			MachineSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				"unbounded-cloud.io/hardware-class": "gb200",
			}},
			SiteSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				unboundedv1alpha3.MachineSiteLabelKey: "hillsboro",
			}},
			DomainSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				unboundedv1alpha3.MachineCapacityDomainLabelKey: "hillsboro",
			}},
			Consumer: unboundedv1alpha3.CapacityConsumerReference{
				APIGroup: "karpenter.sh", Kind: "NodeClaim", Name: name + "-consumer", UID: types.UID(name + "-consumer-uid"),
			},
			ConfigurationRef: unboundedv1alpha3.MachineConfigurationRef{Name: "hillsboro-gb200", Version: &version},
			ImagePolicy: unboundedv1alpha3.CapacityImagePolicy{
				RequiredImage: testNVIDIAImage,
				RequireNVIDIA: true,
			},
			GPU: &unboundedv1alpha3.CapacityGPUPolicy{
				Vendor: "NVIDIA", Model: "GB200", Count: 4, RequireFabric: true,
			},
		},
	}
}

func testMachine(name string, allocationReady bool, quarantined bool) *unboundedv1alpha3.Machine {
	conditions := []metav1.Condition{
		{Type: unboundedv1alpha3.MachineConditionAvailable, Status: metav1.ConditionTrue, Reason: "InventoryReady"},
		{Type: unboundedv1alpha3.MachineConditionAllocationReady, Status: metav1.ConditionFalse, Reason: "Blocked"},
		{Type: unboundedv1alpha3.MachineConditionQuarantined, Status: metav1.ConditionFalse, Reason: "Healthy"},
	}
	if allocationReady {
		conditions[1].Status = metav1.ConditionTrue
		conditions[1].Reason = "Observed"
	}

	if quarantined {
		conditions[2].Status = metav1.ConditionTrue
		conditions[2].Reason = "OperatorBlocked"
	}

	return &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(name + "-uid"),
			Labels: map[string]string{
				"unbounded-cloud.io/hardware-class":             "gb200",
				unboundedv1alpha3.MachineSiteLabelKey:           "hillsboro",
				unboundedv1alpha3.MachineCapacityDomainLabelKey: "hillsboro",
			},
		},
		Status: unboundedv1alpha3.MachineStatus{
			Conditions: conditions,
			Allocation: &unboundedv1alpha3.MachineAllocationStatus{
				Node: &unboundedv1alpha3.MachineNodeObservation{
					Name: name, Ready: true, InternalIP: "192.0.2.10", Image: testNVIDIAImage,
				},
				GPU: &unboundedv1alpha3.MachineGPUObservation{
					Vendor: "NVIDIA", Model: "GB200", Count: 4, HealthyCount: 4, FabricHealthy: true,
				},
			},
		},
	}
}

func testConfigurationVersion() *unboundedv1alpha3.MachineConfigurationVersion {
	return &unboundedv1alpha3.MachineConfigurationVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: unboundedv1alpha3.MachineConfigurationVersionName("hillsboro-gb200", 1),
			Labels: map[string]string{
				unboundedv1alpha3.MCVConfigurationLabelKey: "hillsboro-gb200",
			},
		},
		Spec: unboundedv1alpha3.MachineConfigurationVersionSpec{
			Version: 1,
			Template: unboundedv1alpha3.MachineConfigurationTemplate{
				Agent: &unboundedv1alpha3.MachineConfigurationAgent{Image: testNVIDIAImage},
			},
		},
	}
}
