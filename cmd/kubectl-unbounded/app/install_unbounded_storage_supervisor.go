// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagesupervisordeploy "github.com/Azure/unbounded/deploy/unbounded-storage-supervisor"
	"github.com/Azure/unbounded/internal/kube"
)

const (
	unboundedStorageSupervisorNamespace     = "unbounded-kube"
	unboundedStorageSupervisorDaemonSetName = "unbounded-storage-supervisor"
)

// installUnboundedStorageSupervisor installs the unbounded-storage supervisor
// manifests and waits for the supervisor DaemonSet to roll out on all nodes.
type installUnboundedStorageSupervisor struct {
	*kubeComponentInstaller

	daemonSetName string
}

func newInstallUnboundedStorageSupervisor(fileOrURL string, httpClient *http.Client, logger *slog.Logger, kubeResourcesCli client.Client, kubeCli kubernetes.Interface) *installUnboundedStorageSupervisor {
	inst := &kubeComponentInstaller{
		fileOrURL:        fileOrURL,
		httpClient:       httpClient,
		logger:           logger,
		kubeResourcesCli: kubeResourcesCli,
		kubeCli:          kubeCli,
		namespace:        unboundedStorageSupervisorNamespace,
		controllerName:   unboundedStorageSupervisorDaemonSetName,
		waitTimeout:      5 * time.Minute,
		pollInterval:     5 * time.Second,
		tempPrefix:       "unbounded-storage-supervisor",
	}

	// When no explicit manifests path/URL is provided, fall back to the
	// manifests embedded in the binary from deploy/unbounded-storage-supervisor/.
	if fileOrURL == "" {
		inst.embeddedFS = storagesupervisordeploy.Manifests
	}

	return &installUnboundedStorageSupervisor{
		kubeComponentInstaller: inst,
		daemonSetName:          unboundedStorageSupervisorDaemonSetName,
	}
}

func (i *installUnboundedStorageSupervisor) run(ctx context.Context) error {
	manifestDir, err := i.resolveManifests()
	if err != nil {
		return fmt.Errorf("resolving manifests: %w", err)
	}

	defer func() {
		if err := os.RemoveAll(manifestDir); err != nil {
			i.logger.Warn("failed to clean up temp manifest directory", "path", manifestDir, "error", err)
		}
	}()

	if err := kube.ApplyManifestsInDirectory(ctx, i.logger, i.kubeResourcesCli, fieldManagerID, manifestDir, i.skipPaths); err != nil {
		return fmt.Errorf("applying manifests: %w", err)
	}

	if err := i.waitForDaemonSetRollout(ctx); err != nil {
		return fmt.Errorf("waiting for %s DaemonSet to roll out: %w", i.daemonSetName, err)
	}

	return nil
}

func (i *installUnboundedStorageSupervisor) waitForDaemonSetRollout(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, i.timeout())
	defer cancel()

	ticker := time.NewTicker(i.interval())
	defer ticker.Stop()

	var lastStatus appsv1.DaemonSetStatus

	for {
		ds, err := i.kubeCli.AppsV1().DaemonSets(i.namespace).Get(ctx, i.daemonSetName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting DaemonSet %s/%s: %w", i.namespace, i.daemonSetName, err)
		}

		if err == nil {
			lastStatus = ds.Status
			if daemonSetRolledOut(ds) {
				i.logger.Info("DaemonSet is rolled out", "daemonset", i.daemonSetName, "namespace", i.namespace)
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf(
				"timed out waiting for DaemonSet %s/%s rollout: ready %d/%d, updated %d/%d",
				i.namespace,
				i.daemonSetName,
				lastStatus.NumberReady,
				lastStatus.DesiredNumberScheduled,
				lastStatus.UpdatedNumberScheduled,
				lastStatus.DesiredNumberScheduled,
			)
		case <-ticker.C:
		}
	}
}

func daemonSetRolledOut(ds *appsv1.DaemonSet) bool {
	if ds.Status.ObservedGeneration < ds.Generation {
		return false
	}

	if ds.Status.DesiredNumberScheduled == 0 {
		return false
	}

	return ds.Status.UpdatedNumberScheduled == ds.Status.DesiredNumberScheduled &&
		ds.Status.NumberReady == ds.Status.DesiredNumberScheduled
}
