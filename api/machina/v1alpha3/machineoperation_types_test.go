// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

const machineOperationCRD = "unbounded-cloud.io_machineoperations.yaml"

// Messages of the MachineOperation allocation rules.
const (
	msgAllocationRefRequired     = "allocationRef is required for HostAllocate and HostRelease"
	msgAllocationRefNotAllowed   = "allocationRef is allowed only for HostAllocate and HostRelease"
	msgAllocationMachineRef      = "machineRef is required for HostAllocate and HostRelease"
	msgAllocationMachineSelector = "machineSelector is not allowed for HostAllocate and HostRelease"
	msgAllocationTTL             = "ttlSecondsAfterFinished is not allowed for HostAllocate and HostRelease"
	msgAllocationSpecImmutable   = "spec is immutable for HostAllocate and HostRelease"
	msgAllocationRefAdded        = "allocationRef cannot be added after creation"
	msgAllocationResultImmutable = "allocation is immutable once set"
	msgAllocationResultKind      = "allocation is allowed only for HostAllocate"
)

// Messages of the MachineOperation selector and rollout rules.
const (
	msgExactlyOneTarget         = "exactly one of machineRef and machineSelector must be set"
	msgMachineSelectorEmpty     = "machineSelector must have at least one matchLabels or matchExpressions entry"
	msgRolloutWithoutSelector   = "rollout is allowed only with machineSelector"
	msgMachineSelectorImmutable = "machineSelector cannot be changed, added or removed after creation"
	msgSelectorSpecImmutable    = "spec is immutable for a selector operation"
	msgMaxConcurrentInvalid     = "maxConcurrent must be an integer of at least 1 or a percentage from 1% to 100%"
)

func TestOperationKindHostRebootValues(t *testing.T) {
	t.Parallel()

	if got := string(OperationHostHardReboot); got != "HostHardReboot" {
		t.Fatalf("OperationHostHardReboot = %q, want %q", got, "HostHardReboot")
	}

	if got := string(OperationHostSoftReboot); got != "HostSoftReboot" {
		t.Fatalf("OperationHostSoftReboot = %q, want %q", got, "HostSoftReboot")
	}
}

