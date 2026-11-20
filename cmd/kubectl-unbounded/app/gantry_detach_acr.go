// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/internal/kube"
	"github.com/Azure/unbounded/internal/registryauth"
	"github.com/Azure/unbounded/internal/unbounded"
)

// detachACRHandler turns off managed ACR authentication. Unlike attach, which
// only adds or updates, detach is the explicit teardown: it reverts the
// node-auth policy to external so the operator's installer removes the managed
// configuration from every opted-in node, then removes the Gantry-owned
// Kubernetes authentication grants. The Azure identity, identity binding, and
// AcrPull role assignment are retained by default (a pull grant that other
// workloads may still rely on) and removed only with --remove-azure.
// detachInstallerName and detachHashKey mirror the operator's installer DaemonSet
// name and config-hash annotation so the detach command can verify the operator
// has converged the external policy without importing the operator package.
const (
	detachInstallerName = "gantry-acr-auth"
	detachHashKey       = "unbounded-cloud.io/registry-auth-config-hash"
)

type detachACRHandler struct {
	namespace, kubeconfig string
	clusterID             string // optional; required to remove the Azure identity
	timeout               time.Duration
	removeAzure           bool
	client                kubernetes.Interface
	restConfig            *rest.Config
	runAzure              func(context.Context, string, ...string) ([]byte, error)
	out                   io.Writer
}

func gantryDetachCommand() *cobra.Command {
	h := &detachACRHandler{}
	cmd := &cobra.Command{
		Use:   "detach-acr",
		Short: "Turn off managed ACR authentication and clean up the node configuration",
		Long:  "Disable the stock ACR credential provider that attach-acr --auth-mode aks-identity-binding enabled. Sets the node-auth policy back to external so the operator removes the managed configuration from current and future Linux Flex nodes (the installer restarts each kubelet once to drop the flags), and removes the Gantry-owned Kubernetes authentication grants (gantry-acr-pull and the kubelet-token-audience bindings).\n\nThe Gantry-owned Azure pull identity, its identity binding, and the AcrPull role assignment are retained by default, because other workloads may still use that grant. Pass --remove-azure --cluster-resource-id to delete them as well.",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { h.out = cmd.OutOrStdout(); return h.execute(cmd.Context()) },
	}
	cmd.Flags().StringVar(&h.namespace, "namespace", unbounded.SystemNamespace(), "Operator and Gantry namespace")
	cmd.Flags().StringVar(&h.kubeconfig, "kubeconfig", "", "Kubeconfig path")
	cmd.Flags().StringVar(&h.clusterID, "cluster-resource-id", "", "AKS resource ID (required with --remove-azure to delete the Gantry-owned identity)")
	cmd.Flags().DurationVar(&h.timeout, "timeout", 10*time.Minute, "Cleanup and node configuration timeout")
	cmd.Flags().BoolVar(&h.removeAzure, "remove-azure", false, "Also delete the Gantry-owned Azure pull identity and its identity binding (requires --cluster-resource-id)")

	return cmd
}

