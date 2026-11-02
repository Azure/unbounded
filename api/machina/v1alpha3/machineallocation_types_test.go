// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

const machineAllocationCRD = "unbounded-cloud.io_machineallocations.yaml"

func newMachineAllocationObject(t *testing.T) map[string]any {
	t.Helper()

	return parseObject(t, `
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineAllocation
metadata:
  name: alloc-1
spec:
  consumerRef:
    apiGroup: karpenter.sh
    kind: NodeClaim
    name: default-x7k2p
    uid: 6d3f0d6e-8d1a-4c1e-9f61-0d7b8f0b2a10
  classRef:
    name: general
    uid: 0b9f8e2c-41a7-4a53-8b6d-3c2f1e0d9a87
`)
}

func newFullMachineAllocationObject(t *testing.T) map[string]any {
	t.Helper()

	return parseObject(t, `
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineAllocation
metadata:
  name: alloc-1
spec:
  consumerRef:
    apiGroup: karpenter.sh
    kind: NodeClaim
    name: default-x7k2p
    uid: 6d3f0d6e-8d1a-4c1e-9f61-0d7b8f0b2a10
  classRef:
    name: general
    uid: 0b9f8e2c-41a7-4a53-8b6d-3c2f1e0d9a87
  resources:
    cpu: "4"
    memory: 8Gi
  requirements:
  - key: topology.kubernetes.io/zone
    operator: In
    values: [eastus-1, eastus-2]
    minValues: 1
  - key: unbounded-cloud.io/capacity-type
    operator: Exists
  nodeTemplate:
    labels:
      team: a
    taints:
    - key: dedicated
      value: a
      effect: NoSchedule
    startupTaints:
    - key: example.com/not-ready
      effect: NoSchedule
`)
}

func newMachineAllocationSnapshot(t *testing.T) map[string]any {
	t.Helper()

	return parseObject(t, `
classUID: 0b9f8e2c-41a7-4a53-8b6d-3c2f1e0d9a87
classGeneration: 3
classRevision: rev-1
provider: AzureVM
siteName: site-a
siteUID: 4a2c1b0e-1d3f-4e5a-8b7c-9d0e1f2a3b4c
credential:
  name: azure
  uid: 7e6d5c4b-3a29-4180-9f8e-7d6c5b4a3928
offering:
  name: standard-d4s-v5-eastus-1-ondemand
  instanceType: Standard_D4s_v5
  zone: eastus-1
  capacityType: OnDemand
  availability: Available
providerConfig:
  namespace: unbounded-system
  name: azure-general
  uid: 1f2e3d4c-5b6a-4798-8a7b-6c5d4e3f2a1b
  hash: sha256-provider
configuration:
  name: default
  version: 1
  versionName: default-v1
  uid: 2a3b4c5d-6e7f-4809-9a1b-2c3d4e5f6a7b
  hash: sha256-configuration
configurationTemplate:
  kubernetes:
    version: 1.33.0
hostImage: ubuntu-24.04
hostProviderID: azure:///subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/alloc-1
`)
}

