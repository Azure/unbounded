// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ociinstance

import (
	"context"
	"fmt"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
)

const defaultConfigProfile = "DEFAULT"

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

// computeClient contains the OCI instance actions used by the provider.
type computeClient interface {
	InstanceAction(ctx context.Context, instanceID, action string) error
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
	case unboundedv1alpha3.OperationHardReboot,
		unboundedv1alpha3.OperationPowerOff,
		unboundedv1alpha3.OperationPowerOn:
		return true
	default:
		return false
	}
}

func (p *Provider) Execute(ctx context.Context, request machineops.OperationRequest) error {
	action, err := actionForOperation(request.Operation)
	if err != nil {
		return err
	}

	instanceID := strings.TrimSpace(strings.TrimPrefix(request.ProviderID, "oci://"))
	if instanceID == "" {
		return fmt.Errorf("oci providerID is required")
	}

	newClient := p.NewClient
	if newClient == nil {
		newClient = p.newDefaultComputeClient
	}

	client, err := newClient()
	if err != nil {
		return fmt.Errorf("create OCI compute client: %w", err)
	}

	return client.InstanceAction(ctx, instanceID, action)
}

func actionForOperation(operation unboundedv1alpha3.OperationKind) (string, error) {
	switch operation {
	case unboundedv1alpha3.OperationHardReboot:
		return instanceActionReset, nil
	case unboundedv1alpha3.OperationPowerOff:
		return instanceActionStop, nil
	case unboundedv1alpha3.OperationPowerOn:
		return instanceActionStart, nil
	default:
		return "", fmt.Errorf("unsupported OCI instance operation %q", operation)
	}
}

func (p *Provider) newDefaultComputeClient() (computeClient, error) {
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

	client, err := core.NewComputeClientWithConfigurationProvider(provider)
	if err != nil {
		return nil, fmt.Errorf("create compute client: %w", err)
	}

	return &ociComputeClient{client: client}, nil
}

type ociComputeClient struct {
	client core.ComputeClient
}

func (c *ociComputeClient) InstanceAction(ctx context.Context, instanceID, action string) error {
	_, err := c.client.InstanceAction(ctx, core.InstanceActionRequest{
		InstanceId: &instanceID,
		Action:     core.InstanceActionActionEnum(action),
	})
	if err != nil {
		return fmt.Errorf("run OCI instance action %s on %s: %w", action, instanceID, err)
	}

	return nil
}
