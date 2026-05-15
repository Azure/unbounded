// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ociinstance

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/Azure/unbounded/internal/machineops"
)

func replacementSourceInstance(ctx context.Context, client computeClient, instanceID string) (core.Instance, error) {
	// These fields are required to recreate the host in the same OCI placement
	// and shape without making later helpers handle nil pointer cases.
	instance, err := client.GetInstance(ctx, instanceID)
	if err != nil {
		return core.Instance{}, fmt.Errorf("get OCI instance %s: %w", instanceID, err)
	}
	if instance.CompartmentId == nil || *instance.CompartmentId == "" {
		return core.Instance{}, fmt.Errorf("OCI instance %s has no compartment ID", instanceID)
	}
	if instance.AvailabilityDomain == nil || *instance.AvailabilityDomain == "" {
		return core.Instance{}, fmt.Errorf("OCI instance %s has no availability domain", instanceID)
	}
	if instance.Shape == nil || *instance.Shape == "" {
		return core.Instance{}, fmt.Errorf("OCI instance %s has no shape", instanceID)
	}

	return instance, nil
}

func rejectAttachedDataVolumes(ctx context.Context, client computeClient, instance core.Instance) error {
	// v1 replacement intentionally handles only disposable root disks. Data volume
	// preservation needs explicit detach/attach semantics before it is safe.
	volumeAttachments, err := client.ListVolumeAttachments(ctx, *instance.CompartmentId, *instance.Id)
	if err != nil {
		return fmt.Errorf("list OCI volume attachments for %s: %w", *instance.Id, err)
	}
	if hasActiveVolumeAttachment(volumeAttachments) {
		return fmt.Errorf("OCI HostReplace does not support preserving attached data volumes")
	}

	return nil
}

func primaryVNIC(ctx context.Context, client computeClient, instance core.Instance) (core.Vnic, error) {
	// OCI launch uses CreateVnicDetails, so we copy only the network placement
	// fields needed for the replacement rather than cloning every VNIC property.
	attachments, err := client.ListVnicAttachments(ctx, *instance.CompartmentId, *instance.Id)
	if err != nil {
		return core.Vnic{}, fmt.Errorf("list OCI VNIC attachments for %s: %w", *instance.Id, err)
	}

	for _, attachment := range attachments {
		if attachment.LifecycleState == core.VnicAttachmentLifecycleStateDetached || attachment.VnicId == nil || *attachment.VnicId == "" {
			continue
		}

		vnic, err := client.GetVnic(ctx, *attachment.VnicId)
		if err != nil {
			return core.Vnic{}, fmt.Errorf("get OCI VNIC %s: %w", *attachment.VnicId, err)
		}
		if vnic.IsPrimary != nil && *vnic.IsPrimary {
			if vnic.SubnetId == nil || *vnic.SubnetId == "" {
				return core.Vnic{}, fmt.Errorf("OCI primary VNIC %s has no subnet ID", *attachment.VnicId)
			}

			return vnic, nil
		}
	}

	return core.Vnic{}, fmt.Errorf("OCI instance %s has no primary VNIC", *instance.Id)
}

func (p *Provider) findExistingReplacement(ctx context.Context, client computeClient, oldInstance core.Instance, request machineops.OperationRequest) (core.Instance, bool, error) {
	// LaunchInstance retry tokens are not enough after controller restarts. Tags
	// give us an OCI-visible idempotency record for replacement lookup.
	instances, err := client.ListInstances(ctx, *oldInstance.CompartmentId, *oldInstance.AvailabilityDomain)
	if err != nil {
		return core.Instance{}, false, fmt.Errorf("list OCI instances for replacement lookup: %w", err)
	}

	oldProviderID := providerIDPrefix + *oldInstance.Id
	operationUID := string(request.OperationUID)
	for _, instance := range instances {
		if isTerminalInstance(instance) {
			continue
		}
		if instance.FreeformTags[tagOperationUID] == operationUID && instance.FreeformTags[tagOldProviderID] == oldProviderID {
			return instance, true, nil
		}
	}

	return core.Instance{}, false, nil
}

