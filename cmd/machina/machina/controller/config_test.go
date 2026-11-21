// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestLoadConfigExperimentalContainerIsolation(t *testing.T) {
	t.Parallel()

	load := func(t *testing.T, body string) Config {
		t.Helper()

		path := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

		cfg, err := LoadConfig(path)
		require.NoError(t, err)

		return cfg
	}

	require.False(t, DefaultConfig().ExperimentalContainerIsolation, "off by default")
	require.False(t, load(t, "metricsAddr: \":8080\"\n").ExperimentalContainerIsolation)
	require.True(t, load(t, "experimentalContainerIsolation: true\n").ExperimentalContainerIsolation)
}

// The cache holds every Machine and MachineOperation, so objects are stored
// without managedFields.
func TestMachinaCacheDropsManagedFields(t *testing.T) {
	t.Parallel()

	transform := machinaCacheOptions().DefaultTransform
	if transform == nil {
		t.Fatal("the cache has no default transform")
	}

	operation := &unboundedv1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "op-1",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Spec: unboundedv1alpha3.MachineOperationSpec{MachineRef: "machine-1"},
	}

	stored, err := transform(operation)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}

	got, ok := stored.(*unboundedv1alpha3.MachineOperation)
	if !ok {
		t.Fatalf("transform returned %T", stored)
	}

	if len(got.ManagedFields) != 0 {
		t.Errorf("managedFields were kept: %v", got.ManagedFields)
	}

	if got.Spec.MachineRef != "machine-1" {
		t.Errorf("spec was changed: %+v", got.Spec)
	}
}
