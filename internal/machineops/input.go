// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
	providerRef := machine.Spec.ProviderRef
	if providerRef == nil {
		if strings.TrimSpace(machine.Spec.ProviderID) == "" {
			return fmt.Errorf("machine %s must set spec.providerRef or legacy spec.providerID", machine.Name)
		}

		return nil
	}

	if strings.TrimSpace(providerRef.APIGroup) == "" ||
		strings.TrimSpace(providerRef.Kind) == "" ||
		strings.TrimSpace(providerRef.Name) == "" {
		return fmt.Errorf("machine %s spec.providerRef must set apiGroup, kind, and name", machine.Name)
	}

	expected, ok := provider.ProviderMachineKind()
	if !ok {
		return fmt.Errorf("provider %s does not accept spec.providerRef", provider.Name())
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

	if machine.Spec.ProviderRef != nil {
		providerRef, err := r.snapshotProviderMachine(ctx, machine.Spec.ProviderRef)
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
	}

	return input, nil
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
		return nil, permanentTargetInputError(fmt.Errorf("resolve providerRef kind %s: %w", groupKind, err))
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
