// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package karpenterprovider

import (
	"context"
	"errors"
	"testing"

	"github.com/awslabs/operatorpkg/status"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpenterv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	karpenterv1alpha1 "github.com/Azure/unbounded/api/karpenter/v1alpha1"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const testImage = "ghcr.io/azure/unbounded-gb200@sha256:1111111111111111111111111111111111111111111111111111111111111111"

type fakeLifecycle struct {
	provisionErr error
	revokeErr    error
	resetErr     error
	deleteErr    error
	calls        []string
}

func (f *fakeLifecycle) ProvisionWorker(context.Context, *machinav1alpha3.Machine) error {
	f.calls = append(f.calls, "provision")

	return f.provisionErr
}

func (f *fakeLifecycle) RevokeCredentials(context.Context, *machinav1alpha3.Machine) error {
	f.calls = append(f.calls, "revoke")

	return f.revokeErr
}

func (f *fakeLifecycle) ResetWorker(context.Context, *machinav1alpha3.Machine) error {
	f.calls = append(f.calls, "reset")

	return f.resetErr
}

func (f *fakeLifecycle) DeleteNode(context.Context, *machinav1alpha3.Machine) error {
	f.calls = append(f.calls, "delete")

	return f.deleteErr
}

func TestStableProviderIDIgnoresDHCPAddress(t *testing.T) {
	machine := readyMachine(t, "machine-a")
	first, err := StableProviderID(machine)
	require.NoError(t, err)

	machine.Spec.SSH.Host = "192.0.2.44"
	machine.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.45"
	second, err := StableProviderID(machine)
	require.NoError(t, err)
	require.Equal(t, first, second)

	machine.Spec.Identity = nil
	machine.Spec.Host = nil
	machine.Spec.PXE = nil
	machine.Spec.ProviderID = ""
	_, err = StableProviderID(machine)
	require.ErrorContains(t, err, "immutable host ownership identity")
}

func TestGetInstanceTypesReportsUnavailableCapacity(t *testing.T) {
	nodeClass, configVersion := fixtures()
	kubeClient := newFakeClient(t, nodeClass, configVersion)
	provider := New(kubeClient, &MemoryDomainLocker{}, &fakeLifecycle{})
	nodePool := testNodePool(nodeClass.Name)

	instanceTypes, err := provider.GetInstanceTypes(t.Context(), nodePool)
	require.NoError(t, err)
	require.Len(t, instanceTypes, 1)
	require.Equal(t, "hillsboro-gb200-4g", instanceTypes[0].Name)
	gpuCapacity := instanceTypes[0].Capacity[corev1.ResourceName("nvidia.com/gpu")]
	require.Equal(t, int64(4), gpuCapacity.Value())
	require.False(t, instanceTypes[0].Offerings[0].Available)
	require.Zero(t, instanceTypes[0].Offerings[0].ReservationCapacity)
}

func TestGetInstanceTypesReportsFreeReservedInventory(t *testing.T) {
	nodeClass, configVersion := fixtures()
	kubeClient := newFakeClient(t, nodeClass, configVersion, readyMachine(t, "machine-a"))
	provider := New(kubeClient, &MemoryDomainLocker{}, &fakeLifecycle{})

	instanceTypes, err := provider.GetInstanceTypes(t.Context(), testNodePool(nodeClass.Name))
	require.NoError(t, err)
	require.True(t, instanceTypes[0].Offerings[0].Available)
	require.Equal(t, 1, instanceTypes[0].Offerings[0].ReservationCapacity)
}

func TestCreateReservesHydratesAndRecovers(t *testing.T) {
	nodeClass, configVersion := fixtures()
	machine := readyMachine(t, "machine-a")
	machine.Spec.Kubernetes = &machinav1alpha3.KubernetesSpec{
		NodeLabels: map[string]string{"stale.example.com/owner": "old-claim"},
	}
	kubeClient := newFakeClient(t, nodeClass, configVersion, machine)
	lifecycle := &fakeLifecycle{}
	provider := New(kubeClient, &MemoryDomainLocker{}, lifecycle)
	nodeClaim := testNodeClaim(nodeClass.Name, "claim-a", "claim-a-uid")
	nodeClaim.Spec.Taints = []corev1.Taint{{
		Key: "dedicated", Value: "gb200", Effect: corev1.TaintEffectNoSchedule,
	}}
	nodeClaim.Spec.StartupTaints = []corev1.Taint{{
		Key: "node.cilium.io/agent-not-ready", Effect: corev1.TaintEffectNoExecute,
	}}

	hydrated, err := provider.Create(t.Context(), nodeClaim)
	require.NoError(t, err)
	require.Equal(t, "hillsboro-gb200-4g", hydrated.Labels[corev1.LabelInstanceTypeStable])
	require.Equal(t, testImage, hydrated.Status.ImageID)
	require.Equal(t, resource.MustParse("4"), hydrated.Status.Allocatable[corev1.ResourceName("nvidia.com/gpu")])
	require.Empty(t, lifecycle.calls)

	var reserved machinav1alpha3.Machine
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKey{Name: machine.Name}, &reserved))
	require.Equal(t, nodeClaim.UID, reserved.Status.Reservation.NodeClaimUID)
	require.Equal(t, hydrated.Status.ProviderID, reserved.Spec.Kubernetes.ProviderID)
	require.Equal(t, []string{
		"dedicated=gb200:NoSchedule",
		"node.cilium.io/agent-not-ready:NoExecute",
	}, reserved.Spec.Kubernetes.RegisterWithTaints)
	require.NotContains(t, reserved.Spec.Kubernetes.NodeLabels, "stale.example.com/owner")

	duplicate, err := provider.Create(t.Context(), nodeClaim.DeepCopy())
	require.NoError(t, err)
	require.Equal(t, hydrated.Status.ProviderID, duplicate.Status.ProviderID)

	got, err := provider.Get(t.Context(), hydrated.Status.ProviderID)
	require.NoError(t, err)
	require.Equal(t, nodeClaim.Name, got.Name)
	listed, err := provider.List(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)

	restarted := New(kubeClient, &MemoryDomainLocker{}, lifecycle)
	recovered, err := restarted.Get(t.Context(), hydrated.Status.ProviderID)
	require.NoError(t, err)
	require.Equal(t, nodeClaim.UID, recovered.UID)

	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKey{Name: machine.Name}, &reserved))
	reserved.Spec.Kubernetes = nil
	require.NoError(t, kubeClient.Update(t.Context(), &reserved))
	recovered, err = restarted.Create(t.Context(), nodeClaim.DeepCopy())
	require.NoError(t, err)
	require.Equal(t, hydrated.Status.ProviderID, recovered.Status.ProviderID)
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKey{Name: machine.Name}, &reserved))
	require.Equal(t, hydrated.Status.ProviderID, reserved.Spec.Kubernetes.ProviderID)

	require.NoError(t, kubeClient.Delete(t.Context(), nodeClass))
	recovered, err = restarted.Create(t.Context(), nodeClaim.DeepCopy())
	require.NoError(t, err)
	require.Equal(t, hydrated.Status.ProviderID, recovered.Status.ProviderID)
	recovered, err = restarted.Get(t.Context(), hydrated.Status.ProviderID)
	require.NoError(t, err)
	require.Equal(t, nodeClaim.UID, recovered.UID)
	require.Equal(t, nodeClaim.Spec.Taints, recovered.Spec.Taints)
	require.Equal(t, nodeClaim.Spec.StartupTaints, recovered.Spec.StartupTaints)
	listed, err = restarted.List(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)
}

