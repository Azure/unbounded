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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type errorRESTMapper struct {
	apimeta.RESTMapper
	err error
}

type changingVersionClient struct {
	client.Client
	lists int
}

func (c *changingVersionClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	c.lists++
	versions := list.(*unboundedv1alpha3.MachineConfigurationVersionList)

	image, format := "ignition-image", unboundedv1alpha3.ProvisioningFormatIgnition
	if c.lists > 1 {
		image, format = "cloud-init-image", unboundedv1alpha3.ProvisioningFormatCloudInit
	}

	versions.Items = []unboundedv1alpha3.MachineConfigurationVersion{{
		ObjectMeta: metav1.ObjectMeta{Name: image},
		Spec: unboundedv1alpha3.MachineConfigurationVersionSpec{
			Version:  int32(c.lists),
			Template: unboundedv1alpha3.MachineConfigurationTemplate{Host: &unboundedv1alpha3.MachineConfigurationHostSpec{Image: image, ProvisioningFormat: format}},
		},
	}}

	return nil
}

func TestReplacementPairUsesOneVersionSelection(t *testing.T) {
	t.Parallel()

	for _, image := range []string{"", " ", "\t"} {
		for _, observed := range []unboundedv1alpha3.ProvisioningFormat{"", unboundedv1alpha3.ProvisioningFormatCloudInit} {
			c := &changingVersionClient{Client: fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).Build()}
			r := &MachineOperationReconciler{Client: c}
			m := newExternalMachine("worker", unboundedv1alpha3.ExternalProviderAzureVM)
			m.Spec.Host = &unboundedv1alpha3.HostSpec{Image: image}
			m.Spec.ConfigurationRef = &unboundedv1alpha3.MachineConfigurationRef{Name: "worker"}
			m.Status.ObservedProvisioningFormat = observed
			input, err := r.resolveOperationTargetInput(context.Background(), newMachineOperation("replace", "worker", unboundedv1alpha3.OperationHostReplace), m)
			require.NoError(t, err)
			require.Equal(t, 1, c.lists)
			require.Equal(t, "ignition-image", input.HostImage)
			require.Equal(t, unboundedv1alpha3.ProvisioningFormatIgnition, input.ProvisioningFormat)
		}
	}
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

func TestReplacementFormatFollowsSelectedImageAndSnapshot(t *testing.T) {
	t.Parallel()

	version := int32(2)
	v := &unboundedv1alpha3.MachineConfigurationVersion{
		ObjectMeta: metav1.ObjectMeta{Name: unboundedv1alpha3.MachineConfigurationVersionName("worker", version)},
		Spec: unboundedv1alpha3.MachineConfigurationVersionSpec{
			Version: version,
			Template: unboundedv1alpha3.MachineConfigurationTemplate{Host: &unboundedv1alpha3.MachineConfigurationHostSpec{
				Image: "template-image", ProvisioningFormat: unboundedv1alpha3.ProvisioningFormatIgnition,
			}},
		},
	}
	r := &MachineOperationReconciler{Client: fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(v).Build()}
	machine := newExternalMachine("worker", unboundedv1alpha3.ExternalProviderAzureVM)
	machine.Spec.ConfigurationRef = &unboundedv1alpha3.MachineConfigurationRef{Name: "worker", Version: &version}
	op := newMachineOperation("replace", "worker", unboundedv1alpha3.OperationHostReplace)
	input, err := r.resolveOperationTargetInput(context.Background(), op, machine)
	require.NoError(t, err)
	require.Equal(t, "template-image", input.HostImage)
	require.Equal(t, unboundedv1alpha3.ProvisioningFormatIgnition, input.ProvisioningFormat)
	machine.Spec.Host = &unboundedv1alpha3.HostSpec{Image: "other-image", ProvisioningFormat: unboundedv1alpha3.ProvisioningFormatCloudInit}
	_, err = r.operationRequest(context.Background(), op, machine, &unboundedv1alpha3.MachineOperationTargetStatus{Input: input}, machine.Spec.ProviderID, nil, true)
	require.ErrorContains(t, err, "cannot generate Ignition", "retry must use frozen format despite Machine edit")

	machine.Spec.Host.ProvisioningFormat = ""
	machine.Status.ObservedProvisioningFormat = unboundedv1alpha3.ProvisioningFormatIgnition
	_, err = r.resolveOperationTargetInput(context.Background(), op, machine)
	require.ErrorContains(t, err, "explicit target", "an overridden image must not inherit the template format")
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