func TestMachineOperationKindSchemaEnum(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../../deploy/machina/crd/unbounded-cloud.io_machineoperations.yaml")
	if err != nil {
		t.Fatalf("read MachineOperation CRD: %v", err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("parse MachineOperation CRD: %v", err)
	}

	kindSchema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["operationKind"]

	var got []string

	for _, value := range kindSchema.Enum {
		var kind string
		if err := yaml.Unmarshal(value.Raw, &kind); err != nil {
			t.Fatalf("parse operationKind enum value %s: %v", value.Raw, err)
		}

		got = append(got, kind)
	}

	want := []string{
		string(OperationNodeReboot),
		string(OperationAgentUpgrade),
		string(OperationAgentReset),
		string(OperationHostHardReboot),
		string(OperationHostSoftReboot),
		string(OperationHostPowerOff),
		string(OperationHostPowerOn),
		string(OperationHostReplace),
		string(OperationHostAllocate),
		string(OperationHostRelease),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("operationKind enum = %v, want %v", got, want)
	}
}

func TestOperationKindIsAllocationOperation(t *testing.T) {
	t.Parallel()

	if got := string(OperationHostAllocate); got != "HostAllocate" {
		t.Fatalf("OperationHostAllocate = %q, want %q", got, "HostAllocate")
	}

	if got := string(OperationHostRelease); got != "HostRelease" {
		t.Fatalf("OperationHostRelease = %q, want %q", got, "HostRelease")
	}

	tests := []struct {
		kind OperationKind
		want bool
	}{
		{OperationNodeReboot, false},
		{OperationAgentUpgrade, false},
		{OperationAgentReset, false},
		{OperationHostHardReboot, false},
		{OperationHostSoftReboot, false},
		{OperationHostPowerOff, false},
		{OperationHostPowerOn, false},
		{OperationHostReplace, false},
		{OperationHostAllocate, true},
		{OperationHostRelease, true},
		{"", false},
		{"HostAllocation", false},
	}

	for _, tt := range tests {
		if got := tt.kind.IsAllocationOperation(); got != tt.want {
			t.Errorf("OperationKind(%q).IsAllocationOperation() = %t, want %t", tt.kind, got, tt.want)
		}
	}
}

func TestMachineOperationSpecIsSelectorOperation(t *testing.T) {
	t.Parallel()

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "a"}}

	tests := []struct {
		name string
		spec MachineOperationSpec
		want bool
	}{
		{name: "machineRef only", spec: MachineOperationSpec{MachineRef: "worker-01"}, want: false},
		{name: "selector only", spec: MachineOperationSpec{MachineSelector: selector}, want: true},
		{name: "machineRef and selector", spec: MachineOperationSpec{MachineRef: "worker-01", MachineSelector: selector}, want: false},
		{name: "empty spec", spec: MachineOperationSpec{}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.spec.IsSelectorOperation(); got != tt.want {
				t.Errorf("IsSelectorOperation() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestMachineOperationCRD(t *testing.T) {
	t.Parallel()

	// newCRDValidator fails the test when ValidateCustomResourceDefinition,
	// including the CEL cost estimate, rejects the CRD.
	newCRDValidator(t, machineOperationCRD)

	crd, _ := loadCRD(t, machineOperationCRD)
	root := *crd.Spec.Versions[0].Schema.OpenAPIV3Schema

	allocationRef := root.Properties["spec"].Properties["allocationRef"]
	if got := sortedKeys(allocationRef.Properties); !slices.Equal(got, []string{"name", "uid"}) {
		t.Errorf("spec.allocationRef properties = %v, want [name uid]", got)
	}

	if got := slices.Sorted(slices.Values(allocationRef.Required)); !slices.Equal(got, []string{"name", "uid"}) {
		t.Errorf("spec.allocationRef required = %v, want [name uid]", got)
	}

	allocation := root.Properties["status"].Properties["allocation"]

	want := []string{"image", "instanceType", "ownedResources", "providerResourceID", "zone"}
	if got := sortedKeys(allocation.Properties); !slices.Equal(got, want) {
		t.Errorf("status.allocation properties = %v, want %v", got, want)
	}

	ownedResources := allocation.Properties["ownedResources"]
	if ownedResources.XListType == nil || *ownedResources.XListType != "map" {
		t.Errorf("status.allocation.ownedResources list type = %v, want map", ownedResources.XListType)
	}

	if !slices.Equal(ownedResources.XListMapKeys, []string{"id"}) {
		t.Errorf("status.allocation.ownedResources list map keys = %v, want [id]", ownedResources.XListMapKeys)
	}

	rollout := root.Properties["spec"].Properties["rollout"]
	if got := sortedKeys(rollout.Properties); !slices.Equal(got, []string{"childRetention", "maxConcurrent", "maxFailures", "onFailure"}) {
		t.Errorf("spec.rollout properties = %v, want [childRetention maxConcurrent maxFailures onFailure]", got)
	}

	if !rollout.Properties["maxConcurrent"].XIntOrString {
		t.Error("spec.rollout.maxConcurrent is not x-kubernetes-int-or-string")
	}

	var policies []string

	for _, value := range rollout.Properties["onFailure"].Enum {
		var policy string
		if err := yaml.Unmarshal(value.Raw, &policy); err != nil {
			t.Fatalf("parse onFailure enum value %s: %v", value.Raw, err)
		}

		policies = append(policies, policy)
	}

	wantPolicies := []string{string(RolloutFailurePolicyStop), string(RolloutFailurePolicyContinue)}
	if !slices.Equal(policies, wantPolicies) {
		t.Errorf("spec.rollout.onFailure enum = %v, want %v", policies, wantPolicies)
	}

	targets := root.Properties["status"].Properties["targets"]
	if targets.Items == nil || targets.Items.Schema == nil {
		t.Fatal("status.targets items are missing")
	}

	if _, ok := targets.Items.Schema.Properties["operationRef"]; ok {
		t.Error("status.targets.operationRef exists: the children are the per-machine record")
	}

	summary := root.Properties["status"].Properties["summary"]

	wantSummary := []string{"active", "complete", "failed", "failedMachines", "held", "total"}
	if got := sortedKeys(summary.Properties); !slices.Equal(got, wantSummary) {
		t.Errorf("status.summary properties = %v, want %v", got, wantSummary)
	}

	if max := summary.Properties["failedMachines"].MaxItems; max == nil || *max != 10 {
		t.Errorf("status.summary.failedMachines maxItems = %v, want 10", max)
	}

	if _, ok := root.Properties["spec"].Properties["suspend"]; !ok {
		t.Error("spec.suspend is missing")
	}

	// spec.machineRef is how an agent watches only its own operations.
	// status.phase lets an operator list the failed children of a rollout.
	var selectable []string
	for _, field := range crd.Spec.Versions[0].SelectableFields {
		selectable = append(selectable, field.JSONPath)
	}

	if want := []string{".spec.machineRef", ".status.phase"}; !slices.Equal(selectable, want) {
		t.Errorf("selectable fields = %v, want %v", selectable, want)
	}
}

func sortedKeys(properties map[string]apiextensionsv1.JSONSchemaProps) []string {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	return keys
}

func newAllocationOperationObject(t *testing.T, kind OperationKind) map[string]any {
	t.Helper()

	obj := parseObject(t, `
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: op-1
spec:
  machineRef: worker-01
  allocationRef:
    name: alloc-1
    uid: 5d8e2f3a-1b4c-4d6e-8f7a-9b0c1d2e3f4a
`)
	setField(t, obj, string(kind), "spec", "operationKind")

	return obj
}

func newHostOperationObject(t *testing.T, kind OperationKind) map[string]any {
	t.Helper()

	obj := parseObject(t, `
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: op-1
spec:
  machineRef: worker-01
`)
	setField(t, obj, string(kind), "spec", "operationKind")

	return obj
}

func newSelectorOperationObject(t *testing.T) map[string]any {
	t.Helper()

	return parseObject(t, `
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: op-1
spec:
  operationKind: NodeReboot
  machineSelector:
    matchLabels:
      pool: a
`)
}

func newAllocationResult() map[string]any {
	return map[string]any{
		"providerResourceID": "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/alloc-1",
		"instanceType":       "Standard_D4s_v5",
		"zone":               "eastus-1",
		"image":              "ubuntu-24.04",
		"ownedResources": []any{
			map[string]any{"id": "vm", "nativeID": "vm-0001"},
			map[string]any{"id": "nic", "nativeID": "nic-0001"},
		},
	}
}

// assertNoEvaluationErrors fails if a CEL rule failed to evaluate rather than
// reporting its own message.
func assertNoEvaluationErrors(t *testing.T, errs field.ErrorList) {
	t.Helper()

	if len(errs) == 0 {
		return
	}

	if got := errs.ToAggregate().Error(); strings.Contains(got, "no such key") || strings.Contains(got, "evaluation") {
		t.Errorf("errors contain a CEL evaluation error: %s", got)
	}
}

func TestMachineOperationAllocationCreate(t *testing.T) {
	t.Parallel()

	v := newCRDValidator(t, machineOperationCRD)

	allocation := func(kind OperationKind) func(*testing.T) map[string]any {
		return func(t *testing.T) map[string]any {
			return newAllocationOperationObject(t, kind)
		}
	}
	host := func(kind OperationKind) func(*testing.T) map[string]any {
		return func(t *testing.T) map[string]any {
			return newHostOperationObject(t, kind)
		}
	}
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
	selector := map[string]any{"matchLabels": map[string]any{"pool": "a"}}
	allocationRef := map[string]any{"name": "alloc-1", "uid": "5d8e2f3a-1b4c-4d6e-8f7a-9b0c1d2e3f4a"}

	tests := []struct {
		name   string
		base   func(*testing.T) map[string]any
		mutate func(*testing.T, map[string]any)
		want   []string
	}{
		{name: "HostAllocate with machineRef and allocationRef", base: allocation(OperationHostAllocate)},
		{name: "HostRelease with machineRef and allocationRef", base: allocation(OperationHostRelease)},
		{
			name:   "HostAllocate with parameters",
			base:   allocation(OperationHostAllocate),
			mutate: set(map[string]any{"reason": "scale-up"}, "spec", "parameters"),
		},
		{
			name:   "HostHardReboot with machineRef and ttlSecondsAfterFinished",
			base:   host(OperationHostHardReboot),
			mutate: set(int64(30), "spec", "ttlSecondsAfterFinished"),
		},
		{
			name: "NodeReboot with only machineSelector",
			base: host(OperationNodeReboot),
			mutate: func(t *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "spec", "machineRef")
				setField(t, obj, selector, "spec", "machineSelector")
			},
		},
		{name: "HostReplace with machineRef", base: host(OperationHostReplace)},

		{
			name:   "HostAllocate without allocationRef",
			base:   allocation(OperationHostAllocate),
			mutate: remove("spec", "allocationRef"),
			want:   []string{msgAllocationRefRequired},
		},
		{
			name:   "HostRelease without allocationRef",
			base:   allocation(OperationHostRelease),
			mutate: remove("spec", "allocationRef"),
			want:   []string{msgAllocationRefRequired},
		},
		{
			name:   "HostHardReboot with allocationRef",
			base:   host(OperationHostHardReboot),
			mutate: set(allocationRef, "spec", "allocationRef"),
			want:   []string{msgAllocationRefNotAllowed},
		},
		{
			name: "NodeReboot with machineSelector and allocationRef",
			base: host(OperationNodeReboot),
			mutate: func(t *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "spec", "machineRef")
				setField(t, obj, selector, "spec", "machineSelector")
				setField(t, obj, allocationRef, "spec", "allocationRef")
			},
			want: []string{msgAllocationRefNotAllowed},
		},
		{
			name:   "HostAllocate without machineRef",
			base:   allocation(OperationHostAllocate),
			mutate: remove("spec", "machineRef"),
			want:   []string{msgAllocationMachineRef},
		},
		{
			name:   "HostAllocate with empty machineRef",
			base:   allocation(OperationHostAllocate),
			mutate: set("", "spec", "machineRef"),
			want:   []string{msgAllocationMachineRef},
		},
		{
			name:   "HostRelease with machineRef and machineSelector",
			base:   allocation(OperationHostRelease),
			mutate: set(selector, "spec", "machineSelector"),
			want:   []string{msgAllocationMachineSelector, msgExactlyOneTarget},
		},
		{
			name:   "HostAllocate with ttlSecondsAfterFinished 0",
			base:   allocation(OperationHostAllocate),
			mutate: set(int64(0), "spec", "ttlSecondsAfterFinished"),
			want:   []string{msgAllocationTTL},
		},

		{
			name:   "allocationRef with empty uid",
			base:   allocation(OperationHostAllocate),
			mutate: set("", "spec", "allocationRef", "uid"),
			want:   []string{"spec.allocationRef.uid: Invalid value", "at least 1 chars long"},
		},
		{
			name:   "allocationRef without uid",
			base:   allocation(OperationHostAllocate),
			mutate: remove("spec", "allocationRef", "uid"),
			want:   []string{"spec.allocationRef.uid: Required value"},
		},
		{
			name:   "allocationRef with empty name",
			base:   allocation(OperationHostAllocate),
			mutate: set("", "spec", "allocationRef", "name"),
			want:   []string{"spec.allocationRef.name: Invalid value", "at least 1 chars long"},
		},
		{
			name:   "operationKind HostAllocation",
			base:   allocation(OperationHostAllocate),
			mutate: set("HostAllocation", "spec", "operationKind"),
			want:   []string{"spec.operationKind: Unsupported value"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			obj := tt.base(t)
			if tt.mutate != nil {
				tt.mutate(t, obj)
			}

			errs := v.validateCreate(obj)
			assertErrors(t, errs, tt.want)
			assertNoEvaluationErrors(t, errs)
		})
	}
}

func TestMachineOperationSelectorCreate(t *testing.T) {
	t.Parallel()

	v := newCRDValidator(t, machineOperationCRD)

	machineRef := func(t *testing.T) map[string]any {
		return newHostOperationObject(t, OperationNodeReboot)
	}
	selector := func(t *testing.T) map[string]any {
		return newSelectorOperationObject(t)
	}
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
	matchExpressions := []any{map[string]any{
		"key":      "pool",
		"operator": "In",
		"values":   []any{"a", "b"},
	}}
	rollout := func(value any) map[string]any {
		return map[string]any{"maxConcurrent": value}
	}

	tests := []struct {
		name   string
		base   func(*testing.T) map[string]any
		mutate func(*testing.T, map[string]any)
		want   []string
	}{
		{name: "machineRef only", base: machineRef},
		{name: "selector with matchLabels", base: selector},
		{
			name: "selector with matchExpressions",
			base: selector,
			mutate: func(t *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "spec", "machineSelector", "matchLabels")
				setField(t, obj, matchExpressions, "spec", "machineSelector", "matchExpressions")
			},
		},
		{
			name:   "selector with maxConcurrent 1",
			base:   selector,
			mutate: set(rollout(int64(1)), "spec", "rollout"),
		},
		{
			name:   "selector with maxConcurrent 5",
			base:   selector,
			mutate: set(rollout(int64(5)), "spec", "rollout"),
		},
		{
			name:   "selector with maxConcurrent 100%",
			base:   selector,
			mutate: set(rollout("100%"), "spec", "rollout"),
		},
		{
			name:   "selector with maxConcurrent 25%",
			base:   selector,
			mutate: set(rollout("25%"), "spec", "rollout"),
		},
		{
			name:   "selector with onFailure Continue",
			base:   selector,
			mutate: set(map[string]any{"onFailure": "Continue"}, "spec", "rollout"),
		},

		{
			name:   "machineRef and machineSelector",
			base:   machineRef,
			mutate: set(map[string]any{"matchLabels": map[string]any{"pool": "a"}}, "spec", "machineSelector"),
			want:   []string{msgExactlyOneTarget},
		},
		{
			name:   "neither machineRef nor machineSelector",
			base:   machineRef,
			mutate: remove("spec", "machineRef"),
			want:   []string{msgExactlyOneTarget},
		},
		{
			name: "empty machineSelector",
			base: machineRef,
			mutate: func(t *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "spec", "machineRef")
				setField(t, obj, map[string]any{}, "spec", "machineSelector")
			},
			want: []string{msgMachineSelectorEmpty},
		},
		{
			name:   "rollout with machineRef",
			base:   machineRef,
			mutate: set(rollout(int64(1)), "spec", "rollout"),
			want:   []string{msgRolloutWithoutSelector},
		},
		{
			name:   "maxConcurrent 0",
			base:   selector,
			mutate: set(rollout(int64(0)), "spec", "rollout"),
			want:   []string{msgMaxConcurrentInvalid},
		},
		{
			name:   "maxConcurrent -1",
			base:   selector,
			mutate: set(rollout(int64(-1)), "spec", "rollout"),
			want:   []string{msgMaxConcurrentInvalid},
		},
		{
			// The largest number the field can decode into is accepted, and
			// one more is not: stored, it would break every typed read.
			name:   "maxConcurrent 2147483647",
			base:   selector,
			mutate: set(rollout(int64(2147483647)), "spec", "rollout"),
		},
		{
			name:   "maxConcurrent 2147483648",
			base:   selector,
			mutate: set(rollout(int64(2147483648)), "spec", "rollout"),
			want:   []string{msgMaxConcurrentInvalid},
		},
		{
			name:   "maxConcurrent 0%",
			base:   selector,
			mutate: set(rollout("0%"), "spec", "rollout"),
			want:   []string{msgMaxConcurrentInvalid},
		},
		{
			name:   "maxConcurrent 101%",
			base:   selector,
			mutate: set(rollout("101%"), "spec", "rollout"),
			want:   []string{msgMaxConcurrentInvalid},
		},
		{
			name:   "maxConcurrent abc",
			base:   selector,
			mutate: set(rollout("abc"), "spec", "rollout"),
			want:   []string{msgMaxConcurrentInvalid},
		},
		{
			name:   "onFailure Pause",
			base:   selector,
			mutate: set(map[string]any{"onFailure": "Pause"}, "spec", "rollout"),
			want:   []string{"spec.rollout.onFailure: Unsupported value"},
		},
		{
			name:   "maxFailures with Continue",
			base:   selector,
			mutate: set(map[string]any{"onFailure": "Continue", "maxFailures": int64(50)}, "spec", "rollout"),
		},
		{
			name:   "maxFailures at the ceiling",
			base:   selector,
			mutate: set(map[string]any{"onFailure": "Continue", "maxFailures": int64(1000)}, "spec", "rollout"),
		},
		{
			name:   "maxFailures above the ceiling",
			base:   selector,
			mutate: set(map[string]any{"onFailure": "Continue", "maxFailures": int64(1001)}, "spec", "rollout"),
			want:   []string{"spec.rollout.maxFailures"},
		},
		{
			name:   "maxFailures zero",
			base:   selector,
			mutate: set(map[string]any{"onFailure": "Continue", "maxFailures": int64(0)}, "spec", "rollout"),
			want:   []string{"spec.rollout.maxFailures"},
		},
		{
			name:   "maxFailures with Stop",
			base:   selector,
			mutate: set(map[string]any{"onFailure": "Stop", "maxFailures": int64(5)}, "spec", "rollout"),
			want:   []string{"maxFailures is allowed only with onFailure Continue"},
		},
		{
			name:   "maxFailures without onFailure",
			base:   selector,
			mutate: set(map[string]any{"maxFailures": int64(5)}, "spec", "rollout"),
			want:   []string{"maxFailures is allowed only with onFailure Continue"},
		},
		{
			name:   "childRetention All",
			base:   selector,
			mutate: set(map[string]any{"childRetention": "All"}, "spec", "rollout"),
		},
		{
			name:   "childRetention Failed",
			base:   selector,
			mutate: set(map[string]any{"childRetention": "Failed"}, "spec", "rollout"),
		},
		{
			name:   "childRetention Some",
			base:   selector,
			mutate: set(map[string]any{"childRetention": "Some"}, "spec", "rollout"),
			want:   []string{"spec.rollout.childRetention: Unsupported value"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			obj := tt.base(t)
			if tt.mutate != nil {
				tt.mutate(t, obj)
			}

			errs := v.validateCreate(obj)
			assertErrors(t, errs, tt.want)
			assertNoEvaluationErrors(t, errs)
		})
	}
}

