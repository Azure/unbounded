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
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/internal/kube"
	"github.com/Azure/unbounded/internal/registryauth"
)

// The built-in Azure role definition GUIDs the attach path can grant on an ACR.
// RoleDefinitionIDs end in these GUIDs (not in names), so the detach reversal
// must compare against them.
const (
	// acrPullRoleID is the AcrPull built-in role
	// (7f951dda-4ed3-4680-a7ca-43fe172d538d).
	acrPullRoleID = "7f951dda-4ed3-4680-a7ca-43fe172d538d"

	// repositoryReaderRoleID is the Container Registry Repository Reader
	// built-in role (b93aa761-3e63-49ed-ac28-beffa264f7ac), as published by
	// Microsoft:
	// https://learn.microsoft.com/en-us/azure/role-based-access-control/built-in-roles/containers#container-registry-repository-reader
	repositoryReaderRoleID = "b93aa761-3e63-49ed-ac28-beffa264f7ac"
)

type detachACRHandler struct {
	acrID, clusterID, namespace, kubeconfig string
	timeout                                 time.Duration
	removeAzure                             bool
	client                                  kubernetes.Interface
	restConfig                              *rest.Config
	machineClient                           machineStatusReader
	runAzure                                func(context.Context, string, ...string) ([]byte, error)
	out                                     io.Writer
}

// execute reverts managed ACR authentication to external mode: it sets the
// node-auth policy to mode: external so the operator clears
// Machine.spec.registryAuth, waits for the fleet to report the external policy
// converged, and (optionally) removes the operator-owned cluster pull grant.
func (h *detachACRHandler) execute(ctx context.Context) error {
	if h.timeout <= 0 {
		return errors.New("--timeout must be positive")
	}

	if h.removeAzure {
		// Validate ALL required remove-azure inputs before any policy mutation.
		// The ACR resource ID identifies the registry whose pull grant is being
		// reversed (and is required because the grant may live in a different
		// subscription than the AKS cluster); a missing or malformed value must
		// be refused up front, before the node-auth policy is rewritten to
		// external, not after the fleet has been told to converge.
		if _, err := azureResourceID(h.clusterID, "Microsoft.ContainerService/managedClusters"); err != nil {
			return fmt.Errorf("--cluster-resource-id is required with --remove-azure: %w", err)
		}

		if _, err := azureResourceID(h.acrID, "Microsoft.ContainerRegistry/registries"); err != nil {
			return fmt.Errorf("--acr-resource-id is required with --remove-azure: %w", err)
		}

		if h.runAzure == nil {
			h.runAzure = defaultAzureCLI
		}
	}

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	if h.client == nil {
		var err error
		// The REST config is retained so the remove-azure path can verify the
		// selected kubeconfig actually targets the requested AKS cluster before
		// any policy mutation or Azure deletion happens.
		h.client, h.restConfig, err = kube.ClientAndConfigFromFile(getKubeconfigPath(h.kubeconfig))
		if err != nil {
			return err
		}
	}

	if h.machineClient == nil {
		c, err := newMachineClientWithKubeconfig(h.kubeconfig)
		if err != nil {
			return fmt.Errorf("create Machine client: %w", err)
		}

		h.machineClient = machineStatusClient{client: c}
	}

	// With --remove-azure, validate the Azure cluster against the selected
	// kubeconfig before any mutation. The policy mutation below and the Azure
	// deletions both assume the kubeconfig targets the cluster whose pull grant
	// is being removed; a mismatched kubeconfig would detach the wrong fleet or
	// delete the wrong grant. A fake that rejects cluster preflight is now
	// called, so a preflight failure aborts before policy changes.
	if h.removeAzure {
		if err := h.preflightRemoveAzure(ctx); err != nil {
			return err
		}
	}

	// Set the node-auth policy to external. The operator derives the external
	// policy from the ConfigMap and clears Machine.spec.registryAuth on the
	// affected nodes, so the CLI only rewrites the policy field.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		source, err := h.client.CoreV1().ConfigMaps(h.namespace).Get(ctx, "gantry-config", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read gantry-config; install operator-managed Gantry first: %w", err)
		}

		external, err := yaml.Marshal(registryauth.Config{Mode: registryauth.ExternalMode})
		if err != nil {
			return err
		}

		source.Data[registryauth.ConfigKey] = string(external)
		_, err = h.client.CoreV1().ConfigMaps(h.namespace).Update(ctx, source, metav1.UpdateOptions{})

		return err
	}); err != nil {
		return fmt.Errorf("set node-auth policy to external: %w", err)
	}

	if err := h.waitForExternalConvergence(ctx); err != nil {
		return err
	}

	if h.removeAzure {
		if err := h.removePullGrant(ctx); err != nil {
			return err
		}
	}

	if h.out != nil {
		_, err := fmt.Fprintln(h.out, "Managed ACR authentication disabled; nodes converged to external authentication.")
		if err != nil {
			return err
		}
	}

	return nil
}

