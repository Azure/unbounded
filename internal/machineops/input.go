// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineconfigs"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

type targetInputError struct {
	err error
}

func (e *targetInputError) Error() string {
	return e.err.Error()
}

func (e *targetInputError) Unwrap() error {
	return e.err
}

func permanentTargetInputError(err error) error {
	return &targetInputError{err: err}
}

func validateProviderReference(machine *unboundedv1alpha3.Machine, provider *publicmachineops.Provider) error {
	host, err := resolveMachineHost(machine)
	if err != nil {
		return err
	}

	providerRef := host.machineRef
	if providerRef == nil {
		if strings.TrimSpace(host.providerID) == "" {
			return fmt.Errorf("machine %s must set a host providerID or machineRef", machine.Name)
		}

		return nil
	}

	if strings.TrimSpace(providerRef.APIGroup) == "" ||
		strings.TrimSpace(providerRef.Kind) == "" ||
		strings.TrimSpace(providerRef.Name) == "" {
		return fmt.Errorf("machine %s host machineRef must set apiGroup, kind, and name", machine.Name)
	}

	expected, ok := provider.ProviderMachineKind()
	if !ok {
		return fmt.Errorf("provider %s does not accept host.external.machineRef", provider.Name())
	}

	actual := schema.GroupKind{Group: providerRef.APIGroup, Kind: providerRef.Kind}
	if actual != expected {
		return fmt.Errorf("provider %s accepts %s, not %s", provider.Name(), expected, actual)
	}

	return nil
}

func (r *MachineOperationReconciler) initializeOperationTarget(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
	machine *unboundedv1alpha3.Machine,
	providerMatch providerMatch,
) error {
	input, err := r.resolveOperationTargetInput(ctx, op, machine)
	if err != nil {
		return err
	}

	return r.updateOperationStatus(ctx, op.Name, func(latest *unboundedv1alpha3.MachineOperation) {
		if _, ok := operationTarget(latest, machine.Name); ok {
			return
		}

		latest.Status.Targets = append(latest.Status.Targets, unboundedv1alpha3.MachineOperationTargetStatus{
			MachineRef:         machine.Name,
			Phase:              unboundedv1alpha3.OperationPhasePending,
			Message:            fmt.Sprintf("target initialized for %s", providerMatch.provider.Name()),
			ObservedGeneration: machine.Generation,
			Input:              input,
		})
	})
}

func (r *MachineOperationReconciler) resolveOperationTargetInput(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
	machine *unboundedv1alpha3.Machine,
) (*unboundedv1alpha3.MachineOperationTargetInput, error) {
	input := &unboundedv1alpha3.MachineOperationTargetInput{}

	host, err := resolveMachineHost(machine)
	if err != nil {
		return nil, permanentTargetInputError(err)
	}

	if host.machineRef != nil {
		providerRef, err := r.snapshotProviderMachine(ctx, host.machineRef)
		if err != nil {
			return nil, err
		}

		input.ProviderRef = providerRef
	}

	if op.Spec.OperationKind == unboundedv1alpha3.OperationHostReplace {
		hostImage, err := r.resolveHostImage(ctx, machine)
		if err != nil {
			return nil, err
		}

		input.HostImage = hostImage

		// Frozen with the image, and for the same reason: the operation acts on
		// the inputs it was admitted with, so editing the Machine while it is in
		// flight cannot change what a retry generates.
		input.ProvisioningFormat, err = r.resolveReplacementProvisioningFormat(ctx, machine, hostImage)
		if err != nil {
			return nil, permanentTargetInputError(err)
		}
	}

	return input, nil
}

func (r *MachineOperationReconciler) resolveReplacementProvisioningFormat(ctx context.Context, machine *unboundedv1alpha3.Machine, image string) (unboundedv1alpha3.ProvisioningFormat, error) {
	resolved := machine.DeepCopy()
	// Machine-level settings override the versioned template. An explicit
	// Machine image without its own format must not inherit a format describing
	// a different template image.
	if (machine.Spec.Host == nil || (machine.Spec.Host.ProvisioningFormat == "" && machine.Spec.Host.Image == "")) && machine.Spec.ConfigurationRef != nil {
		version, err := machineconfigs.ResolveVersionFromRef(ctx, r.Client, machine.Spec.ConfigurationRef)
		if err != nil {
			return "", err
		}

		if host := version.Spec.Template.Host; host != nil && host.ProvisioningFormat != "" {
			if resolved.Spec.Host == nil {
				resolved.Spec.Host = &unboundedv1alpha3.HostSpec{}
			}

			resolved.Spec.Host.ProvisioningFormat = host.ProvisioningFormat
		}
	}

	return replacementProvisioningFormat(resolved, image)
}

