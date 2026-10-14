// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"os"
	"testing"

	"github.com/google/cel-go/cel"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestGeneratedSignalSchemaRulesCompile(t *testing.T) {
	t.Parallel()

	env, err := cel.NewEnv(cel.Variable("self", cel.DynType), cel.Variable("oldSelf", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}

	for _, resource := range []string{"certificationprofiles", "certificationruns"} {
		data, err := os.ReadFile("../../../deploy/signal/crd/signal.unbounded-cloud.io_" + resource + ".yaml")
		if err != nil {
			t.Fatal(err)
		}

		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(data, &crd); err != nil {
			t.Fatal(err)
		}

		checkSchemaRules(t, env, resource, crd.Spec.Versions[0].Schema.OpenAPIV3Schema)
	}
}

func checkSchemaRules(t *testing.T, env *cel.Env, path string, schema *apiextensionsv1.JSONSchemaProps) {
	t.Helper()

	for _, validation := range schema.XValidations {
		if _, issues := env.Compile(validation.Rule); issues.Err() != nil {
			t.Errorf("%s %q: %v", path, validation.Rule, issues.Err())
		}
	}

	for name, property := range schema.Properties {
		checkSchemaRules(t, env, path+"."+name, &property)
	}

	if schema.Items != nil && schema.Items.Schema != nil {
		checkSchemaRules(t, env, path+"[]", schema.Items.Schema)
	}
}
