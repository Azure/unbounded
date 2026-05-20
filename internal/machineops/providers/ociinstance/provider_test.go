// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ociinstance

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

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
		{name: "hard reboot", operation: unboundedv1alpha3.OperationHostReboot, wantCall: "RESET:ocid1.instance.oc1.test"},
		{name: "power off", operation: unboundedv1alpha3.OperationHostPowerOff, wantCall: "STOP:ocid1.instance.oc1.test"},
		{name: "power on", operation: unboundedv1alpha3.OperationHostPowerOn, wantCall: "START:ocid1.instance.oc1.test"},
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

			_, err := provider.Execute(context.Background(), machineops.OperationRequest{ProviderID: "oci://ocid1.instance.oc1.test", Operation: tt.operation})

			require.NoError(t, err)
			require.Equal(t, []string{tt.wantCall}, client.calls)
		})
	}
}

func TestProviderExecutePassesAuthToClientFactory(t *testing.T) {
	t.Parallel()

	auth := &machineops.OperationAuth{
		Mode: unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin,
		SecretData: map[string]string{
			"tenancyOCID": "tenancy",
			"userOCID":    "user",
			"region":      "us-phoenix-1",
			"fingerprint": "fingerprint",
			"privateKey":  "private-key",
		},
	}
	client := &recordingComputeClient{}
	provider := &Provider{
		NewClientWithAuth: func(gotAuth *machineops.OperationAuth) (computeClient, error) {
			require.Equal(t, auth, gotAuth)
			return client, nil
		},
	}

	_, err := provider.Execute(context.Background(), machineops.OperationRequest{
		ProviderID: "oci://ocid1.instance.oc1.test",
		Operation:  unboundedv1alpha3.OperationHostPowerOn,
		Auth:       auth,
	})

	require.NoError(t, err)
	require.Equal(t, []string{"START:ocid1.instance.oc1.test"}, client.calls)
}

func TestNewComputeClientForAuthValidatesAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		auth    *machineops.OperationAuth
		wantErr string
	}{
		{
			name:    "missing auth",
			wantErr: "OCI auth is required",
		},
		{
			name: "api key missing private key",
			auth: &machineops.OperationAuth{
				Mode: unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin,
				SecretData: map[string]string{
					"tenancyOCID": "tenancy",
					"userOCID":    "user",
					"region":      "us-phoenix-1",
					"fingerprint": "fingerprint",
				},
			},
			wantErr: "privateKey",
		},
		{
			name: "unsupported auth mode",
			auth: &machineops.OperationAuth{
				Mode: unboundedv1alpha3.MachineOperationCredentialAuthWorkloadIdentity,
			},
			wantErr: "unsupported OCI auth mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := &Provider{}
			_, err := provider.newComputeClientForAuth(tt.auth)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestProviderExecuteRequiresProviderID(t *testing.T) {
	t.Parallel()

	provider := &Provider{NewClient: func() (computeClient, error) {
		return &recordingComputeClient{}, nil
	}}

	_, err := provider.Execute(context.Background(), machineops.OperationRequest{Operation: unboundedv1alpha3.OperationHostPowerOn})
	require.Error(t, err)
	require.Contains(t, err.Error(), "providerID is required")
}

func TestProviderExecuteHostReplaceRequiresMachine(t *testing.T) {
	t.Parallel()

	provider := &Provider{NewClient: func() (computeClient, error) {
		return &recordingComputeClient{}, nil
	}}

	require.True(t, provider.Supports(unboundedv1alpha3.OperationHostReplace))
	_, err := provider.Execute(context.Background(), machineops.OperationRequest{ProviderID: "oci://old-instance", Operation: unboundedv1alpha3.OperationHostReplace})
	require.Error(t, err)
	require.Contains(t, err.Error(), "machine is required")
}

func TestProviderExecuteHostReplaceRequiresUserData(t *testing.T) {
	t.Parallel()

	provider := &Provider{NewClient: func() (computeClient, error) {
		return &recordingComputeClient{}, nil
	}}
	machine := &unboundedv1alpha3.Machine{}
	machine.Name = "machine-1"
	machine.Spec.ProviderID = "oci://old-instance"

	_, err := provider.Execute(context.Background(), machineops.OperationRequest{Machine: machine, ProviderID: "oci://old-instance", Operation: unboundedv1alpha3.OperationHostReplace})
	require.Error(t, err)
	require.Contains(t, err.Error(), "replacement user data is required")
}

func TestProviderExecuteRequiresNonEmptyProviderID(t *testing.T) {
	t.Parallel()

	provider := &Provider{NewClient: func() (computeClient, error) {
		return &recordingComputeClient{}, nil
	}}

	_, err := provider.Execute(context.Background(), machineops.OperationRequest{ProviderID: "oci:// ", Operation: unboundedv1alpha3.OperationHostPowerOn})
	require.Error(t, err)
	require.Contains(t, err.Error(), "providerID is required")
}

func TestProviderExecuteReturnsClientError(t *testing.T) {
	t.Parallel()

	provider := &Provider{NewClient: func() (computeClient, error) {
		return nil, fmt.Errorf("boom")
	}}

	_, err := provider.Execute(context.Background(), machineops.OperationRequest{ProviderID: "oci://ocid1.instance.oc1.test", Operation: unboundedv1alpha3.OperationHostPowerOn})
	require.Error(t, err)
	require.Contains(t, err.Error(), "create OCI compute client")
}

func TestProviderExecuteHostReplaceLaunchesDefaultUbuntuReplacement(t *testing.T) {
	t.Parallel()

	client := newReplacementClient()
	provider := &Provider{NewClient: func() (computeClient, error) { return client, nil }}
	machine := &unboundedv1alpha3.Machine{}
	machine.Name = "machine-1"
	machine.Spec.ProviderID = "oci://old-instance"

	result, err := provider.Execute(context.Background(), machineops.OperationRequest{
		Machine:         machine,
		OperationName:   "replace-machine-1",
		OperationUID:    types.UID("operation-uid"),
		ProviderID:      "oci://old-instance",
		Operation:       unboundedv1alpha3.OperationHostReplace,
		Parameters:      map[string]string{parameterSSHAuthorizedKeys: "ssh-rsa debug-key"},
		ReplaceUserData: "#cloud-config\n",
	})

	require.NoError(t, err)
	require.Equal(t, machineops.OperationResult{ProviderID: "oci://new-instance", CleanupProviderID: "oci://old-instance"}, result)
	require.Equal(t, []string{"STOP:old-instance"}, client.calls)
	require.Len(t, client.launches, 1)
	launch := client.launches[0]
	require.Equal(t, "image-new", *launch.SourceDetails.(core.InstanceSourceViaImageDetails).ImageId)
	require.Equal(t, "subnet-1", *launch.CreateVnicDetails.SubnetId)
	require.Equal(t, []string{"nsg-1"}, launch.CreateVnicDetails.NsgIds)
	require.True(t, *launch.CreateVnicDetails.AssignPublicIp)
	require.Nil(t, launch.CreateVnicDetails.PrivateIp)
	require.Nil(t, launch.LaunchOptions)
	require.Contains(t, launch.Metadata["ssh_authorized_keys"], "ssh-rsa debug-key")
	require.Equal(t, "machine-1", launch.FreeformTags[tagMachine])
	require.Equal(t, "replace-machine-1", launch.FreeformTags[tagOperation])
	require.Equal(t, "operation-uid", launch.FreeformTags[tagOperationUID])
	require.Equal(t, "oci://old-instance", launch.FreeformTags[tagOldProviderID])

	for key := range launch.FreeformTags {
		require.NotContains(t, key, "/")
	}

	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("#cloud-config\n")), launch.Metadata["user_data"])
}

func TestProviderExecuteHostReplaceImageIDOverride(t *testing.T) {
	t.Parallel()

	client := newReplacementClient()
	provider := &Provider{NewClient: func() (computeClient, error) { return client, nil }}
	machine := &unboundedv1alpha3.Machine{}
	machine.Name = "machine-1"
	machine.Spec.ProviderID = "oci://old-instance"

	_, err := provider.Execute(context.Background(), machineops.OperationRequest{
		Machine:         machine,
		OperationName:   "replace-machine-1",
		OperationUID:    types.UID("operation-uid"),
		ProviderID:      "oci://old-instance",
		Operation:       unboundedv1alpha3.OperationHostReplace,
		Parameters:      map[string]string{parameterImageID: "custom-image"},
		ReplaceUserData: "#cloud-config\n",
	})

	require.NoError(t, err)
	require.Equal(t, "custom-image", *client.launches[0].SourceDetails.(core.InstanceSourceViaImageDetails).ImageId)
	require.Empty(t, client.listImagesCalls)
}

