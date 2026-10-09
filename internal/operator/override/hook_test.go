// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestApplyOperationValidationHook(t *testing.T) {
	for _, tc := range []struct {
		name      string
		component string
		hook      bool
		reject    bool
	}{
		{name: "nil hook even for racer", component: "racer"},
		{name: "accept other component", component: "machina", hook: true},
		{name: "reject other component", component: "machina", hook: true, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workload := testWorkload("test")
			original := workload.DeepCopy()
			plan := planWith(workload, tc.component, "")
			called := false

			if tc.hook {
				plan.Operations[0].ValidateOverride = func(before, after *unstructured.Unstructured) error {
					called = true

					if before != workload || after == before || after.GetAnnotations()["team"] != "platform" {
						t.Fatal("hook did not receive original and merged copy")
					}

					if tc.reject {
						return errors.New("component invariant")
					}

					return nil
				}
			}

			entries := []SourcedEntry{{Source: Source{Key: "hook.yaml"}, Entry: Entry{Component: tc.component, Kind: "Deployment", Patch: map[string]any{"metadata": map[string]any{"annotations": map[string]any{"team": "platform"}}}}}}

			report := Apply(plan, entries, nil)
			if called != tc.hook || report.Failed() != tc.reject {
				t.Fatalf("called = %v, report = %+v", called, report)
			}

			if tc.reject {
				if plan.Len() != 0 || len(report.Withheld) != 1 || report.Withheld[0].Component != tc.component || !strings.Contains(report.Err().Error(), "hook.yaml") || !strings.Contains(report.Err().Error(), "component invariant") {
					t.Fatalf("rejected hook was not attributed and withheld: %+v", report)
				}
			} else if plan.Len() != 1 || plan.Operations[0].Object.GetAnnotations()["team"] != "platform" {
				t.Fatal("accepted override not assigned")
			}

			if !reflect.DeepEqual(workload, original) {
				t.Fatal("original mutated")
			}
		})
	}
}