// waitForExternalConvergence blocks until every agent-backed Flex node reports
// the external policy converged (RegistryAuth condition True with reason
// External, observed at the current generation, with the delivered spec
// matching the external policy). Every eligible not-ready node is checked,
// including a node with no Machine yet (still installing) and a node that still
// reports a managed (Configured) condition, stale generation, or undelivered
// policy: all are retried until the timeout. A node reporting a terminal
// failure is surfaced immediately, not polled to the timeout.
func (h *detachACRHandler) waitForExternalConvergence(ctx context.Context) error {
	// The desired policy for detach is external (nil spec.registryAuth).
	var lastError error

	err := wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		statuses, err := h.machineClient.flexMachines(ctx, nil)
		if err != nil {
			return false, err
		}

		for name, st := range statuses {
			if st.terminal {
				// A terminal failure is surfaced immediately, not polled to the
				// timeout: the node reported the external policy will not
				// converge.
				return false, fmt.Errorf("flex node %s reports a terminal failure: %s", name, st.detail)
			}

			if !st.ready {
				// Not converged: a node with no Machine is still installing, and
				// a node that still reports a managed or stale condition is still
				// converging. Both are retried until the timeout.
				lastError = fmt.Errorf("flex node %s has not converged to external authentication: %s", name, st.detail)

				return false, nil
			}
		}

		return true, nil
	})
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("timed out waiting for Flex nodes to converge to external authentication; last: %v", lastError)
		}

		return errors.Join(err, lastError)
	}

	return nil
}

