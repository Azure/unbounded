// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1alpha1 "github.com/Azure/unbounded/api/infrastructure/v1alpha1"
	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops/providers/azurevm"
)

func newMachineMigrateAzureProviderRefCommand(rt *machineCommandRuntime) *cobra.Command {
	var (
		azureMachineName string
		kubeconfig       string
	)

	cmd := &cobra.Command{
		Use:   "migrate-azure-provider-ref MACHINE",
		Short: "Migrate an Azure Machine from providerID to AzureMachine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := rt.context(cmd.Context())

			kubeClient, err := rt.clientWithKubeconfig(kubeconfig)
			if err != nil {
				return err
			}

			if azureMachineName == "" {
				azureMachineName = args[0]
			}

			if err := migrateAzureProviderRef(ctx, kubeClient, args[0], azureMachineName); err != nil {
				return err
			}

			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "migrated Machine %s to AzureMachine %s\n", args[0], azureMachineName); err != nil {
				return fmt.Errorf("write migration result: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&azureMachineName, "azure-machine-name", "", "Name of the AzureMachine resource (defaults to the Machine name)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to the kubeconfig file")

	return cmd
}

func migrateAzureProviderRef(
	ctx context.Context,
	kubeClient client.Client,
	machineName string,
	azureMachineName string,
) error {
	var machine unboundedv1alpha3.Machine
	if err := kubeClient.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
		return fmt.Errorf("get Machine %s: %w", machineName, err)
	}

	if machine.Spec.Provider != unboundedv1alpha3.ExternalProviderAzureVM {
		return fmt.Errorf("machine %s provider is %q, not %q", machineName, machine.Spec.Provider, unboundedv1alpha3.ExternalProviderAzureVM)
	}

	expectedGroupKind := infrastructurev1alpha1.GroupVersion.WithKind(infrastructurev1alpha1.AzureMachineKind).GroupKind()
	alreadyReferenced := machine.Spec.ProviderRef != nil

	if machine.Spec.ProviderRef != nil {
		if err := validateAzureProviderRef(machine.Spec.ProviderRef, expectedGroupKind, azureMachineName); err != nil {
			return fmt.Errorf("machine %s already has an incompatible providerRef: %w", machineName, err)
		}
	}

	resourceID, err := azurevm.NormalizeResourceID(machine.Spec.ProviderID)
	if err != nil {
		return fmt.Errorf("normalize Machine %s providerID: %w", machineName, err)
	}

	if err := ensureAzureMachine(ctx, kubeClient, &machine, azureMachineName, resourceID); err != nil {
		return err
	}

	if alreadyReferenced {
		return nil
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest unboundedv1alpha3.Machine
		if err := kubeClient.Get(ctx, client.ObjectKey{Name: machineName}, &latest); err != nil {
			return err
		}

		if latest.Spec.ProviderRef != nil {
			return validateAzureProviderRef(latest.Spec.ProviderRef, expectedGroupKind, azureMachineName)
		}

		patch := client.MergeFrom(latest.DeepCopy())
		latest.Spec.ProviderRef = &unboundedv1alpha3.ProviderMachineReference{
			APIGroup: expectedGroupKind.Group,
			Kind:     expectedGroupKind.Kind,
			Name:     azureMachineName,
		}

		return kubeClient.Patch(ctx, &latest, patch)
	}); err != nil {
		return fmt.Errorf("patch Machine %s providerRef: %w", machineName, err)
	}

	return nil
}

func ensureAzureMachine(
	ctx context.Context,
	kubeClient client.Client,
	machine *unboundedv1alpha3.Machine,
	azureMachineName string,
	resourceID string,
) error {
	var existing infrastructurev1alpha1.AzureMachine

	err := kubeClient.Get(ctx, client.ObjectKey{Name: azureMachineName}, &existing)
	if apierrors.IsNotFound(err) {
		azureMachine := &infrastructurev1alpha1.AzureMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name: azureMachineName,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(machine, unboundedv1alpha3.GroupVersion.WithKind("Machine")),
				},
			},
			Spec: infrastructurev1alpha1.AzureMachineSpec{ResourceID: resourceID},
		}

		if err := kubeClient.Create(ctx, azureMachine); err != nil {
			return fmt.Errorf("create AzureMachine %s: %w", azureMachineName, err)
		}

		return nil
	}

	if err != nil {
		return fmt.Errorf("get AzureMachine %s: %w", azureMachineName, err)
	}

	if !strings.EqualFold(existing.Spec.ResourceID, resourceID) {
		return fmt.Errorf("AzureMachine %s resourceID %q does not match %q", azureMachineName, existing.Spec.ResourceID, resourceID)
	}

	owner := metav1.GetControllerOf(&existing)
	if owner == nil {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.OwnerReferences = append(
			existing.OwnerReferences,
			*metav1.NewControllerRef(machine, unboundedv1alpha3.GroupVersion.WithKind("Machine")),
		)

		if err := kubeClient.Patch(ctx, &existing, patch); err != nil {
			return fmt.Errorf("set AzureMachine %s controller owner: %w", azureMachineName, err)
		}

		return nil
	}

	if owner.APIVersion != unboundedv1alpha3.GroupVersion.String() ||
		owner.Kind != "Machine" ||
		owner.Name != machine.Name ||
		owner.UID != machine.UID {
		return fmt.Errorf("AzureMachine %s is controlled by %s %s, not Machine %s", azureMachineName, owner.Kind, owner.Name, machine.Name)
	}

	return nil
}

func validateAzureProviderRef(
	providerRef *unboundedv1alpha3.ProviderMachineReference,
	expected schema.GroupKind,
	expectedName string,
) error {
	if providerRef.APIGroup != expected.Group || providerRef.Kind != expected.Kind || providerRef.Name != expectedName {
		return fmt.Errorf("got %s/%s %s, want %s/%s %s", providerRef.APIGroup, providerRef.Kind, providerRef.Name, expected.Group, expected.Kind, expectedName)
	}

	return nil
}
