// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azurevm

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
)

func TestParseAzureVMProviderID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		providerID string
		want       azureVMResourceRef
		wantErr    string
	}{
		{
			name:       "raw ARM ID",
			providerID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
			want: azureVMResourceRef{
				SubscriptionID: "sub",
				ResourceGroup:  "rg",
				VMName:         "vm1",
			},
		},
		{
			name:       "azure provider ID",
			providerID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
			want: azureVMResourceRef{
				SubscriptionID: "sub",
				ResourceGroup:  "rg",
				VMName:         "vm1",
			},
		},
		{
			name:       "wrong provider",
			providerID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/ip1",
			wantErr:    "Microsoft.Compute/virtualMachines",
		},
		{
			name:    "empty",
			wantErr: "required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseAzureVMProviderID(tt.providerID)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestProviderExecute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		operation unboundedv1alpha3.OperationKind
		wantCall  string
	}{
		{name: "hard reboot", operation: unboundedv1alpha3.OperationHardReboot, wantCall: "restart:rg/vm1"},
		{name: "power off", operation: unboundedv1alpha3.OperationPowerOff, wantCall: "powerOff:rg/vm1"},
		{name: "power on", operation: unboundedv1alpha3.OperationPowerOn, wantCall: "start:rg/vm1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &recordingAzureVMClient{}
			provider := &Provider{
				NewClient: func(subscriptionID string) (azureVMClient, error) {
					require.Equal(t, "sub", subscriptionID)
					return client, nil
				},
			}

			err := provider.Execute(context.Background(), machineops.OperationRequest{ProviderID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1", Operation: tt.operation})

			require.NoError(t, err)
			require.Equal(t, []string{tt.wantCall}, client.calls)
		})
	}
}

type recordingAzureVMClient struct {
	calls []string
	err   error
}

func (c *recordingAzureVMClient) Restart(_ context.Context, resourceGroupName, vmName string) error {
	c.calls = append(c.calls, fmt.Sprintf("restart:%s/%s", resourceGroupName, vmName))
	return c.err
}

func (c *recordingAzureVMClient) PowerOff(_ context.Context, resourceGroupName, vmName string) error {
	c.calls = append(c.calls, fmt.Sprintf("powerOff:%s/%s", resourceGroupName, vmName))
	return c.err
}

func (c *recordingAzureVMClient) Start(_ context.Context, resourceGroupName, vmName string) error {
	c.calls = append(c.calls, fmt.Sprintf("start:%s/%s", resourceGroupName, vmName))
	return c.err
}
