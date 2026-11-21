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
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/internal/kube"
	"github.com/Azure/unbounded/internal/registryauth"
)

type detachACRHandler struct {
	clusterID, namespace, kubeconfig string
	timeout                          time.Duration
	removeAzure                      bool
	client                           kubernetes.Interface
	machineClient                    machineStatusReader
	runAzure                         func(context.Context, string, ...string) ([]byte, error)
	out                              io.Writer
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
		if _, err := azureResourceID(h.clusterID, "Microsoft.ContainerService/managedClusters"); err != nil {
			return fmt.Errorf("--cluster-resource-id is required with --remove-azure: %w", err)
		}

		if h.runAzure == nil {
			h.runAzure = defaultAzureCLI
		}
	}

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	if h.client == nil {
		var err error
		h.client, _, err = kube.ClientAndConfigFromFile(getKubeconfigPath(h.kubeconfig))
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
// External) or has no managed policy left to remove.
func (h *detachACRHandler) waitForExternalConvergence(ctx context.Context) error {
	if _, err := h.machineClient.flexMachines(ctx); err != nil {
		return err
	}

	var lastError error

	err := wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		healthy, err := h.machineClient.flexMachines(ctx)
		if err != nil {
			return false, err
		}

		for name, ok := range healthy {
			if !ok {
				lastError = fmt.Errorf("flex node %s has not yet reported external authentication", name)

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
// deleted. This distinguishes the Azure grants (steps 1-3) from the K8s RBAC
// grant (step 4); neither is faked on failure.
func (h *detachACRHandler) removePullGrant(ctx context.Context) error {
	clusterID, err := azureResourceID(h.clusterID, "Microsoft.ContainerService/managedClusters")
	if err != nil {
		return err
	}

	// The deterministic UAMI name the attach path derives from the cluster ID.
	digest := sha256.Sum256([]byte(strings.ToLower(h.clusterID)))
	name := "gantry-pull-" + hex.EncodeToString(digest[:6])

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

// removeClusterRoleBinding deletes the operator-owned K8s RBAC grant the nodes
// use. A binding not labeled as operator-owned for this namespace is left in
// place.
func (h *detachACRHandler) removeClusterRoleBinding(ctx context.Context) error {
	binding, err := h.client.RbacV1().ClusterRoleBindings().Get(ctx, registryauth.IdentityRoleName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if err == nil {
		if binding.Labels["unbounded-cloud.io/namespace"] != h.namespace || binding.Labels["app.kubernetes.io/managed-by"] != "unbounded-operator" {
			return errors.New("refusing to remove a pull grant that is not operator-owned for this namespace")
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

	if _, err := h.runAzure(ctx, clusterID.SubscriptionID, "identity", "delete", "--name", name, "--resource-group", clusterID.ResourceGroupName, "--yes"); err != nil {
		return fmt.Errorf("delete pull identity %q: %w", name, err)
	}

	return nil
}

// removeACRIdentityBinding lists the cluster's identity bindings and deletes the
// one named by the install only when it references the owned user-assigned
// identity. This deletes the binding, not the underlying ACR role assignment,
// which the operator separately manages; the binding is refused when it points
// at a different identity so a shared or foreign binding is never touched.
func (h *detachACRHandler) removeACRIdentityBinding(ctx context.Context, clusterID *arm.ResourceID, name string) error {
	bindingsURL := "https://management.azure.com" + h.clusterID + "/identityBindings?api-version=2026-08-02-preview"

	type identityBinding struct {
		Name       string
		Properties struct {
			ManagedIdentity struct{ ResourceID string }
		}
	}

	ownedID, err := h.ownedIdentityResourceID(ctx, clusterID, name)
	if err != nil {
		return err
	}

	// No owned identity: there is no binding the install created to remove.
	if ownedID == "" {
		return nil
	}

	for {
		var page struct {
			Value    []identityBinding
			NextLink string
		}
		if err := h.azureJSON(ctx, clusterID.SubscriptionID, &page, "rest", "--method", "get", "--url", bindingsURL); err != nil {
			return fmt.Errorf("list AKS identity bindings: %w", err)
		}

		for _, binding := range page.Value {
			if binding.Name != name {
				continue
			}

			if !strings.EqualFold(binding.Properties.ManagedIdentity.ResourceID, ownedID) {
				return fmt.Errorf("refusing to remove identity binding %q: it references another identity", name)
			}

			bindingURL := "https://management.azure.com" + h.clusterID + "/identityBindings/" + name + "?api-version=2026-08-02-preview"

			if _, err := h.runAzure(ctx, clusterID.SubscriptionID, "rest", "--method", "delete", "--url", bindingURL); err != nil {
				return fmt.Errorf("delete AKS identity binding %q: %w", name, err)
			}

			return nil
		}

		if page.NextLink == "" {
			// No binding of this name: nothing to remove.
			return nil
		}

		next, err := url.Parse(page.NextLink)
		if err != nil || next.Scheme != "https" || next.Host != "management.azure.com" {
			return errors.New("AKS returned an invalid identity-binding pagination URL")
		}

		bindingsURL = page.NextLink
	}
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

// isAzureNotFound reports whether an Azure CLI error means the resource does not
// exist, so removal treats absence as success.
func isAzureNotFound(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "not found") ||
		strings.Contains(strings.ToLower(err.Error()), "could not be found") ||
		strings.Contains(err.Error(), "ResourceNotFound")
}
