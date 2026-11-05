// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

const (
	allocTestName        = "alloc-1"
	allocTestUID         = types.UID("6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b")
	allocTestConsumer    = "consumer-a"
	allocTestConsumerUID = types.UID("consumer-a-uid")
	allocTestHostImage   = "image-1-planned"
)

var (
	allocTestNow   = classTestNow
	allocTestNames = allocationNamesFor(allocTestUID)
	// allocTestConsumerKind is the stand-in consumer kind: a cluster-scoped
	// kind in the core group.
	allocTestConsumerKind = schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
)

func TestMachineAllocationReconcilerLaunches(t *testing.T) {
	t.Parallel()

	h := newAllocationHarness(t, newAllocationFixture(t))

	// 1. The snapshot is written first, in one status write.
	h.reconcile()
	allocation := h.allocation()
	require.Empty(t, allocation.Finalizers)
	require.Equal(t, []string{"status MachineAllocation alloc-1"}, h.writes())

	snapshot := allocation.Status.Snapshot
	require.NotNil(t, snapshot)
	fixture := newAllocationFixture(t)
	require.Equal(t, fixture.class.UID, snapshot.ClassUID)
	require.Equal(t, fixture.class.Generation, snapshot.ClassGeneration)
	require.Equal(t, fixture.class.Status.ResolvedRevision, snapshot.ClassRevision)
	require.Equal(t, classTestProvider, snapshot.Provider)
	require.Equal(t, classTestSite, snapshot.SiteName)
	require.Equal(t, types.UID("site-a-uid"), snapshot.SiteUID)
	require.Equal(t, unboundedv1alpha3.UIDReference{Name: "cred-a", UID: "cred-a-uid"}, snapshot.Credential)
	require.Equal(t, "spot-small", snapshot.Offering.Name)
	require.Equal(t, *fixture.class.Status.ProviderConfig, snapshot.ProviderConfig)
	require.Equal(t, fixture.class.Status.BootstrapFiles, snapshot.BootstrapFiles)
	require.Equal(t, fixture.class.Spec.Bootstrap.AgentArtifact, snapshot.AgentArtifact)
	require.Equal(t, *fixture.class.Status.Configuration, snapshot.Configuration)
	require.Equal(t, fixture.version.Spec.Template, snapshot.ConfigurationTemplate)
	require.Equal(t, allocTestHostImage, snapshot.HostImage)
	require.Equal(t, "example:///hosts/"+allocTestNames.machine, snapshot.HostProviderID)

	status := allocation.Status
	require.Equal(t, unboundedv1alpha3.AllocationProviderIDPrefix+string(allocTestUID), status.ProviderID)
	require.Equal(t, "Standard_D2", status.InstanceType)
	require.Equal(t, "zone-1", status.Zone)
	require.Equal(t, unboundedv1alpha3.CapacityTypeSpot, status.CapacityType)
	require.Equal(t, allocTestHostImage, status.Image)
	requireQuantity(t, "2", status.Capacity[corev1.ResourceCPU])
	requireQuantity(t, "1700m", status.Allocatable[corev1.ResourceCPU])
	requireQuantity(t, "7Gi", status.Allocatable[corev1.ResourceMemory])
	require.Equal(t, map[string]string{
		"tier":                              "gpu",
		"node-role.kubernetes.io/worker":    "",
		"team":                              "from-template",
		"example.com/family":                "d",
		corev1.LabelInstanceTypeStable:      "Standard_D2",
		corev1.LabelTopologyZone:            "zone-1",
		unboundedv1alpha3.LabelCapacityType: "Spot",
	}, status.Labels)
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionFalse, allocationReasonLaunching)

	plans := h.provider.planRequests()
	require.Len(t, plans, 1)
	require.Equal(t, publicmachineops.AllocationPlanRequest{
		AllocationName: allocTestName,
		AllocationUID:  allocTestUID,
		HostName:       allocTestNames.machine,
		ProviderConfig: publicmachineops.ProviderConfig{Data: fixture.providerConfig.Data},
		Offering:       snapshot.Offering,
	}, plans[0])

	// 2. Then the finalizer and the labels, in a metadata patch.
	h.resetCalls()
	h.reconcile()
	allocation = h.allocation()
	require.Equal(t, []string{unboundedv1alpha3.MachineAllocationFinalizer}, allocation.Finalizers)
	require.Equal(t, map[string]string{
		unboundedv1alpha3.LabelAllocationUID: string(allocTestUID),
		unboundedv1alpha3.LabelConsumerUID:   string(allocTestConsumerUID),
		unboundedv1alpha3.LabelMachineClass:  classTestName,
	}, allocation.Labels)
	require.Equal(t, []string{"patch MachineAllocation alloc-1"}, h.writes())

	// 3. Then the input finalizers, the token, the Machine and the op, and
	// the record in one status write.
	h.resetCalls()
	h.reconcile()
	require.Equal(t, []string{
		"patch ConfigMap provider-config",
		"patch ConfigMap bootstrap-files",
		"create Secret " + allocTestNames.tokenSecret,
		"create Machine " + allocTestNames.machine,
		"create MachineOperation " + allocTestNames.allocateOperation,
		"status MachineAllocation alloc-1",
	}, h.writes())

	for _, name := range []string{"provider-config", "bootstrap-files"} {
		require.Contains(t, h.configMap(name).Finalizers, allocTestNames.inputFinalizer)
	}

	token := h.secret(allocTestNames.tokenSecret)
	require.NotNil(t, token)
	require.Equal(t, corev1.SecretTypeBootstrapToken, token.Type)
	require.Equal(t, map[string]string{
		unboundedv1alpha3.MachineSiteLabelKey: classTestSite,
		unboundedv1alpha3.LabelAllocationUID:  string(allocTestUID),
	}, token.Labels)
	require.ElementsMatch(t, []string{
		"auth-extra-groups", "expiration", "token-id", "token-secret", "usage-bootstrap-authentication", "usage-bootstrap-signing",
	}, mapKeys(token.Data))
	require.Equal(t, allocTestNames.tokenID, string(token.Data["token-id"]))
	require.Len(t, token.Data["token-secret"], 16)
	require.Equal(t, allocTestNow.Add(24*time.Hour).Format(time.RFC3339), string(token.Data["expiration"]))
	require.Regexp(t, `^bootstrap-token-[a-z0-9]{6}$`, allocTestNames.tokenSecret)

	machine := h.machine()
	require.NotNil(t, machine)
	requireAllocationMachine(t, machine)

	op := h.operation(allocTestNames.allocateOperation)
	require.Equal(t, unboundedv1alpha3.MachineOperationSpec{
		MachineRef:    allocTestNames.machine,
		OperationKind: unboundedv1alpha3.OperationHostAllocate,
		AllocationRef: &unboundedv1alpha3.UIDReference{Name: allocTestName, UID: allocTestUID},
	}, op.Spec)
	require.Equal(t, []string{unboundedv1alpha3.MachineAllocationFinalizer}, op.Finalizers)
	require.Equal(t, map[string]string{unboundedv1alpha3.LabelAllocationUID: string(allocTestUID)}, op.Labels)
	requireControllerOwner(t, op.OwnerReferences)

	allocation = h.allocation()
	require.Equal(t, &unboundedv1alpha3.UIDReference{Name: op.Name, UID: op.UID}, allocation.Status.Operations.Allocate)
	require.Equal(t, &unboundedv1alpha3.UIDReference{Name: machine.Name, UID: machine.UID}, allocation.Status.MachineRef)

	// A reconcile with nothing new writes nothing.
	h.resetCalls()
	h.reconcile()
	require.Empty(t, h.writes())

	// 4. The engine persists a handle: the input finalizers go.
	h.setOperationHandle(allocTestNames.allocateOperation)
	h.reconcile()

	for _, name := range []string{"provider-config", "bootstrap-files"} {
		require.NotContains(t, h.configMap(name).Finalizers, allocTestNames.inputFinalizer)
	}

	// 5. The operation completes: the result is copied and Allocated is True
	// while the Node is not registered, and then while it is NotReady.
	h.completeOperation(allocTestNames.allocateOperation, unboundedv1alpha3.OperationPhaseComplete, "Succeeded", &unboundedv1alpha3.MachineOperationAllocationResult{
		ProviderResourceID: "native-host-1",
		InstanceType:       "Standard_D2",
		Zone:               "zone-1",
		Image:              "image-1-native",
		OwnedResources:     []unboundedv1alpha3.ProviderOwnedResource{{ID: "vm", NativeID: "vm-native-1"}},
	})
	h.reconcile()

	allocation = h.allocation()
	require.Equal(t, "native-host-1", allocation.Status.ProviderResourceID)
	require.Equal(t, "image-1-native", allocation.Status.Image)
	require.Equal(t, []unboundedv1alpha3.ProviderOwnedResource{{ID: "vm", NativeID: "vm-native-1"}}, allocation.Status.OwnedResources)
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionTrue, allocationReasonAllocated)
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionRegistered, metav1.ConditionFalse, allocationReasonNodeNotRegistered)

	h.createNode(allocTestNames.machine, allocation.Status.ProviderID, false)
	h.resetCalls()
	h.reconcile()

	allocation = h.allocation()
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionTrue, allocationReasonAllocated)
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionRegistered, metav1.ConditionTrue, allocationReasonNodeRegistered)
	require.Equal(t, &unboundedv1alpha3.UIDReference{Name: allocTestNames.machine, UID: types.UID(allocTestNames.machine + "-node-uid")}, allocation.Status.NodeRef)
	requireQuantity(t, "1500m", allocation.Status.Allocatable[corev1.ResourceCPU])
	require.Equal(t, &unboundedv1alpha3.LocalObjectReference{Name: allocTestNames.machine}, h.machine().Spec.Kubernetes.NodeRef)
	require.Contains(t, h.writes(), "patch Machine "+allocTestNames.machine)
	require.Contains(t, h.patchBodies("Machine"), `{"spec":{"kubernetes":{"nodeRef":{"name":"`+allocTestNames.machine+`"}}}}`)
	require.True(t, allocTestNow.Equal(allocation.Status.Conditions[0].LastTransitionTime.Time))
}

func TestMachineAllocationNodeConflict(t *testing.T) {
	t.Parallel()

	h := newAllocationHarness(t, newAllocationFixture(t))
	h.settle()
	h.completeOperation(allocTestNames.allocateOperation, unboundedv1alpha3.OperationPhaseComplete, "Succeeded", nil)
	h.createNode(allocTestNames.machine, "example:///another", true)
	h.settle()

	allocation := h.allocation()
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionRegistered, metav1.ConditionFalse, allocationReasonNodeConflict)
	require.Nil(t, allocation.Status.NodeRef)
	require.Nil(t, h.machine().Spec.Kubernetes.NodeRef)
}

func TestMachineAllocationNeverWritesSpec(t *testing.T) {
	t.Parallel()

	h := newAllocationHarness(t, newAllocationFixture(t))
	h.settle()
	h.setOperationHandle(allocTestNames.allocateOperation)
	h.completeOperation(allocTestNames.allocateOperation, unboundedv1alpha3.OperationPhaseComplete, "Succeeded", &unboundedv1alpha3.MachineOperationAllocationResult{
		OwnedResources: []unboundedv1alpha3.ProviderOwnedResource{{ID: "vm", NativeID: "vm-1"}},
	})
	h.createNode(allocTestNames.machine, unboundedv1alpha3.AllocationProviderIDPrefix+string(allocTestUID), true)
	h.settle()
	h.setLifecycle(unboundedv1alpha3.MachineAllocationReleased)
	h.settle()
	h.completeOperation(allocTestNames.releaseOperation, unboundedv1alpha3.OperationPhaseComplete, "Succeeded", nil)
	h.settle()

	requireAllocationCondition(t, h.allocation(), unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionTrue, allocationReasonReleased)

	calls := h.callsSnapshot()
	for _, call := range calls {
		if call.kind == "MachineAllocation" {
			require.NotEqual(t, "update", call.verb, "Update on a MachineAllocation")

			if call.verb == "patch" {
				require.NotContains(t, call.body, `"spec"`, "MachineAllocation patch body %s", call.body)
				require.Contains(t, call.body, `"resourceVersion"`, "MachineAllocation patch without an optimistic lock")
			}
		}

		if call.kind == "Machine" && call.verb == "patch" {
			require.NotContains(t, call.body, "allocationRef", "Machine patch body %s", call.body)
		}
	}
}

func TestMachineAllocationSnapshotOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*testing.T, *allocationHarness)
		reason  string
		failure unboundedv1alpha3.MachineAllocationFailureReason
		requeue time.Duration
		// planned reports whether PlanAllocation is called.
		planned bool
		// err is the error the reconcile returns instead of writing status.
		err string
	}{
		{
			name: "class missing",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.deleteFromStore(t, &unboundedv1alpha3.MachineClass{}, classTestName)
			},
			failure: unboundedv1alpha3.FailureInvalidRequest,
		},
		{
			name: "class UID mismatch",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.updateClass(t, func(class *unboundedv1alpha3.MachineClass) { class.UID = "other-uid" })
			},
			failure: unboundedv1alpha3.FailureInvalidRequest,
		},
		{
			name: "class not ready",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.updateClass(t, func(class *unboundedv1alpha3.MachineClass) {
					apimeta.SetStatusCondition(&class.Status.Conditions, metav1.Condition{
						Type: unboundedv1alpha3.MachineClassConditionReady, Status: metav1.ConditionFalse, Reason: "SiteNotFound", ObservedGeneration: 1,
					})
				})
			},
			reason:  allocationReasonClassNotReady,
			requeue: 15 * time.Second,
		},
		{
			name: "class stale generation",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.updateClass(t, func(class *unboundedv1alpha3.MachineClass) { class.Generation = 2 })
			},
			reason:  allocationReasonClassNotReady,
			requeue: 15 * time.Second,
		},
		{
			name: "site replaced",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.deleteFromStore(t, &unboundedv1alpha3.Site{}, classTestSite)
				require.NoError(t, h.store.Create(context.Background(), newClassTestSite("site-a-new-uid")))
			},
			reason:  allocationReasonClassInputsChanged,
			requeue: 15 * time.Second,
		},
		{
			name: "site being deleted",
			mutate: func(t *testing.T, h *allocationHarness) {
				var site unboundedv1alpha3.Site
				require.NoError(t, h.store.Get(context.Background(), client.ObjectKey{Name: classTestSite}, &site))
				site.Finalizers = []string{"example.com/hold"}
				require.NoError(t, h.store.Update(context.Background(), &site))
				require.NoError(t, h.store.Delete(context.Background(), &site))
			},
			reason:  allocationReasonClassInputsChanged,
			requeue: 15 * time.Second,
		},
		{
			name: "provider config replaced",
			mutate: func(t *testing.T, h *allocationHarness) {
				configMap := h.configMap("provider-config")
				h.deleteConfigMap(t, "provider-config")

				configMap.UID = "provider-config-new-uid"
				configMap.ResourceVersion = ""
				require.NoError(t, h.configMaps.Create(context.Background(), configMap))
			},
			reason:  allocationReasonClassInputsChanged,
			requeue: 15 * time.Second,
		},
		{
			name: "provider config content changed",
			mutate: func(t *testing.T, h *allocationHarness) {
				configMap := h.configMap("provider-config")
				configMap.Data["sku"] = "Standard_Y"
				require.NoError(t, h.configMaps.Update(context.Background(), configMap))
			},
			reason:  allocationReasonClassInputsChanged,
			requeue: 15 * time.Second,
		},
		{
			name:    "bootstrap files missing",
			mutate:  func(t *testing.T, h *allocationHarness) { h.deleteConfigMap(t, "bootstrap-files") },
			reason:  allocationReasonClassInputsChanged,
			requeue: 15 * time.Second,
		},
		{
			name: "configuration version changed",
			mutate: func(t *testing.T, h *allocationHarness) {
				var version unboundedv1alpha3.MachineConfigurationVersion
				require.NoError(t, h.store.Get(context.Background(), client.ObjectKey{Name: "workers-v1"}, &version))
				version.Spec.Template.Host.Image = "image-2"
				require.NoError(t, h.store.Update(context.Background(), &version))
			},
			reason:  allocationReasonClassInputsChanged,
			requeue: 15 * time.Second,
		},
		{
			name: "configuration version missing",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.deleteFromStore(t, &unboundedv1alpha3.MachineConfigurationVersion{}, "workers-v1")
			},
			reason:  allocationReasonClassInputsChanged,
			requeue: 15 * time.Second,
		},
		{
			name: "ignition format",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.updateTemplate(t, func(template *unboundedv1alpha3.MachineConfigurationTemplate) {
					template.Host.ProvisioningFormat = unboundedv1alpha3.ProvisioningFormatIgnition
				})
			},
			failure: unboundedv1alpha3.FailureInvalidRequest,
		},
		{
			name: "no candidate",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.updateAllocationSpec(t, func(spec *unboundedv1alpha3.MachineAllocationSpec) {
					spec.Resources = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64")}
				})
			},
			failure: unboundedv1alpha3.FailureInsufficientCapacity,
		},
		{
			name: "malformed requirement",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.updateAllocationSpec(t, func(spec *unboundedv1alpha3.MachineAllocationSpec) {
					spec.Requirements = []unboundedv1alpha3.MachineAllocationRequirement{{Key: "a", Operator: unboundedv1alpha3.RequirementOpIn}}
				})
			},
			failure: unboundedv1alpha3.FailureInvalidRequest,
		},
		{
			name: "plan permanent error",
			mutate: func(_ *testing.T, h *allocationHarness) {
				h.provider.setPlan(publicmachineops.AllocationPlan{}, &publicmachineops.PermanentError{Reason: "BadSKU", Err: errors.New("sku is retired")})
			},
			failure: unboundedv1alpha3.FailureInvalidRequest,
			planned: true,
		},
		{
			name: "plan empty providerID",
			mutate: func(_ *testing.T, h *allocationHarness) {
				h.provider.setPlan(publicmachineops.AllocationPlan{HostImage: "x"}, nil)
			},
			failure: unboundedv1alpha3.FailureInvalidRequest,
			planned: true,
		},
		{
			name: "plan transient error",
			mutate: func(_ *testing.T, h *allocationHarness) {
				h.provider.setPlan(publicmachineops.AllocationPlan{}, errors.New("throttled"))
			},
			planned: true,
			err:     "throttled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newAllocationHarness(t, newAllocationFixture(t))
			tt.mutate(t, h)
			h.resetCalls()

			result, err := h.reconcileResult()

			require.Equal(t, tt.planned, len(h.provider.planRequests()) > 0, "PlanAllocation calls")

			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				require.Empty(t, h.writes())

				return
			}

			require.NoError(t, err)
			require.Equal(t, ctrl.Result{RequeueAfter: tt.requeue}, result)
			require.Equal(t, []string{"status MachineAllocation alloc-1"}, h.writes())

			allocation := h.allocation()
			require.Nil(t, allocation.Status.Snapshot)
			require.Empty(t, allocation.Status.ProviderID)

			if tt.failure != "" {
				require.Equal(t, tt.failure, allocation.Status.FailureReason)
				require.NotEmpty(t, allocation.Status.FailureMessage)
				requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionFalse, string(tt.failure))

				// A failure is terminal: nothing is written again and the
				// allocation never launches.
				h.resetCalls()
				h.reconcile()
				require.Empty(t, h.writes())

				return
			}

			require.Empty(t, allocation.Status.FailureReason)
			requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionFalse, tt.reason)
		})
	}
}

func TestMachineAllocationSnapshotMissingHolds(t *testing.T) {
	t.Parallel()

	h := newAllocationHarness(t, newAllocationFixture(t))
	h.settle()
	require.NotNil(t, h.machine())
	require.NotNil(t, h.secret(allocTestNames.tokenSecret))

	// A restore drops the status: the finalizer stays, the snapshot and the
	// records are gone, and so are the token and the Machine of the old run.
	h.loseStatus(t)
	h.deleteMachineForcibly(t)
	h.deleteOperationForcibly(t, allocTestNames.allocateOperation)
	h.deleteSecret(t, allocTestNames.tokenSecret)
	h.provider.reset()
	h.resetCalls()

	result, err := h.reconcileResult()
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	allocation := h.allocation()
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionFalse, allocationReasonSnapshotMissing)
	require.Empty(t, h.provider.planRequests())
	require.Equal(t, []string{"status MachineAllocation alloc-1"}, h.writes())

	h.resetCalls()
	h.reconcile()
	require.Empty(t, h.writes(), "the hold writes nothing again")

	// Retired, the hold still fences and revokes, and keeps the finalizer.
	h2 := newAllocationHarness(t, newAllocationFixture(t))
	h2.settle()
	h2.loseStatus(t)
	h2.setLifecycle(unboundedv1alpha3.MachineAllocationReleased)
	h2.resetCalls()
	h2.settle()

	allocation = h2.allocation()
	require.True(t, h2.machine().Spec.Fenced)
	require.Nil(t, h2.secret(allocTestNames.tokenSecret))
	require.Contains(t, allocation.Finalizers, unboundedv1alpha3.MachineAllocationFinalizer)
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionFalse, allocationReasonSnapshotMissing)
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionFenced, metav1.ConditionTrue, allocationReasonLifecycleReleased)
	require.Nil(t, apimeta.FindStatusCondition(allocation.Status.Conditions, unboundedv1alpha3.MachineAllocationConditionReleased))
	require.Empty(t, h2.creates())
}

