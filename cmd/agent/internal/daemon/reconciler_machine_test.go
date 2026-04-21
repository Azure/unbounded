// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
// desiredConfigFromMCV
// ---------------------------------------------------------------------------

func Test_desiredConfigFromMCV_OverlaysVersion(t *testing.T) {
	applied := baseConfig()
	tmpl := &v1alpha3.MachineConfigurationTemplate{
		Kubernetes: &v1alpha3.MachineConfigurationKubernetes{
			Version: "v1.34.0",
		},
	}

	desired := desiredConfigFromMCV(applied, tmpl)
	assert.Equal(t, "1.34.0", desired.Cluster.Version)

	// Original should be untouched.
	assert.Equal(t, "1.33.1", applied.Cluster.Version)
}

func Test_desiredConfigFromMCV_PreservesAppliedWhenEmptyTemplate(t *testing.T) {
	applied := baseConfig()
	tmpl := &v1alpha3.MachineConfigurationTemplate{}

	desired := desiredConfigFromMCV(applied, tmpl)
	assert.Equal(t, applied.Cluster.Version, desired.Cluster.Version)
	assert.Equal(t, applied.Kubelet.ApiServer, desired.Kubelet.ApiServer)
	assert.Equal(t, applied.OCIImage, desired.OCIImage)
}

func Test_desiredConfigFromMCV_OverlaysLabels(t *testing.T) {
	applied := baseConfig()
	tmpl := &v1alpha3.MachineConfigurationTemplate{
		Kubernetes: &v1alpha3.MachineConfigurationKubernetes{
			NodeLabels: map[string]string{"env": "prod"},
		},
	}

	desired := desiredConfigFromMCV(applied, tmpl)
	assert.Equal(t, map[string]string{"env": "prod"}, desired.Kubelet.Labels)

	// Original should be untouched.
	assert.Equal(t, map[string]string{"env": "test"}, applied.Kubelet.Labels)
}

func Test_desiredConfigFromMCV_OverlaysAgentImage(t *testing.T) {
	applied := baseConfig()
	tmpl := &v1alpha3.MachineConfigurationTemplate{
		Agent: &v1alpha3.MachineConfigurationAgent{Image: "custom:v2"},
	}

	desired := desiredConfigFromMCV(applied, tmpl)
	assert.Equal(t, "custom:v2", desired.OCIImage)
}

func Test_desiredConfigFromMCV_DoesNotAliasLabels(t *testing.T) {
	applied := &provision.AgentConfig{
		Kubelet: provision.AgentKubeletConfig{
			Labels: map[string]string{"a": "1"},
		},
	}
	tmpl := &v1alpha3.MachineConfigurationTemplate{}

	desired := desiredConfigFromMCV(applied, tmpl)
	desired.Kubelet.Labels["b"] = "2"

	// Mutation of desired should not affect applied.
	assert.NotContains(t, applied.Kubelet.Labels, "b")
}

func Test_desiredConfigFromMCV_OverlaysTaints(t *testing.T) {
	applied := baseConfig()
	tmpl := &v1alpha3.MachineConfigurationTemplate{
		Kubernetes: &v1alpha3.MachineConfigurationKubernetes{
			RegisterWithTaints: []string{"key=val:NoSchedule"},
		},
	}

	desired := desiredConfigFromMCV(applied, tmpl)
	assert.Equal(t, []string{"key=val:NoSchedule"}, desired.Kubelet.RegisterWithTaints)
}

// ---------------------------------------------------------------------------
// resolveMCV
// ---------------------------------------------------------------------------

func Test_resolveMCV_Success(t *testing.T) {
	mcv := &v1alpha3.MachineConfigurationVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "myconfig-v3"},
		Spec: v1alpha3.MachineConfigurationVersionSpec{
			Version: 3,
			Template: v1alpha3.MachineConfigurationTemplate{
				Kubernetes: &v1alpha3.MachineConfigurationKubernetes{
					Version: "v1.34.0",
				},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(mcv).
		Build()

	r := &reconciler{client: c}

	machine := &v1alpha3.Machine{
		Spec: v1alpha3.MachineSpec{
			ConfigurationRef: &v1alpha3.MachineConfigurationRef{
				Name:    "myconfig",
				Version: ptr.To(int32(3)),
			},
		},
	}

	got, err := r.resolveMCV(context.Background(), slog.Default(), machine)
	require.NoError(t, err)
	assert.Equal(t, "myconfig-v3", got.Name)
	assert.Equal(t, int32(3), got.Spec.Version)
}

func Test_resolveMCV_NilConfigurationRef(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		Build()

	r := &reconciler{client: c}

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "mymachine"},
	}

	_, err := r.resolveMCV(context.Background(), slog.Default(), machine)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no spec.configurationRef")
}

func Test_resolveMCV_NilVersion(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		Build()

	r := &reconciler{client: c}

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "mymachine"},
		Spec: v1alpha3.MachineSpec{
			ConfigurationRef: &v1alpha3.MachineConfigurationRef{
				Name: "myconfig",
			},
		},
	}

	_, err := r.resolveMCV(context.Background(), slog.Default(), machine)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no version set")
}

func Test_resolveMCV_NotFound(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		Build()

	r := &reconciler{client: c}

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "mymachine"},
		Spec: v1alpha3.MachineSpec{
			ConfigurationRef: &v1alpha3.MachineConfigurationRef{
				Name:    "myconfig",
				Version: ptr.To(int32(1)),
			},
		},
	}

	_, err := r.resolveMCV(context.Background(), slog.Default(), machine)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get MachineConfigurationVersion")
}