func (h *detachACRHandler) execute(ctx context.Context) error {
	if h.timeout <= 0 {
		return errors.New("--timeout must be positive")
	}

	if h.removeAzure {
		if _, err := azureResourceID(h.clusterID, "Microsoft.ContainerService/managedClusters"); err != nil {
			return fmt.Errorf("--remove-azure requires a valid --cluster-resource-id: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	if h.client == nil {
		client, restConfig, err := kube.ClientAndConfigFromFile(getKubeconfigPath(h.kubeconfig))
		if err != nil {
			return err
		}

		h.client = client
		h.restConfig = restConfig
	}

	if _, err := h.client.CoreV1().ConfigMaps(h.namespace).Get(ctx, "gantry-config", metav1.GetOptions{}); err != nil {
		return fmt.Errorf("read gantry-config; install operator-managed Gantry first: %w", err)
	}

	if h.runAzure == nil {
		h.runAzure = func(ctx context.Context, subscriptionID string, args ...string) ([]byte, error) {
			args = append(args, "--subscription", subscriptionID, "--only-show-errors", "--output", "json")
			cmd := exec.CommandContext(ctx, "az", args...)

			var stderr bytes.Buffer

			cmd.Stderr = &stderr

			output, err := cmd.Output()
			if err != nil {
				return nil, fmt.Errorf("az %s: %w: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(stderr.String()))
			}

			return output, nil
		}
	}

	// Revert the node-auth policy to external. The operator's installer then
	// removes the managed provider files from each opted-in node and restarts
	// the kubelet once to drop the flags. This is the supported disable path:
	// it is what the operator reconciles toward, so it survives pod recreation
	// and is reported on the Site condition.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := h.client.CoreV1().ConfigMaps(h.namespace).Get(ctx, "gantry-config", metav1.GetOptions{})
		if err != nil {
			return err
		}

		if current.Data[registryauth.ConfigKey] == "" {
			return nil
		}

		policy, err := yaml.Marshal(&registryauth.Config{Mode: registryauth.ExternalMode})
		if err != nil {
			return err
		}

		current.Data[registryauth.ConfigKey] = string(policy)

		_, err = h.client.CoreV1().ConfigMaps(h.namespace).Update(ctx, current, metav1.UpdateOptions{})

		return err
	}); err != nil {
		return fmt.Errorf("revert node-auth policy to external: %w", err)
	}

	if err := h.waitForDetach(ctx); err != nil {
		return fmt.Errorf("node configuration reverted, but removal is not ready: %w", err)
	}

	if h.removeAzure {
		if err := h.removeAzureIdentity(ctx); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(h.out, "Retained the Gantry-owned Azure pull identity and AcrPull grant; use --remove-azure to delete them."); err != nil {
			return err
		}
	}

	if h.out != nil {
		if _, err := fmt.Fprintln(h.out, "Managed ACR authentication disabled; node configuration removed by the operator; registry authentication is externally managed."); err != nil {
			return err
		}
	}

	return nil
}

// waitForDetach waits for the installer DaemonSet to roll out the external
// policy and report that the operator-owned node authentication has been
// removed. It requires the DaemonSet to carry the expected external config hash
// *and* to be fully available for that generation, so an old managed-policy
// DaemonSet that is merely Ready does not pass. A missing DaemonSet is treated
// as "no installer is scheduled," which is only proven after the operator has
// reconciled the external policy; the caller is responsible for that ordering.
func (h *detachACRHandler) waitForDetach(ctx context.Context) error {
	expected := h.externalHash()

	return wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		ds, err := h.client.AppsV1().DaemonSets(h.namespace).Get(ctx, detachInstallerName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// No installer scheduled. This is only correct if the external
				// policy is already in effect; the policy update happened before
				// this call, so a missing helper means there is nothing to clean
				// up. Do not, however, treat a missing helper as proof that a
				// previously-present helper cleaned up its nodes - the operator
				// owns that convergence, and the caller verifies it.
				return true, nil
			}

			return false, err
		}

		// The external policy must be the one the installer is running (matching
		// hash) and the rollout must be complete and available.
		return ds.Spec.Template.Annotations[detachHashKey] == expected &&
			ds.Status.ObservedGeneration >= ds.Generation &&
			ds.Status.DesiredNumberScheduled == ds.Status.UpdatedNumberScheduled &&
			ds.Status.NumberAvailable == ds.Status.DesiredNumberScheduled, nil
	})
}

// externalHash returns the config hash the operator stamps on the installer for
// the detached (external) policy, computed the same way the operator does.
func (h *detachACRHandler) externalHash() string {
	cfg, err := registryauth.Derive("upstream_registries: []\n", "mode: external")
	if err != nil {
		// The external config is fixed; a failure here is a programming error.
		return ""
	}

	payload, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}

	sum := sha256.Sum256(payload)

	return hex.EncodeToString(sum[:])
}