func TestMachineAllocationRecordedOperationFence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// mutate runs after the allocation recorded its HostAllocate, unless
		// beforeLaunch is set.
		mutate       func(*testing.T, *allocationHarness)
		beforeLaunch bool
		failure      unboundedv1alpha3.MachineAllocationFailureReason
		// recordedKept reports whether the record must still name the first
		// operation.
		recordedKept bool
		noOpCreate   bool
		adopted      bool
	}{
		{
			name: "recorded op missing live",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.deleteOperationForcibly(t, allocTestNames.allocateOperation)
			},
			failure:      unboundedv1alpha3.FailureProviderConflict,
			recordedKept: true,
			noOpCreate:   true,
		},
		{
			name: "recorded op with another UID",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.deleteOperationForcibly(t, allocTestNames.allocateOperation)
				require.NoError(t, h.store.Create(context.Background(), h.foreignOperation(func(op *unboundedv1alpha3.MachineOperation) {
					op.UID = "replacement-uid"
					op.Spec.AllocationRef = &unboundedv1alpha3.UIDReference{Name: allocTestName, UID: allocTestUID}
				})))
			},
			failure:      unboundedv1alpha3.FailureProviderConflict,
			recordedKept: true,
			noOpCreate:   true,
		},
		{
			name:         "cache miss but present live",
			mutate:       func(_ *testing.T, h *allocationHarness) { h.hide("MachineOperation", allocTestNames.allocateOperation) },
			recordedKept: true,
			noOpCreate:   true,
		},
		{
			name:         "existing op with a matching spec is adopted",
			beforeLaunch: true,
			mutate: func(t *testing.T, h *allocationHarness) {
				require.NoError(t, h.store.Create(context.Background(), h.foreignOperation(func(op *unboundedv1alpha3.MachineOperation) {
					op.UID = "precreated-uid"
					op.Spec.AllocationRef = &unboundedv1alpha3.UIDReference{Name: allocTestName, UID: allocTestUID}
				})))
			},
			noOpCreate: true,
			adopted:    true,
		},
		{
			name:         "existing op for another allocation UID",
			beforeLaunch: true,
			mutate: func(t *testing.T, h *allocationHarness) {
				require.NoError(t, h.store.Create(context.Background(), h.foreignOperation(nil)))
			},
			failure:    unboundedv1alpha3.FailureProviderConflict,
			noOpCreate: true,
		},
		{
			name:         "existing op of another kind",
			beforeLaunch: true,
			mutate: func(t *testing.T, h *allocationHarness) {
				require.NoError(t, h.store.Create(context.Background(), h.foreignOperation(func(op *unboundedv1alpha3.MachineOperation) {
					op.Spec.AllocationRef = &unboundedv1alpha3.UIDReference{Name: allocTestName, UID: allocTestUID}
					op.Spec.OperationKind = unboundedv1alpha3.OperationHostRelease
				})))
			},
			failure:    unboundedv1alpha3.FailureProviderConflict,
			noOpCreate: true,
		},
		{
			name:         "existing op with parameters",
			beforeLaunch: true,
			mutate: func(t *testing.T, h *allocationHarness) {
				require.NoError(t, h.store.Create(context.Background(), h.foreignOperation(func(op *unboundedv1alpha3.MachineOperation) {
					op.Spec.AllocationRef = &unboundedv1alpha3.UIDReference{Name: allocTestName, UID: allocTestUID}
					op.Spec.Parameters = map[string]string{"x": "y"}
				})))
			},
			failure:    unboundedv1alpha3.FailureProviderConflict,
			noOpCreate: true,
		},
		{
			name:         "machine bound to another allocation",
			beforeLaunch: true,
			mutate: func(t *testing.T, h *allocationHarness) {
				require.NoError(t, h.store.Create(context.Background(), &unboundedv1alpha3.Machine{
					ObjectMeta: metav1.ObjectMeta{Name: allocTestNames.machine, UID: "other-machine-uid"},
					Spec: unboundedv1alpha3.MachineSpec{
						AllocationRef: &unboundedv1alpha3.UIDReference{Name: "other", UID: "other-uid"},
					},
				}))
			},
			failure:    unboundedv1alpha3.FailureProviderConflict,
			noOpCreate: true,
		},
		{
			name:         "token Secret with another allocation label",
			beforeLaunch: true,
			mutate: func(t *testing.T, h *allocationHarness) {
				require.NoError(t, h.secrets.Create(context.Background(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: metav1.NamespaceSystem,
						Name:      allocTestNames.tokenSecret,
						Labels:    map[string]string{unboundedv1alpha3.LabelAllocationUID: "other-uid"},
					},
					Type: corev1.SecretTypeBootstrapToken,
				}))
			},
			failure:    unboundedv1alpha3.FailureProviderConflict,
			noOpCreate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newAllocationHarness(t, newAllocationFixture(t))

			var first *unboundedv1alpha3.UIDReference

			if tt.beforeLaunch {
				h.reconcile() // snapshot
				h.reconcile() // finalizer
			} else {
				h.settle()
				first = h.allocation().Status.Operations.Allocate
				require.NotNil(t, first)
			}

			tt.mutate(t, h)
			h.resetCalls()
			h.settle()

			allocation := h.allocation()
			require.Equal(t, tt.failure, allocation.Status.FailureReason, allocation.Status.FailureMessage)

			if tt.noOpCreate {
				require.NotContains(t, h.writes(), "create MachineOperation "+allocTestNames.allocateOperation)
			}

			if tt.recordedKept {
				require.Equal(t, first, allocation.Status.Operations.Allocate)
			}

			if tt.failure == unboundedv1alpha3.FailureProviderConflict && tt.recordedKept {
				requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonAllocateOperationMissing)

				// The create is never retried.
				h.resetCalls()
				h.settle()
				require.Empty(t, h.creates())
			}

			if tt.adopted {
				require.Equal(t, &unboundedv1alpha3.UIDReference{Name: allocTestNames.allocateOperation, UID: "precreated-uid"}, allocation.Status.Operations.Allocate)
			}

			if tt.failure != "" && !tt.recordedKept {
				require.Nil(t, allocation.Status.Operations.Allocate)
			}
		})
	}
}

func TestMachineAllocationStaleRecordReadConflicts(t *testing.T) {
	t.Parallel()

	h := newAllocationHarness(t, newAllocationFixture(t))
	h.reconcile() // snapshot
	h.reconcile() // finalizer
	stale := h.allocation()
	h.reconcile() // launch and record

	first := h.allocation().Status.Operations.Allocate
	require.NotNil(t, first)

	// The recorded op is force-deleted, and the cache still serves the
	// allocation from before the record.
	h.deleteOperationForcibly(t, allocTestNames.allocateOperation)
	h.serveStaleAllocation(stale)
	h.resetCalls()

	_, err := h.reconcileResult()
	require.True(t, apierrors.IsConflict(errors.Unwrap(err)) || apierrors.IsConflict(err), "record write error %v", err)
	require.Contains(t, h.writes(), "create MachineOperation "+allocTestNames.allocateOperation)

	second := h.operation(allocTestNames.allocateOperation)
	require.NotEqual(t, first.UID, second.UID)
	require.Equal(t, first, h.allocation().Status.Operations.Allocate)

	// From the current allocation, the second op is not the recorded one.
	h.resetCalls()
	h.settle()
	require.Equal(t, first, h.allocation().Status.Operations.Allocate)
	require.Equal(t, unboundedv1alpha3.FailureProviderConflict, h.allocation().Status.FailureReason)
	require.Empty(t, h.creates())
}

func TestMachineAllocationOfferingExpiredBeforeLaunch(t *testing.T) {
	t.Parallel()

	h := newAllocationHarness(t, newAllocationFixture(t))
	h.reconcile() // snapshot
	h.reconcile() // finalizer

	h.setNow(allocTestNow.Add(2 * time.Hour))
	h.resetCalls()
	h.settle()

	allocation := h.allocation()
	require.Equal(t, unboundedv1alpha3.FailureInsufficientCapacity, allocation.Status.FailureReason)
	require.Nil(t, allocation.Status.Operations.Allocate)
	require.Nil(t, h.operation(allocTestNames.allocateOperation))
	require.NotContains(t, h.writes(), "create MachineOperation "+allocTestNames.allocateOperation)
}

func TestMachineAllocationInputProtection(t *testing.T) {
	t.Parallel()

	t.Run("added before the op and removed once the op has a handle", func(t *testing.T) {
		t.Parallel()

		h := newAllocationHarness(t, newAllocationFixture(t))
		h.settle()

		writes := h.writes()
		opCreate := slices.Index(writes, "create MachineOperation "+allocTestNames.allocateOperation)
		require.Positive(t, opCreate)
		require.Less(t, slices.Index(writes, "patch ConfigMap provider-config"), opCreate)
		require.Less(t, slices.Index(writes, "patch ConfigMap bootstrap-files"), opCreate)
		require.Contains(t, h.configMap("provider-config").Finalizers, allocTestNames.inputFinalizer)

		h.setOperationHandle(allocTestNames.allocateOperation)
		h.settle()
		require.NotContains(t, h.configMap("provider-config").Finalizers, allocTestNames.inputFinalizer)
		require.NotContains(t, h.configMap("bootstrap-files").Finalizers, allocTestNames.inputFinalizer)
	})

	t.Run("removed once the op is terminal", func(t *testing.T) {
		t.Parallel()

		h := newAllocationHarness(t, newAllocationFixture(t))
		h.settle()
		h.completeOperation(allocTestNames.allocateOperation, unboundedv1alpha3.OperationPhaseFailed, reasonRequestBuildFailed, nil)
		h.settle()
		require.NotContains(t, h.configMap("provider-config").Finalizers, allocTestNames.inputFinalizer)
		require.Equal(t, unboundedv1alpha3.FailureInvalidRequest, h.allocation().Status.FailureReason)
	})

	t.Run("a deleting ConfigMap fails the allocation", func(t *testing.T) {
		t.Parallel()

		h := newAllocationHarness(t, newAllocationFixture(t))
		h.reconcile() // snapshot
		h.reconcile() // finalizer

		configMap := h.configMap("bootstrap-files")
		configMap.Finalizers = []string{"example.com/hold"}
		require.NoError(t, h.configMaps.Update(context.Background(), configMap))
		require.NoError(t, h.configMaps.Delete(context.Background(), configMap))

		h.resetCalls()
		h.settle()

		allocation := h.allocation()
		require.Equal(t, unboundedv1alpha3.FailureInvalidRequest, allocation.Status.FailureReason)
		require.Nil(t, h.operation(allocTestNames.allocateOperation))
		require.Nil(t, h.secret(allocTestNames.tokenSecret))
		require.Empty(t, h.creates())
	})
}