func TestMachineOperationSelectorSpecImmutable(t *testing.T) {
	t.Parallel()

	selector := func(t *testing.T) map[string]any {
		return newSelectorOperationObject(t)
	}
	selectorWithRollout := func(t *testing.T) map[string]any {
		obj := newSelectorOperationObject(t)
		setField(t, obj, map[string]any{"maxConcurrent": int64(5)}, "spec", "rollout")

		return obj
	}
	machineRef := func(t *testing.T) map[string]any {
		return newHostOperationObject(t, OperationHostHardReboot)
	}
	set := func(value any, path ...string) func(*testing.T, map[string]any) {
		return func(t *testing.T, obj map[string]any) {
			setField(t, obj, value, path...)
		}
	}

	runOperationUpdateCases(t, []operationUpdateCase{
		{
			name:   "machineSelector matchLabels changed",
			stored: selector,
			mutate: set(map[string]any{"pool": "b"}, "spec", "machineSelector", "matchLabels"),
			want:   []string{"spec.machineSelector: Invalid value", msgMachineSelectorImmutable},
		},
		{
			name:   "machineSelector added",
			stored: machineRef,
			mutate: func(t *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "spec", "machineRef")
				setField(t, obj, map[string]any{"matchLabels": map[string]any{"pool": "a"}}, "spec", "machineSelector")
			},
			want: []string{"spec.machineSelector: Invalid value", msgMachineSelectorImmutable},
		},
		{
			name:   "machineSelector removed",
			stored: selector,
			mutate: func(_ *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "spec", "machineSelector")
			},
			want: []string{"spec.machineSelector: Invalid value", msgMachineSelectorImmutable, msgExactlyOneTarget},
		},
		{
			name:   "rollout maxConcurrent changed",
			stored: selectorWithRollout,
			mutate: set(map[string]any{"maxConcurrent": int64(6)}, "spec", "rollout"),
			want:   []string{"spec: Invalid value", msgSelectorSpecImmutable},
		},
		{
			name:   "rollout added",
			stored: selector,
			mutate: set(map[string]any{"maxConcurrent": int64(5)}, "spec", "rollout"),
			want:   []string{"spec: Invalid value", msgSelectorSpecImmutable},
		},
		{
			name:   "rollout removed",
			stored: selectorWithRollout,
			mutate: func(_ *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "spec", "rollout")
			},
			want: []string{"spec: Invalid value", msgSelectorSpecImmutable},
		},
		{
			// Every machine of a group must get the same operation, so the
			// kind and the parameters cannot change while children are
			// being created.
			name:   "selector operation kind changed",
			stored: selector,
			mutate: set(string(OperationAgentReset), "spec", "operationKind"),
			want:   []string{"spec: Invalid value", msgSelectorSpecImmutable},
		},
		{
			name:   "selector operation parameters changed",
			stored: selector,
			mutate: set(map[string]any{"reason": "b"}, "spec", "parameters"),
			want:   []string{"spec: Invalid value", msgSelectorSpecImmutable},
		},
		{
			name:   "selector operation ttlSecondsAfterFinished changed",
			stored: selector,
			mutate: set(int64(60), "spec", "ttlSecondsAfterFinished"),
			want:   []string{"spec: Invalid value", msgSelectorSpecImmutable},
		},
		{
			name:   "selector operation label and finalizer added",
			stored: selector,
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "x", "metadata", "labels", "team")
				setField(t, obj, []any{"example.com/hold"}, "metadata", "finalizers")
			},
		},
		{
			name:   "selector operation status updated",
			stored: selector,
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "InProgress", "status", "phase")
			},
		},
		{
			name:   "machineRef operation still updated",
			stored: machineRef,
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, map[string]any{"reason": "b"}, "spec", "parameters")
				setField(t, obj, int64(60), "spec", "ttlSecondsAfterFinished")
			},
		},
	})
}

