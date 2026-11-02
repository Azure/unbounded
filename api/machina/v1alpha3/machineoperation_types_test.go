// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"os"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestOperationKindHostRebootValues(t *testing.T) {
	t.Parallel()

	if got := string(OperationHostHardReboot); got != "HostHardReboot" {
		t.Fatalf("OperationHostHardReboot = %q, want %q", got, "HostHardReboot")
	}

	if got := string(OperationHostSoftReboot); got != "HostSoftReboot" {
		t.Fatalf("OperationHostSoftReboot = %q, want %q", got, "HostSoftReboot")
	}
}

func TestMachineOperationKindSchemaEnum(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../../deploy/machina/crd/unbounded-cloud.io_machineoperations.yaml")
	if err != nil {
		t.Fatalf("read MachineOperation CRD: %v", err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("parse MachineOperation CRD: %v", err)
	}

	kindSchema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["operationKind"]

	var got []string

	for _, value := range kindSchema.Enum {
		var kind string
		if err := yaml.Unmarshal(value.Raw, &kind); err != nil {
			t.Fatalf("parse operationKind enum value %s: %v", value.Raw, err)
		}

		got = append(got, kind)
	}

	want := []string{
		string(OperationNodeReboot),
		string(OperationAgentUpgrade),
		string(OperationAgentReset),
		string(OperationHostHardReboot),
		string(OperationHostSoftReboot),
		string(OperationHostPowerOff),
		string(OperationHostPowerOn),
		string(OperationHostReplace),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("operationKind enum = %v, want %v", got, want)
	}
}