func TestCreateFailsClosedForGPUHealthAndUnavailableLifecycle(t *testing.T) {
	tests := map[string]func(*machinav1alpha3.Machine){
		"allocatable GPU count": func(machine *machinav1alpha3.Machine) {
			machine.Status.Allocation.GPU.HealthyCount = 3
		},
		"fabric health": func(machine *machinav1alpha3.Machine) {
			machine.Status.Allocation.GPU.FabricHealthy = false
		},
		"worker image": func(machine *machinav1alpha3.Machine) {
			machine.Status.Allocation.Node.Image = "ghcr.io/azure/incorrect:v1"
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			nodeClass, configVersion := fixtures()
			machine := readyMachine(t, "machine-a")
			mutate(machine)
			kubeClient := newFakeClient(t, nodeClass, configVersion, machine)
			provider := New(kubeClient, &MemoryDomainLocker{}, UnavailableLifecycle{})

			_, err := provider.Create(t.Context(), testNodeClaim(nodeClass.Name, "claim-a", "claim-a-uid"))
			require.ErrorIs(t, err, ErrLifecycleUnavailable)

			var reserved machinav1alpha3.Machine
			require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKey{Name: machine.Name}, &reserved))
			require.NotNil(t, reserved.Status.Reservation)
		})
	}
}