// operationUpdateCase applies one update to a stored MachineOperation.
type operationUpdateCase struct {
	name   string
	stored func(*testing.T) map[string]any
	mutate func(*testing.T, map[string]any)
	want   []string
}

func runOperationUpdateCases(t *testing.T, tests []operationUpdateCase) {
	t.Helper()

	v := newCRDValidator(t, machineOperationCRD)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stored := tt.stored(t)
			if errs := v.validateCreate(stored); len(errs) > 0 {
				t.Fatalf("stored operation is invalid: %v", errs.ToAggregate())
			}

			updated := runtime.DeepCopyJSON(stored)
			tt.mutate(t, updated)

			errs := v.validateUpdate(updated, stored)
			assertErrors(t, errs, tt.want)
			assertNoEvaluationErrors(t, errs)
		})
	}
}

func TestMachineOperationAllocationSpecImmutable(t *testing.T) {
	t.Parallel()

	allocation := func(kind OperationKind) func(*testing.T) map[string]any {
		return func(t *testing.T) map[string]any {
			return newAllocationOperationObject(t, kind)
		}
	}
	hostHardReboot := func(t *testing.T) map[string]any {
		return newHostOperationObject(t, OperationHostHardReboot)
	}
	set := func(value any, path ...string) func(*testing.T, map[string]any) {
		return func(t *testing.T, obj map[string]any) {
			setField(t, obj, value, path...)
		}
	}
	allocationRef := map[string]any{"name": "alloc-1", "uid": "5d8e2f3a-1b4c-4d6e-8f7a-9b0c1d2e3f4a"}

	runOperationUpdateCases(t, []operationUpdateCase{
		{
			name:   "HostAllocate label and finalizer added",
			stored: allocation(OperationHostAllocate),
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "x", "metadata", "labels", "team")
				setField(t, obj, []any{"example.com/hold"}, "metadata", "finalizers")
			},
		},
		{
			name: "HostHardReboot parameters and ttlSecondsAfterFinished changed",
			stored: func(t *testing.T) map[string]any {
				obj := hostHardReboot(t)
				setField(t, obj, map[string]any{"reason": "a"}, "spec", "parameters")
				setField(t, obj, int64(30), "spec", "ttlSecondsAfterFinished")

				return obj
			},
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, map[string]any{"reason": "b"}, "spec", "parameters")
				setField(t, obj, int64(60), "spec", "ttlSecondsAfterFinished")
			},
		},
		{
			name:   "HostHardReboot kind changed to HostPowerOn",
			stored: hostHardReboot,
			mutate: set(string(OperationHostPowerOn), "spec", "operationKind"),
		},

		{
			name:   "HostAllocate changed to HostRelease",
			stored: allocation(OperationHostAllocate),
			mutate: set(string(OperationHostRelease), "spec", "operationKind"),
			want:   []string{"spec: Invalid value", msgAllocationSpecImmutable},
		},
		{
			name:   "allocationRef uid changed",
			stored: allocation(OperationHostAllocate),
			mutate: set("9c8b7a69-5847-4362-a1b0-c9d8e7f6a5b4", "spec", "allocationRef", "uid"),
			want:   []string{"spec: Invalid value", msgAllocationSpecImmutable},
		},
		{
			name:   "machineRef changed",
			stored: allocation(OperationHostRelease),
			mutate: set("worker-02", "spec", "machineRef"),
			want:   []string{"spec: Invalid value", msgAllocationSpecImmutable},
		},
		{
			name:   "parameters added",
			stored: allocation(OperationHostAllocate),
			mutate: set(map[string]any{"reason": "scale-up"}, "spec", "parameters"),
			want:   []string{"spec: Invalid value", msgAllocationSpecImmutable},
		},
		{
			name:   "allocationRef removed and kind changed to HostHardReboot",
			stored: allocation(OperationHostAllocate),
			mutate: func(t *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "spec", "allocationRef")
				setField(t, obj, string(OperationHostHardReboot), "spec", "operationKind")
			},
			want: []string{"spec: Invalid value", msgAllocationSpecImmutable},
		},
		{
			name:   "whole spec removed",
			stored: allocation(OperationHostAllocate),
			mutate: func(_ *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "spec")
			},
			want: []string{"spec: Invalid value", msgAllocationSpecImmutable},
		},

		{
			name:   "HostHardReboot changed to HostAllocate with allocationRef",
			stored: hostHardReboot,
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, string(OperationHostAllocate), "spec", "operationKind")
				setField(t, obj, allocationRef, "spec", "allocationRef")
			},
			want: []string{"spec.allocationRef: Invalid value", msgAllocationRefAdded},
		},
		{
			name:   "HostHardReboot with allocationRef added",
			stored: hostHardReboot,
			mutate: set(allocationRef, "spec", "allocationRef"),
			want:   []string{"spec.allocationRef: Invalid value", msgAllocationRefAdded, msgAllocationRefNotAllowed},
		},
	})
}

