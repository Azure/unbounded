// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"os"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestGeneratedPlanImmutabilityRulesAllowLegacyStatusUpdatesAndRejectPlanChanges(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../../deploy/signal/crd/signal.unbounded-cloud.io_certificationruns.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}

	env, err := cel.NewEnv(cel.Variable("self", cel.DynType), cel.Variable("oldSelf", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]

	for _, field := range []string{"participantPlan", "testPlan"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()

			rule := ""

			for _, validation := range spec.XValidations {
				if validation.Message == field+" is immutable" {
					rule = validation.Rule
				}
			}

			if rule == "" {
				t.Fatal("immutable field rule missing from generated schema")
			}

			ast, issues := env.Compile(rule)
			if issues.Err() != nil {
				t.Fatal(issues.Err())
			}

			program, err := env.Program(ast)
			if err != nil {
				t.Fatal(err)
			}

			oldPlan := []any{map[string]any{"nodeName": "gpu-0", "nodeUID": "gpu-0-uid", "parameters": map[string]any{"mode": "baseline"}}}

			newPlan := []any{map[string]any{"nodeName": "gpu-0", "nodeUID": "replacement-uid", "parameters": map[string]any{"mode": "changed"}}}
			for _, tc := range []struct {
				name string
				old  map[string]any
				new  map[string]any
				want bool
			}{
				{"legacy absent plans", map[string]any{}, map[string]any{"cancel": true}, true},
				{"unchanged plan", map[string]any{field: oldPlan}, map[string]any{field: oldPlan, "cancel": true}, true},
				{"added plan", map[string]any{}, map[string]any{field: oldPlan}, false},
				{"removed plan", map[string]any{field: oldPlan}, map[string]any{}, false},
				{"mutated plan", map[string]any{field: oldPlan}, map[string]any{field: newPlan}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					value, _, err := program.Eval(map[string]any{"self": tc.new, "oldSelf": tc.old})
					if err != nil {
						t.Fatal(err)
					}

					got, ok := value.Value().(bool)
					if !ok || got != tc.want {
						t.Fatalf("rule %q = %v, want %t", rule, value, tc.want)
					}
				})
			}

			if !strings.Contains(rule, "has(self."+field+")") {
				t.Fatal("optional plan rule does not guard field absence")
			}
		})
	}
}
