// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMachineCRDKubernetesSpecDoesNotRequireBootstrapTokenRef(t *testing.T) {
	data, err := os.ReadFile("../../../deploy/machina/crd/unbounded-cloud.io_machines.yaml")
	if err != nil {
		t.Fatalf("read Machine CRD: %v", err)
	}

	var crd map[string]any
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshal Machine CRD: %v", err)
	}

	kubernetesSchema, ok := lookupMap(crd,
		"spec", "versions", "0", "schema", "openAPIV3Schema", "properties", "spec", "properties", "kubernetes",
	)
	if !ok {
		t.Fatal("Machine CRD missing spec.kubernetes schema")
	}

	required, _ := kubernetesSchema["required"].([]any)
	for _, field := range required {
		if field == "bootstrapTokenRef" {
			t.Fatal("spec.kubernetes.bootstrapTokenRef must be optional")
		}
	}
}

func lookupMap(root map[string]any, path ...string) (map[string]any, bool) {
	var current any = root
	for _, part := range path {
		switch typed := current.(type) {
		case map[string]any:
			current = typed[part]
		case []any:
			if part != "0" || len(typed) == 0 {
				return nil, false
			}

			current = typed[0]
		default:
			return nil, false
		}
	}

	typed, ok := current.(map[string]any)
	return typed, ok
}