func TestProviderExecuteHostReplacePreflightsBeforeStop(t *testing.T) {
	t.Parallel()

	client := newReplacementClient()
	client.images = nil
	provider := &Provider{NewClient: func() (computeClient, error) { return client, nil }}
	machine := &unboundedv1alpha3.Machine{}
	machine.Name = "machine-1"
	machine.Spec.ProviderID = "oci://old-instance"

	_, err := provider.Execute(context.Background(), machineops.OperationRequest{
		Machine:         machine,
		OperationName:   "replace-machine-1",
		OperationUID:    types.UID("operation-uid"),
		ProviderID:      "oci://old-instance",
		Operation:       unboundedv1alpha3.OperationHostReplace,
		ReplaceUserData: "#cloud-config\n",
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "no compatible")
	require.Empty(t, client.calls)
	require.Empty(t, client.launches)
}

func TestProviderExecuteHostReplaceWaitsForStoppingInstance(t *testing.T) {
	t.Parallel()

	client := newReplacementClient()
	client.instances[0].LifecycleState = core.InstanceLifecycleStateStopping
	client.stopAfterGet = map[string]int{"old-instance": 2}
	provider := &Provider{NewClient: func() (computeClient, error) { return client, nil }}
	machine := &unboundedv1alpha3.Machine{}
	machine.Name = "machine-1"
	machine.Spec.ProviderID = "oci://old-instance"

	result, err := provider.Execute(context.Background(), machineops.OperationRequest{
		Machine:         machine,
		OperationName:   "replace-machine-1",
		OperationUID:    types.UID("operation-uid"),
		ProviderID:      "oci://old-instance",
		Operation:       unboundedv1alpha3.OperationHostReplace,
		ReplaceUserData: "#cloud-config\n",
	})

	require.NoError(t, err)
	require.Equal(t, machineops.OperationResult{ProviderID: "oci://new-instance", CleanupProviderID: "oci://old-instance"}, result)
	require.Empty(t, client.calls)
	require.Len(t, client.launches, 1)
	require.GreaterOrEqual(t, client.getCalls["old-instance"], 2)
}

func TestProviderExecuteHostReplaceFailsWithDataVolume(t *testing.T) {
	t.Parallel()

	client := newReplacementClient()
	client.volumeAttachments = []core.VolumeAttachment{core.ParavirtualizedVolumeAttachment{LifecycleState: core.VolumeAttachmentLifecycleStateAttached}}
	provider := &Provider{NewClient: func() (computeClient, error) { return client, nil }}
	machine := &unboundedv1alpha3.Machine{}
	machine.Name = "machine-1"
	machine.Spec.ProviderID = "oci://old-instance"

	_, err := provider.Execute(context.Background(), machineops.OperationRequest{
		Machine:         machine,
		OperationName:   "replace-machine-1",
		OperationUID:    types.UID("operation-uid"),
		ProviderID:      "oci://old-instance",
		Operation:       unboundedv1alpha3.OperationHostReplace,
		ReplaceUserData: "#cloud-config\n",
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "does not support preserving attached data volumes")
	require.Empty(t, client.launches)
}

func TestProviderExecuteHostReplaceReusesTaggedReplacement(t *testing.T) {
	t.Parallel()

	client := newReplacementClient()
	client.instances = append(client.instances, core.Instance{
		Id:                 ptrTo("existing-replacement"),
		LifecycleState:     core.InstanceLifecycleStateRunning,
		CompartmentId:      ptrTo("compartment-1"),
		AvailabilityDomain: ptrTo("ad-1"),
		Shape:              ptrTo("VM.Standard.E4.Flex"),
		FreeformTags: map[string]string{
			tagOperationUID:  "operation-uid",
			tagOldProviderID: "oci://old-instance",
		},
	})
	provider := &Provider{NewClient: func() (computeClient, error) { return client, nil }}
	machine := &unboundedv1alpha3.Machine{}
	machine.Name = "machine-1"
	machine.Spec.ProviderID = "oci://old-instance"

	result, err := provider.Execute(context.Background(), machineops.OperationRequest{
		Machine:         machine,
		OperationName:   "replace-machine-1",
		OperationUID:    types.UID("operation-uid"),
		ProviderID:      "oci://old-instance",
		Operation:       unboundedv1alpha3.OperationHostReplace,
		ReplaceUserData: "#cloud-config\n",
	})

	require.NoError(t, err)
	require.Equal(t, "oci://existing-replacement", result.ProviderID)
	require.Empty(t, client.launches)
	require.Empty(t, client.calls)
}

func TestProviderExecuteHostReplaceAfterProviderIDHandoffReturnsCleanup(t *testing.T) {
	t.Parallel()

	client := newReplacementClient()
	client.instances = append(client.instances, core.Instance{
		Id:                 ptrTo("new-instance"),
		LifecycleState:     core.InstanceLifecycleStateRunning,
		CompartmentId:      ptrTo("compartment-1"),
		AvailabilityDomain: ptrTo("ad-1"),
		Shape:              ptrTo("VM.Standard.E4.Flex"),
		FreeformTags: map[string]string{
			tagOperationUID:  "operation-uid",
			tagOldProviderID: "oci://old-instance",
		},
	})
	provider := &Provider{NewClient: func() (computeClient, error) { return client, nil }}
	machine := &unboundedv1alpha3.Machine{}
	machine.Name = "machine-1"
	machine.Spec.ProviderID = "oci://new-instance"

	result, err := provider.Execute(context.Background(), machineops.OperationRequest{
		Machine:         machine,
		OperationName:   "replace-machine-1",
		OperationUID:    types.UID("operation-uid"),
		ProviderID:      "oci://new-instance",
		Operation:       unboundedv1alpha3.OperationHostReplace,
		ReplaceUserData: "#cloud-config\n",
	})

	require.NoError(t, err)
	require.Equal(t, machineops.OperationResult{ProviderID: "oci://new-instance", CleanupProviderID: "oci://old-instance"}, result)
	require.Empty(t, client.launches)
	require.Empty(t, client.calls)
}

func TestProviderCleanupTerminatesOldInstance(t *testing.T) {
	t.Parallel()

	client := newReplacementClient()
	provider := &Provider{NewClient: func() (computeClient, error) { return client, nil }}
	machine := &unboundedv1alpha3.Machine{}
	machine.Name = "machine-1"
	machine.Spec.ProviderID = "oci://new-instance"

	err := provider.Cleanup(context.Background(), machineops.OperationRequest{
		Machine:       machine,
		OperationName: "replace-machine-1",
		OperationUID:  types.UID("operation-uid"),
	}, machineops.OperationResult{CleanupProviderID: "oci://old-instance"})

	require.NoError(t, err)
	require.Equal(t, []string{"old-instance"}, client.terminated)
}

func TestActionForOperation(t *testing.T) {
	t.Parallel()

	_, err := actionForOperation(unboundedv1alpha3.OperationNodeReboot)
	require.Error(t, err)
}

type recordingComputeClient struct {
	calls             []string
	err               error
	instances         []core.Instance
	vnics             map[string]core.Vnic
	volumeAttachments []core.VolumeAttachment
	images            []core.Image
	launches          []core.LaunchInstanceDetails
	terminated        []string
	listImagesCalls   []string
	getCalls          map[string]int
	stopAfterGet      map[string]int
}

func (c *recordingComputeClient) InstanceAction(_ context.Context, instanceID, action string) error {
	c.calls = append(c.calls, fmt.Sprintf("%s:%s", action, instanceID))
	for i := range c.instances {
		if c.instances[i].Id != nil && *c.instances[i].Id == instanceID && action == instanceActionStop {
			c.instances[i].LifecycleState = core.InstanceLifecycleStateStopped
		}
	}

	return c.err
}

func (c *recordingComputeClient) GetInstance(_ context.Context, instanceID string) (core.Instance, error) {
	if c.getCalls == nil {
		c.getCalls = map[string]int{}
	}

	c.getCalls[instanceID]++
	for i, instance := range c.instances {
		if instance.Id != nil && *instance.Id == instanceID {
			if stopAt := c.stopAfterGet[instanceID]; stopAt > 0 && c.getCalls[instanceID] >= stopAt {
				c.instances[i].LifecycleState = core.InstanceLifecycleStateStopped
				return c.instances[i], nil
			}

			return instance, nil
		}
	}

	return core.Instance{}, fmt.Errorf("not found")
}

func (c *recordingComputeClient) ListInstances(_ context.Context, _, _ string) ([]core.Instance, error) {
	return c.instances, nil
}

func (c *recordingComputeClient) LaunchInstance(_ context.Context, details core.LaunchInstanceDetails, _ string) (core.Instance, error) {
	c.launches = append(c.launches, details)
	replacement := core.Instance{
		Id:                 ptrTo("new-instance"),
		LifecycleState:     core.InstanceLifecycleStateRunning,
		CompartmentId:      details.CompartmentId,
		AvailabilityDomain: details.AvailabilityDomain,
		Shape:              details.Shape,
		FreeformTags:       details.FreeformTags,
	}
	c.instances = append(c.instances, replacement)

	return replacement, nil
}

func (c *recordingComputeClient) TerminateInstance(_ context.Context, instanceID, _ string) error {
	c.terminated = append(c.terminated, instanceID)
	return nil
}

func (c *recordingComputeClient) ListImages(_ context.Context, compartmentID, shape string) ([]core.Image, error) {
	c.listImagesCalls = append(c.listImagesCalls, fmt.Sprintf("%s:%s", compartmentID, shape))
	return c.images, nil
}

func (c *recordingComputeClient) ListVnicAttachments(_ context.Context, _, instanceID string) ([]core.VnicAttachment, error) {
	return []core.VnicAttachment{{InstanceId: &instanceID, VnicId: ptrTo("vnic-1"), LifecycleState: core.VnicAttachmentLifecycleStateAttached}}, nil
}

func (c *recordingComputeClient) GetVnic(_ context.Context, vnicID string) (core.Vnic, error) {
	vnic, ok := c.vnics[vnicID]
	if !ok {
		return core.Vnic{}, fmt.Errorf("not found")
	}

	return vnic, nil
}

func (c *recordingComputeClient) ListVolumeAttachments(_ context.Context, _, _ string) ([]core.VolumeAttachment, error) {
	return c.volumeAttachments, nil
}

func newReplacementClient() *recordingComputeClient {
	return &recordingComputeClient{
		instances: []core.Instance{{
			Id:                 ptrTo("old-instance"),
			LifecycleState:     core.InstanceLifecycleStateRunning,
			CompartmentId:      ptrTo("compartment-1"),
			AvailabilityDomain: ptrTo("ad-1"),
			Shape:              ptrTo("VM.Standard.E4.Flex"),
			DisplayName:        ptrTo("machine-1"),
			FreeformTags:       map[string]string{"existing": "tag"},
		}},
		vnics: map[string]core.Vnic{
			"vnic-1": {
				Id:                  ptrTo("vnic-1"),
				IsPrimary:           ptrTo(true),
				SubnetId:            ptrTo("subnet-1"),
				NsgIds:              []string{"nsg-1"},
				SkipSourceDestCheck: ptrTo(true),
			},
		},
		images: []core.Image{
			{
				Id:                     ptrTo("image-old"),
				LifecycleState:         core.ImageLifecycleStateAvailable,
				OperatingSystem:        ptrTo(defaultUbuntuOS),
				OperatingSystemVersion: ptrTo(defaultUbuntuOSVersion),
				TimeCreated:            &common.SDKTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			},
			{
				Id:                     ptrTo("image-new"),
				LifecycleState:         core.ImageLifecycleStateAvailable,
				OperatingSystem:        ptrTo(defaultUbuntuOS),
				OperatingSystemVersion: ptrTo(defaultUbuntuOSVersion),
				TimeCreated:            &common.SDKTime{Time: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
	}
}
