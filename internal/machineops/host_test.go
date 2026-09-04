// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"testing"

	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestResolveMachineHost(t *testing.T) {
	t.Parallel()

	machineRef := &unboundedv1alpha3.ProviderMachineReference{
		APIGroup: "example.io",
		Kind:     "ExampleMachine",
		Name:     "machine-1",
	}

	tests := []struct {
		name    string
		spec    unboundedv1alpha3.MachineSpec
		want    resolvedMachineHost
		wantErr string
	}{
		{
			name: "Azure host infers built-in provider",
			spec: unboundedv1alpha3.MachineSpec{Host: &unboundedv1alpha3.HostSpec{
				Azure: &unboundedv1alpha3.AzureHostSpec{ResourceID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"},
			}},
			want: resolvedMachineHost{
				kind:       machineHostKindAzure,
				provider:   unboundedv1alpha3.ExternalProviderAzureVM,
				providerID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
			},
		},
		{
			name: "external host carries routing and identity",
			spec: unboundedv1alpha3.MachineSpec{Host: &unboundedv1alpha3.HostSpec{
				External: &unboundedv1alpha3.ExternalHostSpec{
					Provider:   "ExampleProvider",
					ProviderID: "example://machine-1",
					MachineRef: machineRef,
				},
			}},
			want: resolvedMachineHost{
				kind:       machineHostKindExternal,
				provider:   "ExampleProvider",
				providerID: "example://machine-1",
				machineRef: machineRef,
			},
		},
		{
			name: "netboot host is owned by Metalman",
			spec: unboundedv1alpha3.MachineSpec{Host: &unboundedv1alpha3.HostSpec{
				Netboot: &unboundedv1alpha3.PXESpec{Image: "example/image:v1"},
			}},
			want: resolvedMachineHost{kind: machineHostKindNetboot},
		},
		{
			name: "legacy provider remains readable",
			spec: unboundedv1alpha3.MachineSpec{
				Provider:   "LegacyProvider",
				ProviderID: "legacy://machine-1",
				Host:       &unboundedv1alpha3.HostSpec{Image: "image-v2"},
			},
			want: resolvedMachineHost{
				kind:       machineHostKindLegacy,
				provider:   "LegacyProvider",
				providerID: "legacy://machine-1",
			},
		},
		{
			name: "reject multiple canonical owners",
			spec: unboundedv1alpha3.MachineSpec{Host: &unboundedv1alpha3.HostSpec{
				Azure:    &unboundedv1alpha3.AzureHostSpec{ResourceID: "azure-resource"},
				External: &unboundedv1alpha3.ExternalHostSpec{Provider: "external", ProviderID: "external-resource"},
			}},
			wantErr: "exactly one",
		},
		{
			name: "reject canonical and legacy ownership",
			spec: unboundedv1alpha3.MachineSpec{
				Provider: "LegacyProvider",
				Host: &unboundedv1alpha3.HostSpec{
					Azure: &unboundedv1alpha3.AzureHostSpec{ResourceID: "azure-resource"},
				},
			},
			wantErr: "cannot be combined",
		},
		{
			name: "reject external host without identity",
			spec: unboundedv1alpha3.MachineSpec{Host: &unboundedv1alpha3.HostSpec{
				External: &unboundedv1alpha3.ExternalHostSpec{Provider: "external"},
			}},
			wantErr: "providerID or machineRef",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveMachineHost(&unboundedv1alpha3.Machine{Spec: tt.spec})
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestMachineSpecNetbootPrefersCanonicalHost(t *testing.T) {
	t.Parallel()

	legacy := &unboundedv1alpha3.PXESpec{Image: "legacy"}
	canonical := &unboundedv1alpha3.PXESpec{Image: "canonical"}
	spec := &unboundedv1alpha3.MachineSpec{
		PXE:  legacy,
		Host: &unboundedv1alpha3.HostSpec{Netboot: canonical},
	}

	require.Same(t, canonical, spec.Netboot())
}