func TestMachineAllocationFenceAndRevoke(t *testing.T) {
	t.Parallel()

	signals := []struct {
		name   string
		reason string
		retire func(*testing.T, *allocationHarness)
		// authorized reports whether release is authorized.
		authorized bool
	}{
		{
			name:       "lifecycle released",
			reason:     allocationReasonLifecycleReleased,
			retire:     func(_ *testing.T, h *allocationHarness) { h.setLifecycle(unboundedv1alpha3.MachineAllocationReleased) },
			authorized: true,
		},
		{
			name:   "allocation deleting",
			reason: allocationReasonAllocationDeleting,
			retire: func(t *testing.T, h *allocationHarness) {
				require.NoError(t, h.store.Delete(context.Background(), h.allocation()))
			},
		},
		{
			name:   "consumer terminating",
			reason: allocationReasonConsumerTerminating,
			retire: func(t *testing.T, h *allocationHarness) {
				consumer := h.consumer()
				consumer.Finalizers = []string{"example.com/hold"}
				require.NoError(t, h.store.Update(context.Background(), consumer))
				require.NoError(t, h.store.Delete(context.Background(), consumer))
			},
		},
		{
			name:   "consumer deleted",
			reason: allocationReasonConsumerGone,
			retire: func(t *testing.T, h *allocationHarness) {
				require.NoError(t, h.store.Delete(context.Background(), h.consumer()))
			},
		},
		{
			name:   "consumer replaced",
			reason: allocationReasonConsumerGone,
			retire: func(t *testing.T, h *allocationHarness) {
				require.NoError(t, h.store.Delete(context.Background(), h.consumer()))
				require.NoError(t, h.store.Create(context.Background(), &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: allocTestConsumer, UID: "consumer-a-new-uid"},
				}))
			},
		},
		{
			name:   "machine deleting",
			reason: allocationReasonMachineDeleting,
			retire: func(t *testing.T, h *allocationHarness) {
				require.NoError(t, h.store.Delete(context.Background(), h.machine()))
			},
		},
	}

	for _, signal := range signals {
		t.Run(signal.name, func(t *testing.T) {
			t.Parallel()

			h := newAllocationHarness(t, newAllocationFixture(t))
			h.settle()
			signal.retire(t, h)
			h.resetCalls()

			h.reconcile()

			allocation := h.allocation()
			requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionFenced, metav1.ConditionTrue, signal.reason)

			machine := h.machine()
			if machine.DeletionTimestamp.IsZero() {
				require.True(t, machine.Spec.Fenced)
				require.Equal(t, []string{`{"spec":{"fenced":true}}`}, h.patchBodies("Machine"))
			} else {
				require.Empty(t, h.patchBodies("Machine"), "a deleting Machine is not patched")
			}

			require.Nil(t, h.secret(allocTestNames.tokenSecret))

			deletes := h.deleteCalls("Secret")
			require.Len(t, deletes, 1)
			require.Equal(t, allocTestNames.tokenSecret, deletes[0].name)
			require.NotEmpty(t, deletes[0].preconditionUID)
			require.Empty(t, h.creates())

			if !signal.authorized {
				requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonAwaitingRelease)
				require.Contains(t, apimeta.FindStatusCondition(allocation.Status.Conditions, unboundedv1alpha3.MachineAllocationConditionReleased).Message, signal.reason)
			}

			// Nothing is created again afterwards.
			h.resetCalls()
			h.settle()
			require.Empty(t, h.creates())
			requireAllocationCondition(t, h.allocation(), unboundedv1alpha3.MachineAllocationConditionFenced, metav1.ConditionTrue, signal.reason)
		})
	}

	t.Run("fenced only after both are confirmed", func(t *testing.T) {
		t.Parallel()

		h := newAllocationHarness(t, newAllocationFixture(t))
		h.settle()

		// The token Secret has a finalizer, so the delete leaves it in place.
		token := h.secret(allocTestNames.tokenSecret)
		token.Finalizers = []string{"example.com/hold"}
		require.NoError(t, h.secrets.Update(context.Background(), token))

		h.setLifecycle(unboundedv1alpha3.MachineAllocationReleased)
		result, err := h.reconcileResult()
		require.NoError(t, err)
		require.Equal(t, 15*time.Second, result.RequeueAfter)
		requireAllocationCondition(t, h.allocation(), unboundedv1alpha3.MachineAllocationConditionFenced, metav1.ConditionFalse, allocationReasonFencing)
		require.True(t, h.machine().Spec.Fenced)

		token = h.secret(allocTestNames.tokenSecret)
		token.Finalizers = nil
		require.NoError(t, h.secrets.Update(context.Background(), token))

		h.reconcile()
		requireAllocationCondition(t, h.allocation(), unboundedv1alpha3.MachineAllocationConditionFenced, metav1.ConditionTrue, allocationReasonLifecycleReleased)
	})

	t.Run("consumer missing from the cache but present live is not fenced", func(t *testing.T) {
		t.Parallel()

		h := newAllocationHarness(t, newAllocationFixture(t))
		h.settle()
		h.hide("Namespace", allocTestConsumer)
		h.resetCalls()
		h.settle()

		allocation := h.allocation()
		require.Nil(t, apimeta.FindStatusCondition(allocation.Status.Conditions, unboundedv1alpha3.MachineAllocationConditionFenced))
		require.False(t, h.machine().Spec.Fenced)
		require.NotNil(t, h.secret(allocTestNames.tokenSecret))
		require.Contains(t, h.reads(), "live Namespace "+allocTestConsumer)
	})

	t.Run("a consumer kind that is not listed is never read", func(t *testing.T) {
		t.Parallel()

		h := newAllocationHarness(t, newAllocationFixture(t))
		h.reconciler.ConsumerKinds = nil
		require.NoError(t, h.store.Delete(context.Background(), h.consumer()))
		h.settle()

		allocation := h.allocation()
		require.Nil(t, apimeta.FindStatusCondition(allocation.Status.Conditions, unboundedv1alpha3.MachineAllocationConditionFenced))
		require.NotNil(t, h.operation(allocTestNames.allocateOperation))

		for _, read := range h.reads() {
			require.NotContains(t, read, "Namespace")
		}
	})

	for _, label := range []struct {
		name   string
		labels map[string]string
	}{
		{name: "token name with another allocation label", labels: map[string]string{unboundedv1alpha3.LabelAllocationUID: "other-uid"}},
		{name: "token name without the label"},
	} {
		t.Run(label.name, func(t *testing.T) {
			t.Parallel()

			h := newAllocationHarness(t, newAllocationFixture(t))
			h.settle()
			h.deleteSecret(t, allocTestNames.tokenSecret)
			require.NoError(t, h.secrets.Create(context.Background(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: allocTestNames.tokenSecret, Labels: label.labels},
				Type:       corev1.SecretTypeBootstrapToken,
			}))
			h.setLifecycle(unboundedv1alpha3.MachineAllocationReleased)
			h.resetCalls()
			h.settle()

			allocation := h.allocation()
			fenced := requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionFenced, metav1.ConditionFalse, allocationReasonFencing)
			require.Contains(t, fenced.Message, allocTestNames.tokenSecret)
			require.Empty(t, h.deleteCalls("Secret"))
			require.NotNil(t, h.secret(allocTestNames.tokenSecret))
			require.True(t, h.machine().Spec.Fenced)
			require.Nil(t, h.operation(allocTestNames.releaseOperation))
		})
	}
}

func TestMachineAllocationReleaseFlow(t *testing.T) {
	t.Parallel()

	h := newAllocationHarness(t, newAllocationFixture(t))
	h.settle()
	h.setOperationHandle(allocTestNames.allocateOperation)
	h.settle()

	// Released while the HostAllocate is InProgress: no HostRelease yet.
	h.setLifecycle(unboundedv1alpha3.MachineAllocationReleased)
	result, err := h.reconcileResult()
	require.NoError(t, err)
	require.Equal(t, 15*time.Second, result.RequeueAfter)

	allocation := h.allocation()
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionFenced, metav1.ConditionTrue, allocationReasonLifecycleReleased)
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonAwaitingAllocateOperation)
	require.Nil(t, h.operation(allocTestNames.releaseOperation))

	// The HostAllocate finishes: its result is copied, and HostRelease is
	// created and recorded.
	owned := []unboundedv1alpha3.ProviderOwnedResource{{ID: "vm", NativeID: "vm-1"}}
	h.completeOperation(allocTestNames.allocateOperation, unboundedv1alpha3.OperationPhaseComplete, "Succeeded", &unboundedv1alpha3.MachineOperationAllocationResult{OwnedResources: owned})
	h.resetCalls()
	h.reconcile()

	allocation = h.allocation()
	require.Equal(t, owned, allocation.Status.OwnedResources)

	release := h.operation(allocTestNames.releaseOperation)
	require.NotNil(t, release)
	require.Equal(t, unboundedv1alpha3.OperationHostRelease, release.Spec.OperationKind)
	require.Equal(t, []string{unboundedv1alpha3.MachineAllocationFinalizer}, release.Finalizers)
	require.Equal(t, &unboundedv1alpha3.UIDReference{Name: release.Name, UID: release.UID}, allocation.Status.Operations.Release)
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonReleasing)

	// HostRelease completes: the finalizers go in order, then Released.
	h.completeOperation(allocTestNames.releaseOperation, unboundedv1alpha3.OperationPhaseComplete, "Succeeded", nil)
	h.resetCalls()
	h.reconcile()

	require.Equal(t, []string{
		"patch MachineOperation " + allocTestNames.allocateOperation,
		"patch MachineOperation " + allocTestNames.releaseOperation,
		"patch Machine " + allocTestNames.machine,
		"status MachineAllocation alloc-1",
		"patch MachineAllocation alloc-1",
	}, h.writes())

	allocation = h.allocation()
	requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionTrue, allocationReasonReleased)
	require.Empty(t, allocation.Finalizers)
	require.Empty(t, h.operation(allocTestNames.allocateOperation).Finalizers)
	require.Empty(t, h.operation(allocTestNames.releaseOperation).Finalizers)
	require.Empty(t, h.machine().Finalizers)
	require.NotContains(t, h.configMap("provider-config").Finalizers, allocTestNames.inputFinalizer)

	// A finished allocation is left alone.
	h.resetCalls()
	h.reconcile()
	require.Empty(t, h.writes())
}

func TestMachineAllocationReleaseHolds(t *testing.T) {
	t.Parallel()

	launchedAndReleased := func(t *testing.T, h *allocationHarness) {
		t.Helper()

		h.settle()
		h.setOperationHandle(allocTestNames.allocateOperation)
		h.completeOperation(allocTestNames.allocateOperation, unboundedv1alpha3.OperationPhaseComplete, "Succeeded", nil)
		h.settle()
		h.setLifecycle(unboundedv1alpha3.MachineAllocationReleased)
	}

	tests := []struct {
		name          string
		setup         func(*testing.T, *allocationHarness)
		reason        string
		releaseCreate bool
	}{
		{
			name: "allocate operation missing",
			setup: func(t *testing.T, h *allocationHarness) {
				h.settle()
				h.setLifecycle(unboundedv1alpha3.MachineAllocationReleased)
				h.deleteOperationForcibly(t, allocTestNames.allocateOperation)
			},
			reason: allocationReasonAllocateOperationMissing,
		},
		{
			name: "release operation missing",
			setup: func(t *testing.T, h *allocationHarness) {
				launchedAndReleased(t, h)
				h.settle()
				require.NotNil(t, h.operation(allocTestNames.releaseOperation))
				h.deleteOperationForcibly(t, allocTestNames.releaseOperation)
			},
			reason: allocationReasonReleaseOperationMissing,
		},
		{
			name: "release failed",
			setup: func(t *testing.T, h *allocationHarness) {
				launchedAndReleased(t, h)
				h.settle()
				h.completeOperation(allocTestNames.releaseOperation, unboundedv1alpha3.OperationPhaseFailed, "ProviderFailed", nil)
			},
			reason:        allocationReasonReleaseFailed,
			releaseCreate: true,
		},
		{
			name: "machine missing",
			setup: func(t *testing.T, h *allocationHarness) {
				launchedAndReleased(t, h)
				h.deleteMachineForcibly(t)
			},
			reason: allocationReasonMachineMissing,
		},
		{
			name: "consumer gone with lifecycle active",
			setup: func(t *testing.T, h *allocationHarness) {
				h.settle()
				h.completeOperation(allocTestNames.allocateOperation, unboundedv1alpha3.OperationPhaseComplete, "Succeeded", nil)
				require.NoError(t, h.store.Delete(context.Background(), h.consumer()))
			},
			reason: allocationReasonAwaitingRelease,
		},
		{
			name: "begin outcome unknown",
			setup: func(t *testing.T, h *allocationHarness) {
				h.settle()
				h.setLifecycle(unboundedv1alpha3.MachineAllocationReleased)
				h.setOperationStatus(t, allocTestNames.allocateOperation, func(op *unboundedv1alpha3.MachineOperation) {
					op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
					op.Status.Conditions = []metav1.Condition{{
						Type: OperationConditionCompleted, Status: metav1.ConditionFalse, Reason: allocationReasonBeginOutcomeUnknown,
						Message: "BeginOutcomeUnknown: the token is gone", LastTransitionTime: metav1.NewTime(allocTestNow),
					}}
				})
			},
			reason: allocationReasonBeginOutcomeUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newAllocationHarness(t, newAllocationFixture(t))
			tt.setup(t, h)
			h.resetCalls()

			result, err := h.reconcileResult()
			require.NoError(t, err)
			require.Equal(t, ctrl.Result{}, result, "a hold never requeues on a timer")
			h.settle()

			allocation := h.allocation()
			requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, tt.reason)
			require.Contains(t, allocation.Finalizers, unboundedv1alpha3.MachineAllocationFinalizer)

			if machine := h.machine(); machine != nil {
				require.Contains(t, machine.Finalizers, unboundedv1alpha3.MachineAllocationFinalizer)
			}

			if op := h.operation(allocTestNames.allocateOperation); op != nil {
				require.Contains(t, op.Finalizers, unboundedv1alpha3.MachineAllocationFinalizer)
			}

			if !tt.releaseCreate {
				require.NotContains(t, h.writes(), "create MachineOperation "+allocTestNames.releaseOperation)
			}
		})
	}
}

func TestMachineAllocationReleaseWithoutLaunch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// steps are the reconciles before release is authorized.
		steps  int
		fences bool
	}{
		{name: "released before the snapshot"},
		{name: "released after the snapshot and before the finalizer", steps: 1},
		{name: "released after the token and Machine but before any record", steps: 2, fences: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newAllocationHarness(t, newAllocationFixture(t))
			for range tt.steps {
				h.reconcile()
			}

			if tt.fences {
				// The launch stops after the Machine: the op create fails.
				h.failCreates("MachineOperation")
				_, err := h.reconcileResult()
				require.Error(t, err)
				require.NotNil(t, h.machine())
				require.NotNil(t, h.secret(allocTestNames.tokenSecret))
				require.Nil(t, h.allocation().Status.Operations.Allocate)
				h.failCreates("")
			}

			h.setLifecycle(unboundedv1alpha3.MachineAllocationReleased)
			h.resetCalls()
			h.settle()

			allocation := h.allocation()
			requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionTrue, allocationReasonReleased)
			require.Empty(t, allocation.Finalizers)
			require.Nil(t, h.operation(allocTestNames.releaseOperation))
			require.NotContains(t, h.writes(), "create MachineOperation "+allocTestNames.releaseOperation)

			if tt.fences {
				require.True(t, h.machine().Spec.Fenced)
				require.Nil(t, h.secret(allocTestNames.tokenSecret))
				require.Empty(t, h.machine().Finalizers)
				requireAllocationCondition(t, allocation, unboundedv1alpha3.MachineAllocationConditionFenced, metav1.ConditionTrue, allocationReasonLifecycleReleased)

				writes := h.writes()
				require.Less(t, slices.Index(writes, "delete Secret "+allocTestNames.tokenSecret), slices.Index(writes, "patch MachineAllocation alloc-1"))
			} else {
				require.Nil(t, h.machine())
				require.Empty(t, h.creates())
			}
		})
	}
}