// removePullGrant reverses the Azure-side resources the attach path created, in
// the order that is safe to delete, each ownership-gated so a resource not
// owned by this installation is left in place:
//
//  1. the ACR role assignment (AcrPull or Repository Reader) on the owned
//     principal,
//  2. the AKS identity binding that references the owned user-assigned identity,
//  3. the user-assigned identity itself,
//  4. the Kubernetes RBAC grant (ClusterRoleBinding) the nodes use to reach the
//     ACR proxy.
//
// The UAMI and binding are identified by the deterministic name derived from
// the cluster resource ID plus the unbounded-cluster-id ownership tag, so a
// resource at the same name owned by another installation is refused, not
// deleted. The role assignment is identified by the owned principal and the
// ACR role the attach path granted, so a shared or inherited grant (one this
// installation did not create) is left in place and its removal is reported as
// uncertain rather than faked. This distinguishes the Azure grants (steps 1-3)
// from the K8s RBAC grant (step 4); neither is faked on failure.
func (h *detachACRHandler) removePullGrant(ctx context.Context) error {
	clusterID, err := azureResourceID(h.clusterID, "Microsoft.ContainerService/managedClusters")
	if err != nil {
		return err
	}

	// The ACR resource ID identifies the registry whose pull grant is being
	// reversed. The grant is scoped to the ACR and lives in the ACR's
	// subscription, which may differ from the AKS cluster's, so the ACR
	// authority (not the cluster authority) is what finds and reverses it.
	acrID, err := azureResourceID(h.acrID, "Microsoft.ContainerRegistry/registries")
	if err != nil {
		return fmt.Errorf("--acr-resource-id is required with --remove-azure: %w", err)
	}

	// The deterministic UAMI name the attach path derives from the cluster ID.
	digest := sha256.Sum256([]byte(strings.ToLower(h.clusterID)))
	name := "gantry-pull-" + hex.EncodeToString(digest[:6])

	// --- Phase 1: validate ALL ownership that can refuse the operation, before
	// any deletion. A foreign resource at the same name, a foreign AKS identity
	// binding, or a foreign Kubernetes grant must abort the whole operation
	// before anything is deleted, so the detach never deletes some resources and
	// then refuse on the rest.
	if err := h.checkClusterRoleBindingOwned(ctx); err != nil {
		return err
	}

	principal, err := h.ownedIdentityPrincipalID(ctx, clusterID, name)
	if err != nil {
		return err
	}

	// The AKS identity binding that references the owned UAMI must be owned
	// (or absent) before any deletion: a same-name binding that points at a
	// foreign identity is refused here, before a role assignment is deleted.
	if err := h.checkACRIdentityBindingOwned(ctx, clusterID, name); err != nil {
		return err
	}

	// The role assignments to reverse and the ones to preserve. Owned grants
	// carry the deterministic ownership-receipt name; shared/inherited/reused
	// grants do not and are left in place, reported for manual cleanup.
	owned, preserved, err := h.findACRRoleAssignments(ctx, acrID, principal)
	if err != nil {
		return err
	}

	if len(preserved) > 0 {
		if h.out != nil {
			for _, p := range preserved {
				if _, werr := fmt.Fprintln(h.out, "Preserving ACR role assignment not created by this installation (manual cleanup required): "+p.ID); werr != nil {
					return fmt.Errorf("report preserved role assignment %s: %w", p.ID, werr)
				}
			}
		}
	}

	// --- Phase 2: all ownership checks passed; perform the deletions in the
	// safe order (grant, binding, identity, then the K8s RBAC grant).
	if err := h.deleteACRRoleAssignments(ctx, acrID, owned); err != nil {
		return err
	}

	if err := h.removeACRIdentityBinding(ctx, clusterID, name); err != nil {
		return err
	}

	if err := h.removeUserAssignedIdentity(ctx, clusterID, name); err != nil {
		return err
	}

	if err := h.removeClusterRoleBinding(ctx); err != nil {
		return err
	}

	return nil
}

