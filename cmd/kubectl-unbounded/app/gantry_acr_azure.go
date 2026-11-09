// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/google/uuid"

	"github.com/Azure/unbounded/internal/registryauth"
)

func (h *attachACRHandler) configureIdentityBinding(ctx context.Context, acrID *arm.ResourceID, roleMode string, cfg *registryauth.Config) error {
	clusterID, err := azureResourceID(h.clusterID, "Microsoft.ContainerService/managedClusters")
	if err != nil {
		return err
	}

	var cluster struct {
		ID, Location, FQDN, PrivateFQDN, ProvisioningState string
		APIServerAccessProfile                             struct{ EnableVnetIntegration bool }
		SecurityProfile                                    struct{ WorkloadIdentity struct{ Enabled bool } }
		OidcIssuerProfile                                  struct{ Enabled bool }
	}
	if err := h.azureJSON(ctx, &cluster, "aks", "show", "--name", clusterID.Name, "--resource-group", clusterID.ResourceGroupName); err != nil {
		return err
	}

	if h.restConfig == nil {
		return errors.New("kubeconfig server is required to verify the AKS cluster")
	}

	server, err := url.Parse(h.restConfig.Host)
	if err != nil || server.Scheme != "https" || server.Hostname() == "" || !strings.EqualFold(cluster.ID, h.clusterID) ||
		(!strings.EqualFold(server.Hostname(), cluster.FQDN) && (cluster.PrivateFQDN == "" || !strings.EqualFold(server.Hostname(), cluster.PrivateFQDN))) {
		return errors.New("selected kubeconfig does not match the requested AKS cluster")
	}

	if cluster.APIServerAccessProfile.EnableVnetIntegration {
		return errors.New("AKS identity bindings do not support API-server VNet integration; use external authentication")
	}

	if cluster.ProvisioningState != "Succeeded" || cluster.Location == "" {
		return errors.New("AKS cluster must have successfully completed provisioning before ACR attachment")
	}

	if err := h.checkFlexVersions(ctx); err != nil {
		return err
	}

	role := "7f951dda-4ed3-4680-a7ca-43fe172d538d"

	switch roleMode {
	case "", "LegacyRegistryPermissions":
	case "AbacRepositoryPermissions":
		var roles []struct{ Name, RoleType string }
		if err := h.azureJSON(ctx, &roles, "role", "definition", "list", "--name", "Container Registry Repository Reader"); err != nil {
			return err
		}

		if len(roles) != 1 || roles[0].RoleType != "BuiltInRole" {
			return errors.New("registry repository-reader role is unavailable or ambiguous")
		}

		role = roles[0].Name
	default:
		return fmt.Errorf("unsupported ACR role-assignment mode %q", roleMode)
	}

	if !cluster.OidcIssuerProfile.Enabled || !cluster.SecurityProfile.WorkloadIdentity.Enabled {
		if _, err := h.runAzure(ctx, "aks", "update", "--name", clusterID.Name, "--resource-group", clusterID.ResourceGroupName,
			"--enable-oidc-issuer", "--enable-workload-identity"); err != nil {
			return fmt.Errorf("enable AKS identity-binding prerequisites: %w", err)
		}
	}

	digest := sha256.Sum256([]byte(strings.ToLower(h.clusterID)))
	name := "gantry-pull-" + hex.EncodeToString(digest[:6])

	type identity struct {
		ID, PrincipalID, ClientID, TenantID, Name string
		Tags                                      map[string]string
	}

	var identities []identity
	if err := h.azureJSON(ctx, &identities, "identity", "list", "--resource-group", clusterID.ResourceGroupName); err != nil {
		return err
	}

	var selected identity

	for _, candidate := range identities {
		if strings.EqualFold(candidate.Name, name) {
			if !strings.EqualFold(candidate.Tags["unbounded-cluster-id"], h.clusterID) {
				return errors.New("cluster pull identity name is already owned by another installation")
			}

			selected = candidate
		}
	}

	if selected.ID == "" {
		if err := h.azureJSON(ctx, &selected, "identity", "create", "--resource-group", clusterID.ResourceGroupName,
			"--name", name, "--location", cluster.Location, "--tags", "unbounded-cluster-id="+h.clusterID); err != nil {
			return err
		}
	}

	parsed, err := azureResourceID(selected.ID, "Microsoft.ManagedIdentity/userAssignedIdentities")
	if err != nil || !strings.EqualFold(parsed.SubscriptionID, acrID.SubscriptionID) ||
		!strings.EqualFold(parsed.ResourceGroupName, clusterID.ResourceGroupName) ||
		!strings.EqualFold(selected.Tags["unbounded-cluster-id"], h.clusterID) {
		return errors.New("pull identity response did not identify an identity in the requested subscription")
	}

	for _, value := range []string{selected.PrincipalID, selected.ClientID, selected.TenantID} {
		if id, err := uuid.Parse(value); err != nil || id == uuid.Nil {
			return errors.New("pull identity returned invalid principal, client, or tenant IDs")
		}
	}

	if cfg.ClientID != "" && (!strings.EqualFold(cfg.ClientID, selected.ClientID) || !strings.EqualFold(cfg.TenantID, selected.TenantID)) {
		return errors.New("existing node-auth policy selects another identity; refusing to replace it")
	}

	cfg.ClientID, cfg.TenantID = selected.ClientID, selected.TenantID

	type identityBinding struct {
		Name       string
		Properties struct {
			ManagedIdentity   struct{ ResourceID string }
			ProvisioningState string
		}
	}

	var bindings struct {
		Value    []identityBinding
		NextLink string
	}

	bindingsURL := "https://management.azure.com" + h.clusterID + "/identityBindings?api-version=2026-08-02-preview"

	var found bool

	for {
		if err := h.azureJSON(ctx, &bindings, "rest", "--method", "get", "--url", bindingsURL); err != nil {
			return fmt.Errorf("AKS identity bindings are unavailable: %w", err)
		}

		for _, binding := range bindings.Value {
			if binding.Name == name {
				if !strings.EqualFold(binding.Properties.ManagedIdentity.ResourceID, selected.ID) {
					return errors.New("existing AKS identity binding refers to another identity")
				}

				found = true
			}
		}

		if bindings.NextLink == "" {
			break
		}

		next, err := url.Parse(bindings.NextLink)
		if err != nil || next.Scheme != "https" || next.Host != "management.azure.com" ||
			!strings.HasPrefix(strings.ToLower(next.Path), strings.ToLower(h.clusterID)+"/identitybindings") {
			return errors.New("AKS returned an invalid identity-binding pagination URL")
		}

		bindingsURL = bindings.NextLink
		bindings = struct {
			Value    []identityBinding
			NextLink string
		}{}
	}

	bindingURL := "https://management.azure.com" + h.clusterID + "/identityBindings/" + name + "?api-version=2026-08-02-preview"

	if !found {
		body, err := json.Marshal(map[string]any{"properties": map[string]any{"managedIdentity": map[string]string{"resourceId": selected.ID}}})
		if err != nil {
			return err
		}

		if _, err := h.runAzure(ctx, "rest", "--method", "put", "--url", bindingURL, "--body", string(body)); err != nil {
			return err
		}
	}

	for {
		var binding identityBinding
		if err := h.azureJSON(ctx, &binding, "rest", "--method", "get", "--url", bindingURL); err != nil {
			return err
		}

		if !strings.EqualFold(binding.Properties.ManagedIdentity.ResourceID, selected.ID) {
			return errors.New("AKS identity binding changed to a different identity")
		}

		switch binding.Properties.ProvisioningState {
		case "Succeeded":
			if err := h.waitForACRProxy(ctx, cfg); err != nil {
				return err
			}

			if err := h.ensureACRPullGrant(ctx, selected.PrincipalID, role); err != nil {
				return err
			}

			return nil
		case "Creating", "Updating":
		default:
			return fmt.Errorf("AKS identity binding provisioning state: %s", binding.Properties.ProvisioningState)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (h *attachACRHandler) ensureACRPullGrant(ctx context.Context, principal, role string) error {
	var assignments []struct{ PrincipalID, RoleDefinitionID, Scope, Condition string }
	if err := h.azureJSON(ctx, &assignments, "role", "assignment", "list", "--scope", h.acrID, "--include-inherited",
		"--fill-principal-name", "false", "--fill-role-definition-name", "false"); err != nil {
		return err
	}

	conditional := false

	for _, assignment := range assignments {
		if !strings.EqualFold(assignment.PrincipalID, principal) ||
			!strings.HasSuffix(strings.ToLower(assignment.RoleDefinitionID), "/"+strings.ToLower(role)) {
			continue
		}

		if assignment.Condition == "" {
			return nil
		}

		conditional = true
	}

	if conditional {
		return errors.New("existing pull grant is conditional; refusing to broaden it with an unconditional grant")
	}

	_, err := h.runAzure(ctx, "role", "assignment", "create", "--assignee-object-id", principal,
		"--assignee-principal-type", "ServicePrincipal", "--role", role, "--scope", h.acrID)

	return err
}
