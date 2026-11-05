// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestCertificationSuiteKindsRegistered(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add Signal types to scheme: %v", err)
	}

	for _, object := range []runtime.Object{&CertificationSuite{}, &CertificationSuiteList{}} {
		kinds, _, err := scheme.ObjectKinds(object)
		if err != nil {
			t.Fatalf("look up registered kind for %T: %v", object, err)
		}

		if len(kinds) != 1 || kinds[0].GroupVersion() != GroupVersion {
			t.Fatalf("registered kinds for %T = %v, want Signal v1alpha1", object, kinds)
		}
	}
}

func TestCertificationCheckListContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		file       string
		schemaPath []string
	}{
		{
			name:       "suite spec",
			file:       "signal.unbounded-cloud.io_certificationsuites.yaml",
			schemaPath: []string{"spec", "checks"},
		},
		{
			name:       "run spec",
			file:       "signal.unbounded-cloud.io_certificationruns.yaml",
			schemaPath: []string{"spec", "checks"},
		},
		{
			name:       "run status",
			file:       "signal.unbounded-cloud.io_certificationruns.yaml",
			schemaPath: []string{"status", "checks"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			crd := readCRD(t, tt.file)

			schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			for _, property := range tt.schemaPath {
				next, found := schema.Properties[property]
				if !found {
					t.Fatalf("generated CRD has no %q property", property)
				}

				schema = &next
			}

			if schema.MaxItems == nil || *schema.MaxItems != MaxCertificationChecks {
				t.Fatalf("maxItems = %v, want %d", schema.MaxItems, MaxCertificationChecks)
			}

			if tt.schemaPath[0] == "spec" {
				if schema.MinItems == nil || *schema.MinItems != MinCertificationChecks {
					t.Fatalf("minItems = %v, want %d", schema.MinItems, MinCertificationChecks)
				}
			}

			if schema.XListType == nil || *schema.XListType != "atomic" {
				t.Fatalf("x-kubernetes-list-type = %v, want atomic", schema.XListType)
			}
		})
	}
}

func TestCertificationSuiteWorkloadPathBounds(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationsuites.yaml")
	hostPath := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.
		Properties["spec"].
		Properties["checks"].
		Items.Schema.
		Properties["workloadPolicy"].
		Properties["hostPaths"].
		Items.Schema

	for _, property := range []string{"mountPath", "path"} {
		maxLength := hostPath.Properties[property].MaxLength
		if maxLength == nil || *maxLength != MaxWorkloadPathLength {
			t.Fatalf("%s maxLength = %v, want %d", property, maxLength, MaxWorkloadPathLength)
		}
	}
}

func TestCertificationProfileReferencesSuite(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationprofiles.yaml")

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]

	suite, found := spec.Properties["suite"]
	if !found {
		t.Fatal("generated Profile CRD has no suite reference")
	}

	if _, found := suite.Properties["name"]; !found {
		t.Fatal("generated Profile suite reference has no name")
	}

	if !contains(spec.Required, "suite") {
		t.Fatal("generated Profile CRD does not require suite")
	}

	for _, removed := range []string{"checks", "scriptBundle"} {
		if _, found := spec.Properties[removed]; found {
			t.Fatalf("generated Profile CRD still owns %s", removed)
		}
	}
}

func TestCertificationRunPinsSuiteAndRemainsImmutable(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationruns.yaml")

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]

	suite, found := spec.Properties["suite"]
	if !found {
		t.Fatal("generated Run CRD has no pinned suite")
	}

	for _, property := range []string{"name", "uid", "generation"} {
		if _, found := suite.Properties[property]; !found {
			t.Fatalf("generated Run pinned suite has no %s", property)
		}

		if !contains(suite.Required, property) {
			t.Fatalf("generated Run pinned suite does not require %s", property)
		}
	}

	if !contains(spec.Required, "suite") {
		t.Fatal("generated Run CRD does not require suite")
	}

	requiredRules := map[string]bool{
		"self.profile == oldSelf.profile":           false,
		"self.suite == oldSelf.suite":               false,
		"self.scriptBundle == oldSelf.scriptBundle": false,
		"self.checks == oldSelf.checks":             false,
	}

	for _, validation := range spec.XValidations {
		if _, found := requiredRules[validation.Rule]; found {
			requiredRules[validation.Rule] = true
		}
	}

	for rule, found := range requiredRules {
		if !found {
			t.Fatalf("generated Run CRD has no %q immutability rule", rule)
		}
	}
}

func readCRD(t *testing.T, name string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()

	content, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "signal", "crd", name))
	if err != nil {
		t.Fatalf("read generated CRD: %v", err)
	}

	jsonContent, err := yaml.ToJSON(content)
	if err != nil {
		t.Fatalf("convert generated CRD to JSON: %v", err)
	}

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := json.Unmarshal(jsonContent, crd); err != nil {
		t.Fatalf("decode generated CRD: %v", err)
	}

	return crd
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}
