// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ociinstance

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
)

func TestProviderExecute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		operation unboundedv1alpha3.OperationKind
		wantCall  string
	}{
		{name: "hard reboot", operation: unboundedv1alpha3.OperationHardReboot, wantCall: "RESET:ocid1.instance.oc1.test"},
		{name: "power off", operation: unboundedv1alpha3.OperationPowerOff, wantCall: "STOP:ocid1.instance.oc1.test"},
		{name: "power on", operation: unboundedv1alpha3.OperationPowerOn, wantCall: "START:ocid1.instance.oc1.test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &recordingComputeClient{}
			provider := &Provider{
				NewClient: func() (computeClient, error) {
					return client, nil
				},
			}

			err := provider.Execute(context.Background(), machineops.OperationRequest{ProviderID: "oci://ocid1.instance.oc1.test", Operation: tt.operation})

			require.NoError(t, err)
			require.Equal(t, []string{tt.wantCall}, client.calls)
		})
	}
}

func TestProviderExecuteRequiresProviderID(t *testing.T) {
	t.Parallel()

	provider := &Provider{NewClient: func() (computeClient, error) {
		return &recordingComputeClient{}, nil
	}}

	err := provider.Execute(context.Background(), machineops.OperationRequest{Operation: unboundedv1alpha3.OperationPowerOn})
	require.Error(t, err)
	require.Contains(t, err.Error(), "providerID is required")
}

func TestProviderExecuteRequiresNonEmptyProviderID(t *testing.T) {
	t.Parallel()

	provider := &Provider{NewClient: func() (computeClient, error) {
		return &recordingComputeClient{}, nil
	}}

	err := provider.Execute(context.Background(), machineops.OperationRequest{ProviderID: "oci:// ", Operation: unboundedv1alpha3.OperationPowerOn})
	require.Error(t, err)
	require.Contains(t, err.Error(), "providerID is required")
}

func TestProviderExecuteReturnsClientError(t *testing.T) {
	t.Parallel()

	provider := &Provider{NewClient: func() (computeClient, error) {
		return nil, fmt.Errorf("boom")
	}}

	err := provider.Execute(context.Background(), machineops.OperationRequest{ProviderID: "oci://ocid1.instance.oc1.test", Operation: unboundedv1alpha3.OperationPowerOn})
	require.Error(t, err)
	require.Contains(t, err.Error(), "create OCI compute client")
}

func TestNewDefaultComputeClientValidatesAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  string
		auth    string
		wantErr string
	}{
		{name: "unsupported auth", config: "/not-used", auth: "instance_principal", wantErr: "unsupported OCI auth mode"},
		{name: "security token requires config file", auth: AuthSecurityToken, wantErr: "requires --oci-config-file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := &Provider{ConfigFile: tt.config, Auth: tt.auth}
			_, err := provider.newDefaultComputeClient()
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestActionForOperation(t *testing.T) {
	t.Parallel()

	_, err := actionForOperation(unboundedv1alpha3.OperationSoftReboot)
	require.Error(t, err)
}

type recordingComputeClient struct {
	calls []string
	err   error
}

func (c *recordingComputeClient) InstanceAction(_ context.Context, instanceID, action string) error {
	c.calls = append(c.calls, fmt.Sprintf("%s:%s", action, instanceID))
	return c.err
}