func (p *Provider) buildReplacementLaunchDetails(ctx context.Context, client computeClient, oldInstance core.Instance, primaryVNIC core.Vnic, request machineops.OperationRequest) (core.LaunchInstanceDetails, error) {
	imageID, err := p.resolveImageID(ctx, client, oldInstance, request.Parameters)
	if err != nil {
		return core.LaunchInstanceDetails{}, err
	}

	return buildReplacementLaunchDetails(oldInstance, primaryVNIC, imageID, request)
}

func (p *Provider) resolveImageID(ctx context.Context, client computeClient, oldInstance core.Instance, parameters map[string]string) (string, error) {
	// Operators can pin an image for controlled rollout; otherwise choose the
	// newest Ubuntu image OCI reports as compatible with the source shape.
	if imageID := strings.TrimSpace(parameters[parameterImageID]); imageID != "" {
		return imageID, nil
	}

	images, err := client.ListImages(ctx, *oldInstance.CompartmentId, *oldInstance.Shape)
	if err != nil {
		return "", fmt.Errorf("list OCI images: %w", err)
	}
	sort.SliceStable(images, func(i, j int) bool {
		if images[i].TimeCreated == nil {
			return false
		}
		if images[j].TimeCreated == nil {
			return true
		}

		return images[i].TimeCreated.After(images[j].TimeCreated.Time)
	})

	for _, image := range images {
		if image.Id == nil || image.LifecycleState != core.ImageLifecycleStateAvailable {
			continue
		}
		if image.OperatingSystem == nil || image.OperatingSystemVersion == nil {
			continue
		}
		if *image.OperatingSystem == defaultUbuntuOS && *image.OperatingSystemVersion == defaultUbuntuOSVersion {
			return *image.Id, nil
		}
	}

	return "", fmt.Errorf("no compatible %s %s image found for shape %s; set spec.parameters.%s", defaultUbuntuOS, defaultUbuntuOSVersion, *oldInstance.Shape, parameterImageID)
}

func buildReplacementLaunchDetails(oldInstance core.Instance, primaryVNIC core.Vnic, imageID string, request machineops.OperationRequest) (core.LaunchInstanceDetails, error) {
	metadata, err := replacementMetadata(oldInstance, request)
	if err != nil {
		return core.LaunchInstanceDetails{}, err
	}

	details := core.LaunchInstanceDetails{
		AvailabilityDomain:      oldInstance.AvailabilityDomain,
		CompartmentId:           oldInstance.CompartmentId,
		CapacityReservationId:   oldInstance.CapacityReservationId,
		DefinedTags:             oldInstance.DefinedTags,
		DisplayName:             oldInstance.DisplayName,
		ExtendedMetadata:        oldInstance.ExtendedMetadata,
		FaultDomain:             oldInstance.FaultDomain,
		ClusterPlacementGroupId: oldInstance.ClusterPlacementGroupId,
		FreeformTags:            replacementTags(oldInstance, request),
		IpxeScript:              oldInstance.IpxeScript,
		InstanceOptions:         oldInstance.InstanceOptions,
		Metadata:                metadata,
		Shape:                   oldInstance.Shape,
		SourceDetails: core.InstanceSourceViaImageDetails{
			ImageId: &imageID,
		},
		CreateVnicDetails: replacementVNIC(primaryVNIC),
	}
	copyOptionalLaunchSettings(&details, oldInstance)

	return details, nil
}