func TestMachineOperationAllocationStatus(t *testing.T) {
	t.Parallel()

	allocation := func(kind OperationKind) func(*testing.T) map[string]any {
		return func(t *testing.T) map[string]any {
			return newAllocationOperationObject(t, kind)
		}
	}
	storedWithResult := func(t *testing.T) map[string]any {
		obj := newAllocationOperationObject(t, OperationHostAllocate)
		setField(t, obj, "InProgress", "status", "phase")
		setField(t, obj, newAllocationResult(), "status", "allocation")

		return obj
	}
	setStatus := func(value any, path ...string) func(*testing.T, map[string]any) {
		return func(t *testing.T, obj map[string]any) {
			setField(t, obj, value, append([]string{"status"}, path...)...)
		}
	}
	ownedResources := func(entries ...map[string]any) []any {
		list := make([]any, 0, len(entries))
		for _, entry := range entries {
			list = append(list, entry)
		}

		return list
	}
	vm := map[string]any{"id": "vm", "nativeID": "vm-0001"}
	nic := map[string]any{"id": "nic", "nativeID": "nic-0001"}
	disk := map[string]any{"id": "disk", "nativeID": "disk-0001"}

	runOperationUpdateCases(t, []operationUpdateCase{
		{
			name:   "set full allocation",
			stored: allocation(OperationHostAllocate),
			mutate: setStatus(newAllocationResult(), "allocation"),
		},
		{
			name:   "set allocation with only ownedResources",
			stored: allocation(OperationHostAllocate),
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "Failed", "status", "phase")
				setField(t, obj, map[string]any{"ownedResources": ownedResources(vm)}, "status", "allocation")
			},
		},
		{
			name:   "rewrite identical allocation",
			stored: storedWithResult,
			mutate: setStatus(newAllocationResult(), "allocation"),
		},
		{
			name:   "rewrite ownedResources in another order",
			stored: storedWithResult,
			mutate: setStatus(ownedResources(nic, vm), "allocation", "ownedResources"),
		},
		{
			name:   "change phase, message and conditions",
			stored: storedWithResult,
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "Complete", "status", "phase")
				setField(t, obj, "allocated", "status", "message")
				setField(t, obj, []any{map[string]any{
					"type":               MachineOperationConditionCompleted,
					"status":             "True",
					"reason":             "Succeeded",
					"message":            "",
					"lastTransitionTime": "2026-01-01T00:00:00Z",
				}}, "status", "conditions")
			},
		},

		{
			name:   "change zone",
			stored: storedWithResult,
			mutate: setStatus("eastus-2", "allocation", "zone"),
			want:   []string{"status.allocation: Invalid value", msgAllocationResultImmutable},
		},
		{
			name:   "add ownedResources entry",
			stored: storedWithResult,
			mutate: setStatus(ownedResources(vm, nic, disk), "allocation", "ownedResources"),
			want:   []string{"status.allocation: Invalid value", msgAllocationResultImmutable},
		},
		{
			name:   "remove allocation",
			stored: storedWithResult,
			mutate: func(_ *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "status", "allocation")
			},
			want: []string{"status.allocation: Invalid value", msgAllocationResultImmutable},
		},
		{
			name:   "remove whole status",
			stored: storedWithResult,
			mutate: func(_ *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "status")
			},
			want: []string{"status.allocation: Invalid value", msgAllocationResultImmutable},
		},

		{
			name:   "allocation on HostRelease",
			stored: allocation(OperationHostRelease),
			mutate: setStatus(newAllocationResult(), "allocation"),
			want:   []string{"status.allocation: Invalid value", msgAllocationResultKind},
		},
		{
			name: "allocation on HostHardReboot",
			stored: func(t *testing.T) map[string]any {
				return newHostOperationObject(t, OperationHostHardReboot)
			},
			mutate: setStatus(newAllocationResult(), "allocation"),
			want:   []string{"status.allocation: Invalid value", msgAllocationResultKind},
		},

		{
			name:   "duplicate ownedResources id",
			stored: allocation(OperationHostAllocate),
			mutate: setStatus(map[string]any{
				"ownedResources": ownedResources(vm, map[string]any{"id": "vm", "nativeID": "vm-0002"}),
			}, "allocation"),
			want: []string{"status.allocation.ownedResources[1]: Duplicate value"},
		},
		{
			name:   "ownedResources entry with empty nativeID",
			stored: allocation(OperationHostAllocate),
			mutate: setStatus(map[string]any{
				"ownedResources": ownedResources(map[string]any{"id": "vm", "nativeID": ""}),
			}, "allocation"),
			want: []string{"status.allocation.ownedResources[0].nativeID: Invalid value", "at least 1 chars long"},
		},
	})
}

