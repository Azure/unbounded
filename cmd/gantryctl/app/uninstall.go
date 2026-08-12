// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type uninstallOptions struct {
	root            *rootOptions
	deleteNamespace bool
}

func newUninstallCommand(root *rootOptions) *cobra.Command {
	options := &uninstallOptions{root: root}
	command := &cobra.Command{
		Use:   "uninstall",
		Short: "Restore node routes and remove standalone Gantry",
		Example: `  gantryctl uninstall
  gantryctl uninstall --delete-namespace`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return options.run(command.Context(), command.OutOrStdout())
		},
	}
	command.Flags().BoolVar(&options.deleteNamespace, "delete-namespace", false, "Delete the installation namespace after removing Gantry resources")

	return command
}

func (o *uninstallOptions) run(ctx context.Context, output io.Writer) error {
	clients, err := o.root.clusterClients()
	if err != nil {
		return err
	}

	if err := withRegistryMutationLock(ctx, clients.kube, o.root.namespace, o.root.timeout, func(lockCtx context.Context) error {
		return o.runLocked(lockCtx, output, clients)
	}); err != nil {
		return err
	}

	if err := clients.kube.CoordinationV1().Leases(o.root.namespace).Delete(ctx, registryMutationLeaseName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete registry mutation Lease: %w", err)
	}

	return nil
}

func (o *uninstallOptions) runLocked(ctx context.Context, output io.Writer, clients *clusterClients) error {
	store, err := loadRegistryStore(ctx, clients.kube, o.root.namespace)
	if err != nil {
		return err
	}

	if o.deleteNamespace {
		namespace, err := clients.kube.CoreV1().Namespaces().Get(ctx, o.root.namespace, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get standalone namespace: %w", err)
		}

		if err == nil && namespace.Labels["app.kubernetes.io/managed-by"] != fieldManager {
			return fmt.Errorf("namespace %s is not owned by gantryctl; refusing --delete-namespace", o.root.namespace)
		}
	}

	agent, agentErr := clients.kube.AppsV1().DaemonSets(o.root.namespace).Get(ctx, agentDaemonSetName, metav1.GetOptions{})
	if agentErr != nil && !apierrors.IsNotFound(agentErr) {
		return fmt.Errorf("get agent DaemonSet: %w", agentErr)
	}

	if agentErr == nil && agent.Labels["app.kubernetes.io/managed-by"] != fieldManager {
		return fmt.Errorf("DaemonSet %s/%s is not owned by gantryctl", o.root.namespace, agentDaemonSetName)
	}

	nodeConfig, nodeConfigErr := clients.kube.AppsV1().DaemonSets(o.root.namespace).Get(ctx, nodeConfigDaemonSetName, metav1.GetOptions{})

	nodeConfigExists := nodeConfigErr == nil
	if nodeConfigErr != nil && !apierrors.IsNotFound(nodeConfigErr) {
		return fmt.Errorf("get node-config DaemonSet: %w", nodeConfigErr)
	}

	if nodeConfigExists && nodeConfig.Labels["app.kubernetes.io/managed-by"] != fieldManager {
		return fmt.Errorf("DaemonSet %s/%s is not owned by gantryctl", o.root.namespace, nodeConfigDaemonSetName)
	}

	if len(store.routes.Registries) > 0 {
		if !nodeConfigExists {
			return errors.New("cannot restore node routes because the node-config DaemonSet is unavailable")
		}

		snapshot := store.snapshot()

		store.routes.Registries = nil
		if err := applyRouteState(ctx, clients.kube, o.root.namespace, store); err != nil {
			return errors.Join(err, rollbackRouteSnapshot(ctx, clients.kube, o.root.namespace, o.root.timeout, snapshot))
		}

		if err := waitForDaemonSet(ctx, clients.kube, o.root.namespace, nodeConfigDaemonSetName, o.root.timeout); err != nil {
			return errors.Join(fmt.Errorf("wait for node route restoration: %w", err), rollbackRouteSnapshot(ctx, clients.kube, o.root.namespace, o.root.timeout, snapshot))
		}
	}

	if nodeConfigExists {
		if err := waitForDaemonSet(ctx, clients.kube, o.root.namespace, nodeConfigDaemonSetName, o.root.timeout); err != nil {
			return fmt.Errorf("verify restored node routes before uninstall: %w", err)
		}
	}

	deleteOptions := metav1.DeleteOptions{}
	for _, name := range []string{nodeConfigDaemonSetName, agentDaemonSetName} {
		if err := clients.kube.AppsV1().DaemonSets(o.root.namespace).Delete(ctx, name, deleteOptions); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete DaemonSet %s: %w", name, err)
		}
	}

	for _, name := range []string{agentConfigMapName, nodeRoutesConfigMapName} {
		if err := clients.kube.CoreV1().ConfigMaps(o.root.namespace).Delete(ctx, name, deleteOptions); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete ConfigMap %s: %w", name, err)
		}
	}

	if err := clients.kube.CoreV1().Secrets(o.root.namespace).Delete(ctx, registryCredentialsName, deleteOptions); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete registry credentials: %w", err)
	}

	if err := clients.kube.RbacV1().RoleBindings(o.root.namespace).Delete(ctx, "gantry-standalone-agent", deleteOptions); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete standalone RoleBinding: %w", err)
	}

	if err := clients.kube.RbacV1().Roles(o.root.namespace).Delete(ctx, "gantry-standalone-agent", deleteOptions); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete standalone Role: %w", err)
	}

	if err := clients.kube.CoreV1().ServiceAccounts(o.root.namespace).Delete(ctx, "gantry-standalone", deleteOptions); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete standalone ServiceAccount: %w", err)
	}

	if err := clients.kube.RbacV1().ClusterRoleBindings().Delete(ctx, "gantry-standalone-agent", deleteOptions); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete standalone ClusterRoleBinding: %w", err)
	}

	if err := clients.kube.RbacV1().ClusterRoles().Delete(ctx, "gantry-standalone-agent", deleteOptions); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete standalone ClusterRole: %w", err)
	}

	if o.deleteNamespace {
		namespace, err := clients.kube.CoreV1().Namespaces().Get(ctx, o.root.namespace, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("get standalone namespace: %w", err)
		}

		if err == nil && namespace.Labels["app.kubernetes.io/managed-by"] != fieldManager {
			return fmt.Errorf("namespace %s is not owned by gantryctl; refusing --delete-namespace", o.root.namespace)
		}

		if err := clients.kube.CoreV1().Namespaces().Delete(ctx, o.root.namespace, deleteOptions); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete standalone namespace: %w", err)
		}
	}

	return writeOutputf(output, "Standalone Gantry removed; all managed containerd routes were restored first.\n")
}
