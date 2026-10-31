// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestHostOSRebootSchema(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../../deploy/machina/crd/unbounded-cloud.io_machineoperations.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}

	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec := schema.Properties["spec"]
	assertSchemaValidations(t, spec, map[string]string{
		"self.operationKind != 'HostOSReboot' || (has(self.machineRef) && size(self.machineRef) > 0)": "HostOSReboot requires machineRef",
	})

	if got := schema.Properties["status"].Properties["hostBootID"].Type; got != "string" {
		t.Errorf("hostBootID type = %q, want string", got)
	}

	for _, kind := range spec.Properties["operationKind"].Enum {
		if string(kind.Raw) == `"HostOSReboot"` {
			return
		}
	}

	t.Error("operationKind enum is missing HostOSReboot")
}