func TestMachineAllocationValidation(t *testing.T) {
	t.Parallel()

	v := newCRDValidator(t, machineAllocationCRD)

	set := func(value any, path ...string) func(*testing.T, map[string]any) {
		return func(t *testing.T, obj map[string]any) {
			setField(t, obj, value, path...)
		}
	}
	remove := func(path ...string) func(*testing.T, map[string]any) {
		return func(_ *testing.T, obj map[string]any) {
			unstructured.RemoveNestedField(obj, path...)
		}
	}
	requirement := func(fields map[string]any) func(*testing.T, map[string]any) {
		return set([]any{fields}, "spec", "requirements")
	}

	tests := []struct {
		name   string
		base   func(*testing.T) map[string]any
		mutate func(*testing.T, map[string]any)
		want   []string
	}{
		{name: "full allocation", base: newFullMachineAllocationObject},
		{name: "core consumer apiGroup", mutate: set("", "spec", "consumerRef", "apiGroup")},
		{name: "released at create", mutate: set("Released", "spec", "lifecycle")},

		{name: "missing spec", mutate: remove("spec"), want: []string{"spec: Required value"}},
		{name: "missing consumerRef", mutate: remove("spec", "consumerRef"), want: []string{"spec.consumerRef: Required value"}},
		{name: "missing classRef", mutate: remove("spec", "classRef"), want: []string{"spec.classRef: Required value"}},
		{name: "missing consumerRef apiGroup", mutate: remove("spec", "consumerRef", "apiGroup"), want: []string{"spec.consumerRef.apiGroup: Required value"}},
		{
			name:   "empty consumerRef uid",
			mutate: set("", "spec", "consumerRef", "uid"),
			want:   []string{"spec.consumerRef.uid: Invalid value", "at least 1 chars long"},
		},
		{
			name:   "empty consumerRef kind",
			mutate: set("", "spec", "consumerRef", "kind"),
			want:   []string{"spec.consumerRef.kind: Invalid value", "at least 1 chars long"},
		},
		{
			name:   "empty consumerRef name",
			mutate: set("", "spec", "consumerRef", "name"),
			want:   []string{"spec.consumerRef.name: Invalid value", "at least 1 chars long"},
		},
		{
			name:   "empty classRef uid",
			mutate: set("", "spec", "classRef", "uid"),
			want:   []string{"spec.classRef.uid: Invalid value", "at least 1 chars long"},
		},
		{
			name:   "empty classRef name",
			mutate: set("", "spec", "classRef", "name"),
			want:   []string{"spec.classRef.name: Invalid value", "at least 1 chars long"},
		},
		{name: "lifecycle Fenced", mutate: set("Fenced", "spec", "lifecycle"), want: []string{"spec.lifecycle: Unsupported value"}},
		{
			name:   "requirement operator Foo",
			mutate: requirement(map[string]any{"key": "a", "operator": "Foo"}),
			want:   []string{"spec.requirements[0].operator: Unsupported value"},
		},
		{
			name:   "minValues 0",
			mutate: requirement(map[string]any{"key": "a", "operator": "Exists", "minValues": int64(0)}),
			want:   []string{"spec.requirements[0].minValues: Invalid value", "greater than or equal to 1"},
		},
		{
			name:   "empty requirement key",
			mutate: requirement(map[string]any{"key": "", "operator": "Exists"}),
			want:   []string{"spec.requirements[0].key: Invalid value", "at least 1 chars long"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			base := newMachineAllocationObject
			if tt.base != nil {
				base = tt.base
			}

			obj := base(t)
			if tt.mutate != nil {
				tt.mutate(t, obj)
			}

			assertErrors(t, v.validateCreate(obj), tt.want)
		})
	}

	t.Run("minimal allocation defaults lifecycle", func(t *testing.T) {
		t.Parallel()

		obj := newMachineAllocationObject(t)
		assertErrors(t, v.validateCreate(obj), nil)

		lifecycle, _, _ := unstructured.NestedString(obj, "spec", "lifecycle")
		if lifecycle != string(MachineAllocationActive) {
			t.Fatalf("spec.lifecycle = %q, want %q", lifecycle, MachineAllocationActive)
		}
	})
}

// allocationUpdateCase applies one update to a stored allocation.
type allocationUpdateCase struct {
	name   string
	stored func(*testing.T) map[string]any
	mutate func(*testing.T, map[string]any)
	want   []string
}

func runAllocationUpdateCases(t *testing.T, tests []allocationUpdateCase) {
	t.Helper()

	v := newCRDValidator(t, machineAllocationCRD)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			newStored := newFullMachineAllocationObject
			if tt.stored != nil {
				newStored = tt.stored
			}

			stored := newStored(t)
			if errs := v.validateCreate(stored); len(errs) > 0 {
				t.Fatalf("stored allocation is invalid: %v", errs.ToAggregate())
			}

			updated := runtime.DeepCopyJSON(stored)
			tt.mutate(t, updated)
			assertErrors(t, v.validateUpdate(updated, stored), tt.want)
		})
	}
}

