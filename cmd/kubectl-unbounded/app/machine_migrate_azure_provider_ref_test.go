// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1alpha1 "github.com/Azure/unbounded/api/infrastructure/v1alpha1"
	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestMigrateAzureProviderRefCreatesCompanionAndPatchesMachine(t *testing.T) {
	t.Parallel()

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1", UID: types.UID("machine-uid")},
		Spec: unboundedv1alpha3.MachineSpec{
			Provider:   unboundedv1alpha3.ExternalProviderAzureVM,
			ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(buildScheme()).WithObjects(machine).Build()

	err := migrateAzureProviderRef(context.Background(), kubeClient, machine.Name, "azure-machine-1")
	require.NoError(t, err)

	var updatedMachine unboundedv1alpha3.Machine
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKeyFromObject(machine), &updatedMachine))
	require.Equal(t, machine.Spec.ProviderID, updatedMachine.Spec.ProviderID)
	require.Equal(t, &unboundedv1alpha3.ProviderMachineReference{
		APIGroup: infrastructurev1alpha1.GroupVersion.Group,
		Kind:     infrastructurev1alpha1.AzureMachineKind,
		Name:     "azure-machine-1",
	}, updatedMachine.Spec.ProviderRef)

	var azureMachine infrastructurev1alpha1.AzureMachine
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: "azure-machine-1"}, &azureMachine))
	require.Equal(t, "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1", azureMachine.Spec.ResourceID)
	owner := metav1.GetControllerOf(&azureMachine)
	require.NotNil(t, owner)
	require.Equal(t, machine.Name, owner.Name)
	require.Equal(t, machine.UID, owner.UID)

	require.NoError(t, migrateAzureProviderRef(context.Background(), kubeClient, machine.Name, azureMachine.Name))
}

func TestMigrateAzureProviderRefRejectsConflictingAzureMachine(t *testing.T) {
	t.Parallel()

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1", UID: types.UID("machine-uid")},
		Spec: unboundedv1alpha3.MachineSpec{
			Provider:   unboundedv1alpha3.ExternalProviderAzureVM,
			ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
		},
	}
	azureMachine := &infrastructurev1alpha1.AzureMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-machine-1"},
		Spec: infrastructurev1alpha1.AzureMachineSpec{
			ResourceID: "/subscriptions/other/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(buildScheme()).WithObjects(machine, azureMachine).Build()

	err := migrateAzureProviderRef(context.Background(), kubeClient, machine.Name, azureMachine.Name)
	require.ErrorContains(t, err, "does not match")

	var updated unboundedv1alpha3.Machine
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKeyFromObject(machine), &updated))
	require.Nil(t, updated.Spec.ProviderRef)
}

func TestMigrateAzureProviderRefRejectsNonAzureMachine(t *testing.T) {
	t.Parallel()

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1"},
		Spec: unboundedv1alpha3.MachineSpec{
			Provider:   unboundedv1alpha3.ExternalProviderOCIInstance,
			ProviderID: "oci://instance-1",
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(buildScheme()).WithObjects(machine).Build()

	err := migrateAzureProviderRef(context.Background(), kubeClient, machine.Name, machine.Name)
	require.ErrorContains(t, err, "not \"AzureVM\"")
}