func TestCreateSkipsQuarantinedAndConflictingMachines(t *testing.T) {
	nodeClass, configVersion := fixtures()
	quarantined := readyMachine(t, "machine-a")
	setCondition(quarantined, machinav1alpha3.MachineConditionQuarantined, metav1.ConditionTrue)

	reserved := readyMachine(t, "machine-b")
	reserved.Status.Reservation = &machinav1alpha3.MachineReservationStatus{
		NodeClaimName: "other",
		NodeClaimUID:  "other-uid",
		ProviderID:    "unbounded://other",
		Domain:        "hillsboro",
	}
	kubeClient := newFakeClient(t, nodeClass, configVersion, quarantined, reserved)
	provider := New(kubeClient, &MemoryDomainLocker{}, &fakeLifecycle{})

	_, err := provider.Create(t.Context(), testNodeClaim(nodeClass.Name, "claim-a", "claim-a-uid"))

	var insufficient *cloudprovider.InsufficientCapacityError
	require.ErrorAs(t, err, &insufficient)
}

func TestDeletePersistsStagesAndReturnsNotFoundAfterRelease(t *testing.T) {
	nodeClass, configVersion := fixtures()
	machine := readyMachine(t, "machine-a")
	kubeClient := newFakeClient(t, nodeClass, configVersion, machine)
	lifecycle := &fakeLifecycle{}
	provider := New(kubeClient, &MemoryDomainLocker{}, lifecycle)
	nodeClaim := testNodeClaim(nodeClass.Name, "claim-a", "claim-a-uid")
	_, err := provider.Create(t.Context(), nodeClaim)
	require.NoError(t, err)

	for range 4 {
		require.NoError(t, provider.Delete(t.Context(), nodeClaim))
	}

	require.Equal(t, []string{"revoke", "reset", "delete"}, lifecycle.calls)

	err = provider.Delete(t.Context(), nodeClaim)

	var notFound *cloudprovider.NodeClaimNotFoundError
	require.ErrorAs(t, err, &notFound)
}

func TestDeleteDoesNotAdvanceFailedBarrier(t *testing.T) {
	nodeClass, configVersion := fixtures()
	machine := readyMachine(t, "machine-a")
	kubeClient := newFakeClient(t, nodeClass, configVersion, machine)
	lifecycle := &fakeLifecycle{revokeErr: errors.New("revoke unavailable")}
	provider := New(kubeClient, &MemoryDomainLocker{}, lifecycle)
	nodeClaim := testNodeClaim(nodeClass.Name, "claim-a", "claim-a-uid")
	_, err := provider.Create(t.Context(), nodeClaim)
	require.NoError(t, err)

	err = provider.Delete(t.Context(), nodeClaim)
	require.ErrorContains(t, err, "revoke unavailable")

	var current machinav1alpha3.Machine
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKey{Name: machine.Name}, &current))
	require.Equal(t, machinav1alpha3.MachineReleaseStageRevokeCredentials, current.Status.Reservation.ReleaseStage)
}

func TestMemoryDomainLockerRejectsContention(t *testing.T) {
	locker := &MemoryDomainLocker{}
	err := locker.WithLock(t.Context(), "hillsboro", func(ctx context.Context) error {
		return locker.WithLock(ctx, "hillsboro", func(context.Context) error { return nil })
	})
	require.ErrorIs(t, err, ErrDomainLocked)
}

func TestCreateFailsClosedWhenDomainIsLocked(t *testing.T) {
	nodeClass, configVersion := fixtures()
	machine := readyMachine(t, "machine-a")
	provider := New(
		newFakeClient(t, nodeClass, configVersion, machine),
		domainLockerFunc(func(context.Context, string, func(context.Context) error) error {
			return ErrDomainLocked
		}),
		&fakeLifecycle{},
	)

	_, err := provider.Create(t.Context(), testNodeClaim(nodeClass.Name, "claim-a", "claim-a-uid"))

	var insufficient *cloudprovider.InsufficientCapacityError
	require.ErrorAs(t, err, &insufficient)
}

func TestDuplicateReservationsAreQuarantined(t *testing.T) {
	nodeClass, configVersion := fixtures()
	first := readyMachine(t, "machine-a")

	second := readyMachine(t, "machine-b")
	for _, machine := range []*machinav1alpha3.Machine{first, second} {
		machine.Status.Reservation = &machinav1alpha3.MachineReservationStatus{
			NodeClaimName: "claim-a",
			NodeClaimUID:  "claim-a-uid",
			ProviderID:    "unbounded://" + machine.Name,
			Domain:        "hillsboro",
		}
	}

	kubeClient := newFakeClient(t, nodeClass, configVersion, first, second)
	provider := New(kubeClient, &MemoryDomainLocker{}, &fakeLifecycle{})
	_, err := provider.Create(t.Context(), testNodeClaim(nodeClass.Name, "claim-a", "claim-a-uid"))
	require.ErrorContains(t, err, "reserved on multiple Machines")

	for _, name := range []string{first.Name, second.Name} {
		var machine machinav1alpha3.Machine
		require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKey{Name: name}, &machine))
		require.True(t, conditionTrue(machine.Status.Conditions, machinav1alpha3.MachineConditionQuarantined))
	}
}