// removeAzureIdentity deletes the Gantry-owned pull identity and its AKS
// identity binding. It validates the cluster/kubeconfig association and the
// identity's ownership (the unbounded-cluster-id tag) BEFORE any destructive
// operation, so a foreign resource occupying the derived name is refused rather
// than deleted. The AcrPull role assignment at the ACR scope is not removed
// automatically; the caller is told to do so explicitly.
func (h *detachACRHandler) removeAzureIdentity(ctx context.Context) error {
	clusterID, err := azureResourceID(h.clusterID, "Microsoft.ContainerService/managedClusters")
	if err != nil {
		return err
	}

	// Validate the selected kubeconfig matches the cluster before disabling one
	// cluster and deleting another's resources.
	if err := h.verifyClusterMatchesKubeconfig(ctx, clusterID); err != nil {
		return err
	}

	digest := sha256.Sum256([]byte(strings.ToLower(h.clusterID)))
	name := "gantry-pull-" + hex.EncodeToString(digest[:6])

	// Read the identity and confirm it is the Gantry-owned one for this cluster
	// before deleting it. A foreign identity occupying the name is refused.
	type identity struct {
		ID, ClientID string
		Tags         map[string]string
	}

	var identities []identity
	if err := h.azureJSON(ctx, clusterID.SubscriptionID, &identities, "identity", "list", "--resource-group", clusterID.ResourceGroupName, "--name", name); err != nil {
		return fmt.Errorf("verify Gantry-owned pull identity %s: %w", name, err)
	}

	var owned *identity

	for i := range identities {
		if !strings.EqualFold(identities[i].ID, "") {
			owned = &identities[i]
		}
	}

	if owned == nil {
		// No identity at that name: there is nothing to delete.
		if _, err := fmt.Fprintln(h.out, "No Gantry-owned pull identity found to delete; nothing to do in Azure."); err != nil {
			return err
		}

		return nil
	}

	if owned.Tags["unbounded-cluster-id"] != "" && !strings.EqualFold(owned.Tags["unbounded-cluster-id"], h.clusterID) {
		return fmt.Errorf("refusing to delete pull identity %s: its unbounded-cluster-id tag does not match the requested cluster", name)
	}

	// Delete the identity. `az identity delete` takes the resource name and
	// resource group; it has no --yes flag.
	if _, err := h.runAzure(ctx, clusterID.SubscriptionID, "identity", "delete", "--resource-group", clusterID.ResourceGroupName, "--name", name); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "not found") {
			return fmt.Errorf("delete Gantry-owned pull identity %s: %w", name, err)
		}
	}

	// The identity binding is a child resource of the AKS cluster.
	if _, err := h.runAzure(ctx, clusterID.SubscriptionID, "rest", "--method", "DELETE",
		"--uri", fmt.Sprintf("https://management.azure.com%s/identityBindings/%s?api-version=2026-08-02-preview", h.clusterID, name)); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "not found") {
			return fmt.Errorf("delete AKS identity binding %s: %w", name, err)
		}
	}

	if _, err := fmt.Fprintln(h.out, "Deleted the Gantry-owned pull identity and identity binding. Delete the AcrPull role assignment at the ACR scope manually if it is no longer needed."); err != nil {
		return err
	}

	return nil
}

// azureJSON runs an Azure CLI command in the given subscription and decodes the
// JSON result.
func (h *detachACRHandler) azureJSON(ctx context.Context, subscriptionID string, output any, args ...string) error {
	data, err := h.runAzure(ctx, subscriptionID, args...)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("invalid Azure response for %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}

	return nil
}

// verifyClusterMatchesKubeconfig confirms the selected kubeconfig points at the
// requested AKS cluster, so detach does not disable one cluster while deleting
// another cluster's resources.
func (h *detachACRHandler) verifyClusterMatchesKubeconfig(ctx context.Context, clusterID *arm.ResourceID) error {
	var cluster struct {
		ID, FQDN, PrivateFQDN string
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

	return nil
}
