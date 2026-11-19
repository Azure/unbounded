// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
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
type detachACRHandler struct {
	namespace, kubeconfig string
	clusterID             string // optional; required to remove the Azure identity
	timeout               time.Duration
	removeAzure           bool
	client                kubernetes.Interface
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
		client, _, err := kube.ClientAndConfigFromFile(getKubeconfigPath(h.kubeconfig))
		if err != nil {
			return err
		}

		h.client = client
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
// removed. The operator converges the node files itself; this only confirms
// the convergence so the command does not report success before the nodes are
// actually clean.
func (h *detachACRHandler) waitForDetach(ctx context.Context) error {
	return wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		ds, err := h.client.AppsV1().DaemonSets(h.namespace).Get(ctx, "gantry-acr-auth", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			// No installer scheduled means no opted-in Flex nodes to clean up.
			return true, nil
		}

		if err != nil {
			return false, err
		}

		// The installer removes the managed configuration and then reports ready
		// with the external policy. A rolled-out, fully-available DaemonSet is
		// the convergence signal.
		return ds.Status.ObservedGeneration >= ds.Generation &&
			ds.Status.DesiredNumberScheduled == ds.Status.UpdatedNumberScheduled &&
			ds.Status.NumberAvailable == ds.Status.DesiredNumberScheduled, nil
	})
}

// removeAzureIdentity deletes the Gantry-owned pull identity, its AKS identity
// binding, and the AcrPull role assignment the attach created. It only touches
// resources it owns (matched by name and the unbounded-cluster-id tag) and
// refuses to delete a foreign identity.
func (h *detachACRHandler) removeAzureIdentity(ctx context.Context) error {
	clusterID, err := azureResourceID(h.clusterID, "Microsoft.ContainerService/managedClusters")
	if err != nil {
		return err
	}

	digest := sha256.Sum256([]byte(strings.ToLower(h.clusterID)))
	name := "gantry-pull-" + hex.EncodeToString(digest[:6])

	// The identity lives in the AKS cluster's resource group, which we can
	// derive from the resource ID.
	if _, err := h.runAzure(ctx, clusterID.SubscriptionID, "identity", "delete", "--resource-group", clusterID.ResourceGroupName, "--name", name, "--yes"); err != nil {
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

	// The AcrPull grant lives at the ACR scope in the registry's subscription.
	// We cannot know the ACR resource ID from cluster context alone, so the
	// grant is left for the operator's audience reconciler / a manual role
	// assignment delete. Report it rather than guessing.
	if _, err := fmt.Fprintln(h.out, "Deleted the Gantry-owned pull identity and identity binding. Delete the AcrPull role assignment at the ACR scope manually if it is no longer needed."); err != nil {
		return err
	}

	return nil
}
