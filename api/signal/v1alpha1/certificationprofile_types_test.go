// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"os"
	"regexp"
	"testing"

	"github.com/google/cel-go/cel"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestGeneratedParticipantMembershipValidation(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../../deploy/signal/crd/signal.unbounded-cloud.io_certificationprofiles.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}

	participants := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["participants"]
	count := participants.Properties["count"]

	names := participants.Properties["nodeNames"]
	if count.Minimum == nil || *count.Minimum != 1 || count.Maximum == nil || *count.Maximum != 128 ||
		names.XListType == nil || *names.XListType != "set" || names.MaxItems == nil || *names.MaxItems != 128 {
		t.Fatal("participant count or explicit unique membership bounds are missing")
	}

	env, err := cel.NewEnv(cel.Variable("self", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}

	programs := make([]cel.Program, 0, len(participants.XValidations))
	for _, validation := range participants.XValidations {
		ast, issues := env.Compile(validation.Rule)
		if issues.Err() != nil {
			t.Fatal(issues.Err())
		}

		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}

		programs = append(programs, program)
	}

	if len(programs) < 3 {
		t.Fatal("membership validation rules are missing")
	}

	for _, tc := range []struct {
		name  string
		value map[string]any
		valid bool
	}{
		{"legacy singleton", map[string]any{"count": int64(1)}, true},
		{"explicit singleton", map[string]any{"count": int64(1), "nodeNames": []string{"gpu-0"}}, true},
		{"rack pair", map[string]any{"count": int64(2), "nodeNames": []string{"gpu-0", "gpu-1"}, "topologyKey": "example.com/rack"}, true},
		{"no names", map[string]any{"count": int64(2), "topologyKey": "example.com/rack"}, false},
		{"no topology", map[string]any{"count": int64(2), "nodeNames": []string{"gpu-0", "gpu-1"}}, false},
		{"empty topology", map[string]any{"count": int64(2), "nodeNames": []string{"gpu-0", "gpu-1"}, "topologyKey": ""}, false},
		{"too few names", map[string]any{"count": int64(2), "nodeNames": []string{"gpu-0"}, "topologyKey": "rack"}, false},
		{"too many names", map[string]any{"count": int64(1), "nodeNames": []string{"gpu-0", "gpu-1"}}, false},
		{"duplicate names", map[string]any{"count": int64(2), "nodeNames": []string{"gpu-0", "gpu-0"}, "topologyKey": "rack"}, false},
		{"empty supplied set", map[string]any{"count": int64(1), "nodeNames": []string{}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			valid := true

			for _, program := range programs {
				result, _, err := program.Eval(map[string]any{"self": tc.value})
				if err != nil {
					t.Fatal(err)
				}

				valid = valid && result.Value() == true
			}

			if valid != tc.valid {
				t.Fatalf("membership validation = %t, want %t", valid, tc.valid)
			}
		})
	}

	for _, tc := range []struct {
		field string
		valid []string
		bad   []string
	}{
		{
			"topologyKey",
			[]string{"rack", "example.com/rack", "topology.example.com/Rack_1"},
			[]string{"", "/rack", "example.com/", "bad prefix/rack", "EXAMPLE.com/rack", "example..com/rack", "a/b/c"},
		},
		{"nodeNames", []string{"gpu-0", "gpu-0.example.com"}, []string{"", "GPU-0", "-gpu", "gpu_", "gpu..0"}},
	} {
		schema := participants.Properties[tc.field]
		if tc.field == "nodeNames" {
			schema = *schema.Items.Schema
		}

		pattern, err := regexp.Compile(schema.Pattern)
		if err != nil {
			t.Fatal(err)
		}

		for _, value := range tc.valid {
			if !pattern.MatchString(value) {
				t.Errorf("%s pattern rejects %q", tc.field, value)
			}
		}

		for _, value := range tc.bad {
			if pattern.MatchString(value) {
				t.Errorf("%s pattern accepts %q", tc.field, value)
			}
		}
	}
}
