// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
	"github.com/Azure/unbounded/plugins/karpenter/internal/nodeclass"
)

const testProviderID = capacity.ProviderIDPrefix + allocationTestUID

// released marks the allocation's release confirmed for its generation.
func released(allocation *capacity.Allocation) {
	allocation.Spec.Lifecycle = capacity.Released
	setCondition(allocation, capacity.ConditionReleased, metav1.ConditionTrue, "Released")
}

func TestGetAndListReportWhatMayStillExist(t *testing.T) {
	for _, test := range []struct {
		name string
		// change edits the allocation after its snapshot. nil leaves the
		// allocation without a snapshot.
		change func(*capacity.Allocation)
		exists bool
	}{
		{name: "no snapshot"},
		{name: "snapshot before the host is confirmed", change: func(*capacity.Allocation) {}, exists: true},
		{
			name: "failed launch that may have created resources",
			change: func(allocation *capacity.Allocation) {
				allocation.Status.FailureReason = "ProviderFailed"
			},
			exists: true,
		},
		{
			name: "fenced and awaiting release",
			change: func(allocation *capacity.Allocation) {
				setCondition(allocation, capacity.ConditionFenced, metav1.ConditionTrue, "AllocationDeleting")
				setCondition(allocation, capacity.ConditionReleased, metav1.ConditionFalse, "AwaitingRelease")
			},
			exists: true,
		},
		{
			name: "release requested and not confirmed",
			change: func(allocation *capacity.Allocation) {
				allocation.Spec.Lifecycle = capacity.Released
				setCondition(allocation, capacity.ConditionReleased, metav1.ConditionFalse, "Releasing")
			},
			exists: true,
		},
		{
			name: "release confirmed for an older generation",
			change: func(allocation *capacity.Allocation) {
				released(allocation)
				allocation.Generation = 2
			},
			exists: true,
		},
		{name: "release confirmed", change: released},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			provider, store := testProvider(t, testClass())
			object := claimAllocation(t, provider, testClaim())

			if test.change == nil {
				// Give the bare allocation the UID the lookup uses.
				object.SetUID(allocationTestUID)
				must(t, store.Update(ctx, object))
			} else {
				allocation := setSnapshot(t, store, object)
				test.change(allocation)
				writeAllocation(t, store, object, allocation)
			}

			claim, err := provider.Get(ctx, testProviderID)

			claims, listErr := provider.List(ctx)
			must(t, listErr)

			if !test.exists {
				if !cloudprovider.IsNodeClaimNotFoundError(err) || len(claims) != 0 {
					t.Fatalf("nothing can exist: Get returned %v and List %d claims", err, len(claims))
				}

				return
			}

			must(t, err)

			if claim.Status.ProviderID != testProviderID || len(claims) != 1 || claims[0].Status.ProviderID != testProviderID {
				t.Fatal("infrastructure that may still exist must stay visible")
			}
		})
	}
}

func TestGetReportsAnAbsentAllocationAsNotFound(t *testing.T) {
	provider, _ := testProvider(t, testClass())

	if _, err := provider.Get(t.Context(), testProviderID); !cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("expected not found, got %v", err)
	}

	if _, err := provider.Get(t.Context(), "azure:///not-ours"); err == nil || cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("a foreign provider ID is an error, not absence: %v", err)
	}
}

func TestListIgnoresOtherConsumers(t *testing.T) {
	ctx := t.Context()
	provider, store := testProvider(t, testClass())
	object := claimAllocation(t, provider, testClaim())

	allocation := setSnapshot(t, store, object)
	allocation.Spec.ConsumerRef.Kind = "MachinePool"
	writeAllocation(t, store, object, allocation)

	claims, err := provider.List(ctx)
	must(t, err)

	if len(claims) != 0 {
		t.Fatal("an allocation of another consumer kind is not Karpenter's")
	}
}

func TestDeleteAuthorizesReleaseAndConfirmsOnlyAfterIt(t *testing.T) {
	ctx := t.Context()
	provider, store := testProvider(t, testClass())
	claim := testClaim()
	object := claimAllocation(t, provider, claim)
	setAllocated(t, store, object)

	claim.Status.ProviderID = testProviderID

	// Writing release intent is not confirmed absence.
	for range 2 {
		must(t, provider.Delete(ctx, claim))
	}

	stored, err := provider.readAllocation(ctx, capacity.AllocationName(claim.UID))
	must(t, err)

	allocation, err := decodeAllocation(stored)
	must(t, err)

	if allocation.Spec.Lifecycle != capacity.Released {
		t.Fatal("Delete must authorize release")
	}

	if _, err := provider.Get(ctx, testProviderID); err != nil {
		t.Fatalf("an unconfirmed release must stay visible: %v", err)
	}

	released(allocation)
	writeAllocation(t, store, stored, allocation)

	if err := provider.Delete(ctx, claim); !cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("a confirmed release must be reported not found, got %v", err)
	}

	remaining, err := provider.readAllocation(ctx, capacity.AllocationName(claim.UID))
	must(t, err)

	if remaining != nil {
		t.Fatal("Delete must remove the released allocation so its Machine and operations are collected")
	}

	if err := provider.Delete(ctx, claim); !cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("an absent allocation must be reported not found, got %v", err)
	}
}