// suspend holds a machineRef operation. A selector operation is paced by its
// rollout, not suspended, and an allocation operation is never held.
func TestMachineOperationSuspend(t *testing.T) {
	t.Parallel()

	const msgSuspend = "suspend is allowed only on a machineRef operation that is not HostAllocate or HostRelease"

	v := newCRDValidator(t, machineOperationCRD)

	tests := []struct {
		name   string
		object func(*testing.T) map[string]any
		want   []string
	}{
		{
			name: "machineRef operation suspended",
			object: func(t *testing.T) map[string]any {
				obj := newHostOperationObject(t, OperationHostHardReboot)
				setField(t, obj, true, "spec", "suspend")

				return obj
			},
		},
		{
			name: "suspend false on a selector operation",
			object: func(t *testing.T) map[string]any {
				obj := newHostOperationObject(t, OperationHostHardReboot)
				unstructured.RemoveNestedField(obj, "spec", "machineRef")
				setField(t, obj, map[string]any{"matchLabels": map[string]any{"pool": "a"}}, "spec", "machineSelector")
				setField(t, obj, false, "spec", "suspend")

				return obj
			},
		},
		{
			name: "selector operation suspended",
			object: func(t *testing.T) map[string]any {
				obj := newHostOperationObject(t, OperationHostHardReboot)
				unstructured.RemoveNestedField(obj, "spec", "machineRef")
				setField(t, obj, map[string]any{"matchLabels": map[string]any{"pool": "a"}}, "spec", "machineSelector")
				setField(t, obj, true, "spec", "suspend")

				return obj
			},
			want: []string{msgSuspend},
		},
		{
			name: "HostAllocate suspended",
			object: func(t *testing.T) map[string]any {
				obj := newAllocationOperationObject(t, OperationHostAllocate)
				setField(t, obj, true, "spec", "suspend")

				return obj
			},
			want: []string{msgSuspend},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assertErrors(t, v.validateCreate(tt.object(t)), tt.want)
		})
	}

	suspended := func(t *testing.T) map[string]any {
		obj := newHostOperationObject(t, OperationHostHardReboot)
		setField(t, obj, true, "spec", "suspend")

		return obj
	}
	running := func(t *testing.T) map[string]any {
		return newHostOperationObject(t, OperationHostHardReboot)
	}

	// A held operation is released by clearing spec.suspend. It can never be
	// suspended again, and one created running can never be suspended, so a
	// held operation is one that has never started.
	runOperationUpdateCases(t, []operationUpdateCase{
		{
			name:   "released by clearing suspend",
			stored: suspended,
			mutate: func(t *testing.T, obj map[string]any) { setField(t, obj, false, "spec", "suspend") },
		},
		{
			name:   "released by removing suspend",
			stored: suspended,
			mutate: func(_ *testing.T, obj map[string]any) { unstructured.RemoveNestedField(obj, "spec", "suspend") },
		},
		{
			name:   "status of a held operation updated",
			stored: suspended,
			mutate: func(t *testing.T, obj map[string]any) { setField(t, obj, "Failed", "status", "phase") },
		},
		{
			name:   "suspended after creation",
			stored: running,
			mutate: func(t *testing.T, obj map[string]any) { setField(t, obj, true, "spec", "suspend") },
			want:   []string{"spec.suspend: Invalid value", "suspend can be set only when the operation is created"},
		},
		{
			name:   "machineRef changed",
			stored: running,
			mutate: func(t *testing.T, obj map[string]any) { setField(t, obj, "another-machine", "spec", "machineRef") },
			want:   []string{"spec.machineRef: Invalid value", "machineRef cannot be changed"},
		},
		{
			name:   "machineRef changed on a held operation",
			stored: suspended,
			mutate: func(t *testing.T, obj map[string]any) { setField(t, obj, "another-machine", "spec", "machineRef") },
			want:   []string{"spec.machineRef: Invalid value", "machineRef cannot be changed"},
		},
	})
}

