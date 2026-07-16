// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestResolveHostImage(t *testing.T) {
	t.Parallel()

	version := int32(2)
	configurationVersion := &unboundedv1alpha3.MachineConfigurationVersion{
		ObjectMeta: metav1.ObjectMeta{Name: unboundedv1alpha3.MachineConfigurationVersionName("worker", version)},
		Spec: unboundedv1alpha3.MachineConfigurationVersionSpec{
			Version: version,
			Template: unboundedv1alpha3.MachineConfigurationTemplate{
				Host: &unboundedv1alpha3.HostSpec{Image: "configuration-image"},
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(newOperationTestScheme(t)).
		WithObjects(configurationVersion).
		Build()
	reconciler := &MachineOperationReconciler{Client: client}

	tests := []struct {
		name    string
		machine *unboundedv1alpha3.Machine
		want    string
	}{
		{
			name: "Machine override wins",
			machine: &unboundedv1alpha3.Machine{Spec: unboundedv1alpha3.MachineSpec{
				Host:             &unboundedv1alpha3.HostSpec{Image: "machine-image"},
				ConfigurationRef: &unboundedv1alpha3.MachineConfigurationRef{Name: "worker", Version: &version},
			}},
			want: "machine-image",
		},
		{
			name: "configuration image is inherited",
			machine: &unboundedv1alpha3.Machine{Spec: unboundedv1alpha3.MachineSpec{
				ConfigurationRef: &unboundedv1alpha3.MachineConfigurationRef{Name: "worker", Version: &version},
			}},
			want: "configuration-image",
		},
		{
			name:    "omitted image preserves current image",
			machine: &unboundedv1alpha3.Machine{},
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := reconciler.resolveHostImage(context.Background(), tt.machine)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