func replacementMetadata(oldInstance core.Instance, request machineops.OperationRequest) (map[string]string, error) {
	// OCI cannot update launch user_data in place, so HostReplace recreates the
	// instance with fresh bootstrap metadata and a new providerID.
	metadata := copyStringMap(oldInstance.Metadata)
	if extraKeys := strings.TrimSpace(request.Parameters[parameterSSHAuthorizedKeys]); extraKeys != "" {
		metadata["ssh_authorized_keys"] = strings.TrimSpace(metadata["ssh_authorized_keys"] + "\n" + extraKeys)
	}
	metadata["user_data"] = base64.StdEncoding.EncodeToString([]byte(request.ReplaceUserData))
	if metadataSize(metadata, oldInstance.ExtendedMetadata) > ociMetadataMaxBytes {
		return nil, fmt.Errorf("replacement metadata exceeds OCI limit of %d bytes", ociMetadataMaxBytes)
	}

	return metadata, nil
}

func replacementTags(oldInstance core.Instance, request machineops.OperationRequest) map[string]string {
	freeformTags := copyStringMap(oldInstance.FreeformTags)
	// OCI freeform tag keys cannot use slash-delimited Kubernetes-style names.
	// These tags are also the restart-safe replacement lookup keys.
	freeformTags[tagMachine] = request.Machine.Name
	freeformTags[tagOperation] = request.OperationName
	freeformTags[tagOperationUID] = string(request.OperationUID)
	freeformTags[tagOldProviderID] = request.ProviderID

	return freeformTags
}

func replacementVNIC(primaryVNIC core.Vnic) *core.CreateVnicDetails {
	return &core.CreateVnicDetails{
		AssignPublicIp:      ptrTo(true),
		AssignIpv6Ip:        ptrTo(false),
		NsgIds:              primaryVNIC.NsgIds,
		SkipSourceDestCheck: primaryVNIC.SkipSourceDestCheck,
		SubnetId:            primaryVNIC.SubnetId,
	}
}

func copyOptionalLaunchSettings(details *core.LaunchInstanceDetails, oldInstance core.Instance) {
	if oldInstance.ShapeConfig != nil {
		details.ShapeConfig = &core.LaunchInstanceShapeConfigDetails{
			Ocpus:       oldInstance.ShapeConfig.Ocpus,
			Vcpus:       oldInstance.ShapeConfig.Vcpus,
			MemoryInGBs: oldInstance.ShapeConfig.MemoryInGBs,
		}
	}
	if oldInstance.AvailabilityConfig != nil {
		details.AvailabilityConfig = &core.LaunchInstanceAvailabilityConfigDetails{
			IsLiveMigrationPreferred: oldInstance.AvailabilityConfig.IsLiveMigrationPreferred,
		}
		if oldInstance.AvailabilityConfig.RecoveryAction != "" {
			details.AvailabilityConfig.RecoveryAction = core.LaunchInstanceAvailabilityConfigDetailsRecoveryActionEnum(oldInstance.AvailabilityConfig.RecoveryAction)
		}
	}
	if oldInstance.AgentConfig != nil {
		details.AgentConfig = &core.LaunchInstanceAgentConfigDetails{
			IsMonitoringDisabled:  oldInstance.AgentConfig.IsMonitoringDisabled,
			IsManagementDisabled:  oldInstance.AgentConfig.IsManagementDisabled,
			AreAllPluginsDisabled: oldInstance.AgentConfig.AreAllPluginsDisabled,
			PluginsConfig:         oldInstance.AgentConfig.PluginsConfig,
		}
	}
}

func hasActiveVolumeAttachment(attachments []core.VolumeAttachment) bool {
	for _, attachment := range attachments {
		switch attachment.GetLifecycleState() {
		case core.VolumeAttachmentLifecycleStateAttached, core.VolumeAttachmentLifecycleStateAttaching:
			return true
		}
	}

	return false
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+4)
	for k, v := range in {
		out[k] = v
	}

	return out
}

func metadataSize(metadata map[string]string, extended map[string]interface{}) int {
	total := 0
	for k, v := range metadata {
		total += len(k) + len(v)
	}
	for k := range extended {
		total += len(k)
	}

	return total
}

func ptrTo[T any](value T) *T { return &value }
