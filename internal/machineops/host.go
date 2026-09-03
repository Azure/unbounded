// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"fmt"
	"strings"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type machineHostKind string

const (
	machineHostKindLegacy   machineHostKind = "legacy"
	machineHostKindNetboot  machineHostKind = "netboot"
	machineHostKindAzure    machineHostKind = "azure"
	machineHostKindExternal machineHostKind = "external"
)

type resolvedMachineHost struct {
	kind       machineHostKind
	provider   string
	providerID string
	machineRef *unboundedv1alpha3.ProviderMachineReference
}

func resolveMachineHost(machine *unboundedv1alpha3.Machine) (resolvedMachineHost, error) {
	if machine == nil {
		return resolvedMachineHost{}, fmt.Errorf("machine is required")
	}

	host := machine.Spec.Host
	if host == nil || (host.Netboot == nil && host.Azure == nil && host.External == nil) {
		return resolvedMachineHost{
			kind:       machineHostKindLegacy,
			provider:   strings.TrimSpace(machine.Spec.Provider),
			providerID: machine.Spec.ProviderID,
		}, nil
	}

	variants := 0
	if host.Netboot != nil {
		variants++
	}

	if host.Azure != nil {
		variants++
	}

	if host.External != nil {
		variants++
	}

	if variants != 1 {
		return resolvedMachineHost{}, fmt.Errorf("machine %s spec.host must set exactly one of netboot, azure, or external", machine.Name)
	}

	if machine.Spec.PXE != nil || strings.TrimSpace(machine.Spec.Provider) != "" || strings.TrimSpace(machine.Spec.ProviderID) != "" {
		return resolvedMachineHost{}, fmt.Errorf("machine %s spec.host ownership cannot be combined with legacy spec.pxe, spec.provider, or spec.providerID", machine.Name)
	}

	if host.Netboot != nil {
		return resolvedMachineHost{kind: machineHostKindNetboot}, nil
	}

	if host.Azure != nil {
		if strings.TrimSpace(host.Azure.ResourceID) == "" {
			return resolvedMachineHost{}, fmt.Errorf("machine %s spec.host.azure.resourceID is required", machine.Name)
		}

		return resolvedMachineHost{
			kind:       machineHostKindAzure,
			provider:   unboundedv1alpha3.ExternalProviderAzureVM,
			providerID: host.Azure.ResourceID,
		}, nil
	}

	external := host.External

	provider := strings.TrimSpace(external.Provider)
	if provider == "" {
		return resolvedMachineHost{}, fmt.Errorf("machine %s spec.host.external.provider is required", machine.Name)
	}

	if strings.TrimSpace(external.ProviderID) == "" && external.MachineRef == nil {
		return resolvedMachineHost{}, fmt.Errorf("machine %s spec.host.external must set providerID or machineRef", machine.Name)
	}

	return resolvedMachineHost{
		kind:       machineHostKindExternal,
		provider:   provider,
		providerID: external.ProviderID,
		machineRef: external.MachineRef,
	}, nil
}