func TestDuplicatePhysicalIdentitiesAreQuarantined(t *testing.T) {
	nodeClass, configVersion := fixtures()
	first := readyMachine(t, "machine-a")
	second := readyMachine(t, "machine-b")
	second.Spec.Identity = first.Spec.Identity.DeepCopy()

	kubeClient := newFakeClient(t, nodeClass, configVersion, first, second)
	provider := New(kubeClient, &MemoryDomainLocker{}, &fakeLifecycle{})
	_, err := provider.Create(t.Context(), testNodeClaim(nodeClass.Name, "claim-a", "claim-a-uid"))
	require.ErrorContains(t, err, "immutable provider identity is duplicated")

	for _, name := range []string{first.Name, second.Name} {
		var machine machinav1alpha3.Machine
		require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKey{Name: name}, &machine))
		require.True(t, conditionTrue(machine.Status.Conditions, machinav1alpha3.MachineConditionQuarantined))
		require.Nil(t, machine.Status.Reservation)
	}
}

func TestNodeClassValidationRejectsUnsafeGB200(t *testing.T) {
	nodeClass, configVersion := fixtures()
	nodeClass.Spec.ImagePolicy.RequireNVIDIA = false
	reconciler := &NodeClassReconciler{Client: newFakeClient(t, nodeClass, configVersion)}
	err := reconciler.validate(t.Context(), nodeClass)
	require.ErrorContains(t, err, "GPU capacity requires an NVIDIA image policy")
}

func TestNodeClassReconcilePublishesReadyCondition(t *testing.T) {
	nodeClass, configVersion := fixtures()
	nodeClass.Status.Conditions = nil
	kubeClient := newFakeClient(t, nodeClass, configVersion)
	reconciler := &NodeClassReconciler{Client: kubeClient}

	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: nodeClass.Name},
	})
	require.NoError(t, err)

	var reconciled karpenterv1alpha1.UnboundedNodeClass
	require.NoError(t, kubeClient.Get(t.Context(), client.ObjectKey{Name: nodeClass.Name}, &reconciled))
	require.True(t, reconciled.StatusConditions().Get(status.ConditionReady).IsTrue())
}

func fixtures() (*karpenterv1alpha1.UnboundedNodeClass, *machinav1alpha3.MachineConfigurationVersion) {
	version := int32(1)
	nodeClass := &karpenterv1alpha1.UnboundedNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "hillsboro-gb200", Generation: 3},
		Spec: karpenterv1alpha1.UnboundedNodeClassSpec{
			MachineSelector: metav1.LabelSelector{MatchLabels: map[string]string{"unbounded-cloud.io/rack": "gb200"}},
			Site:            "hillsboro",
			Domain:          "hillsboro",
			ConfigurationRef: machinav1alpha3.MachineConfigurationRef{
				Name:    "hillsboro-gb200",
				Version: &version,
			},
			ImagePolicy: karpenterv1alpha1.ImagePolicy{RequiredImage: testImage, RequireNVIDIA: true},
			InstanceTypes: []karpenterv1alpha1.InstanceType{{
				Name:            "hillsboro-gb200-4g",
				Architecture:    "arm64",
				OperatingSystem: "linux",
				Zone:            "hillsboro",
				CapacityType:    karpenterv1.CapacityTypeReserved,
				Capacity: corev1.ResourceList{
					corev1.ResourceCPU:                    resource.MustParse("80"),
					corev1.ResourceMemory:                 resource.MustParse("512Gi"),
					corev1.ResourcePods:                   resource.MustParse("110"),
					corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("4"),
				},
				GPU: &karpenterv1alpha1.GPUPolicy{Vendor: "NVIDIA", Model: "GB200", Count: 4, RequireFabric: true},
			}},
		},
	}
	nodeClass.StatusConditions().SetTrueWithReason(status.ConditionReady, "Validated", "ready")

	configVersion := &machinav1alpha3.MachineConfigurationVersion{
		ObjectMeta: metav1.ObjectMeta{Name: machinav1alpha3.MachineConfigurationVersionName("hillsboro-gb200", version)},
		Spec: machinav1alpha3.MachineConfigurationVersionSpec{
			Version: version,
			Template: machinav1alpha3.MachineConfigurationTemplate{
				Agent: &machinav1alpha3.MachineConfigurationAgent{Image: testImage},
			},
		},
	}

	return nodeClass, configVersion
}

