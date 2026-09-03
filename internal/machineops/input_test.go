// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type errorRESTMapper struct {
	apimeta.RESTMapper
	err error
}

func (m errorRESTMapper) RESTMapping(schema.GroupKind, ...string) (*apimeta.RESTMapping, error) {
	return nil, m.err
}

func TestResolveHostImage(t *testing.T) {
	t.Parallel()

	version := int32(2)
	configurationVersion := &unboundedv1alpha3.MachineConfigurationVersion{
		ObjectMeta: metav1.ObjectMeta{Name: unboundedv1alpha3.MachineConfigurationVersionName("worker", version)},
		Spec: unboundedv1alpha3.MachineConfigurationVersionSpec{
			Version: version,
			Template: unboundedv1alpha3.MachineConfigurationTemplate{
				Host: &unboundedv1alpha3.MachineConfigurationHostSpec{Image: "configuration-image"},
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

func TestSnapshotProviderMachineClassifiesRESTMappingErrors(t *testing.T) {
	t.Parallel()

	groupKind := schema.GroupKind{Group: "infrastructure.example.com", Kind: "ExampleMachine"}
	providerRef := &unboundedv1alpha3.ProviderMachineReference{
		APIGroup: groupKind.Group,
		Kind:     groupKind.Kind,
		Name:     "machine-1",
	}

	tests := []struct {
		name          string
		mappingErr    error
		wantPermanent bool
	}{
		{
			name:       "discovery failure is retryable",
			mappingErr: errors.New("discovery unavailable"),
		},
		{
			name: "unknown kind is permanent",
			mappingErr: &apimeta.NoKindMatchError{
				GroupKind:        groupKind,
				SearchedVersions: []string{"v1alpha1"},
			},
			wantPermanent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reconciler := &MachineOperationReconciler{
				RESTMapper: errorRESTMapper{err: tt.mappingErr},
			}

			_, err := reconciler.snapshotProviderMachine(context.Background(), providerRef)
			require.ErrorIs(t, err, tt.mappingErr)

			var permanentErr *targetInputError
			require.Equal(t, tt.wantPermanent, errors.As(err, &permanentErr))
		})
	}
}