func replacementProvisioningFormat(machine *unboundedv1alpha3.Machine, image string) (unboundedv1alpha3.ProvisioningFormat, error) {
	if machine.Spec.Host != nil && machine.Spec.Host.ProvisioningFormat != "" {
		return machine.Spec.Host.ProvisioningFormat, nil
	}

	observed := machine.Status.ObservedProvisioningFormat
	if image != "" && observed == unboundedv1alpha3.ProvisioningFormatIgnition {
		return "", fmt.Errorf("HostReplace with an explicit image on an Ignition host requires an explicit target provisioningFormat")
	}

	if observed != "" {
		return observed, nil
	}

	return unboundedv1alpha3.ProvisioningFormatCloudInit, nil
}

func (r *MachineOperationReconciler) snapshotProviderMachine(
	ctx context.Context,
	providerRef *unboundedv1alpha3.ProviderMachineReference,
) (*unboundedv1alpha3.ProviderMachineSnapshot, error) {
	if r.RESTMapper == nil {
		return nil, permanentTargetInputError(fmt.Errorf("REST mapper is required to resolve providerRef"))
	}

	groupKind := schema.GroupKind{Group: providerRef.APIGroup, Kind: providerRef.Kind}

	mapping, err := r.RESTMapper.RESTMapping(groupKind)
	if err != nil {
		mappingErr := fmt.Errorf("resolve providerRef kind %s: %w", groupKind, err)
		if apimeta.IsNoMatchError(err) {
			return nil, permanentTargetInputError(mappingErr)
		}

		return nil, mappingErr
	}

	if mapping.Scope.Name() != apimeta.RESTScopeNameRoot {
		return nil, permanentTargetInputError(fmt.Errorf("providerRef kind %s must be cluster-scoped", groupKind))
	}

	providerMachine := &unstructured.Unstructured{}
	providerMachine.SetGroupVersionKind(mapping.GroupVersionKind)

	if err := r.Get(ctx, client.ObjectKey{Name: providerRef.Name}, providerMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, permanentTargetInputError(fmt.Errorf("get provider Machine %s %s: %w", groupKind, providerRef.Name, err))
		}

		return nil, fmt.Errorf("get provider Machine %s %s: %w", groupKind, providerRef.Name, err)
	}

	if providerMachine.GetUID() == "" {
		return nil, permanentTargetInputError(fmt.Errorf("provider Machine %s %s has no UID", groupKind, providerRef.Name))
	}

	if providerMachine.GetGeneration() < 1 {
		return nil, permanentTargetInputError(fmt.Errorf("provider Machine %s %s has invalid generation %d", groupKind, providerRef.Name, providerMachine.GetGeneration()))
	}

	return &unboundedv1alpha3.ProviderMachineSnapshot{
		APIGroup:   providerRef.APIGroup,
		Kind:       providerRef.Kind,
		Name:       providerRef.Name,
		UID:        providerMachine.GetUID(),
		Generation: providerMachine.GetGeneration(),
	}, nil
}

func (r *MachineOperationReconciler) resolveHostImage(
	ctx context.Context,
	machine *unboundedv1alpha3.Machine,
) (string, error) {
	if machine.Spec.Host != nil && strings.TrimSpace(machine.Spec.Host.Image) != "" {
		return machine.Spec.Host.Image, nil
	}

	if machine.Spec.ConfigurationRef == nil {
		return "", nil
	}

	configurationVersion, err := machineconfigs.ResolveVersionFromRef(ctx, r.Client, machine.Spec.ConfigurationRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", permanentTargetInputError(fmt.Errorf("resolve MachineConfigurationVersion for host image: %w", err))
		}

		return "", fmt.Errorf("resolve MachineConfigurationVersion for host image: %w", err)
	}

	if configurationVersion.Spec.Template.Host == nil {
		return "", nil
	}

	return configurationVersion.Spec.Template.Host.Image, nil
}