// checkClusterRoleBindingOwned verifies, read-only, that the Kubernetes
// identity-use grant (if present) is owned by this installation for this
// namespace, so the ownership gate runs before any Azure deletion rather than
// as the last step. Absence is allowed; a foreign grant is refused.
func (h *detachACRHandler) checkClusterRoleBindingOwned(ctx context.Context) error {
	binding, err := h.client.RbacV1().ClusterRoleBindings().Get(ctx, registryauth.IdentityRoleName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if err == nil &&
		(binding.Labels["unbounded-cloud.io/namespace"] != h.namespace || binding.Labels["app.kubernetes.io/managed-by"] != acrAttachManager) {
		return errors.New("refusing to remove pull grant: the identity-use ClusterRoleBinding is not owned by this installation for this namespace")
	}

	return nil
}

// acrRoleAssignment is a role assignment on the pull principal that scopes an
// ACR role to a registry.
type acrRoleAssignment struct {
	ID        string
	Scope     string
	Condition string
	Owned     bool
	RoleID    string
}

// findACRRoleAssignments lists the role assignments on the pull principal in the
// ACR's subscription and partitions them into owned (carrying the deterministic
// ownership-receipt name for this cluster) and preserved (shared/inherited/
// reused, a different name). Only the owned grants are reversed; the preserved
// ones are left in place and reported. A conditional grant is never reversed.
func (h *detachACRHandler) findACRRoleAssignments(ctx context.Context, acrID *arm.ResourceID, principalID string) (owned, preserved []acrRoleAssignment, err error) {
	if principalID == "" {
		// No owned identity, so no role assignment to reverse.
		return nil, nil, nil
	}

	var raw []struct {
		Name             string
		PrincipalID      string
		RoleDefinitionID string
		Scope            string
		ID               string
		Condition        string
	}

	// The grant is scoped to the ACR and lives in the ACR's subscription, which
	// may differ from the AKS cluster's. List at the ACR's subscription scope.
	if err := h.azureJSON(ctx, acrID.SubscriptionID, &raw, "role", "assignment", "list",
		"--assignee", principalID, "--include-inherited", "--scope", "/subscriptions/"+acrID.SubscriptionID); err != nil {
		return nil, nil, fmt.Errorf("list role assignments for principal %s: %w", principalID, err)
	}

	for _, a := range raw {
		if !strings.EqualFold(a.PrincipalID, principalID) {
			continue
		}

		// The scope must be this ACR (the attach path scopes the grant to the
		// ACR, not the subscription or a resource group).
		scopeID, perr := azureResourceID(a.Scope, "Microsoft.ContainerRegistry/registries")
		if perr != nil {
			continue
		}

		if !strings.EqualFold(scopeID.SubscriptionID, acrID.SubscriptionID) ||
			!strings.EqualFold(scopeID.ResourceGroupName, acrID.ResourceGroupName) ||
			!strings.EqualFold(scopeID.Name, acrID.Name) {
			continue
		}

		// The role must be one the attach path granted: AcrPull or the Container
		// Registry Repository Reader built-in role, matched by their published
		// built-in role definition GUIDs (RoleDefinitionIDs end in GUIDs, not
		// names).
		if !isACRPullRole(a.RoleDefinitionID) && !isRepositoryReaderRole(a.RoleDefinitionID) {
			continue
		}

		// A conditional grant is never the unconditional one the attach path
		// created; it is shared/inherited and is left in place, never reversed.
		if a.Condition != "" {
			preserved = append(preserved, acrRoleAssignment{ID: a.ID, Scope: a.Scope, Condition: a.Condition, RoleID: a.RoleDefinitionID})

			continue
		}

		// Ownership proof is the deterministic name the attach path creates the
		// grant under (the role-assignment API has no tags property, so the
		// receipt is the name). A grant on the same principal+ACR under a
		// different name (for example, one attach merely reused) is not owned
		// and is preserved.
		entry := acrRoleAssignment{ID: a.ID, Scope: a.Scope, Condition: a.Condition, RoleID: a.RoleDefinitionID}

		// Use the canonical parsed resource ID (not the raw command-line
		// spelling) so the receipt matches what attach created.
		for _, grant := range ownedRoleGrantNames(h.clusterID, acrID.String(), principalID) {
			if strings.EqualFold(a.Name, grant) {
				entry.Owned = true

				break
			}
		}

		if entry.Owned {
			owned = append(owned, entry)
		} else {
			preserved = append(preserved, entry)
		}
	}

	return owned, preserved, nil
}

// ownedRoleGrantNames returns the deterministic role-assignment names the attach
// path would create for this cluster on the given ACR, one per ACR role it can
// grant (AcrPull and the Repository Reader role). Detach matches a listed
// assignment's name against this set to tell its own grant from a shared one.
// The name includes the ACR scope and principal so that two grants on different
// ACRs (or different principals) never collide, since Azure role-assignment
// names are tenant-wide unique.
func ownedRoleGrantNames(clusterID, acrScope, principal string) []string {
	return []string{
		acrPullGrantName(clusterID, acrScope, principal, acrPullRoleID),
		acrPullGrantName(clusterID, acrScope, principal, repositoryReaderRoleID),
	}
}

// isACRPullRole reports whether a role definition ID is the AcrPull built-in
// role (7f951dda-4ed3-4680-a7ca-43fe172d538d).
func isACRPullRole(roleDefinitionID string) bool {
	return strings.HasSuffix(strings.ToLower(roleDefinitionID), "/"+acrPullRoleID)
}

// isRepositoryReaderRole reports whether a role definition ID is the Container
// Registry Repository Reader built-in role
// (b93aa761-3e63-49ed-ac28-beffa264f7ac), as published by Microsoft.
func isRepositoryReaderRole(roleDefinitionID string) bool {
	return strings.HasSuffix(strings.ToLower(roleDefinitionID), "/"+repositoryReaderRoleID)
}

// deleteACRRoleAssignments deletes the owned (receipt-tagged) ACR role
// assignments by their full resource ID, in the ACR's subscription. Absence of
// an assignment is success (it may have been removed already).
func (h *detachACRHandler) deleteACRRoleAssignments(ctx context.Context, acrID *arm.ResourceID, owned []acrRoleAssignment) error {
	for _, a := range owned {
		if _, err := h.runAzure(ctx, acrID.SubscriptionID, "role", "assignment", "delete", "--id", a.ID); err != nil {
			if isAzureNotFound(err) {
				continue
			}

			return fmt.Errorf("delete ACR role assignment %s: %w", a.ID, err)
		}
	}

	return nil
}

// preflightRemoveAzure verifies, before any policy mutation or Azure deletion,
// that the selected kubeconfig actually targets the AKS cluster whose pull
// grant is being removed. It is the read-only counterpart of the attach path's
// cluster validation: the kubeconfig's API-server host must match the cluster's
// (public or private) FQDN, and the cluster must be a successfully provisioned
// AKS managed cluster. A mismatched kubeconfig would detach the wrong fleet, so
// it is refused up front rather than after a mutation has happened.
func (h *detachACRHandler) preflightRemoveAzure(ctx context.Context) error {
	clusterID, err := azureResourceID(h.clusterID, "Microsoft.ContainerService/managedClusters")
	if err != nil {
		return err
	}

	var cluster struct {
		ID, Location, FQDN, PrivateFQDN, ProvisioningState string
	}
	if err := h.azureJSON(ctx, clusterID.SubscriptionID, &cluster, "aks", "show", "--name", clusterID.Name, "--resource-group", clusterID.ResourceGroupName); err != nil {
		return err
	}

	if h.restConfig == nil {
		return errors.New("kubeconfig server is required to verify the AKS cluster before removing its pull grant")
	}

	server, err := url.Parse(h.restConfig.Host)
	if err != nil || server.Scheme != "https" || server.Hostname() == "" || !strings.EqualFold(cluster.ID, h.clusterID) ||
		(!strings.EqualFold(server.Hostname(), cluster.FQDN) && (cluster.PrivateFQDN == "" || !strings.EqualFold(server.Hostname(), cluster.PrivateFQDN))) {
		return errors.New("selected kubeconfig does not match the requested AKS cluster")
	}

	if cluster.ProvisioningState != "Succeeded" || cluster.Location == "" {
		return errors.New("AKS cluster must have successfully completed provisioning before its pull grant is removed")
	}

	return nil
}

// ownedIdentityPrincipalID returns the principal ID of the pull UAMI for this
// cluster when it is owned (by tag), or "" when it does not exist. It is the
// assignee the ACR role assignment was created on, so the role-assignment
// reversal is tied to the owned identity rather than to a name that another
// installation could share.
func (h *detachACRHandler) ownedIdentityPrincipalID(ctx context.Context, clusterID *arm.ResourceID, name string) (string, error) {
	var identity struct {
		PrincipalID string
		Tags        map[string]string
	}

	err := h.azureJSON(ctx, clusterID.SubscriptionID, &identity, "identity", "show", "--name", name, "--resource-group", clusterID.ResourceGroupName)
	if err != nil && isAzureNotFound(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}

	if !strings.EqualFold(identity.Tags["unbounded-cluster-id"], h.clusterID) {
		return "", fmt.Errorf("refusing to remove pull grant: identity %q is not owned by this cluster", name)
	}

	return identity.PrincipalID, nil
}

// removeClusterRoleBinding deletes the K8s RBAC grant (the identity-use
// ClusterRoleBinding the nodes use to reach the ACR proxy) that this
// installation owns. The operator stamps the grant with
// app.kubernetes.io/managed-by=unbounded-acr-attach and the namespace label; a
// binding not owned for this namespace is left in place, never deleted.
func (h *detachACRHandler) removeClusterRoleBinding(ctx context.Context) error {
	binding, err := h.client.RbacV1().ClusterRoleBindings().Get(ctx, registryauth.IdentityRoleName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if err == nil {
		if binding.Labels["unbounded-cloud.io/namespace"] != h.namespace || binding.Labels["app.kubernetes.io/managed-by"] != acrAttachManager {
			return errors.New("refusing to remove a pull grant that is not owned by this installation for this namespace")
		}

		if err := h.client.RbacV1().ClusterRoleBindings().Delete(ctx, registryauth.IdentityRoleName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

// removeUserAssignedIdentity deletes the pull UAMI the attach path created for
// this cluster, only when it carries the unbounded-cluster-id ownership tag
// matching this cluster. Absence is success.
func (h *detachACRHandler) removeUserAssignedIdentity(ctx context.Context, clusterID *arm.ResourceID, name string) error {
	var identity struct {
		ID   string
		Tags map[string]string
	}

	err := h.azureJSON(ctx, clusterID.SubscriptionID, &identity, "identity", "show", "--name", name, "--resource-group", clusterID.ResourceGroupName)
	if err != nil && isAzureNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}

	if !strings.EqualFold(identity.Tags["unbounded-cluster-id"], h.clusterID) {
		return fmt.Errorf("refusing to remove pull identity %q: it is not owned by this cluster", name)
	}

	// The installed Azure CLI's identity delete accepts only --name and
	// --resource-group; there is no --yes flag. Confirming ownership above is
	// the guard against deleting a foreign identity.
	if _, err := h.runAzure(ctx, clusterID.SubscriptionID, "identity", "delete", "--name", name, "--resource-group", clusterID.ResourceGroupName); err != nil {
		return fmt.Errorf("delete pull identity %q: %w", name, err)
	}

	return nil
}

// aksIdentityBinding is one of the cluster's AKS identity bindings.
type aksIdentityBinding struct {
	Name       string
	Properties struct {
		ManagedIdentity struct{ ResourceID string }
	}
}

// aksIdentityBindingsList returns every identity binding on the cluster,
// following the API's pagination.
func (h *detachACRHandler) aksIdentityBindingsList(ctx context.Context, clusterID *arm.ResourceID) ([]aksIdentityBinding, error) {
	bindingsURL := "https://management.azure.com" + h.clusterID + "/identityBindings?api-version=2026-08-02-preview"

	var all []aksIdentityBinding

	for {
		var page struct {
			Value    []aksIdentityBinding
			NextLink string
		}
		if err := h.azureJSON(ctx, clusterID.SubscriptionID, &page, "rest", "--method", "get", "--url", bindingsURL); err != nil {
			return nil, fmt.Errorf("list AKS identity bindings: %w", err)
		}

		all = append(all, page.Value...)

		if page.NextLink == "" {
			return all, nil
		}

		next, err := url.Parse(page.NextLink)
		if err != nil || next.Scheme != "https" || next.Host != "management.azure.com" {
			return nil, errors.New("AKS returned an invalid identity-binding pagination URL")
		}

		bindingsURL = page.NextLink
	}
}

// checkACRIdentityBindingOwned verifies, read-only, that the AKS identity
// binding named by the install (if present) references the owned user-assigned
// identity, so the ownership gate runs before any Azure deletion. It is the
// read-only counterpart of removeACRIdentityBinding. Absence of the binding is
// allowed; a binding at the same name that references a different identity is
// refused, because deleting it would drop a foreign binding.
func (h *detachACRHandler) checkACRIdentityBindingOwned(ctx context.Context, clusterID *arm.ResourceID, name string) error {
	ownedID, err := h.ownedIdentityResourceID(ctx, clusterID, name)
	if err != nil {
		return err
	}

	// No owned identity: there is no binding the install created to remove.
	if ownedID == "" {
		return nil
	}

	bindings, err := h.aksIdentityBindingsList(ctx, clusterID)
	if err != nil {
		return err
	}

	for _, binding := range bindings {
		if binding.Name != name {
			continue
		}

		if !strings.EqualFold(binding.Properties.ManagedIdentity.ResourceID, ownedID) {
			return fmt.Errorf("refusing to remove identity binding %q: it references another identity", name)
		}
	}

	return nil
}

// removeACRIdentityBinding deletes the AKS identity binding named by the install
// only when it references the owned user-assigned identity. This deletes the
// binding, not the underlying ACR role assignment, which the operator separately
// manages; the binding is refused when it points at a different identity so a
// shared or foreign binding is never touched. The ownership check runs in
// checkACRIdentityBindingOwned before any deletion; this performs the deletion.
func (h *detachACRHandler) removeACRIdentityBinding(ctx context.Context, clusterID *arm.ResourceID, name string) error {
	ownedID, err := h.ownedIdentityResourceID(ctx, clusterID, name)
	if err != nil {
		return err
	}

	// No owned identity: there is no binding the install created to remove.
	if ownedID == "" {
		return nil
	}

	bindings, err := h.aksIdentityBindingsList(ctx, clusterID)
	if err != nil {
		return err
	}

	for _, binding := range bindings {
		if binding.Name != name {
			continue
		}

		// Guard the deletion against a foreign binding in case the read-only
		// preflight and this deletion observe different state.
		if !strings.EqualFold(binding.Properties.ManagedIdentity.ResourceID, ownedID) {
			return fmt.Errorf("refusing to remove identity binding %q: it references another identity", name)
		}

		bindingURL := "https://management.azure.com" + h.clusterID + "/identityBindings/" + name + "?api-version=2026-08-02-preview"

		if _, err := h.runAzure(ctx, clusterID.SubscriptionID, "rest", "--method", "delete", "--url", bindingURL); err != nil {
			return fmt.Errorf("delete AKS identity binding %q: %w", name, err)
		}

		return nil
	}

	// No binding of this name: nothing to remove.
	return nil
}

// ownedIdentityResourceID returns the resource ID of the pull UAMI for this
// cluster when it is owned (by tag), or "" when it does not exist.
func (h *detachACRHandler) ownedIdentityResourceID(ctx context.Context, clusterID *arm.ResourceID, name string) (string, error) {
	var identity struct {
		ID   string
		Tags map[string]string
	}

	err := h.azureJSON(ctx, clusterID.SubscriptionID, &identity, "identity", "show", "--name", name, "--resource-group", clusterID.ResourceGroupName)
	if err != nil && isAzureNotFound(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}

	if !strings.EqualFold(identity.Tags["unbounded-cluster-id"], h.clusterID) {
		return "", fmt.Errorf("refusing to remove identity binding %q: identity %q is not owned by this cluster", name, name)
	}

	return identity.ID, nil
}

// azureJSON runs an Azure CLI command for a subscription and decodes the JSON
// output into out. It is the detach handler's counterpart to the attach
// handler's azureJSON.
func (h *detachACRHandler) azureJSON(ctx context.Context, subscriptionID string, out any, args ...string) error {
	data, err := h.runAzure(ctx, subscriptionID, args...)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid Azure response for %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}

	return nil
}

// isAzureNotFound reports whether an Azure CLI error is the specific "the
// requested resource does not exist" outcome, so removal treats absence as
// success. It is deliberately narrow: a subscription-level authorization
// failure, a CLI configuration error, or an API throttling error must NOT be
// mistaken for "the resource is gone", because treating those as success would
// report a deletion that did not happen. The match is on the Azure REST error
// code and the Azure CLI's ResourceNotFound wording, not on any incidental
// "not found" substring.
func isAzureNotFound(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()

	return strings.Contains(msg, "ResourceNotFound") ||
		strings.Contains(msg, "Resource '") && strings.Contains(msg, "was not found")
}