func TestMachineAllocationRestartMidOperation(t *testing.T) {
	t.Parallel()

	// Each case stops after a number of writes and continues with a new
	// reconciler on the same store and an empty cache.
	uninterrupted := newAllocationHarness(t, newAllocationFixture(t))
	runAllocationLifecycle(t, uninterrupted)
	want := uninterrupted.lifecycleOutcome(t)
	totalWrites := len(uninterrupted.writes())
	require.Greater(t, totalWrites, 10)

	for stop := 1; stop < totalWrites; stop++ {
		t.Run(fmt.Sprintf("after write %d", stop), func(t *testing.T) {
			t.Parallel()

			h := newAllocationHarness(t, newAllocationFixture(t))
			h.crashAfterWrites(stop)
			runAllocationLifecycle(t, h)
			h.crashAfterWrites(0)
			h.restart()
			runAllocationLifecycle(t, h)

			require.Equal(t, want, h.lifecycleOutcome(t))

			creates := h.creates()
			require.Len(t, slices.Compact(slices.Sorted(slices.Values(creates))), len(creates), "duplicate create in %v", creates)
		})
	}

	t.Run("op created before the record is adopted", func(t *testing.T) {
		t.Parallel()

		h := newAllocationHarness(t, newAllocationFixture(t))
		h.reconcile() // snapshot
		h.reconcile() // finalizer
		h.failStatusWrites(true)
		_, err := h.reconcileResult()
		require.Error(t, err)
		h.failStatusWrites(false)

		created := h.operation(allocTestNames.allocateOperation)
		require.NotNil(t, created)
		require.Nil(t, h.allocation().Status.Operations.Allocate)

		h.restart()
		h.resetCalls()
		h.reconcile()
		require.Equal(t, &unboundedv1alpha3.UIDReference{Name: created.Name, UID: created.UID}, h.allocation().Status.Operations.Allocate)
		require.Empty(t, h.creates())
	})
}

// runAllocationLifecycle drives an allocation from creation to Released,
// playing the engine's part. It stops quietly once the harness crashes.
func runAllocationLifecycle(t *testing.T, h *allocationHarness) {
	t.Helper()

	for range 40 {
		if h.crashed() {
			return
		}

		allocation := h.allocation()
		if allocation == nil || apimeta.IsStatusConditionTrue(allocation.Status.Conditions, unboundedv1alpha3.MachineAllocationConditionReleased) && len(allocation.Finalizers) == 0 {
			return
		}

		_, _ = h.reconcileResult()

		if h.crashed() {
			return
		}

		allocate := h.operation(allocTestNames.allocateOperation)
		if allocate != nil && !allocate.Status.IsTerminal() && h.allocation().Status.Operations.Allocate != nil {
			h.completeOperation(allocTestNames.allocateOperation, unboundedv1alpha3.OperationPhaseComplete, "Succeeded",
				&unboundedv1alpha3.MachineOperationAllocationResult{OwnedResources: []unboundedv1alpha3.ProviderOwnedResource{{ID: "vm", NativeID: "vm-1"}}})

			continue
		}

		if allocate != nil && allocate.Status.IsTerminal() && h.allocation().Spec.Lifecycle != unboundedv1alpha3.MachineAllocationReleased {
			h.setLifecycle(unboundedv1alpha3.MachineAllocationReleased)
			continue
		}

		release := h.operation(allocTestNames.releaseOperation)
		if release != nil && !release.Status.IsTerminal() && h.allocation().Status.Operations.Release != nil {
			h.completeOperation(allocTestNames.releaseOperation, unboundedv1alpha3.OperationPhaseComplete, "Succeeded", nil)
		}
	}

	t.Fatalf("allocation lifecycle did not finish")
}

// lifecycleOutcome is the state that matters at the end of a lifecycle.
func (h *allocationHarness) lifecycleOutcome(t *testing.T) string {
	t.Helper()

	allocation := h.allocation()
	machine := h.machine()
	allocate := h.operation(allocTestNames.allocateOperation)
	release := h.operation(allocTestNames.releaseOperation)

	var conditions []string
	for _, condition := range allocation.Status.Conditions {
		conditions = append(conditions, condition.Type+"="+string(condition.Status)+"/"+condition.Reason)
	}

	slices.Sort(conditions)

	return fmt.Sprintf("finalizers=%v conditions=%v failure=%q owned=%v machineFenced=%t machineFinalizers=%v token=%t allocateFinalizers=%v releaseFinalizers=%v recorded=%t,%t inputFinalizers=%v,%v",
		allocation.Finalizers, conditions, allocation.Status.FailureReason, allocation.Status.OwnedResources,
		machine.Spec.Fenced, machine.Finalizers, h.secret(allocTestNames.tokenSecret) != nil,
		allocate.Finalizers, release.Finalizers,
		allocation.Status.Operations.Allocate != nil, allocation.Status.Operations.Release != nil,
		h.configMap("provider-config").Finalizers, h.configMap("bootstrap-files").Finalizers)
}

func TestMachineAllocationReconcilerIgnoresOutOfScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*testing.T, *allocationHarness)
	}{
		{
			name: "class of another provider",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.updateClass(t, func(class *unboundedv1alpha3.MachineClass) { class.Spec.Provider = "OtherCloud" })
			},
		},
		{
			name:   "class of another Site",
			mutate: func(_ *testing.T, h *allocationHarness) { h.reconciler.SiteName = "site-b" },
		},
		{
			name: "registration without capacity",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.reconciler.Providers = []*Provider{newClassTestPlainProvider(t, classTestProvider)}
			},
		},
		{
			name: "snapshot of another provider",
			mutate: func(t *testing.T, h *allocationHarness) {
				h.settle()
				h.reconciler.Providers = []*Provider{newAllocationTestProvider(t, "OtherCloud", h.provider)}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newAllocationHarness(t, newAllocationFixture(t))
			tt.mutate(t, h)
			h.setLifecycle(unboundedv1alpha3.MachineAllocationReleased)
			before := h.allocation()
			h.resetCalls()
			h.provider.reset()

			result, err := h.reconcileResult()
			require.NoError(t, err)
			require.Equal(t, ctrl.Result{}, result)
			require.Empty(t, h.writes())
			require.Empty(t, h.provider.planRequests())

			for _, read := range h.reads() {
				require.True(t, read == "cache MachineAllocation alloc-1" || read == "cache MachineClass "+classTestName, "unexpected read %s", read)
			}

			require.Equal(t, before.ResourceVersion, h.allocation().ResourceVersion)
		})
	}

	t.Run("no class and no snapshot is in scope and fails", func(t *testing.T) {
		t.Parallel()

		h := newAllocationHarness(t, newAllocationFixture(t))
		h.reconciler.SiteName = "site-b"
		h.deleteFromStore(t, &unboundedv1alpha3.MachineClass{}, classTestName)
		h.reconcile()
		require.Equal(t, unboundedv1alpha3.FailureInvalidRequest, h.allocation().Status.FailureReason)
	})
}

func TestMachineAllocationWatchMapping(t *testing.T) {
	t.Parallel()

	h := newAllocationHarness(t, newAllocationFixture(t))

	other := newAllocationFixture(t).allocation
	other.Name = "alloc-2"
	other.UID = "alloc-2-uid"
	other.Spec.ClassRef = unboundedv1alpha3.UIDReference{Name: "class-b", UID: "class-b-uid"}
	other.Spec.ConsumerRef.Name = "consumer-b"
	other.ResourceVersion = ""
	require.NoError(t, h.store.Create(context.Background(), other))

	request := func(name string) []ctrl.Request {
		return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: name}}}
	}
	ctx := context.Background()

	require.Equal(t, request(allocTestName), h.reconciler.allocationsForClass(ctx, &unboundedv1alpha3.MachineClass{ObjectMeta: metav1.ObjectMeta{Name: classTestName}}))
	require.Equal(t, request("alloc-2"), h.reconciler.allocationsForClass(ctx, &unboundedv1alpha3.MachineClass{ObjectMeta: metav1.ObjectMeta{Name: "class-b"}}))
	require.Empty(t, h.reconciler.allocationsForClass(ctx, &unboundedv1alpha3.MachineClass{ObjectMeta: metav1.ObjectMeta{Name: "class-c"}}))

	require.Equal(t, request(allocTestName), h.reconciler.allocationsForNode(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: allocTestNames.machine}}))
	require.Equal(t, request("alloc-2"), h.reconciler.allocationsForNode(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: allocationNamesFor("alloc-2-uid").machine}}))
	require.Empty(t, h.reconciler.allocationsForNode(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}))

	operation := h.foreignOperation(func(op *unboundedv1alpha3.MachineOperation) {
		op.Spec.AllocationRef = &unboundedv1alpha3.UIDReference{Name: allocTestName, UID: allocTestUID}
	})
	require.Equal(t, request(allocTestName), allocationForOperation(ctx, operation))
	require.Empty(t, allocationForOperation(ctx, newMachineOperation("reboot", "machine-1", unboundedv1alpha3.OperationHostHardReboot)))

	machine := &unboundedv1alpha3.Machine{Spec: unboundedv1alpha3.MachineSpec{AllocationRef: &unboundedv1alpha3.UIDReference{Name: "alloc-2", UID: "alloc-2-uid"}}}
	require.Equal(t, request("alloc-2"), allocationForMachine(ctx, machine))
	require.Empty(t, allocationForMachine(ctx, &unboundedv1alpha3.Machine{}))

	consumers := h.reconciler.allocationsForConsumer(allocTestConsumerKind)
	require.Equal(t, request(allocTestName), consumers(ctx, &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: allocTestConsumer}}))
	require.Equal(t, request("alloc-2"), consumers(ctx, &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "consumer-b"}}))
	require.Empty(t, consumers(ctx, &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "consumer-c"}}))
	require.Empty(t, h.reconciler.allocationsForConsumer(schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodeClaim"})(ctx,
		&metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: allocTestConsumer}}))
}

func TestAllocationNames(t *testing.T) {
	t.Parallel()

	names := allocationNamesFor("6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b")
	require.Equal(t, "ub-6f1c2a3b4d5e4f608a7b9c0d1e2f3a4b", names.machine)
	require.Equal(t, names.machine+"-allocate", names.allocateOperation)
	require.Equal(t, names.machine+"-release", names.releaseOperation)
	require.Regexp(t, `^[a-z0-9]{6}$`, names.tokenID)
	require.Equal(t, "bootstrap-token-"+names.tokenID, names.tokenSecret)
	require.Equal(t, "machineallocations.unbounded-cloud.io/6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b", names.inputFinalizer)
	require.Equal(t, names, allocationNamesFor("6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b"))
	require.NotEqual(t, names.tokenID, allocationNamesFor("6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4c").tokenID)
}

