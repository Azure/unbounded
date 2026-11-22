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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
)

// acrPullGrantName is the deterministic role-assignment name (a GUID) the attach
// path uses for the grant it creates on an ACR. The role-assignment API has no
// tags property, so the ownership receipt is the assignment's own name: a GUID
// derived from the cluster ID, the ACR scope, the principal, and the role.
// Azure role-assignment names must be unique across the ENTIRE Microsoft Entra
// tenant (not just within a scope), so the name includes the scope and principal
// to avoid collisions when the same cluster is attached to multiple ACRs or
// multiple roles. Detach reconstructs the same receipt from the same inputs and
// reverses only the one carrying this exact name, leaving a shared/inherited/
// reused grant (a different name) in place.
func acrPullGrantName(clusterID, acrScope, principal, role string) string {
	digest := sha256.Sum256([]byte(
		strings.ToLower(clusterID) + "|" +
			strings.ToLower(acrScope) + "|" +
			strings.ToLower(principal) + "|" +
			strings.ToLower(role),
	))

	// Render the first 16 bytes as a lowercase GUID (the form the API accepts
	// for the role-assignment name).
	b := digest[:16]

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// acrPullIdentityName returns the deterministic user-assigned identity name the
// attach path derives from the cluster resource ID. Detach reconstructs the
// same name from the same inputs to locate the identity to reverse.
func acrPullIdentityName(clusterID string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(clusterID)))

	return "gantry-pull-" + hex.EncodeToString(digest[:6])
}

// acrPullIdentity is the user-assigned identity the attach path creates for a
// cluster's ACR pull grant.
type acrPullIdentity struct {
	ID          string
	PrincipalID string
	// Tags carry the unbounded-cluster-id ownership tag the attach path sets.
	Tags map[string]string
}

// ownedPullIdentity returns the ACR pull UAMI for the cluster when it is owned
// (by the unbounded-cluster-id tag), or an empty identity when it does not
// exist. A same-name identity owned by another cluster is refused, never
// returned: the tag is the ownership proof that lets detach and re-attach tell
// their identity apart from a foreign one.
func ownedPullIdentity(ctx context.Context, runAzure func(context.Context, string, ...string) ([]byte, error), clusterID *arm.ResourceID, name string) (acrPullIdentity, error) {
	var identity acrPullIdentity

	err := azureJSONWith(ctx, runAzure, clusterID.SubscriptionID, &identity, "identity", "show", "--name", name, "--resource-group", clusterID.ResourceGroupName)
	if err != nil && isAzureNotFound(err) {
		return acrPullIdentity{}, nil
	} else if err != nil {
		return acrPullIdentity{}, err
	}

	// The ownership tag is compared case-insensitively against the caller's
	// cluster resource ID spelling.
	if !strings.EqualFold(identity.Tags["unbounded-cluster-id"], clusterID.String()) {
		return acrPullIdentity{}, fmt.Errorf("pull identity %q is not owned by this cluster", name)
	}

	return identity, nil
}

// aksIdentityBinding is one of the cluster's AKS identity bindings.
type aksIdentityBinding struct {
	Name       string
	Properties struct {
		ManagedIdentity   struct{ ResourceID string }
		ProvisioningState string
	}
}

// listAKSIdentityBindings returns every identity binding on the cluster,
// following the API's pagination. The pagination URL is validated to stay on
// the management plane and within the cluster's identity-bindings collection.
func listAKSIdentityBindings(
	ctx context.Context,
	runAzure func(context.Context, string, ...string) ([]byte, error),
	clusterID string,
	subscriptionID string,
) ([]aksIdentityBinding, error) {
	bindingsURL := "https://management.azure.com" + clusterID + "/identityBindings?api-version=2026-08-02-preview"

	var all []aksIdentityBinding

	for {
		var page struct {
			Value    []aksIdentityBinding
			NextLink string
		}
		if err := azureJSONWith(ctx, runAzure, subscriptionID, &page, "rest", "--method", "get", "--url", bindingsURL); err != nil {
			return nil, fmt.Errorf("list AKS identity bindings: %w", err)
		}

		all = append(all, page.Value...)

		if page.NextLink == "" {
			return all, nil
		}

		next, err := url.Parse(page.NextLink)
		if err != nil || next.Scheme != "https" || next.Host != "management.azure.com" ||
			!strings.HasPrefix(strings.ToLower(next.Path), strings.ToLower(clusterID)+"/identitybindings") {
			return nil, errors.New("AKS returned an invalid identity-binding pagination URL")
		}

		bindingsURL = page.NextLink
	}
}

// aksIdentityBindingByName finds the binding named name in bindings, or nil.
func aksIdentityBindingByName(bindings []aksIdentityBinding, name string) *aksIdentityBinding {
	for i := range bindings {
		if bindings[i].Name == name {
			return &bindings[i]
		}
	}

	return nil
}

// deleteAKSIdentityBinding deletes the AKS identity binding named name only
// when it references identityResourceID. A binding that points at a different
// identity is refused, so a shared or foreign binding is never touched. Absence
// of the binding is success.
func deleteAKSIdentityBinding(
	ctx context.Context,
	runAzure func(context.Context, string, ...string) ([]byte, error),
	clusterID string,
	subscriptionID, name, identityResourceID string,
) error {
	bindings, err := listAKSIdentityBindings(ctx, runAzure, clusterID, subscriptionID)
	if err != nil {
		return err
	}

	binding := aksIdentityBindingByName(bindings, name)
	if binding == nil {
		return nil
	}

	// Guard the deletion against a foreign binding in case the caller's earlier
	// read observed different state.
	if !strings.EqualFold(binding.Properties.ManagedIdentity.ResourceID, identityResourceID) {
		return fmt.Errorf("refusing to remove identity binding %q: it references another identity", name)
	}

	bindingURL := "https://management.azure.com" + clusterID + "/identityBindings/" + name + "?api-version=2026-08-02-preview"

	if _, err := runAzure(ctx, subscriptionID, "rest", "--method", "delete", "--url", bindingURL); err != nil {
		return fmt.Errorf("delete AKS identity binding %q: %w", name, err)
	}

	return nil
}

// azureJSONWith runs an Azure CLI command for a subscription and decodes the
// JSON output into out. Both the attach and detach handlers use it through
// their respective runAzure seams (which tests inject).
func azureJSONWith(ctx context.Context, runAzure func(context.Context, string, ...string) ([]byte, error), subscriptionID string, out any, args ...string) error {
	data, err := runAzure(ctx, subscriptionID, args...)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid Azure response for %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}

	return nil
}