func TestDeleteAuthorizesReleaseOfAFencedDeletingAllocation(t *testing.T) {
	ctx := t.Context()
	provider, store := testProvider(t, testClass())
	claim := testClaim()
	object := claimAllocation(t, provider, claim)
	setAllocated(t, store, object)

	// The lifecycle controller fenced it by deleting it; the allocation
	// controller's finalizer keeps it.
	object.SetFinalizers([]string{"unbounded-cloud.io/machineallocation"})
	must(t, store.Update(ctx, object))
	must(t, store.Delete(ctx, object))

	must(t, provider.Delete(ctx, claim))

	stored, err := provider.readAllocation(ctx, capacity.AllocationName(claim.UID))
	must(t, err)

	if stored == nil || stored.GetDeletionTimestamp() == nil {
		t.Fatal("the finalizer must keep the deleting allocation")
	}

	allocation, err := decodeAllocation(stored)
	must(t, err)

	if allocation.Spec.Lifecycle != capacity.Released {
		t.Fatal("release must be authorized on an allocation that is already deleting")
	}
}

func TestDeleteRefusesAnAllocationThatIsNotTheNodeClaims(t *testing.T) {
	for name, change := range map[string]func(*capacity.Allocation, *v1.NodeClaim){
		"another NodeClaim incarnation": func(allocation *capacity.Allocation, _ *v1.NodeClaim) {
			allocation.Spec.ConsumerRef.UID = "00000000-0000-4000-8000-000000000000"
		},
		"another provider ID": func(_ *capacity.Allocation, claim *v1.NodeClaim) {
			claim.Status.ProviderID = capacity.ProviderIDPrefix + "00000000-0000-4000-8000-000000000000"
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			provider, store := testProvider(t, testClass())
			claim := testClaim()
			object := claimAllocation(t, provider, claim)

			allocation := setSnapshot(t, store, object)
			change(allocation, claim)
			writeAllocation(t, store, object, allocation)

			if err := provider.Delete(ctx, claim); err == nil || cloudprovider.IsNodeClaimNotFoundError(err) {
				t.Fatalf("expected a refusal, got %v", err)
			}

			stored, err := provider.readAllocation(ctx, capacity.AllocationName(claim.UID))
			must(t, err)

			unchanged, err := decodeAllocation(stored)
			must(t, err)

			if unchanged.Spec.Lifecycle != capacity.Active {
				t.Fatal("a refused Delete must not authorize release")
			}
		})
	}
}

func TestIsDrifted(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(client.Client, *nodeclass.MachineClass) error
		want   cloudprovider.DriftReason
	}{
		{name: "class unchanged"},
		{
			name: "class resolved another revision",
			change: func(store client.Client, class *nodeclass.MachineClass) error {
				class.Status.ResolvedRevision = "another-revision"
				return store.Update(t.Context(), class)
			},
			want: DriftClassRevision,
		},
		{
			name: "class edited and not resolved yet",
			change: func(store client.Client, class *nodeclass.MachineClass) error {
				class.Generation = 2
				class.Status.ResolvedRevision = "another-revision"

				return store.Update(t.Context(), class)
			},
		},
		{
			name: "class deleted",
			change: func(store client.Client, class *nodeclass.MachineClass) error {
				return store.Delete(t.Context(), class)
			},
			want: DriftClassReplaced,
		},
		{
			name: "class recreated under the same name",
			change: func(store client.Client, class *nodeclass.MachineClass) error {
				if err := store.Delete(t.Context(), class); err != nil {
					return err
				}

				recreated := testClass()
				recreated.UID = "00000000-0000-4000-8000-000000000000"

				return store.Create(t.Context(), recreated)
			},
			want: DriftClassReplaced,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			provider, store := testProvider(t, testClass())
			claim := testClaim()
			object := claimAllocation(t, provider, claim)
			setAllocated(t, store, object)

			if test.change != nil {
				class := new(nodeclass.MachineClass)
				must(t, store.Get(ctx, client.ObjectKey{Name: "workers"}, class))
				must(t, test.change(store, class))
			}

			reason, err := provider.IsDrifted(ctx, claim)
			must(t, err)

			if reason != test.want {
				t.Fatalf("drift reason %q, want %q", reason, test.want)
			}
		})
	}
}

