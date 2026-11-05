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

func machineSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	data, err := os.ReadFile("../../../deploy/machina/crd/unbounded-cloud.io_machines.yaml")
	if err != nil {
		t.Fatalf("read Machine CRD: %v", err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("parse Machine CRD: %v", err)
	}

	return *crd.Spec.Versions[0].Schema.OpenAPIV3Schema
}

func machineStatusSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	return machineSchema(t).Properties["status"]
}

func enumValues(t *testing.T, schema apiextensionsv1.JSONSchemaProps) []string {
	t.Helper()

	var got []string

	for _, value := range schema.Enum {
		var s string
		if err := yaml.Unmarshal(value.Raw, &s); err != nil {
			t.Fatalf("parse enum value %s: %v", value.Raw, err)
		}

		got = append(got, s)
	}

	return got
}

// status.isolationChange is how the agent reports a mode change made at a
// repave, now that no MachineOperation carries it.
func TestMachineStatusIsolationChangeSchema(t *testing.T) {
	t.Parallel()

	change, ok := machineStatusSchema(t).Properties["isolationChange"]
	if !ok {
		t.Fatal("status.isolationChange is missing from the Machine CRD")
	}

	for _, field := range []string{"targetMode", "phase", "message", "reason", "strategy", "lastTransitionTime"} {
		if _, ok := change.Properties[field]; !ok {
			t.Errorf("status.isolationChange.%s is missing", field)
		}
	}

	if got, want := enumValues(t, change.Properties["phase"]), []string{"InProgress", "Succeeded", "Failed"}; !slices.Equal(got, want) {
		t.Errorf("phase enum = %v, want %v", got, want)
	}

	if got, want := enumValues(t, change.Properties["strategy"]), []string{"InPlace", "Reprovision"}; !slices.Equal(got, want) {
		t.Errorf("strategy enum = %v, want %v", got, want)
	}

	// A refused change can name a mode this release does not know, so the
	// target is bounded but not an enum.
	if got := change.Properties["targetMode"]; len(got.Enum) != 0 || got.MaxLength == nil {
		t.Errorf("targetMode must be a bounded free string, got enum %v maxLength %v", got.Enum, got.MaxLength)
	}

	message := change.Properties["message"]
	if message.MaxLength == nil || *message.MaxLength != IsolationChangeMessageMaxLength {
		t.Errorf("message maxLength = %v, want %d (IsolationChangeMessageMaxLength)", message.MaxLength, IsolationChangeMessageMaxLength)
	}

	if change.Properties["reason"].MaxLength == nil || change.Properties["reason"].Pattern == "" {
		t.Error("reason must be bounded and patterned")
	}
}

// spec.isolation and status.isolation accept the empty string (no
// declaration, not reported) and only the modes this release can run.
func TestMachineIsolationEnums(t *testing.T) {
	t.Parallel()

	want := []string{"", "nspawn", "none"}

	if got := enumValues(t, machineSchema(t).Properties["spec"].Properties["isolation"]); !slices.Equal(got, want) {
		t.Errorf("spec.isolation enum = %v, want %v", got, want)
	}

	if got := enumValues(t, machineStatusSchema(t).Properties["isolation"]); !slices.Equal(got, want) {
		t.Errorf("status.isolation enum = %v, want %v", got, want)
	}
}

func TestIsolationChangePhaseValues(t *testing.T) {
	t.Parallel()

	for phase, want := range map[IsolationChangePhase]string{
		IsolationChangeInProgress: "InProgress",
		IsolationChangeSucceeded:  "Succeeded",
		IsolationChangeFailed:     "Failed",
	} {
		if string(phase) != want {
			t.Errorf("phase %q, want %q", phase, want)
		}
	}
}
