// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

const machineCRD = "unbounded-cloud.io_machines.yaml"

func newPlainMachineObject(t *testing.T) map[string]any {
	t.Helper()

	return parseObject(t, `
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: host-1
spec:
  kubernetes:
    version: v1.34.1
`)
}

func newAllocationMachineObject(t *testing.T) map[string]any {
	t.Helper()

	return parseObject(t, `
apiVersion: unbounded-cloud.io/v1alpha3
kind: Machine
metadata:
  name: host-1
spec:
  kubernetes:
    version: v1.34.1
  allocationRef:
    name: alloc-1
    uid: 6d3f0d6e-8d1a-4c1e-9f61-0d7b8f0b2a10
`)
}

func newFencedMachineObject(t *testing.T) map[string]any {
	t.Helper()

	obj := newAllocationMachineObject(t)
	setField(t, obj, true, "spec", "fenced")

	return obj
}

// machineCreateCase creates one Machine.
type machineCreateCase struct {
	name   string
	object func(*testing.T) map[string]any
	mutate func(*testing.T, map[string]any)
	want   []string
}

func runMachineCreateCases(t *testing.T, tests []machineCreateCase) {
	t.Helper()

	v := newCRDValidator(t, machineCRD)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			obj := tt.object(t)
			if tt.mutate != nil {
				tt.mutate(t, obj)
			}

			assertErrors(t, v.validateCreate(obj), tt.want)
		})
	}
}

// machineUpdateCase applies one update to a stored Machine.
type machineUpdateCase struct {
	name   string
	stored func(*testing.T) map[string]any
	mutate func(*testing.T, map[string]any)
	want   []string
}

func runMachineUpdateCases(t *testing.T, tests []machineUpdateCase) {
	t.Helper()

	v := newCRDValidator(t, machineCRD)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stored := tt.stored(t)
			if errs := v.validateCreate(stored); len(errs) > 0 {
				t.Fatalf("stored machine is invalid: %v", errs.ToAggregate())
			}

			updated := runtime.DeepCopyJSON(stored)
			tt.mutate(t, updated)
			assertErrors(t, v.validateUpdate(updated, stored), tt.want)
		})
	}
}

func setMachineField(value any, path ...string) func(*testing.T, map[string]any) {
	return func(t *testing.T, obj map[string]any) {
		setField(t, obj, value, path...)
	}
}

func removeMachineField(path ...string) func(*testing.T, map[string]any) {
	return func(_ *testing.T, obj map[string]any) {
		unstructured.RemoveNestedField(obj, path...)
	}
}

func TestMachineAllocationRefCreate(t *testing.T) {
	t.Parallel()

	runMachineCreateCases(t, []machineCreateCase{
		{name: "with allocationRef", object: newAllocationMachineObject},
		{name: "without allocationRef", object: newPlainMachineObject},
		{
			name:   "allocationRef without uid",
			object: newAllocationMachineObject,
			mutate: removeMachineField("spec", "allocationRef", "uid"),
			want:   []string{"spec.allocationRef.uid"},
		},
		{
			name:   "allocationRef without name",
			object: newAllocationMachineObject,
			mutate: removeMachineField("spec", "allocationRef", "name"),
			want:   []string{"spec.allocationRef.name"},
		},
		{
			name:   "allocationRef with empty uid",
			object: newAllocationMachineObject,
			mutate: setMachineField("", "spec", "allocationRef", "uid"),
			want:   []string{"spec.allocationRef.uid"},
		},
	})
}

func TestMachineAllocationRefImmutable(t *testing.T) {
	t.Parallel()

	const msg = "allocationRef is immutable"

	runMachineUpdateCases(t, []machineUpdateCase{
		{
			name:   "added after create",
			stored: newPlainMachineObject,
			mutate: setMachineField(map[string]any{"name": "alloc-1", "uid": "6d3f0d6e-8d1a-4c1e-9f61-0d7b8f0b2a10"}, "spec", "allocationRef"),
			want:   []string{msg, "spec.allocationRef"},
		},
		{
			name:   "removed",
			stored: newAllocationMachineObject,
			mutate: removeMachineField("spec", "allocationRef"),
			want:   []string{msg},
		},
		{
			name:   "uid changed",
			stored: newAllocationMachineObject,
			mutate: setMachineField("9c8b7a69-5847-4362-a1b0-c9d8e7f6a5b4", "spec", "allocationRef", "uid"),
			want:   []string{msg},
		},
		{
			name:   "name changed",
			stored: newAllocationMachineObject,
			mutate: setMachineField("alloc-2", "spec", "allocationRef", "name"),
			want:   []string{msg},
		},
		{
			name:   "whole spec dropped",
			stored: newAllocationMachineObject,
			mutate: removeMachineField("spec"),
			want:   []string{msg},
		},
		{
			name:   "label added",
			stored: newAllocationMachineObject,
			mutate: setMachineField("x", "metadata", "labels", "team"),
		},
		{
			name:   "kubernetes version changed",
			stored: newAllocationMachineObject,
			mutate: setMachineField("v1.34.2", "spec", "kubernetes", "version"),
		},
		{
			name:   "neither field",
			stored: newPlainMachineObject,
			mutate: setMachineField("v1.34.2", "spec", "kubernetes", "version"),
		},
		{
			name:   "neither field, spec dropped",
			stored: newPlainMachineObject,
			mutate: removeMachineField("spec"),
		},
	})
}

func TestMachineFencedCreate(t *testing.T) {
	t.Parallel()

	runMachineCreateCases(t, []machineCreateCase{
		{name: "fenced with allocationRef", object: newFencedMachineObject},
		{
			name:   "fenced without allocationRef",
			object: newPlainMachineObject,
			mutate: setMachineField(true, "spec", "fenced"),
			want:   []string{"fenced requires allocationRef"},
		},
		{
			name:   "unfenced without allocationRef",
			object: newPlainMachineObject,
			mutate: setMachineField(false, "spec", "fenced"),
		},
	})
}

func TestMachineFencedTransitions(t *testing.T) {
	t.Parallel()

	const msg = "fenced cannot be unset"

	runMachineUpdateCases(t, []machineUpdateCase{
		{
			name:   "absent to true",
			stored: newAllocationMachineObject,
			mutate: setMachineField(true, "spec", "fenced"),
		},
		{
			name: "false to true",
			stored: func(t *testing.T) map[string]any {
				obj := newAllocationMachineObject(t)
				setField(t, obj, false, "spec", "fenced")

				return obj
			},
			mutate: setMachineField(true, "spec", "fenced"),
		},
		{
			name:   "true stays true with a label added",
			stored: newFencedMachineObject,
			mutate: setMachineField("x", "metadata", "labels", "team"),
		},
		{
			name:   "true to false",
			stored: newFencedMachineObject,
			mutate: setMachineField(false, "spec", "fenced"),
			want:   []string{msg, "spec.fenced"},
		},
		{
			name:   "removed",
			stored: newFencedMachineObject,
			mutate: removeMachineField("spec", "fenced"),
			want:   []string{msg},
		},
		{
			name:   "whole spec dropped",
			stored: newFencedMachineObject,
			mutate: removeMachineField("spec"),
			want:   []string{msg},
		},
		{
			name:   "set on a Machine without allocationRef",
			stored: newPlainMachineObject,
			mutate: setMachineField(true, "spec", "fenced"),
			want:   []string{"fenced requires allocationRef"},
		},
	})
}
