// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/provision"
)

// ---------------------------------------------------------------------------
// hasOperationsDrift
// ---------------------------------------------------------------------------

func Test_hasOperationsDrift_NilSpec(t *testing.T) {
	machine := &v1alpha3.Machine{}
	assert.False(t, hasOperationsDrift(machine))
}

func Test_hasOperationsDrift_NoDrift(t *testing.T) {
	machine := &v1alpha3.Machine{}
	machine.Spec.Operations = &v1alpha3.OperationsSpec{RepaveCounter: 1}
	machine.Status.Operations = &v1alpha3.OperationsStatus{RepaveCounter: 1}

	assert.False(t, hasOperationsDrift(machine))
}

func Test_hasOperationsDrift_RepaveDrift(t *testing.T) {
	machine := &v1alpha3.Machine{}
	machine.Spec.Operations = &v1alpha3.OperationsSpec{RepaveCounter: 2}
	machine.Status.Operations = &v1alpha3.OperationsStatus{RepaveCounter: 1}

	assert.True(t, hasOperationsDrift(machine))
}

func Test_hasOperationsDrift_NilStatus(t *testing.T) {
	machine := &v1alpha3.Machine{}
	machine.Spec.Operations = &v1alpha3.OperationsSpec{RepaveCounter: 1}

	assert.True(t, hasOperationsDrift(machine))
}

// ---------------------------------------------------------------------------
// acknowledgeOperations
// ---------------------------------------------------------------------------

func Test_acknowledgeOperations(t *testing.T) {
	machine := &v1alpha3.Machine{}
	machine.Spec.Operations = &v1alpha3.OperationsSpec{RepaveCounter: 3}

	acknowledgeOperations(machine)

	require.NotNil(t, machine.Status.Operations)
	assert.Equal(t, int64(3), machine.Status.Operations.RepaveCounter)
}

func Test_acknowledgeOperations_NilSpec(t *testing.T) {
	machine := &v1alpha3.Machine{}

	// Should not panic.
	acknowledgeOperations(machine)
	assert.Nil(t, machine.Status.Operations)
}

// ---------------------------------------------------------------------------
// specVersion
// ---------------------------------------------------------------------------

func Test_specVersion(t *testing.T) {
	machine := &v1alpha3.Machine{}
	assert.Equal(t, "", specVersion(machine))

	machine.Spec.Kubernetes = &v1alpha3.KubernetesSpec{Version: "v1.33.1"}
	assert.Equal(t, "v1.33.1", specVersion(machine))
}

// ---------------------------------------------------------------------------
// desiredConfigFromMachine
// ---------------------------------------------------------------------------

func Test_desiredConfigFromMachine_OverlaysVersion(t *testing.T) {
	applied := baseConfig()
	machine := &v1alpha3.Machine{}
	machine.Spec.Kubernetes = &v1alpha3.KubernetesSpec{Version: "v1.34.0"}

	desired := desiredConfigFromMachine(applied, machine)
	assert.Equal(t, "1.34.0", desired.Cluster.Version)

	// Original should be untouched.
	assert.Equal(t, "1.33.1", applied.Cluster.Version)
}

func Test_desiredConfigFromMachine_PreservesAppliedWhenNoSpec(t *testing.T) {
	applied := baseConfig()
	machine := &v1alpha3.Machine{}

	desired := desiredConfigFromMachine(applied, machine)
	assert.Equal(t, applied.Cluster.Version, desired.Cluster.Version)
	assert.Equal(t, applied.Kubelet.ApiServer, desired.Kubelet.ApiServer)
	assert.Equal(t, applied.OCIImage, desired.OCIImage)
}

func Test_desiredConfigFromMachine_OverlaysLabels(t *testing.T) {
	applied := baseConfig()
	machine := &v1alpha3.Machine{}
	machine.Spec.Kubernetes = &v1alpha3.KubernetesSpec{
		NodeLabels: map[string]string{"env": "prod"},
	}

	desired := desiredConfigFromMachine(applied, machine)
	assert.Equal(t, map[string]string{"env": "prod"}, desired.Kubelet.Labels)

	// Original should be untouched.
	assert.Equal(t, map[string]string{"env": "test"}, applied.Kubelet.Labels)
}

func Test_desiredConfigFromMachine_OverlaysAgentImage(t *testing.T) {
	applied := baseConfig()
	machine := &v1alpha3.Machine{
		Spec: v1alpha3.MachineSpec{
			Agent: &v1alpha3.AgentSpec{Image: "custom:v2"},
		},
	}

	desired := desiredConfigFromMachine(applied, machine)
	assert.Equal(t, "custom:v2", desired.OCIImage)
}

func Test_desiredConfigFromMachine_DoesNotAliasLabels(t *testing.T) {
	applied := &provision.AgentConfig{
		Kubelet: provision.AgentKubeletConfig{
			Labels: map[string]string{"a": "1"},
		},
	}
	machine := &v1alpha3.Machine{}

	desired := desiredConfigFromMachine(applied, machine)
	desired.Kubelet.Labels["b"] = "2"

	// Mutation of desired should not affect applied.
	assert.NotContains(t, applied.Kubelet.Labels, "b")
}
