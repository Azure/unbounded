// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
	assertSchemaValidations(t, specSchema, map[string]string{
		"!has(oldSelf.provider) || (has(self.provider) && self.provider == oldSelf.provider)":                                                                                   "provider is immutable once set",
		"!has(self.host) || (!has(self.host.netboot) && !has(self.host.azure) && !has(self.host.external)) || (!has(self.pxe) && !has(self.provider) && !has(self.providerID))": "host ownership cannot be combined with legacy pxe, provider, or providerID fields",
	})

	hostSchema := specSchema.Properties["host"]
	assertSchemaValidations(t, hostSchema, map[string]string{
		"(has(self.netboot) ? 1 : 0) + (has(self.azure) ? 1 : 0) + (has(self.external) ? 1 : 0) <= 1": "at most one of netboot, azure, or external may be set",
		"!has(oldSelf.netboot) || has(self.netboot)":                                                  "netboot host ownership is immutable once set",
		"!has(oldSelf.azure) || has(self.azure)":                                                      "azure host ownership is immutable once set",
		"!has(oldSelf.external) || has(self.external)":                                                "external host ownership is immutable once set",
	})

	externalSchema := hostSchema.Properties["external"]
	assertSchemaValidations(t, externalSchema, map[string]string{
		"has(self.providerID) || has(self.machineRef)": "providerID or machineRef is required",
		"has(self.machineRef) == has(oldSelf.machineRef) && (!has(self.machineRef) || self.machineRef == oldSelf.machineRef)": "machineRef is immutable",
		"self.provider == oldSelf.provider": "provider is immutable",
	})

	azureSchema := hostSchema.Properties["azure"]
	assertSchemaValidations(t, azureSchema, map[string]string{
		"self.resourceID == oldSelf.resourceID": "resourceID is immutable",
	})
}

func assertSchemaValidations(t *testing.T, schema apiextensionsv1.JSONSchemaProps, want map[string]string) {
	t.Helper()

	for _, validation := range schema.XValidations {
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
		t.Errorf("schema is missing validation %q", rule)
	}
}