func TestAllocationFailureReason(t *testing.T) {
	t.Parallel()

	for reason, want := range map[string]unboundedv1alpha3.MachineAllocationFailureReason{
		"QuotaExceeded":         unboundedv1alpha3.FailureQuotaExceeded,
		"InsufficientCapacity":  unboundedv1alpha3.FailureInsufficientCapacity,
		"AuthSecretNotFound":    unboundedv1alpha3.FailureAuthFailed,
		"AuthorityReplaced":     unboundedv1alpha3.FailureAuthFailed,
		"AllocationFenced":      unboundedv1alpha3.FailureInvalidRequest,
		"OperationNotRecorded":  unboundedv1alpha3.FailureInvalidRequest,
		"AllocationUnavailable": unboundedv1alpha3.FailureInvalidRequest,
		"RequestBuildFailed":    unboundedv1alpha3.FailureInvalidRequest,
		"InvalidSpec":           unboundedv1alpha3.FailureInvalidRequest,
		"InvalidHost":           unboundedv1alpha3.FailureInvalidRequest,
		"InvalidProviderRef":    unboundedv1alpha3.FailureInvalidRequest,
		"UnsupportedOperation":  unboundedv1alpha3.FailureInvalidRequest,
		"SkuNotAvailable":       unboundedv1alpha3.FailureProviderFailed,
		"":                      unboundedv1alpha3.FailureProviderFailed,
	} {
		require.Equal(t, want, allocationFailureReason(reason), reason)
	}
}

func TestKubeletNodeLabelsAndTaints(t *testing.T) {
	t.Parallel()

	require.Equal(t, map[string]string{
		"team":                              "a",
		corev1.LabelTopologyZone:            "zone-1",
		corev1.LabelInstanceTypeStable:      "x",
		"node.kubernetes.io/pool":           "p",
		"foo.kubelet.kubernetes.io/x":       "y",
		unboundedv1alpha3.LabelCapacityType: "Spot",
	}, kubeletNodeLabels(map[string]string{
		"team":                              "a",
		corev1.LabelTopologyZone:            "zone-1",
		corev1.LabelInstanceTypeStable:      "x",
		"node.kubernetes.io/pool":           "p",
		"foo.kubelet.kubernetes.io/x":       "y",
		unboundedv1alpha3.LabelCapacityType: "Spot",
		"node-role.kubernetes.io/worker":    "",
		"example.k8s.io/private":            "z",
		"kubernetes.io/custom":              "c",
	}))

	require.Nil(t, registrationTaints(&unboundedv1alpha3.MachineConfigurationTemplate{}, nil))
}

// allocationFixture holds the objects of one allocation from a Ready class.
type allocationFixture struct {
	class          *unboundedv1alpha3.MachineClass
	site           *unboundedv1alpha3.Site
	providerConfig *corev1.ConfigMap
	bootstrapFiles *corev1.ConfigMap
	version        *unboundedv1alpha3.MachineConfigurationVersion
	credential     *unboundedv1alpha3.MachineOperationCredential
	consumer       *corev1.Namespace
	allocation     *unboundedv1alpha3.MachineAllocation
}

func newAllocationFixture(t *testing.T) *allocationFixture {
	t.Helper()

	classFixture := newClassFixture()
	version := classFixture.version
	version.Spec.Template = unboundedv1alpha3.MachineConfigurationTemplate{
		Host: &unboundedv1alpha3.MachineConfigurationHostSpec{Image: "image-1"},
		Kubernetes: &unboundedv1alpha3.MachineConfigurationKubernetes{
			Version:            "v1.34.0",
			NodeLabels:         map[string]string{"tier": "gpu", "node-role.kubernetes.io/worker": "", "team": "from-configuration"},
			RegisterWithTaints: []corev1.Taint{{Key: "dedicated", Value: "a", Effect: corev1.TaintEffectNoSchedule}},
		},
		Agent: &unboundedv1alpha3.MachineConfigurationAgent{
			Image:    "ghcr.io/example/node:1",
			LocalDNS: &unboundedv1alpha3.LocalDNSSpec{Enabled: true},
		},
	}

	providerConfig, err := configMapSnapshot(classFixture.providerConfig)
	require.NoError(t, err)
	bootstrapFiles, err := configMapSnapshot(classFixture.bootstrapFiles)
	require.NoError(t, err)
	templateHash, err := contentHash(version.Spec.Template)
	require.NoError(t, err)

	validUntil := metav1.NewTime(allocTestNow.Add(time.Hour))
	class := classFixture.class
	class.Status = unboundedv1alpha3.MachineClassStatus{
		ObservedGeneration: 1,
		ResolvedRevision:   "rev-1",
		ProviderConfig:     providerConfig,
		BootstrapFiles:     bootstrapFiles,
		Configuration: &unboundedv1alpha3.ConfigurationContentSnapshot{
			Name: classTestConfiguration, Version: 1, VersionName: "workers-v1", UID: "workers-v1-uid", Hash: templateHash,
		},
		Site:       &unboundedv1alpha3.UIDReference{Name: classTestSite, UID: "site-a-uid"},
		Credential: &unboundedv1alpha3.UIDReference{Name: "cred-a", UID: "cred-a-uid"},
		Offerings: []unboundedv1alpha3.MachineClassOffering{
			{
				Name:         "ondemand-large",
				InstanceType: "Standard_D8",
				Zone:         "zone-2",
				CapacityType: unboundedv1alpha3.CapacityTypeOnDemand,
				Capacity:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("32Gi")},
				Availability: unboundedv1alpha3.OfferingAvailable,
				Price:        &unboundedv1alpha3.OfferingPrice{Currency: "USD", PerHour: "0.40"},
				ValidUntil:   &validUntil,
			},
			{
				Name:         "spot-small",
				InstanceType: "Standard_D2",
				Zone:         "zone-1",
				CapacityType: unboundedv1alpha3.CapacityTypeSpot,
				Labels:       map[string]string{"example.com/family": "d", "team": "from-offering-is-not-set"},
				Capacity:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("8Gi")},
				Overhead: unboundedv1alpha3.OfferingOverhead{
					KubeReserved:      corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
					SystemReserved:    corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					EvictionThreshold: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				},
				Availability: unboundedv1alpha3.OfferingAvailable,
				Price:        &unboundedv1alpha3.OfferingPrice{Currency: "USD", PerHour: "0.05"},
				ValidUntil:   &validUntil,
			},
		},
		Conditions: []metav1.Condition{{
			Type: unboundedv1alpha3.MachineClassConditionReady, Status: metav1.ConditionTrue, Reason: classReasonResolved,
			ObservedGeneration: 1, LastTransitionTime: metav1.NewTime(allocTestNow),
		}},
	}
	// The offering's own team label is dropped so the node template label
	// shows through.
	delete(class.Status.Offerings[1].Labels, "team")

	credential := newWorkloadIdentityCredential("cred-a", classTestSite, classTestProvider)
	credential.UID = "cred-a-uid"

	return &allocationFixture{
		class:          class,
		site:           classFixture.site,
		providerConfig: classFixture.providerConfig,
		bootstrapFiles: classFixture.bootstrapFiles,
		version:        version,
		credential:     credential,
		consumer:       &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: allocTestConsumer, UID: allocTestConsumerUID}},
		allocation: &unboundedv1alpha3.MachineAllocation{
			ObjectMeta: metav1.ObjectMeta{Name: allocTestName, UID: allocTestUID, Generation: 1},
			Spec: unboundedv1alpha3.MachineAllocationSpec{
				ConsumerRef: unboundedv1alpha3.MachineAllocationConsumerReference{
					APIGroup: allocTestConsumerKind.Group, Kind: allocTestConsumerKind.Kind, Name: allocTestConsumer, UID: allocTestConsumerUID,
				},
				ClassRef:  unboundedv1alpha3.UIDReference{Name: classTestName, UID: "class-a-uid"},
				Resources: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
				NodeTemplate: &unboundedv1alpha3.MachineAllocationNodeTemplate{
					Labels: map[string]string{"team": "from-template"},
					Taints: []corev1.Taint{
						{Key: "dedicated", Value: "b", Effect: corev1.TaintEffectNoSchedule},
						{Key: "gpu", Effect: corev1.TaintEffectNoExecute},
					},
					StartupTaints: []corev1.Taint{{Key: "example.com/not-ready", Effect: corev1.TaintEffectNoSchedule}},
				},
				Lifecycle: unboundedv1alpha3.MachineAllocationActive,
			},
		},
	}
}

func requireAllocationMachine(t *testing.T, machine *unboundedv1alpha3.Machine) {
	t.Helper()

	version := int32(1)

	require.Equal(t, allocTestNames.machine, machine.Name)
	require.Equal(t, map[string]string{
		unboundedv1alpha3.MachineSiteLabelKey: classTestSite,
		unboundedv1alpha3.LabelAllocationUID:  string(allocTestUID),
		unboundedv1alpha3.LabelMachineClass:   classTestName,
	}, machine.Labels)
	require.Equal(t, []string{unboundedv1alpha3.MachineAllocationFinalizer}, machine.Finalizers)
	requireControllerOwner(t, machine.OwnerReferences)
	require.Equal(t, unboundedv1alpha3.MachineSpec{
		AllocationRef: &unboundedv1alpha3.UIDReference{Name: allocTestName, UID: allocTestUID},
		Host: &unboundedv1alpha3.HostSpec{
			Image:              allocTestHostImage,
			ProvisioningFormat: unboundedv1alpha3.ProvisioningFormatCloudInit,
			External: &unboundedv1alpha3.ExternalHostSpec{
				Provider:   classTestProvider,
				ProviderID: "example:///hosts/" + allocTestNames.machine,
			},
		},
		ConfigurationRef: &unboundedv1alpha3.MachineConfigurationRef{Name: classTestConfiguration, Version: &version},
		Kubernetes: &unboundedv1alpha3.KubernetesSpec{
			Version: "v1.34.0",
			NodeLabels: map[string]string{
				"tier":                              "gpu",
				"team":                              "from-template",
				"example.com/family":                "d",
				corev1.LabelInstanceTypeStable:      "Standard_D2",
				corev1.LabelTopologyZone:            "zone-1",
				unboundedv1alpha3.LabelCapacityType: "Spot",
			},
			RegisterWithTaints: []string{"dedicated=a:NoSchedule", "gpu:NoExecute", "example.com/not-ready:NoSchedule"},
		},
		Agent: &unboundedv1alpha3.AgentSpec{
			Image:    "ghcr.io/example/node:1",
			LocalDNS: &unboundedv1alpha3.LocalDNSSpec{Enabled: true},
		},
	}, machine.Spec)
	require.Nil(t, machine.Spec.Kubernetes.BootstrapTokenRef)
}

func requireControllerOwner(t *testing.T, owners []metav1.OwnerReference) {
	t.Helper()

	require.Len(t, owners, 1)
	require.Equal(t, unboundedv1alpha3.GroupVersion.String(), owners[0].APIVersion)
	require.Equal(t, "MachineAllocation", owners[0].Kind)
	require.Equal(t, allocTestName, owners[0].Name)
	require.Equal(t, allocTestUID, owners[0].UID)
	require.NotNil(t, owners[0].Controller)
	require.True(t, *owners[0].Controller)
	require.Nil(t, owners[0].BlockOwnerDeletion)
}

func requireAllocationCondition(
	t *testing.T,
	allocation *unboundedv1alpha3.MachineAllocation,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
) *metav1.Condition {
	t.Helper()

	condition := apimeta.FindStatusCondition(allocation.Status.Conditions, conditionType)
	require.NotNil(t, condition, "condition %s in %v", conditionType, allocation.Status.Conditions)
	require.Equal(t, status, condition.Status, "condition %s: %s", conditionType, condition.Message)
	require.Equal(t, reason, condition.Reason, "condition %s: %s", conditionType, condition.Message)
	require.Equal(t, allocation.Generation, condition.ObservedGeneration)

	return condition
}

func requireQuantity(t *testing.T, want string, got resource.Quantity) {
	t.Helper()

	expected := resource.MustParse(want)
	require.Zero(t, expected.Cmp(got), "quantity %s, want %s", got.String(), want)
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	return keys
}

// allocationCall is one API call the reconciler made.
type allocationCall struct {
	reader          string
	verb            string
	kind            string
	name            string
	body            string
	preconditionUID types.UID
	// refused reports a write the harness failed before it reached a store.
	refused bool
}

