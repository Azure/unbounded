// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
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
	if err := h.azureJSON(ctx, clusterID.SubscriptionID, &cluster, "aks", "show", "--name", clusterID.Name, "--resource-group", clusterID.ResourceGroupName); err != nil {
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

	tenantID, err := h.azureSubscriptionTenant(ctx, acrID.SubscriptionID)
	if err != nil {
		return err
	}

	if !strings.EqualFold(clusterID.SubscriptionID, acrID.SubscriptionID) {
		clusterTenantID, err := h.azureSubscriptionTenant(ctx, clusterID.SubscriptionID)
		if err != nil {
			return err
		}

		if !strings.EqualFold(tenantID, clusterTenantID) {
			return errors.New("ACR and AKS cluster must be in the same tenant")
		}
	}

	role := "7f951dda-4ed3-4680-a7ca-43fe172d538d"

	switch roleMode {
	case "", "LegacyRegistryPermissions":
	case "AbacRepositoryPermissions":
		var roles []struct{ Name, RoleType string }
		if err := h.azureJSON(ctx, acrID.SubscriptionID, &roles, "role", "definition", "list", "--name", "Container Registry Repository Reader"); err != nil {
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
		if _, err := h.runAzure(ctx, clusterID.SubscriptionID, "aks", "update", "--name", clusterID.Name, "--resource-group", clusterID.ResourceGroupName,
			"--enable-oidc-issuer", "--enable-workload-identity"); err != nil {
			return fmt.Errorf("enable AKS identity-binding prerequisites: %w", err)
		}
	}

	name := acrPullIdentityName(h.clusterID)

	type identity struct {
		ID, PrincipalID, ClientID, TenantID, Name string
		Tags                                      map[string]string
	}

	var identities []identity
	if err := h.azureJSON(ctx, clusterID.SubscriptionID, &identities, "identity", "list", "--resource-group", clusterID.ResourceGroupName); err != nil {
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
		if err := h.azureJSON(ctx, clusterID.SubscriptionID, &selected, "identity", "create", "--resource-group", clusterID.ResourceGroupName,
			"--name", name, "--location", cluster.Location, "--tags", "unbounded-cluster-id="+h.clusterID); err != nil {
			return err
		}
	}

	parsed, err := azureResourceID(selected.ID, "Microsoft.ManagedIdentity/userAssignedIdentities")
	if err != nil || !strings.EqualFold(parsed.SubscriptionID, clusterID.SubscriptionID) ||
		!strings.EqualFold(parsed.ResourceGroupName, clusterID.ResourceGroupName) ||
		!strings.EqualFold(selected.Tags["unbounded-cluster-id"], h.clusterID) {
		return errors.New("pull identity response did not identify an owned identity in the AKS cluster subscription and resource group")
	}

	for _, value := range []string{selected.PrincipalID, selected.ClientID, selected.TenantID} {
		if id, err := uuid.Parse(value); err != nil || id == uuid.Nil {
			return errors.New("pull identity returned invalid principal, client, or tenant IDs")
		}
	}

	if !strings.EqualFold(selected.TenantID, tenantID) {
		return errors.New("pull identity tenant does not match the AKS subscription tenant")
	}

	if cfg.ClientID != "" && (!strings.EqualFold(cfg.ClientID, selected.ClientID) || !strings.EqualFold(cfg.TenantID, selected.TenantID)) {
		return errors.New("existing node-auth policy selects another identity; refusing to replace it")
	}

	cfg.ClientID, cfg.TenantID = selected.ClientID, selected.TenantID

	bindings, err := listAKSIdentityBindings(ctx, h.runAzure, h.clusterID, clusterID.SubscriptionID)
	if err != nil {
		return fmt.Errorf("AKS identity bindings are unavailable: %w", err)
	}

	binding := aksIdentityBindingByName(bindings, name)

	found := binding != nil

	if binding != nil && !strings.EqualFold(binding.Properties.ManagedIdentity.ResourceID, selected.ID) {
		return errors.New("existing AKS identity binding refers to another identity")
	}

	bindingURL := "https://management.azure.com" + h.clusterID + "/identityBindings/" + name + "?api-version=2026-08-02-preview"

	if !found {
		body, err := json.Marshal(map[string]any{"properties": map[string]any{"managedIdentity": map[string]string{"resourceId": selected.ID}}})
		if err != nil {
			return err
		}

		if _, err := h.runAzure(ctx, clusterID.SubscriptionID, "rest", "--method", "put", "--url", bindingURL, "--body", string(body)); err != nil {
			return err
		}
	}

	for {
		var binding aksIdentityBinding
		if err := h.azureJSON(ctx, clusterID.SubscriptionID, &binding, "rest", "--method", "get", "--url", bindingURL); err != nil {
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

			if err := h.ensureACRPullGrant(ctx, acrID, selected.PrincipalID, role); err != nil {
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

func (h *attachACRHandler) azureSubscriptionTenant(ctx context.Context, subscriptionID string) (string, error) {
	var account struct{ ID, TenantID string }
	if err := h.azureJSON(ctx, subscriptionID, &account, "account", "show"); err != nil {
		return "", fmt.Errorf("read tenant for Azure subscription %s: %w", subscriptionID, err)
	}

	if !strings.EqualFold(account.ID, subscriptionID) {
		return "", errors.New("account response did not identify the requested subscription")
	}

	tenantID, err := uuid.Parse(account.TenantID)
	if err != nil || tenantID == uuid.Nil {
		return "", errors.New("subscription returned an invalid tenant ID")
	}

	return tenantID.String(), nil
}

func (h *attachACRHandler) ensureACRPullGrant(ctx context.Context, acrID *arm.ResourceID, principal, role string) error {
	var assignments []struct {
		Name             string
		PrincipalID      string
		RoleDefinitionID string
		Scope            string
		Condition        string
	}
	if err := h.azureJSON(ctx, acrID.SubscriptionID, &assignments, "role", "assignment", "list", "--scope", h.acrID, "--include-inherited",
		"--fill-principal-name", "false", "--fill-role-definition-name", "false"); err != nil {
		return err
	}

	// Use the canonical parsed resource ID (not the raw command-line spelling)
	// so that equivalent accepted forms (trailing slash, case differences)
	// produce the same receipt.
	grantName := acrPullGrantName(h.clusterID, acrID.String(), principal, role)

	conditional := false

	for _, assignment := range assignments {
		if !strings.EqualFold(assignment.PrincipalID, principal) ||
			!strings.HasSuffix(strings.ToLower(assignment.RoleDefinitionID), "/"+strings.ToLower(role)) {
			continue
		}

		if strings.EqualFold(assignment.Name, grantName) {
			// The grant this installation created already exists (same
			// deterministic name). Reuse it; nothing to do.
			return nil
		}

		if assignment.Condition == "" {
			// A pre-existing unconditional grant already grants this principal
			// the pull role on this ACR under a different name. Reuse it; it is
			// NOT created by this installation and detach will preserve it.
			return nil
		}

		conditional = true
	}

	if conditional {
		return errors.New("existing pull grant is conditional; refusing to broaden it with an unconditional grant")
	}

	// Create the grant under its deterministic ownership-receipt name so detach
	// can reverse exactly the grant this installation created, and leave a
	// shared/inherited/reused grant (a different name) in place. The name works
	// for an ACR in another subscription because it is carried by the assignment
	// itself, not by the cluster's subscription.
	_, err := h.runAzure(ctx, acrID.SubscriptionID, "role", "assignment", "create", "--assignee-object-id", principal,
		"--assignee-principal-type", "ServicePrincipal", "--role", role, "--scope", h.acrID,
		"--name", grantName)

	return err
}
