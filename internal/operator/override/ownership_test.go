// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Azure/unbounded/internal/operator/component"
)

func TestValidateReservesRacerOwnership(t *testing.T) {
	for _, key := range []string{"unbounded-cloud.io/racer-manager", "unbounded-cloud.io/racer-installation-uid", "unbounded-cloud.io/racer-operator-claim-uid", "unbounded-cloud.io/another-component"} {
		for _, path := range [][]string{
			{"metadata", "annotations"},
			{"metadata", "labels"},
			{"spec", "template", "metadata", "annotations"},
			{"spec", "template", "metadata", "labels"},
		} {
			for _, value := range []any{"custom", "", nil} {
				t.Run(strings.Join(path, ".")+"/"+key+"/"+describeValueShape(value), func(t *testing.T) {
					patch := map[string]any{}
					if err := setNestedMap(patch, map[string]any{key: value}, path...); err != nil {
						t.Fatal(err)
					}

					err := ValidateErr([]SourcedEntry{{Entry: Entry{Component: "racer", Kind: "Deployment", Patch: patch}}})
					if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "is reserved") {
						t.Fatalf("error = %v, want reserved ownership key %s", err, key)
					}
				})
			}
		}
	}

	for _, key := range []string{"team", "racer.unbounded-cloud.io/custom", "racer.unbounded-cloud.io/manager-note", "example.com/installation-uid"} {
		if err := validateFragment(t, "component: racer\nkind: Deployment\npatch:\n  metadata:\n    annotations:\n      "+key+": custom\n"); err != nil {
			t.Fatalf("ordinary annotation %s rejected: %v", key, err)
		}
	}
}

func TestApplyRestampsReservedMetadata(t *testing.T) {
	const (
		owned  = ReservedPrefix + "other-component-identity"
		forged = ReservedPrefix + "not-operator-declared"
	)

	for _, path := range reservedMetadataPaths {
		for _, tc := range []struct {
			name  string
			value any
		}{
			{name: "overwrite and forge", value: map[string]any{owned: "changed", forged: "forged", "team": "platform"}},
			{name: "delete key", value: map[string]any{owned: nil, "team": "platform"}},
			{name: "delete map", value: nil},
			{name: "replace map", value: map[string]any{"$patch": "replace", forged: "forged", "team": "platform"}},
		} {
			t.Run(strings.Join(path, ".")+"/"+tc.name, func(t *testing.T) {
				workload := testWorkload("test")
				if err := setNestedMap(workload.Object, map[string]any{owned: "canonical"}, path...); err != nil {
					t.Fatal(err)
				}

				original := workload.DeepCopy()

				patch := map[string]any{}
				if err := unstructured.SetNestedField(patch, tc.value, path...); err != nil {
					t.Fatal(err)
				}

				plan := planWith(workload, "machina", "")
				// Deliberately bypass validation, including null and merge directives.
				report := Apply(plan, []SourcedEntry{{Entry: Entry{Component: "machina", Kind: "Deployment", Patch: patch}}}, nil)
				if report.Failed() || plan.Len() != 1 {
					t.Fatalf("Apply: %+v", report)
				}

				values := nestedMap(plan.Operations[0].Object.Object, path...)
				if values[owned] != "canonical" {
					t.Fatalf("reserved metadata lost: %v", values)
				}

				if _, present := values[forged]; present {
					t.Fatalf("reserved metadata forged: %v", values)
				}

				if tc.value != nil && values["team"] != "platform" {
					t.Fatalf("ordinary metadata lost: %v", values)
				}

				if !reflect.DeepEqual(workload, original) {
					t.Fatal("original mutated")
				}
			})
		}
	}
}

func TestApplyRestampsRacerOwnership(t *testing.T) {
	const (
		manager      = "unbounded-cloud.io/racer-manager"
		installation = "unbounded-cloud.io/racer-installation-uid"
	)

	for _, tc := range []struct {
		name     string
		metadata map[string]any
		unowned  bool
	}{
		{name: "ordinary annotation", metadata: map[string]any{"annotations": map[string]any{"team": "platform"}}},
		{name: "overwrite", metadata: map[string]any{"annotations": map[string]any{manager: "custom", installation: "other", "team": "platform"}}},
		{name: "delete keys", metadata: map[string]any{"annotations": map[string]any{manager: nil, installation: nil, "team": "platform"}}},
		{name: "delete annotations", metadata: map[string]any{"annotations": nil}},
		{name: "replace annotations", metadata: map[string]any{"annotations": map[string]any{"$patch": "replace", "team": "platform"}}},
		{name: "replace metadata", metadata: map[string]any{"$patch": "replace", "annotations": map[string]any{"team": "platform"}}},
		{name: "cannot forge ownership", unowned: true, metadata: map[string]any{"annotations": map[string]any{manager: "custom", installation: "other", "team": "platform"}}},
		{name: "absent stays absent", unowned: true, metadata: map[string]any{"annotations": map[string]any{"team": "platform"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workload := testWorkload("racer")
			workload.SetName("racer-controller")

			if err := setNestedSlice(workload.Object, []any{map[string]any{"name": "controller", "image": "racer:test"}}, "spec", "template", "spec", "containers"); err != nil {
				t.Fatal(err)
			}

			if !tc.unowned {
				workload.SetAnnotations(map[string]string{manager: component.FieldOwner, installation: "installation-uid"})
			}

			original := workload.DeepCopy()
			plan := planWith(workload, "racer", "")
			entries := []SourcedEntry{{Source: Source{Key: "ownership.yaml"}, Entry: Entry{
				Component: "racer", Kind: "Deployment", Patch: map[string]any{"metadata": tc.metadata},
			}}}

			// Bypass validation to test the merge's independent identity protection.
			report := Apply(plan, entries, nil)
			if report.Failed() || len(plan.Operations) != 1 {
				t.Fatalf("Apply: %+v", report)
			}

			annotations := plan.Operations[0].Object.GetAnnotations()
			for _, key := range []string{manager, installation} {
				got, present := annotations[key]

				want, wasPresent := original.GetAnnotations()[key]
				if got != want || present != wasPresent {
					t.Fatalf("annotation %s = %q (present %v), want %q (present %v)", key, got, present, want, wasPresent)
				}
			}

			if tc.name != "delete annotations" && annotations["team"] != "platform" {
				t.Fatalf("ordinary annotation lost: %v", annotations)
			}

			if annotations[HashAnnotation] == "" || annotations[SourceAnnotation] == "" {
				t.Fatalf("override bookkeeping lost: %v", annotations)
			}

			if !reflect.DeepEqual(workload.Object, original.Object) {
				t.Fatal("override mutated the original workload")
			}
		})
	}
}
