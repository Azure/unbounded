// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestCertificationCheckListContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		file       string
		schemaPath []string
	}{
		{
			name:       "profile spec",
			file:       "signal.unbounded-cloud.io_certificationprofiles.yaml",
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

func TestCertificationRunChecksRemainImmutable(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "signal.unbounded-cloud.io_certificationruns.yaml")

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	for _, validation := range spec.XValidations {
		if validation.Rule == "self.checks == oldSelf.checks" {
			return
		}
	}

	t.Fatal("generated Run CRD does not preserve checks immutability")
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