func TestIsDriftedBeforeTheSnapshotIsNotDrift(t *testing.T) {
	provider, _ := testProvider(t, testClass())
	claim := testClaim()
	claimAllocation(t, provider, claim)

	reason, err := provider.IsDrifted(t.Context(), claim)
	must(t, err)

	if reason != "" {
		t.Fatalf("an allocation with no frozen revision cannot have drifted, got %q", reason)
	}
}

func TestRestoredSpecCarriesTheAllocationRequest(t *testing.T) {
	provider, store := testProvider(t, testClass())
	claim := testClaim()
	claim.Spec.Taints = []corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoSchedule}}
	object := claimAllocation(t, provider, claim)
	setAllocated(t, store, object)

	hydrated, err := provider.Get(t.Context(), testProviderID)
	must(t, err)

	if hydrated.Spec.NodeClassRef.Name != "workers" || len(hydrated.Spec.Taints) != 1 ||
		len(hydrated.Spec.Requirements) == 0 || !hydrated.Spec.Resources.Requests.Cpu().Equal(*claim.Spec.Resources.Requests.Cpu()) {
		t.Fatalf("hydrated spec lost the request: %+v", hydrated.Spec)
	}
}

// Karpenter deletes a registered NodeClaim whose provider ID is missing from
// List, and List failing stops its garbage collection for every NodeClaim.
// So an allocation that may have a host is always reported, however poorly
// it can be described.
func TestInventoryReportsAllocationsItCannotDescribe(t *testing.T) {
	for name, change := range map[string]func(*capacity.Allocation){
		"no architecture label": func(allocation *capacity.Allocation) {
			delete(allocation.Status.Labels, corev1.LabelArchStable)
		},
		"unknown purchase model": func(allocation *capacity.Allocation) {
			allocation.Status.CapacityType = "Reserved"
		},
		"no resources reported": func(allocation *capacity.Allocation) {
			allocation.Status.Capacity = nil
			allocation.Status.Allocatable = nil
		},
		"provider ID not in the expected form": func(allocation *capacity.Allocation) {
			allocation.Status.ProviderID = "azure:///somewhere"
		},
		"requirement with an unknown purchase model": func(allocation *capacity.Allocation) {
			allocation.Spec.Requirements = []capacity.Requirement{{
				Key: capacity.CapacityTypeLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{"Reserved"},
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			provider, store := testProvider(t, testClass())
			claim := testClaim()
			object := claimAllocation(t, provider, claim)

			allocation := setSnapshot(t, store, object)
			change(allocation)
			writeAllocation(t, store, object, allocation)

			if _, err := hydrate(allocation); err == nil {
				t.Fatal("the fixture must be an allocation Create rejects")
			}

			claims, err := provider.List(ctx)
			must(t, err)

			if len(claims) != 1 || claims[0].Status.ProviderID != testProviderID || claims[0].UID != claim.UID ||
				claims[0].Spec.NodeClassRef == nil || claims[0].Spec.NodeClassRef.Name != "workers" {
				t.Fatalf("List must report the allocation with its identity, got %+v", claims)
			}

			found, err := provider.Get(ctx, testProviderID)
			must(t, err)

			if found.Status.ProviderID != testProviderID || found.Name != claim.Name {
				t.Fatalf("Get must report the allocation with its identity, got %+v", found)
			}
		})
	}
}

// An allocation that names a NodeClaim consumer but was not made by this
// plugin is skipped, and does not fail List for the plugin's own.
func TestListSkipsAllocationsThePluginDidNotMake(t *testing.T) {
	ctx := t.Context()
	provider, store := testProvider(t, testClass())
	object := claimAllocation(t, provider, testClaim())
	setSnapshot(t, store, object)

	const foreignUID = "0d0e3d5e-8a53-4c05-b1a0-2c8a8f5a4f11"

	foreign := object.DeepCopy()
	foreign.SetName("made-by-someone-else")
	foreign.SetUID(foreignUID)
	foreign.SetResourceVersion("")
	must(t, unstructured.SetNestedField(foreign.Object, capacity.ProviderIDPrefix+foreignUID, "status", "providerID"))
	must(t, store.Create(ctx, foreign))

	claims, err := provider.List(ctx)
	must(t, err)

	if len(claims) != 1 || claims[0].Status.ProviderID != testProviderID {
		t.Fatalf("List must report only the plugin's allocation, got %d", len(claims))
	}
}
