// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"os"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

func TestGeneratedRunTransitionRulesAllowAbsentOptionalFields(t *testing.T) {
	t.Parallel()

	schema := generatedRunSchema(t)

	for _, absent := range [][]string{
		{"parameters"},
		{"tolerations"},
		{"parameters", "tolerations", "purpose", "stagePlan"},
	} {
		t.Run(strings.Join(absent, "+"), func(t *testing.T) {
			t.Parallel()

			for _, update := range []string{"cancel", "status", "finalizer"} {
				t.Run(update, func(t *testing.T) {
					t.Parallel()

					old := schemaRun()
					for _, name := range absent {
						delete(old["spec"].(map[string]any), name)
					}

					next := runtime.DeepCopyJSON(old)

					switch update {
					case "cancel":
						next["spec"].(map[string]any)["cancel"] = true
					case "status":
						next["status"] = map[string]any{"phase": "Running"}
					case "finalizer":
						delete(next["metadata"].(map[string]any), "finalizers")
					}

					errs := runTransitionErrors(t, schema, next, old)
					if len(errs) != 0 {
						t.Fatalf("unchanged absent fields blocked %s update: %v", update, errs)
					}
				})
			}
		})
	}
}

func TestGeneratedRunOptionalFrozenFieldsRejectPresenceAndValueChanges(t *testing.T) {
	t.Parallel()

	schema := generatedRunSchema(t)

	for _, tc := range []struct {
		field   string
		changed any
		message string
	}{
		{"parameters", map[string]any{"mode": "changed"}, "parameters are immutable"},
		{"tolerations", []any{map[string]any{"key": "changed", "operator": "Exists"}}, "tolerations are immutable"},
		{"purpose", "FullCertification", "purpose is immutable"},
		{"stagePlan", []any{map[string]any{"id": "changed", "displayName": "Changed", "category": "compute", "required": true}}, "stagePlan is immutable"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()

			for _, change := range []string{"unchanged", "added", "removed", "mutated"} {
				t.Run(change, func(t *testing.T) {
					t.Parallel()

					old := schemaRun()
					next := runtime.DeepCopyJSON(old)
					nextSpec := next["spec"].(map[string]any)
					nextSpec["cancel"] = true

					switch change {
					case "added":
						delete(old["spec"].(map[string]any), tc.field)
					case "removed":
						delete(nextSpec, tc.field)
					case "mutated":
						nextSpec[tc.field] = tc.changed
					}

					errs := runTransitionErrors(t, schema, next, old)
					if change == "unchanged" {
						if len(errs) != 0 {
							t.Fatalf("unchanged %s blocked cancellation: %v", tc.field, errs)
						}
					} else if len(errs) == 0 || !strings.Contains(strings.Join(errs, "; "), tc.message) {
						t.Fatalf("%s %s validation = %v, want %q", change, tc.field, errs, tc.message)
					}
				})
			}
		})
	}
}

func generatedRunSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	data, err := os.ReadFile("../../../deploy/signal/crd/signal.unbounded-cloud.io_certificationruns.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}

	return crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
}

func runTransitionErrors(t *testing.T, schema apiextensionsv1.JSONSchemaProps, next, old map[string]any) []string {
	t.Helper()

	env, err := cel.NewEnv(cel.Variable("self", cel.DynType), cel.Variable("oldSelf", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}

	var errs []string

	evaluate := func(rules apiextensionsv1.ValidationRules, next, old any) {
		for _, validation := range rules {
			ast, issues := env.Compile(validation.Rule)
			if issues.Err() != nil {
				t.Fatal(issues.Err())
			}

			program, err := env.Program(ast)
			if err != nil {
				t.Fatal(err)
			}

			value, _, err := program.Eval(map[string]any{"self": next, "oldSelf": old})
			if err != nil {
				errs = append(errs, validation.Message+": "+err.Error())
			} else if value.Value() != true {
				errs = append(errs, validation.Message)
			}
		}
	}

	nextSpec := next["spec"].(map[string]any)
	oldSpec := old["spec"].(map[string]any)
	evaluate(schema.XValidations, nextSpec, oldSpec)

	for name, property := range schema.Properties {
		nextValue, nextPresent := nextSpec[name]
		oldValue, oldPresent := oldSpec[name]
		// Field-local transition rules run only when the field exists in both objects.
		if nextPresent && oldPresent {
			evaluate(property.XValidations, nextValue, oldValue)
		}
	}

	return errs
}

func schemaRun() map[string]any {
	return map[string]any{
		"apiVersion": GroupVersion.String(),
		"kind":       "CertificationRun",
		"metadata": map[string]any{
			"name": "schema-run", "namespace": "signal", "uid": "run-uid",
			"finalizers": []any{"signal.unbounded-cloud.io/cleanup"},
		},
		"spec": map[string]any{
			"profile":         map[string]any{"name": "profile", "uid": "profile-uid", "generation": int64(1)},
			"participants":    []any{map[string]any{"nodeName": "gpu-0", "nodeUID": "node-uid"}},
			"script":          map[string]any{"configMapName": "run-scripts", "key": "payload.txt", "digest": "sha256:abc"},
			"workflow":        map[string]any{"name": "gpu-cert"},
			"backend":         map[string]any{"apiVersion": "kubeflow.org/v2beta1", "kind": "MPIJob"},
			"runnerImage":     "runner@sha256:abc",
			"purpose":         "Diagnostic",
			"stagePlan":       []any{map[string]any{"id": "workload", "displayName": "Workload", "category": "compute", "required": true}},
			"parameters":      map[string]any{"mode": "baseline"},
			"evidencePath":    "signal/schema-run.json",
			"deadline":        "10m0s",
			"maxAttempts":     int64(2),
			"tolerations":     []any{map[string]any{"key": "dedicated", "operator": "Exists"}},
			"requestNodeName": "gpu-0",
		},
	}
}
