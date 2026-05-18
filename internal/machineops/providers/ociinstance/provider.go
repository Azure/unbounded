// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ociinstance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/core"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
)

const (
	defaultConfigProfile    = "DEFAULT"
	defaultUbuntuOS         = "Canonical Ubuntu"
	defaultUbuntuOSVersion  = "24.04"
	ociMetadataMaxBytes     = 32000
	replacementPollInterval = 15 * time.Second
	replacementPollTimeout  = 20 * time.Minute
)

const (
	// AuthAPIKey uses the standard OCI config-file API key fields.
	AuthAPIKey = "api_key"
	// AuthSecurityToken uses OCI CLI session-token credentials.
	AuthSecurityToken = "security_token"
)

const (
	instanceActionReset = "RESET"
	instanceActionStart = "START"
	instanceActionStop  = "STOP"
)

const (
	parameterImageID           = "imageID"
	parameterSSHAuthorizedKeys = "sshAuthorizedKeys"

	tagMachine       = "unbounded_machine"
	tagOperation     = "unbounded_machine_operation"
	tagOperationUID  = "unbounded_machine_operation_uid"
	tagOldProviderID = "unbounded_old_provider_id"
	retryTokenPrefix = "unbounded-machine-op"
	providerIDPrefix = "oci://"
)

// computeClient is the test seam for OCI calls. The provider logic stays in
// terms of Machine operations while the SDK adapter owns pagination and request
// types.
type computeClient interface {
	InstanceAction(ctx context.Context, instanceID, action string) error
	GetInstance(ctx context.Context, instanceID string) (core.Instance, error)
	ListInstances(ctx context.Context, compartmentID, availabilityDomain string) ([]core.Instance, error)
	LaunchInstance(ctx context.Context, details core.LaunchInstanceDetails, retryToken string) (core.Instance, error)
	TerminateInstance(ctx context.Context, instanceID, retryToken string) error
	ListImages(ctx context.Context, compartmentID, shape string) ([]core.Image, error)
	ListVnicAttachments(ctx context.Context, compartmentID, instanceID string) ([]core.VnicAttachment, error)
	GetVnic(ctx context.Context, vnicID string) (core.Vnic, error)
	ListVolumeAttachments(ctx context.Context, compartmentID, instanceID string) ([]core.VolumeAttachment, error)
}

type computeClientFactory func() (computeClient, error)

// Provider executes operations against Oracle Cloud Infrastructure instances.
type Provider struct {
	ConfigFile    string
	ConfigProfile string
	Auth          string
	NewClient     computeClientFactory
}

func (p *Provider) Name() string {
	return unboundedv1alpha3.ExternalProviderOCIInstance
}

func (p *Provider) Supports(operation unboundedv1alpha3.OperationKind) bool {
	switch operation {
	case unboundedv1alpha3.OperationHostReboot,
		unboundedv1alpha3.OperationHostPowerOff,
		unboundedv1alpha3.OperationHostPowerOn,
		unboundedv1alpha3.OperationHostReplace:
		return true
	default:
		return false
	}
}

func (p *Provider) Execute(ctx context.Context, request machineops.OperationRequest) (machineops.OperationResult, error) {
	instanceID, err := parseOCIInstanceProviderID(request.ProviderID)
	if err != nil {
		return machineops.OperationResult{}, err
	}

	client, err := p.client()
	if err != nil {
		return machineops.OperationResult{}, err
	}

	if request.Operation == unboundedv1alpha3.OperationHostReplace {
		return p.executeReplace(ctx, client, instanceID, request)
	}

	action, err := actionForOperation(request.Operation)
	if err != nil {
		return machineops.OperationResult{}, err
	}

	return machineops.OperationResult{}, client.InstanceAction(ctx, instanceID, action)
}

func (p *Provider) Cleanup(ctx context.Context, request machineops.OperationRequest, result machineops.OperationResult) error {
	if result.CleanupProviderID == "" {
		return nil
	}

	instanceID, err := parseOCIInstanceProviderID(result.CleanupProviderID)
	if err != nil {
		return err
	}

	if request.Machine != nil {
		currentInstanceID, err := parseOCIInstanceProviderID(request.Machine.Spec.ProviderID)
		if err == nil && currentInstanceID == instanceID {
			// ProviderID handoff must happen before old-instance cleanup; otherwise a
			// retry could delete the replacement that the Machine still references.
			return fmt.Errorf("refusing to terminate cleanup target %s because Machine still points to it", result.CleanupProviderID)
		}
	}

	client, err := p.client()
	if err != nil {
		return err
	}

	instance, err := client.GetInstance(ctx, instanceID)
	if err == nil && isTerminalInstance(instance) {
		return nil
	}

	if err != nil && strings.Contains(strings.ToLower(err.Error()), "notfound") {
		return nil
	}

	return client.TerminateInstance(ctx, instanceID, retryToken(request, "terminate-old"))
}

func (p *Provider) client() (computeClient, error) {
	newClient := p.NewClient
	if newClient == nil {
		newClient = p.newDefaultComputeClient
	}

	client, err := newClient()
	if err != nil {
		return nil, fmt.Errorf("create OCI compute client: %w", err)
	}

	return client, nil
}

func actionForOperation(operation unboundedv1alpha3.OperationKind) (string, error) {
	switch operation {
	case unboundedv1alpha3.OperationHostReboot:
		return instanceActionReset, nil
	case unboundedv1alpha3.OperationHostPowerOff:
		return instanceActionStop, nil
	case unboundedv1alpha3.OperationHostPowerOn:
		return instanceActionStart, nil
	default:
		return "", fmt.Errorf("unsupported OCI instance operation %q", operation)
	}
}

func parseOCIInstanceProviderID(providerID string) (string, error) {
	instanceID := strings.TrimSpace(strings.TrimPrefix(providerID, providerIDPrefix))
	if instanceID == "" {
		return "", fmt.Errorf("oci providerID is required")
	}

	return instanceID, nil
}