func readyMachine(t *testing.T, name string) *machinav1alpha3.Machine {
	t.Helper()

	machine := &machinav1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(name + "-uid"),
			Labels: map[string]string{
				"unbounded-cloud.io/rack":                       "gb200",
				machinav1alpha3.MachineSiteLabelKey:             "hillsboro",
				machinav1alpha3.MachineCapacityDomainLabelKey:   "hillsboro",
				machinav1alpha3.MachineGPUVendorLabelKey:        "NVIDIA",
				machinav1alpha3.MachineGPUModelLabelKey:         "GB200",
				machinav1alpha3.MachineGPUFabricHealthyLabelKey: "true",
			},
		},
		Spec: machinav1alpha3.MachineSpec{
			Identity: &machinav1alpha3.MachineIdentity{
				SerialNumber: "SERIAL-" + name,
				MACAddress:   "00:11:22:33:44:55",
			},
			SSH: &machinav1alpha3.SSHSpec{Host: "192.0.2.10"},
			Host: &machinav1alpha3.HostSpec{
				Netboot: &machinav1alpha3.PXESpec{
					Image: "host-image",
					DHCPLeases: []machinav1alpha3.DHCPLease{{
						IPv4: "192.0.2.10",
						MAC:  "00:11:22:33:44:55",
					}},
				},
			},
		},
		Status: machinav1alpha3.MachineStatus{
			Agent: &machinav1alpha3.AgentStatus{Image: testImage},
			Allocation: &machinav1alpha3.MachineAllocationStatus{
				Node: &machinav1alpha3.MachineNodeObservation{
					Name:       name,
					Ready:      true,
					InternalIP: "192.0.2.10",
					Image:      testImage,
				},
				GPU: &machinav1alpha3.MachineGPUObservation{
					Vendor:        "NVIDIA",
					Model:         "GB200",
					Count:         4,
					HealthyCount:  4,
					FabricHealthy: true,
				},
			},
		},
	}
	providerID, err := StableProviderID(machine)
	require.NoError(t, err)

	machine.Status.Allocation.Node.ProviderID = providerID
	setCondition(machine, machinav1alpha3.MachineConditionAvailable, metav1.ConditionTrue)
	setCondition(machine, machinav1alpha3.MachineConditionAllocationReady, metav1.ConditionTrue)

	return machine
}

func testNodeClaim(className, name string, uid types.UID) *karpenterv1.NodeClaim {
	return &karpenterv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid},
		Spec: karpenterv1.NodeClaimSpec{
			NodeClassRef: &karpenterv1.NodeClassReference{
				Group: karpenterv1alpha1.GroupVersion.Group,
				Kind:  karpenterv1alpha1.KindUnboundedNodeClass,
				Name:  className,
			},
			Requirements: []karpenterv1.NodeSelectorRequirementWithMinValues{{
				Key:      corev1.LabelInstanceTypeStable,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"hillsboro-gb200-4g"},
			}},
			Resources: karpenterv1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("4"),
			}},
		},
	}
}

func testNodePool(className string) *karpenterv1.NodePool {
	return &karpenterv1.NodePool{
		Spec: karpenterv1.NodePoolSpec{
			Template: karpenterv1.NodeClaimTemplate{
				Spec: karpenterv1.NodeClaimTemplateSpec{
					NodeClassRef: &karpenterv1.NodeClassReference{
						Group: karpenterv1alpha1.GroupVersion.Group,
						Kind:  karpenterv1alpha1.KindUnboundedNodeClass,
						Name:  className,
					},
				},
			},
		},
	}
}

func setCondition(machine *machinav1alpha3.Machine, conditionType string, conditionStatus metav1.ConditionStatus) {
	machine.Status.Conditions = append(machine.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             "Test",
		LastTransitionTime: metav1.Now(),
	})
}

type domainLockerFunc func(context.Context, string, func(context.Context) error) error

func (f domainLockerFunc) WithLock(ctx context.Context, domain string, fn func(context.Context) error) error {
	return f(ctx, domain, fn)
}

func conditionTrue(conditions []metav1.Condition, conditionType string) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == metav1.ConditionTrue
		}
	}

	return false
}

func newFakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, machinav1alpha3.AddToScheme(scheme))
	require.NoError(t, karpenterv1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&machinav1alpha3.Machine{}, &karpenterv1alpha1.UnboundedNodeClass{}).
		WithObjects(objects...).
		Build()
}