func TestMachineOperationIsHeld(t *testing.T) {
	t.Parallel()

	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "a"}}
	now := metav1.Now()

	tests := []struct {
		name string
		op   MachineOperation
		want bool
	}{
		{name: "suspended", op: MachineOperation{Spec: MachineOperationSpec{MachineRef: "m", Suspend: true}}, want: true},
		{name: "not suspended", op: MachineOperation{Spec: MachineOperationSpec{MachineRef: "m"}}},
		{name: "selector operation", op: MachineOperation{Spec: MachineOperationSpec{MachineSelector: selector, Suspend: true}}},
		// Every agent may write the status of any operation. None of it
		// releases a held one.
		{name: "suspended with a forged start time", op: MachineOperation{Spec: MachineOperationSpec{MachineRef: "m", Suspend: true}, Status: MachineOperationStatus{StartedAt: &now}}, want: true},
		{name: "suspended with a forged phase", op: MachineOperation{Spec: MachineOperationSpec{MachineRef: "m", Suspend: true}, Status: MachineOperationStatus{Phase: OperationPhaseInProgress}}, want: true},
	}

	for _, tt := range tests {
		if got := tt.op.IsHeld(); got != tt.want {
			t.Errorf("%s: IsHeld() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// An operation that was held joins the queue of its machine when it is
// released, not when it was created.
func TestMachineOperationQueueTime(t *testing.T) {
	t.Parallel()

	created := metav1.NewTime(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC))
	released := created.Add(time.Hour)

	plain := MachineOperation{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: created}}
	if got := plain.QueueTime(); !got.Equal(&created) {
		t.Errorf("no annotation: QueueTime() = %v, want the creation time", got)
	}

	held := MachineOperation{ObjectMeta: metav1.ObjectMeta{
		CreationTimestamp: created,
		Annotations:       map[string]string{MachineOperationReleasedAtAnnotation: released.Format(time.RFC3339)},
	}}
	if got := held.QueueTime(); !got.Time.Equal(released) {
		t.Errorf("released later: QueueTime() = %v, want %v", got, released)
	}

	for name, value := range map[string]string{"not a time": "soon", "before creation": created.Add(-time.Hour).Format(time.RFC3339)} {
		op := MachineOperation{ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: created,
			Annotations:       map[string]string{MachineOperationReleasedAtAnnotation: value},
		}}
		if got := op.QueueTime(); !got.Equal(&created) {
			t.Errorf("%s: QueueTime() = %v, want the creation time", name, got)
		}
	}
}
