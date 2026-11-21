// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
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

// removePullGrant deletes the operator-owned cluster pull grant. It is
// ownership-gated: a grant not labeled as operator-owned is left in place.
func (h *detachACRHandler) removePullGrant(ctx context.Context) error {
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