func TestMachineAllocationSpecImmutable(t *testing.T) {
	t.Parallel()

	set := func(value any, path ...string) func(*testing.T, map[string]any) {
		return func(t *testing.T, obj map[string]any) {
			setField(t, obj, value, path...)
		}
	}
	remove := func(path ...string) func(*testing.T, map[string]any) {
		return func(_ *testing.T, obj map[string]any) {
			unstructured.RemoveNestedField(obj, path...)
		}
	}
	storedWith := func(value any, path ...string) func(*testing.T) map[string]any {
		return func(t *testing.T) map[string]any {
			obj := newMachineAllocationObject(t)
			setField(t, obj, value, path...)

			return obj
		}
	}

	runAllocationUpdateCases(t, []allocationUpdateCase{
		{name: "label added", mutate: set("x", "metadata", "labels", "team")},
		{name: "finalizer added", mutate: set([]any{"example.com/hold"}, "metadata", "finalizers")},
		{
			name:   "created without nodeTemplate and updated without it",
			stored: newMachineAllocationObject,
			mutate: set("x", "metadata", "labels", "team"),
		},

		{
			name:   "changed consumerRef uid",
			mutate: set("9c8b7a69-5847-4362-a1b0-c9d8e7f6a5b4", "spec", "consumerRef", "uid"),
			want:   []string{"consumerRef is immutable"},
		},
		{
			name:   "changed classRef uid",
			mutate: set("9c8b7a69-5847-4362-a1b0-c9d8e7f6a5b4", "spec", "classRef", "uid"),
			want:   []string{"classRef is immutable"},
		},
		{name: "changed classRef name", mutate: set("gpu", "spec", "classRef", "name"), want: []string{"classRef is immutable"}},
		{name: "changed resources cpu", mutate: set("8", "spec", "resources", "cpu"), want: []string{"resources is immutable"}},
		{
			name:   "resources added",
			stored: newMachineAllocationObject,
			mutate: set(map[string]any{"cpu": "4"}, "spec", "resources"),
			want:   []string{"resources is immutable"},
		},
		{name: "resources removed", mutate: remove("spec", "resources"), want: []string{"resources is immutable"}},
		{
			name:   "requirements added",
			stored: newMachineAllocationObject,
			mutate: set([]any{map[string]any{"key": "a", "operator": "Exists"}}, "spec", "requirements"),
			want:   []string{"requirements is immutable"},
		},
		{name: "requirements removed", mutate: remove("spec", "requirements"), want: []string{"requirements is immutable"}},
		{
			name:   "requirements changed",
			mutate: set([]any{map[string]any{"key": "a", "operator": "Exists"}}, "spec", "requirements"),
			want:   []string{"requirements is immutable"},
		},
		{
			name:   "nodeTemplate added",
			stored: newMachineAllocationObject,
			mutate: set(map[string]any{"labels": map[string]any{"team": "a"}}, "spec", "nodeTemplate"),
			want:   []string{"nodeTemplate is immutable"},
		},
		{name: "nodeTemplate removed", mutate: remove("spec", "nodeTemplate"), want: []string{"nodeTemplate is immutable"}},
		{name: "nodeTemplate label changed", mutate: set("b", "spec", "nodeTemplate", "labels", "team"), want: []string{"nodeTemplate is immutable"}},
		{
			name: "nodeTemplate startupTaint added",
			mutate: set([]any{
				map[string]any{"key": "example.com/not-ready", "effect": "NoSchedule"},
				map[string]any{"key": "example.com/also-not-ready", "effect": "NoSchedule"},
			}, "spec", "nodeTemplate", "startupTaints"),
			want: []string{"nodeTemplate is immutable"},
		},

		// Immutability compares the stored values exactly, not semantically.
		{
			name:   "integer cpu resent as string",
			stored: storedWith(map[string]any{"cpu": int64(2)}, "spec", "resources"),
			mutate: set(map[string]any{"cpu": "2"}, "spec", "resources"),
			want:   []string{"resources is immutable"},
		},
		{
			name:   "memory resent in another unit",
			stored: storedWith(map[string]any{"memory": "8192Mi"}, "spec", "resources"),
			mutate: set(map[string]any{"memory": "8Gi"}, "spec", "resources"),
			want:   []string{"resources is immutable"},
		},
	})
}

func TestMachineAllocationLifecycle(t *testing.T) {
	t.Parallel()

	storedWithLifecycle := func(lifecycle string) func(*testing.T) map[string]any {
		return func(t *testing.T) map[string]any {
			obj := newMachineAllocationObject(t)
			setField(t, obj, lifecycle, "spec", "lifecycle")

			return obj
		}
	}
	setLifecycle := func(lifecycle string) func(*testing.T, map[string]any) {
		return func(t *testing.T, obj map[string]any) {
			setField(t, obj, lifecycle, "spec", "lifecycle")
		}
	}

	const msg = "lifecycle cannot return to Active"

	runAllocationUpdateCases(t, []allocationUpdateCase{
		{name: "Active to Released", stored: storedWithLifecycle("Active"), mutate: setLifecycle("Released")},
		{name: "Released to Released", stored: storedWithLifecycle("Released"), mutate: setLifecycle("Released")},
		{name: "Active to Active", stored: storedWithLifecycle("Active"), mutate: setLifecycle("Active")},
		{
			name:   "Released to Active",
			stored: storedWithLifecycle("Released"),
			mutate: setLifecycle("Active"),
			want:   []string{"spec.lifecycle", msg},
		},
		{
			name:   "Released to omitted",
			stored: storedWithLifecycle("Released"),
			mutate: func(_ *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "spec", "lifecycle")
			},
			want: []string{"spec.lifecycle", msg},
		},
	})
}