// allocationHarness runs a MachineAllocationReconciler against three fake
// stores: the main store, which holds no ConfigMaps or Secrets, the
// ConfigMap store and the Secret store. Reads through the wrong reader fail.
// The cache is the main store seen through hidden keys and a stale
// allocation; the live reader reads the stores directly.
type allocationHarness struct {
	store      client.WithWatch
	configMaps client.WithWatch
	secrets    client.WithWatch
	provider   *fakeCapacityProvider
	reconciler *MachineAllocationReconciler

	mu              sync.Mutex
	calls           []allocationCall
	hidden          map[string]bool
	staleAllocation *unboundedv1alpha3.MachineAllocation
	now             time.Time
	uids            int
	crashAfter      int
	writesSeen      int
	failCreateKind  string
	failStatus      bool
}

var errAllocationTestCrash = errors.New("controller crashed")

func newAllocationHarness(t *testing.T, fixture *allocationFixture) *allocationHarness {
	t.Helper()

	h := &allocationHarness{provider: newFakeCapacityProvider(), hidden: map[string]bool{}, now: allocTestNow}

	scheme := newOperationTestScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))

	h.store = fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(fixture.class, fixture.site, fixture.version, fixture.credential, fixture.consumer, fixture.allocation).
		WithStatusSubresource(&unboundedv1alpha3.MachineClass{}, &unboundedv1alpha3.MachineAllocation{}, &unboundedv1alpha3.MachineOperation{}, &unboundedv1alpha3.Machine{}).
		Build()

	coreScheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(coreScheme))

	h.configMaps = fake.NewClientBuilder().WithScheme(coreScheme).WithObjects(fixture.providerConfig, fixture.bootstrapFiles).Build()
	h.secrets = fake.NewClientBuilder().WithScheme(coreScheme).Build()

	h.restart()

	return h
}

// restart builds a new reconciler on the same stores with an empty cache.
func (h *allocationHarness) restart() {
	h.mu.Lock()
	h.hidden = map[string]bool{}
	h.staleAllocation = nil
	h.mu.Unlock()

	consumerKinds := []schema.GroupVersionKind{allocTestConsumerKind}

	var providers []*Provider
	if h.reconciler != nil {
		providers = h.reconciler.Providers
		consumerKinds = h.reconciler.ConsumerKinds
	}

	h.reconciler = &MachineAllocationReconciler{
		Client:          interceptor.NewClient(h.store, h.cacheFuncs()),
		ConfigMapReader: interceptor.NewClient(h.configMaps, h.onlyKindFuncs("configmaps", "ConfigMap")),
		APIReader:       &allocationLiveReader{h: h},
		Providers:       providers,
		ConsumerKinds:   consumerKinds,
		Now:             h.clock,
	}

	if providers == nil {
		h.reconciler.Providers = []*Provider{newAllocationTestProvider(nil, classTestProvider, h.provider)}
	}
}

func (h *allocationHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.now
}

func (h *allocationHarness) setNow(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.now = now
}

func (h *allocationHarness) record(call allocationCall) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	err := h.refusal(call)
	call.refused = err != nil
	h.calls = append(h.calls, call)

	return err
}

func (h *allocationHarness) refusal(call allocationCall) error {
	if call.verb == "get" || call.verb == "list" {
		return nil
	}

	if h.crashAfter > 0 && h.writesSeen >= h.crashAfter {
		return errAllocationTestCrash
	}

	h.writesSeen++

	if call.verb == "create" && call.kind == h.failCreateKind {
		return fmt.Errorf("create %s is failing", call.kind)
	}

	if call.verb == "status" && h.failStatus {
		return errors.New("status writes are failing")
	}

	return nil
}

func (h *allocationHarness) cacheFuncs() interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			kind := allocationTestKind(obj)
			_ = h.record(allocationCall{reader: "cache", verb: "get", kind: kind, name: key.Name})

			if kind == "ConfigMap" || kind == "Secret" {
				return fmt.Errorf("%s %s read through the manager client", kind, key.Name)
			}

			h.mu.Lock()
			hidden := h.hidden[kind+"/"+key.Name]
			stale := h.staleAllocation

			allocation, isAllocation := obj.(*unboundedv1alpha3.MachineAllocation)
			if isAllocation {
				h.staleAllocation = nil
			}
			h.mu.Unlock()

			if hidden {
				return apierrors.NewNotFound(schema.GroupResource{Resource: kind}, key.Name)
			}

			if isAllocation && stale != nil {
				stale.DeepCopyInto(allocation)
				return nil
			}

			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			_ = h.record(allocationCall{reader: "cache", verb: "list", kind: fmt.Sprintf("%T", list)})
			return c.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := h.record(allocationCall{verb: "create", kind: allocationTestKind(obj), name: obj.GetName()}); err != nil {
				return err
			}

			h.mu.Lock()
			h.uids++
			obj.SetUID(types.UID(fmt.Sprintf("%s-uid-%d", obj.GetName(), h.uids)))
			h.mu.Unlock()

			return h.storeFor(obj, c).Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := h.record(allocationCall{verb: "update", kind: allocationTestKind(obj), name: obj.GetName()}); err != nil {
				return err
			}

			return h.storeFor(obj, c).Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			body, err := patch.Data(obj)
			if err != nil {
				return err
			}

			if err := h.record(allocationCall{verb: "patch", kind: allocationTestKind(obj), name: obj.GetName(), body: string(body)}); err != nil {
				return err
			}

			return h.storeFor(obj, c).Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleteOptions := &client.DeleteOptions{}
			deleteOptions.ApplyOptions(opts)

			var uid types.UID
			if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil {
				uid = *deleteOptions.Preconditions.UID
			}

			if err := h.record(allocationCall{verb: "delete", kind: allocationTestKind(obj), name: obj.GetName(), preconditionUID: uid}); err != nil {
				return err
			}

			return h.storeFor(obj, c).Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if err := h.record(allocationCall{verb: "status", kind: allocationTestKind(obj), name: obj.GetName()}); err != nil {
				return err
			}

			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if err := h.record(allocationCall{verb: "status-patch", kind: allocationTestKind(obj), name: obj.GetName()}); err != nil {
				return err
			}

			return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
	}
}

// storeFor routes a write to the store that holds the object's kind.
func (h *allocationHarness) storeFor(obj client.Object, main client.WithWatch) client.WithWatch {
	switch obj.(type) {
	case *corev1.Secret:
		return h.secrets
	case *corev1.ConfigMap:
		return h.configMaps
	default:
		return main
	}
}

// onlyKindFuncs records reads through a reader and fails reads of any other
// kind than kind.
func (h *allocationHarness) onlyKindFuncs(reader, kind string) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			got := allocationTestKind(obj)
			_ = h.record(allocationCall{reader: reader, verb: "get", kind: got, name: key.Name})

			if got != kind {
				return fmt.Errorf("%s %s read through the %s reader", got, key.Name, reader)
			}

			return c.Get(ctx, key, obj, opts...)
		},
	}
}

// allocationLiveReader reads Secrets from the Secret store and every other
// kind except ConfigMaps from the main store.
type allocationLiveReader struct {
	h *allocationHarness
}

func (r *allocationLiveReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	kind := allocationTestKind(obj)
	_ = r.h.record(allocationCall{reader: "live", verb: "get", kind: kind, name: key.Name})

	switch kind {
	case "Secret":
		return r.h.secrets.Get(ctx, key, obj, opts...)
	case "ConfigMap":
		return fmt.Errorf("ConfigMap %s read through the API reader", key.Name)
	default:
		return r.h.store.Get(ctx, key, obj, opts...)
	}
}

func (r *allocationLiveReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	_ = r.h.record(allocationCall{reader: "live", verb: "list", kind: fmt.Sprintf("%T", list)})
	return r.h.store.List(ctx, list, opts...)
}

func allocationTestKind(obj runtime.Object) string {
	if partial, ok := obj.(*metav1.PartialObjectMetadata); ok {
		return partial.GroupVersionKind().Kind
	}

	name := fmt.Sprintf("%T", obj)

	return name[strings.LastIndex(name, ".")+1:]
}

func (h *allocationHarness) reconcile() {
	if _, err := h.reconcileResult(); err != nil {
		panic(err)
	}
}

func (h *allocationHarness) reconcileResult() (ctrl.Result, error) {
	return h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: allocTestName}})
}

// settle reconciles until a reconcile writes nothing, as watch events would.
func (h *allocationHarness) settle() {
	for range 20 {
		before := len(h.writes())

		if _, err := h.reconcileResult(); err != nil {
			panic(err)
		}

		if len(h.writes()) == before {
			return
		}
	}

	panic("allocation did not settle")
}

func (h *allocationHarness) callsSnapshot() []allocationCall {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.calls)
}

func (h *allocationHarness) resetCalls() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.calls = nil
}

// writes returns "verb Kind name" for every write.
func (h *allocationHarness) writes() []string {
	var writes []string

	for _, call := range h.callsSnapshot() {
		if call.verb != "get" && call.verb != "list" && !call.refused {
			writes = append(writes, call.verb+" "+call.kind+" "+call.name)
		}
	}

	return writes
}

func (h *allocationHarness) creates() []string {
	var creates []string

	for _, write := range h.writes() {
		if strings.HasPrefix(write, "create ") {
			creates = append(creates, write)
		}
	}

	return creates
}

// reads returns "reader Kind name" for every Get.
func (h *allocationHarness) reads() []string {
	var reads []string

	for _, call := range h.callsSnapshot() {
		if call.verb == "get" {
			reads = append(reads, call.reader+" "+call.kind+" "+call.name)
		}
	}

	return reads
}

func (h *allocationHarness) patchBodies(kind string) []string {
	var bodies []string

	for _, call := range h.callsSnapshot() {
		if call.verb == "patch" && call.kind == kind {
			bodies = append(bodies, call.body)
		}
	}

	return bodies
}

func (h *allocationHarness) deleteCalls(kind string) []allocationCall {
	var deletes []allocationCall

	for _, call := range h.callsSnapshot() {
		if call.verb == "delete" && call.kind == kind {
			deletes = append(deletes, call)
		}
	}

	return deletes
}

func (h *allocationHarness) hide(kind, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.hidden[kind+"/"+name] = true
}

func (h *allocationHarness) serveStaleAllocation(allocation *unboundedv1alpha3.MachineAllocation) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.staleAllocation = allocation.DeepCopy()
}

// crashAfterWrites makes every write after the first n fail, as if the
// controller stopped. 0 lets writes through again.
func (h *allocationHarness) crashAfterWrites(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.crashAfter = n
	h.writesSeen = 0
}

func (h *allocationHarness) crashed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.crashAfter > 0 && h.writesSeen >= h.crashAfter
}

func (h *allocationHarness) failCreates(kind string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.failCreateKind = kind
}

func (h *allocationHarness) failStatusWrites(fail bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.failStatus = fail
}

func (h *allocationHarness) allocation() *unboundedv1alpha3.MachineAllocation {
	var allocation unboundedv1alpha3.MachineAllocation
	if err := h.store.Get(context.Background(), client.ObjectKey{Name: allocTestName}, &allocation); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		panic(err)
	}

	return &allocation
}

func (h *allocationHarness) machine() *unboundedv1alpha3.Machine {
	var machine unboundedv1alpha3.Machine
	if err := h.store.Get(context.Background(), client.ObjectKey{Name: allocTestNames.machine}, &machine); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		panic(err)
	}

	return &machine
}

func (h *allocationHarness) operation(name string) *unboundedv1alpha3.MachineOperation {
	var operation unboundedv1alpha3.MachineOperation
	if err := h.store.Get(context.Background(), client.ObjectKey{Name: name}, &operation); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		panic(err)
	}

	return &operation
}

func (h *allocationHarness) consumer() *corev1.Namespace {
	var consumer corev1.Namespace
	if err := h.store.Get(context.Background(), client.ObjectKey{Name: allocTestConsumer}, &consumer); err != nil {
		panic(err)
	}

	return &consumer
}

