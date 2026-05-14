// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ociinstance

import (
	"context"
	"fmt"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
)

func (p *Provider) newDefaultComputeClient() (computeClient, error) {
	// The controller defaults to long-lived API keys but keeps security-token auth
	// for local/e2e workflows that mount an OCI CLI config file.
	profile := strings.TrimSpace(p.ConfigProfile)
	if profile == "" {
		profile = defaultConfigProfile
	}
	auth := strings.TrimSpace(p.Auth)
	if auth == "" {
		auth = AuthAPIKey
	}
	if auth != AuthAPIKey && auth != AuthSecurityToken {
		return nil, fmt.Errorf("unsupported OCI auth mode %q", p.Auth)
	}

	provider := common.DefaultConfigProvider()
	if strings.TrimSpace(p.ConfigFile) != "" {
		switch auth {
		case AuthAPIKey:
			provider = common.CustomProfileConfigProvider(p.ConfigFile, profile)
		case AuthSecurityToken:
			provider = common.CustomProfileSessionTokenConfigProvider(p.ConfigFile, profile)
		}
	} else if auth != AuthAPIKey {
		return nil, fmt.Errorf("oci auth mode %q requires --oci-config-file", auth)
	}

	computeClient, err := core.NewComputeClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, fmt.Errorf("create compute client: %w", err)
	}
	networkClient, err := core.NewVirtualNetworkClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, fmt.Errorf("create virtual network client: %w", err)
	}

	return &ociComputeClient{compute: computeClient, network: networkClient}, nil
}

type ociComputeClient struct {
	compute core.ComputeClient
	network core.VirtualNetworkClient
}

// ociComputeClient is intentionally a thin SDK adapter. It keeps OCI paging and
// request structs out of Host Operation orchestration code.

func (c *ociComputeClient) InstanceAction(ctx context.Context, instanceID, action string) error {
	_, err := c.compute.InstanceAction(ctx, core.InstanceActionRequest{
		InstanceId: &instanceID,
		Action:     core.InstanceActionActionEnum(action),
	})
	if err != nil {
		return fmt.Errorf("run OCI instance action %s on %s: %w", action, instanceID, err)
	}

	return nil
}

func (c *ociComputeClient) GetInstance(ctx context.Context, instanceID string) (core.Instance, error) {
	response, err := c.compute.GetInstance(ctx, core.GetInstanceRequest{InstanceId: &instanceID})
	if err != nil {
		return core.Instance{}, err
	}

	return response.Instance, nil
}

func (c *ociComputeClient) ListInstances(ctx context.Context, compartmentID, availabilityDomain string) ([]core.Instance, error) {
	return listOCI(ctx, func(ctx context.Context, page *string) ([]core.Instance, *string, error) {
		response, err := c.compute.ListInstances(ctx, core.ListInstancesRequest{
			CompartmentId:      &compartmentID,
			AvailabilityDomain: &availabilityDomain,
			Page:               page,
		})
		return response.Items, response.OpcNextPage, err
	})
}

func (c *ociComputeClient) LaunchInstance(ctx context.Context, details core.LaunchInstanceDetails, retryToken string) (core.Instance, error) {
	response, err := c.compute.LaunchInstance(ctx, core.LaunchInstanceRequest{
		LaunchInstanceDetails: details,
		OpcRetryToken:         &retryToken,
	})
	if err != nil {
		return core.Instance{}, err
	}

	return response.Instance, nil
}

func (c *ociComputeClient) TerminateInstance(ctx context.Context, instanceID, retryToken string) error {
	preserveBootVolume := false
	_, err := c.compute.TerminateInstance(ctx, core.TerminateInstanceRequest{
		InstanceId:         &instanceID,
		PreserveBootVolume: &preserveBootVolume,
		OpcRequestId:       &retryToken,
	})
	if err != nil {
		return fmt.Errorf("terminate OCI instance %s: %w", instanceID, err)
	}

	return nil
}

func (c *ociComputeClient) ListImages(ctx context.Context, compartmentID, shape string) ([]core.Image, error) {
	return listOCI(ctx, func(ctx context.Context, page *string) ([]core.Image, *string, error) {
		response, err := c.compute.ListImages(ctx, core.ListImagesRequest{
			CompartmentId:          &compartmentID,
			OperatingSystem:        ptrTo(defaultUbuntuOS),
			OperatingSystemVersion: ptrTo(defaultUbuntuOSVersion),
			Shape:                  &shape,
			LifecycleState:         core.ImageLifecycleStateAvailable,
			SortBy:                 core.ListImagesSortByTimecreated,
			SortOrder:              core.ListImagesSortOrderDesc,
			Page:                   page,
		})
		return response.Items, response.OpcNextPage, err
	})
}

func (c *ociComputeClient) ListVnicAttachments(ctx context.Context, compartmentID, instanceID string) ([]core.VnicAttachment, error) {
	return listOCI(ctx, func(ctx context.Context, page *string) ([]core.VnicAttachment, *string, error) {
		response, err := c.compute.ListVnicAttachments(ctx, core.ListVnicAttachmentsRequest{
			CompartmentId: &compartmentID,
			InstanceId:    &instanceID,
			Page:          page,
		})
		return response.Items, response.OpcNextPage, err
	})
}

func (c *ociComputeClient) GetVnic(ctx context.Context, vnicID string) (core.Vnic, error) {
	response, err := c.network.GetVnic(ctx, core.GetVnicRequest{VnicId: &vnicID})
	if err != nil {
		return core.Vnic{}, err
	}

	return response.Vnic, nil
}

func (c *ociComputeClient) ListVolumeAttachments(ctx context.Context, compartmentID, instanceID string) ([]core.VolumeAttachment, error) {
	return listOCI(ctx, func(ctx context.Context, page *string) ([]core.VolumeAttachment, *string, error) {
		response, err := c.compute.ListVolumeAttachments(ctx, core.ListVolumeAttachmentsRequest{
			CompartmentId: &compartmentID,
			InstanceId:    &instanceID,
			Page:          page,
		})
		return response.Items, response.OpcNextPage, err
	})
}

func listOCI[T any](ctx context.Context, listPage func(context.Context, *string) ([]T, *string, error)) ([]T, error) {
	var items []T
	var page *string
	for {
		pageItems, nextPage, err := listPage(ctx, page)
		if err != nil {
			return nil, err
		}

		items = append(items, pageItems...)
		if nextPage == nil {
			return items, nil
		}
		page = nextPage
	}
}
