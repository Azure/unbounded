// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
)

func TestRacerOperatorOverrideIsExplicit(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate operator override example")
	}

	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../../../deploy/gantry/examples/racer-operator-overrides.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var cm corev1.ConfigMap
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatal(err)
	}

	entries, problems, err := override.Parse(cm.Data)
	if err != nil || len(problems) != 0 || len(entries) != 1 {
		t.Fatalf("parse Racer override: entries=%v problems=%v err=%v", entries, problems, err)
	}

	if err := override.ValidateErr(entries); err != nil {
		t.Fatalf("validate Racer override: %v", err)
	}

	for _, enabled := range []bool{false, true} {
		env := testEnv(t)
		env.Config.ImageRegistry = "docker.io/library"
		env.Config.ImageTag = "e2e"

		plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
		if err != nil {
			t.Fatal(err)
		}

		if enabled {
			report := override.Apply(plan, entries, []string{"edge"})
			if report.Failed() || len(report.Workloads) != 1 || len(report.InertEntries) != 0 {
				t.Fatalf("apply Racer override: %+v", report)
			}
		}

		found := false

		for _, op := range plan.Operations {
			if op.Object.GetKind() != "DaemonSet" || op.Object.GetName() != daemonSetName || op.Kind != component.OpApply {
				continue
			}

			found = true

			pod, _, err := unstructured.NestedMap(op.Object.Object, "spec", "template", "spec")
			if err != nil {
				t.Fatal(err)
			}

			containers, _, _ := unstructured.NestedSlice(pod, "containers")
			agent := containers[0].(map[string]any)
			uid, _, _ := unstructured.NestedInt64(agent, "securityContext", "runAsUser")

			wantUID := int64(65532)
			if enabled {
				wantUID = 0
			}

			if uid != wantUID || agent["image"] != "docker.io/library/gantry:e2e" {
				t.Fatalf("racer=%t: incorrect identity or operator image: %+v", enabled, agent)
			}

			variables, _, _ := unstructured.NestedSlice(agent, "env")
			active := false

			for _, variable := range variables {
				v := variable.(map[string]any)
				if v["name"] == "GANTRY_RACER_ENABLED" && v["value"] == "true" {
					active = true
				}
			}

			if active != enabled {
				t.Fatalf("racer=%t: active=%t", enabled, active)
			}
		}

		if !found {
			t.Fatal("Gantry DaemonSet missing from plan")
		}
	}
}