func (h *allocationHarness) secret(name string) *corev1.Secret {
	var secret corev1.Secret
	if err := h.secrets.Get(context.Background(), client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		panic(err)
	}

	return &secret
}

func (h *allocationHarness) configMap(name string) *corev1.ConfigMap {
	var configMap corev1.ConfigMap
	if err := h.configMaps.Get(context.Background(), client.ObjectKey{Namespace: classTestConfigNamespace, Name: name}, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		panic(err)
	}

	return &configMap
}

func (h *allocationHarness) deleteFromStore(t *testing.T, object client.Object, name string) {
	t.Helper()

	require.NoError(t, h.store.Get(context.Background(), client.ObjectKey{Name: name}, object))
	require.NoError(t, h.store.Delete(context.Background(), object))
}

// deleteConfigMap removes a ConfigMap, finalizers included.
func (h *allocationHarness) deleteConfigMap(t *testing.T, name string) {
	t.Helper()

	configMap := h.configMap(name)
	configMap.Finalizers = nil
	require.NoError(t, h.configMaps.Update(context.Background(), configMap))
	require.NoError(t, h.configMaps.Delete(context.Background(), configMap))
}

func (h *allocationHarness) deleteSecret(t *testing.T, name string) {
	t.Helper()

	require.NoError(t, h.secrets.Delete(context.Background(), h.secret(name)))
}

// deleteMachineForcibly removes the Machine's finalizers and deletes it.
func (h *allocationHarness) deleteMachineForcibly(t *testing.T) {
	t.Helper()

	machine := h.machine()
	machine.Finalizers = nil
	require.NoError(t, h.store.Update(context.Background(), machine))
	require.NoError(t, h.store.Delete(context.Background(), machine))
}

// deleteOperationForcibly removes the operation's finalizers and deletes it.
func (h *allocationHarness) deleteOperationForcibly(t *testing.T, name string) {
	t.Helper()

	operation := h.operation(name)
	operation.Finalizers = nil
	require.NoError(t, h.store.Update(context.Background(), operation))
	require.NoError(t, h.store.Delete(context.Background(), operation))
}

// loseStatus drops the allocation status, as a restore without status does.
func (h *allocationHarness) loseStatus(t *testing.T) {
	t.Helper()

	allocation := h.allocation()
	allocation.Status = unboundedv1alpha3.MachineAllocationStatus{}
	require.NoError(t, h.store.Status().Update(context.Background(), allocation))
}

func (h *allocationHarness) setLifecycle(lifecycle unboundedv1alpha3.MachineAllocationLifecycle) {
	allocation := h.allocation()
	allocation.Spec.Lifecycle = lifecycle
	allocation.Generation++

	if err := h.store.Update(context.Background(), allocation); err != nil {
		panic(err)
	}
}

func (h *allocationHarness) updateAllocationSpec(t *testing.T, mutate func(*unboundedv1alpha3.MachineAllocationSpec)) {
	t.Helper()

	allocation := h.allocation()
	mutate(&allocation.Spec)
	require.NoError(t, h.store.Update(context.Background(), allocation))
}

func (h *allocationHarness) updateClass(t *testing.T, mutate func(*unboundedv1alpha3.MachineClass)) {
	t.Helper()

	// An update of an object with a status subresource keeps the stored
	// status, and a status update keeps the rest, so mutate is applied for
	// each.
	for _, update := range []func(context.Context, client.Object) error{
		func(ctx context.Context, object client.Object) error { return h.store.Status().Update(ctx, object) },
		func(ctx context.Context, object client.Object) error { return h.store.Update(ctx, object) },
	} {
		var class unboundedv1alpha3.MachineClass
		require.NoError(t, h.store.Get(context.Background(), client.ObjectKey{Name: classTestName}, &class))
		mutate(&class)
		require.NoError(t, update(context.Background(), &class))
	}
}

func (h *allocationHarness) updateTemplate(t *testing.T, mutate func(*unboundedv1alpha3.MachineConfigurationTemplate)) {
	t.Helper()

	var version unboundedv1alpha3.MachineConfigurationVersion
	require.NoError(t, h.store.Get(context.Background(), client.ObjectKey{Name: "workers-v1"}, &version))
	mutate(&version.Spec.Template)
	require.NoError(t, h.store.Update(context.Background(), &version))

	hash, err := contentHash(version.Spec.Template)
	require.NoError(t, err)
	h.updateClass(t, func(class *unboundedv1alpha3.MachineClass) { class.Status.Configuration.Hash = hash })
}

func (h *allocationHarness) setOperationStatus(t *testing.T, name string, mutate func(*unboundedv1alpha3.MachineOperation)) {
	t.Helper()

	operation := h.operation(name)
	mutate(operation)
	require.NoError(t, h.store.Status().Update(context.Background(), operation))
}

// setOperationHandle plays the engine persisting a provider handle.
func (h *allocationHarness) setOperationHandle(name string) {
	operation := h.operation(name)
	operation.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	operation.Status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{{
		MachineRef:        operation.Spec.MachineRef,
		Phase:             unboundedv1alpha3.OperationPhaseInProgress,
		Attempts:          1,
		ProviderOperation: &unboundedv1alpha3.ProviderOperationStatus{Provider: classTestProvider, OperationID: "provider-op-1"},
	}}

	if err := h.store.Status().Update(context.Background(), operation); err != nil {
		panic(err)
	}
}

// completeOperation plays the engine finishing an operation.
func (h *allocationHarness) completeOperation(
	name string,
	phase unboundedv1alpha3.OperationPhase,
	reason string,
	result *unboundedv1alpha3.MachineOperationAllocationResult,
) {
	operation := h.operation(name)
	operation.Status.Phase = phase
	operation.Status.Allocation = result

	status := metav1.ConditionTrue
	if phase == unboundedv1alpha3.OperationPhaseFailed {
		status = metav1.ConditionFalse
	}

	operation.Status.Conditions = []metav1.Condition{{
		Type: OperationConditionCompleted, Status: status, Reason: reason, Message: "done", LastTransitionTime: metav1.NewTime(allocTestNow),
	}}

	if err := h.store.Status().Update(context.Background(), operation); err != nil {
		panic(err)
	}
}

func (h *allocationHarness) createNode(name, providerID string, ready bool) {
	readyStatus := corev1.ConditionFalse
	if ready {
		readyStatus = corev1.ConditionTrue
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-node-uid")},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: readyStatus}},
		},
	}

	if err := h.store.Create(context.Background(), node); err != nil {
		panic(err)
	}
}

// foreignOperation returns an operation under the allocation's HostAllocate
// name that belongs to another allocation, adjusted by mutate.
func (h *allocationHarness) foreignOperation(mutate func(*unboundedv1alpha3.MachineOperation)) *unboundedv1alpha3.MachineOperation {
	operation := &unboundedv1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: allocTestNames.allocateOperation, UID: "foreign-uid"},
		Spec: unboundedv1alpha3.MachineOperationSpec{
			MachineRef:    allocTestNames.machine,
			OperationKind: unboundedv1alpha3.OperationHostAllocate,
			AllocationRef: &unboundedv1alpha3.UIDReference{Name: allocTestName, UID: "other-allocation-uid"},
		},
	}

	if mutate != nil {
		mutate(operation)
	}

	return operation
}

// fakeCapacityProvider records the capacity and allocation callbacks of one
// provider registration and returns configured results.
type fakeCapacityProvider struct {
	mu sync.Mutex

	plan    publicmachineops.AllocationPlan
	planErr error
	plans   []publicmachineops.AllocationPlanRequest

	allocateBegins []OperationRequest
	allocatePolls  []OperationRequest
	releaseBegins  []OperationRequest
	releasePolls   []OperationRequest
	beginErr       error
	pollResult     publicmachineops.PollResult
	pollErr        error
}

func newFakeCapacityProvider() *fakeCapacityProvider {
	return &fakeCapacityProvider{
		plan: publicmachineops.AllocationPlan{ProviderID: "example:///hosts/" + allocTestNames.machine, HostImage: allocTestHostImage},
	}
}

func (f *fakeCapacityProvider) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.plans = nil
	f.allocateBegins = nil
	f.allocatePolls = nil
	f.releaseBegins = nil
	f.releasePolls = nil
}

func (f *fakeCapacityProvider) setPlan(plan publicmachineops.AllocationPlan, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.plan = plan
	f.planErr = err
}

func (f *fakeCapacityProvider) planRequests() []publicmachineops.AllocationPlanRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.plans)
}

func (f *fakeCapacityProvider) ResolveClass(context.Context, publicmachineops.ClassRequest) (publicmachineops.ClassResolution, error) {
	return publicmachineops.ClassResolution{}, nil
}

func (f *fakeCapacityProvider) PlanAllocation(_ context.Context, request publicmachineops.AllocationPlanRequest) (publicmachineops.AllocationPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.plans = append(f.plans, request)

	return f.plan, f.planErr
}

func (f *fakeCapacityProvider) beginAllocate(_ context.Context, request OperationRequest) (publicmachineops.BeginResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.allocateBegins = append(f.allocateBegins, request)
	if f.beginErr != nil {
		return publicmachineops.BeginResult{}, f.beginErr
	}

	return publicmachineops.BeginResult{Operation: publicmachineops.ProviderOperation{OperationID: "allocate-" + string(request.OperationUID)}}, nil
}

func (f *fakeCapacityProvider) pollAllocate(_ context.Context, request OperationRequest, _ publicmachineops.ProviderOperation) (publicmachineops.PollResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.allocatePolls = append(f.allocatePolls, request)

	return f.pollResult, f.pollErr
}

func (f *fakeCapacityProvider) beginRelease(_ context.Context, request OperationRequest) (publicmachineops.BeginResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.releaseBegins = append(f.releaseBegins, request)

	return publicmachineops.BeginResult{Operation: publicmachineops.ProviderOperation{OperationID: "release-" + string(request.OperationUID)}}, nil
}

func (f *fakeCapacityProvider) pollRelease(_ context.Context, request OperationRequest, _ publicmachineops.ProviderOperation) (publicmachineops.PollResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.releasePolls = append(f.releasePolls, request)

	return f.pollResult, f.pollErr
}

func (f *fakeCapacityProvider) setPoll(result publicmachineops.PollResult, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pollResult = result
	f.pollErr = err
}

func (f *fakeCapacityProvider) calls() (allocateBegins, allocatePolls, releaseBegins, releasePolls []OperationRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.allocateBegins), slices.Clone(f.allocatePolls), slices.Clone(f.releaseBegins), slices.Clone(f.releasePolls)
}

// newAllocationTestProvider registers fake with capacity and long-running
// HostAllocate, HostRelease and HostHardReboot.
func newAllocationTestProvider(t *testing.T, name string, fake *fakeCapacityProvider) *Provider {
	provider, err := publicmachineops.NewProvider(
		name,
		publicmachineops.WithCapacity(fake.ResolveClass, fake.PlanAllocation),
		publicmachineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostAllocate, fake.beginAllocate, fake.pollAllocate),
		publicmachineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostRelease, fake.beginRelease, fake.pollRelease),
		publicmachineops.WithImmediateOperation(unboundedv1alpha3.OperationHostHardReboot, func(context.Context, OperationRequest) (OperationResult, error) {
			return OperationResult{}, nil
		}),
		publicmachineops.WithImmediateOperation(unboundedv1alpha3.OperationHostReplace, func(context.Context, OperationRequest) (OperationResult, error) {
			return OperationResult{}, nil
		}),
		publicmachineops.WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOn, func(context.Context, OperationRequest) (OperationResult, error) {
			return OperationResult{}, nil
		}),
		publicmachineops.WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOff, func(context.Context, OperationRequest) (OperationResult, error) {
			return OperationResult{}, nil
		}),
	)
	if err != nil {
		if t != nil {
			t.Fatal(err)
		}

		panic(err)
	}

	return provider
}
