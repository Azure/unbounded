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
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

const (
	ociMetadataMaxBytes     = 32000
	replacementPollInterval = 15 * time.Second
	replacementPollTimeout  = 20 * time.Minute
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
	ListVnicAttachments(ctx context.Context, compartmentID, instanceID string) ([]core.VnicAttachment, error)
	GetVnic(ctx context.Context, vnicID string) (core.Vnic, error)
	ListVolumeAttachments(ctx context.Context, compartmentID, instanceID string) ([]core.VolumeAttachment, error)
}

type computeClientFactory func() (computeClient, error)

type computeClientFactoryWithAuth func(auth *machineops.OperationAuth) (computeClient, error)

// Provider executes operations against Oracle Cloud Infrastructure instances.
type Provider struct {
	NewClient         computeClientFactory
	NewClientWithAuth computeClientFactoryWithAuth
}

func (p *Provider) Name() string {
	return unboundedv1alpha3.ExternalProviderOCIInstance
}

// Registration returns this OCI adapter's MachineOperation lifecycle
// registration.
func (p *Provider) Registration() (*publicmachineops.Provider, error) {
	return publicmachineops.NewProvider(
		p.Name(),
		publicmachineops.WithImmediateOperation(unboundedv1alpha3.OperationHostReboot, p.Execute),
		publicmachineops.WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOff, p.Execute),
		publicmachineops.WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOn, p.Execute),
		publicmachineops.WithImmediateOperation(
			unboundedv1alpha3.OperationHostReplace,
			p.Execute,
			publicmachineops.ReplaySafe(),
			publicmachineops.RequiresReplaceUserData(),
			publicmachineops.WithCleanup(p.Cleanup),
		),
	)
}

func (p *Provider) Execute(ctx context.Context, request machineops.OperationRequest) (machineops.OperationResult, error) {
	instanceID, err := parseOCIInstanceProviderID(request.ProviderID)
	if err != nil {
		return machineops.OperationResult{}, fmt.Errorf("parse OCI providerID: %w", err)
	}

	client, err := p.client(request.Auth)
	if err != nil {
		return machineops.OperationResult{}, err
	}

	if request.Operation == unboundedv1alpha3.OperationHostReplace {
		result, err := p.executeReplace(ctx, client, instanceID, request)
		if err != nil {
			return machineops.OperationResult{}, fmt.Errorf("replace OCI instance %s: %w", instanceID, err)
		}

		return result, nil
	}

	action, err := actionForOperation(request.Operation)
	if err != nil {
		return machineops.OperationResult{}, fmt.Errorf("resolve OCI action for %s: %w", request.Operation, err)
	}

	if err := client.InstanceAction(ctx, instanceID, action); err != nil {
		return machineops.OperationResult{}, fmt.Errorf("run OCI %s for %s: %w", action, instanceID, err)
	}

	return machineops.OperationResult{}, nil
}

func (p *Provider) Cleanup(ctx context.Context, request machineops.OperationRequest, result machineops.OperationResult) error {
	if result.CleanupProviderID == "" {
		return nil
	}

	instanceID, err := parseOCIInstanceProviderID(result.CleanupProviderID)
	if err != nil {
		return fmt.Errorf("parse cleanup OCI providerID: %w", err)
	}

	if result.ProviderID != "" && result.CleanupProviderID == result.ProviderID {
		return fmt.Errorf("refusing to terminate cleanup target %s because it is the replacement provider ID", result.CleanupProviderID)
	}

	client, err := p.client(request.Auth)
	if err != nil {
		return fmt.Errorf("create OCI cleanup client: %w", err)
	}

	instance, err := client.GetInstance(ctx, instanceID)
	if err == nil && isTerminalInstance(instance) {
		return nil
	}

	if err != nil && strings.Contains(strings.ToLower(err.Error()), "notfound") {
		return nil
	}

	if err := client.TerminateInstance(ctx, instanceID, retryToken(request, "terminate-old")); err != nil {
		return fmt.Errorf("terminate old OCI instance %s: %w", instanceID, err)
	}

	return nil
}

func (p *Provider) client(auth *machineops.OperationAuth) (computeClient, error) {
	if p.NewClientWithAuth != nil {
		client, err := p.NewClientWithAuth(auth)
		if err != nil {
			return nil, fmt.Errorf("create OCI compute client: %w", err)
		}

		return client, nil
	}

	if p.NewClient != nil {
		client, err := p.NewClient()
		if err != nil {
			return nil, fmt.Errorf("create OCI compute client: %w", err)
		}

		return client, nil
	}

	client, err := p.newComputeClientForAuth(auth)
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