func TestMachineAllocationStatusWriteOnce(t *testing.T) {
	t.Parallel()

	storedWithStatus := func(t *testing.T) map[string]any {
		obj := newMachineAllocationObject(t)
		setField(t, obj, newMachineAllocationSnapshot(t), "status", "snapshot")
		setField(t, obj, AllocationProviderIDPrefix+"5d8e2f3a-1b4c-4d6e-8f7a-9b0c1d2e3f4a", "status", "providerID")

		return obj
	}
	setStatus := func(value any, path ...string) func(*testing.T, map[string]any) {
		return func(t *testing.T, obj map[string]any) {
			setField(t, obj, value, append([]string{"status"}, path...)...)
		}
	}
	removeStatus := func(path ...string) func(*testing.T, map[string]any) {
		return func(_ *testing.T, obj map[string]any) {
			unstructured.RemoveNestedField(obj, append([]string{"status"}, path...)...)
		}
	}

	runAllocationUpdateCases(t, []allocationUpdateCase{
		{
			name:   "set snapshot and providerID",
			stored: newMachineAllocationObject,
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, newMachineAllocationSnapshot(t), "status", "snapshot")
				setField(t, obj, AllocationProviderIDPrefix+"5d8e2f3a-1b4c-4d6e-8f7a-9b0c1d2e3f4a", "status", "providerID")
			},
		},
		{
			name:   "rewrite identical snapshot",
			stored: storedWithStatus,
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, newMachineAllocationSnapshot(t), "status", "snapshot")
			},
		},
		{
			name:   "change conditions",
			stored: storedWithStatus,
			mutate: setStatus([]any{map[string]any{
				"type":               MachineAllocationConditionAllocated,
				"status":             "True",
				"reason":             "Launched",
				"message":            "",
				"lastTransitionTime": "2026-01-01T00:00:00Z",
			}}, "conditions"),
		},
		{
			name:   "label added with status set",
			stored: storedWithStatus,
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "x", "metadata", "labels", "team")
			},
		},
		{
			name:   "change nodeRef",
			stored: storedWithStatus,
			mutate: setStatus(map[string]any{"name": "node-a", "uid": "3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f"}, "nodeRef"),
		},

		{
			name:   "change snapshot hostImage",
			stored: storedWithStatus,
			mutate: setStatus("ubuntu-26.04", "snapshot", "hostImage"),
			want:   []string{"status.snapshot", "snapshot is immutable"},
		},
		{
			name:   "remove snapshot",
			stored: storedWithStatus,
			mutate: removeStatus("snapshot"),
			want:   []string{"status.snapshot: Invalid value", "snapshot cannot be removed"},
		},
		{
			name:   "remove whole status",
			stored: storedWithStatus,
			mutate: func(_ *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "status")
			},
			want: []string{
				"status.snapshot: Invalid value", "snapshot cannot be removed",
				"status.providerID: Invalid value", "providerID is immutable once set",
			},
		},
		{
			name:   "null status",
			stored: storedWithStatus,
			mutate: func(_ *testing.T, obj map[string]any) {
				obj["status"] = nil
			},
			want: []string{
				"status.snapshot: Invalid value", "snapshot cannot be removed",
				"status.providerID: Invalid value", "providerID is immutable once set",
			},
		},
		{
			name:   "change providerID",
			stored: storedWithStatus,
			mutate: setStatus(AllocationProviderIDPrefix+"other", "providerID"),
			want:   []string{"status.providerID: Invalid value", "providerID is immutable once set"},
		},
		{
			name:   "remove providerID",
			stored: storedWithStatus,
			mutate: removeStatus("providerID"),
			want:   []string{"status.providerID: Invalid value", "providerID is immutable once set"},
		},
		{name: "failureReason Timeout", mutate: setStatus("Timeout", "failureReason"), want: []string{"status.failureReason: Unsupported value"}},
		{name: "capacityType Reserved", mutate: setStatus("Reserved", "capacityType"), want: []string{"status.capacityType: Unsupported value"}},
		{
			name:   "offering availability Maybe",
			stored: newMachineAllocationObject,
			mutate: func(t *testing.T, obj map[string]any) {
				snapshot := newMachineAllocationSnapshot(t)
				setField(t, snapshot, "Maybe", "offering", "availability")
				setField(t, obj, snapshot, "status", "snapshot")
			},
			want: []string{"status.snapshot.offering.availability: Unsupported value"},
		},
		{
			name:   "price currency usd",
			stored: newMachineAllocationObject,
			mutate: func(t *testing.T, obj map[string]any) {
				snapshot := newMachineAllocationSnapshot(t)
				setField(t, snapshot, map[string]any{"currency": "usd", "perHour": "0.192"}, "offering", "price")
				setField(t, obj, snapshot, "status", "snapshot")
			},
			want: []string{"status.snapshot.offering.price.currency: Invalid value"},
		},
		{
			name:   "ownedResources entry with empty nativeID",
			mutate: setStatus([]any{map[string]any{"id": "vm", "nativeID": ""}}, "ownedResources"),
			want:   []string{"status.ownedResources[0].nativeID: Invalid value", "at least 1 chars long"},
		},
	})
}
