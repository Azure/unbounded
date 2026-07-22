// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestNetbootEndpointSchema(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "../../../deploy/machina/crd/unbounded-cloud.io_netbootendpoints.yaml")
	if crd.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Errorf("scope = %q, want %q", crd.Spec.Scope, apiextensionsv1.ClusterScoped)
	}

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	for _, field := range []string{"siteRef", "type", "externalURL", "tls", "managedL2", "http"} {
		if _, ok := spec.Properties[field]; !ok {
			t.Errorf("endpoint spec is missing %q", field)
		}
	}

	assertSchemaValidations(t, spec, map[string]string{
		"self.tls.trust != 'Public' || (self.externalURL.startsWith('https://') && self.tls.mode != 'Disabled')": "public endpoints require HTTPS",
		"self.type == 'ManagedL2' ? has(self.managedL2) : !has(self.managedL2)":                                  "managedL2 configuration must be set only for ManagedL2 endpoints",
		"self.type == 'HTTP' ? has(self.http) : !has(self.http)":                                                 "http configuration must be set only for HTTP endpoints",
	})
	assertSchemaValidations(t, spec.Properties["tls"], map[string]string{
		"self.mode == 'Secret' ? has(self.secretRef) : !has(self.secretRef)": "secretRef must be set only when TLS mode is Secret",
	})

	status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	for _, field := range []string{"observedGeneration", "claim", "conditions"} {
		if _, ok := status.Properties[field]; !ok {
			t.Errorf("endpoint status is missing %q", field)
		}
	}
}

func TestNetbootSessionSchema(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "../../../deploy/machina/crd/unbounded-cloud.io_netbootsessions.yaml")
	if crd.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Errorf("scope = %q, want %q", crd.Spec.Scope, apiextensionsv1.ClusterScoped)
	}

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	for _, field := range []string{"machine", "operation", "endpoint", "boot", "provisioning", "artifacts", "expiresAt"} {
		if _, ok := spec.Properties[field]; !ok {
			t.Errorf("session spec is missing %q", field)
		}
	}

	assertSchemaValidations(t, spec, map[string]string{
		"self == oldSelf": "netboot session spec is immutable",
	})
	boot := spec.Properties["boot"]

	firmware, ok := boot.Properties["firmwareArtifact"]
	if !ok {
		t.Fatal("session boot snapshot is missing firmwareArtifact")
	}

	if firmware.MinLength == nil || *firmware.MinLength != 1 {
		t.Error("session boot firmwareArtifact must be non-empty")
	}

	provisioning := spec.Properties["provisioning"]
	for _, field := range []string{"cluster", "kubernetes", "agent", "providerLabels", "userData"} {
		if _, ok := provisioning.Properties[field]; !ok {
			t.Errorf("session provisioning snapshot is missing %q", field)
		}
	}

	artifactSource := spec.Properties["artifacts"].Properties["files"].Items.Schema.Properties["source"]
	requireEnumValue(t, artifactSource.Enum, "Session")

	status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
	for _, field := range []string{"phase", "conditions"} {
		if _, ok := status.Properties[field]; !ok {
			t.Errorf("session status is missing %q", field)
		}
	}
}

func requireEnumValue(t *testing.T, values []apiextensionsv1.JSON, want string) {
	t.Helper()

	for _, value := range values {
		var got string
		if err := yaml.Unmarshal(value.Raw, &got); err == nil && got == want {
			return
		}
	}

	t.Errorf("enum does not contain %q", want)
}

func TestMachineOperationTargetInputHasNetbootSessionRef(t *testing.T) {
	t.Parallel()

	crd := readCRD(t, "../../../deploy/machina/crd/unbounded-cloud.io_machineoperations.yaml")

	input := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].
		Properties["targets"].Items.Schema.Properties["input"]
	if _, ok := input.Properties["netbootSessionRef"]; !ok {
		t.Error("operation target input is missing netbootSessionRef")
	}
}

func readCRD(t *testing.T, path string) apiextensionsv1.CustomResourceDefinition {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("parse CRD: %v", err)
	}

	return crd
}
