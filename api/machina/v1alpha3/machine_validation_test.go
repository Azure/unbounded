// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestMachineProviderOwnershipSchema(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../../deploy/machina/crd/unbounded-cloud.io_machines.yaml")
	if err != nil {
		t.Fatalf("read Machine CRD: %v", err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("parse Machine CRD: %v", err)
	}

	specSchema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	want := map[string]string{
		"!has(oldSelf.providerRef) || (has(self.providerRef) && self.providerRef == oldSelf.providerRef)": "providerRef is immutable once set",
		"!has(self.providerRef) || has(self.provider)":                                                    "provider is required when providerRef is set",
		"!has(oldSelf.provider) || (has(self.provider) && self.provider == oldSelf.provider)":             "provider is immutable once set",
	}

	for _, validation := range specSchema.XValidations {
		message, ok := want[validation.Rule]
		if !ok {
			continue
		}

		if validation.Message != message {
			t.Fatalf("validation %q message = %q, want %q", validation.Rule, validation.Message, message)
		}

		delete(want, validation.Rule)
	}

	for rule := range want {
		t.Errorf("Machine spec schema is missing validation %q", rule)
	}
}
