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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gantrydeploy "github.com/Azure/unbounded/deploy/gantry"
	"github.com/Azure/unbounded/internal/kube"
)

const (
	gantryNamespace     = "gantry-system"
	gantryDaemonSetName = "gantry"
)

// installGantry installs the gantry manifests and waits for the gantry
// DaemonSet to finish rolling out on every eligible node. Unlike the generic
// kubeComponentInstaller (which waits for a single controller pod), gantry runs
// one pod per node, so installation is only complete once the whole DaemonSet
// is ready.
type installGantry struct {
	*kubeComponentInstaller

	daemonSetName string
}

func newInstallGantry(fileOrURL string, httpClient *http.Client, logger *slog.Logger, kubeResourcesCli client.Client, kubeCli kubernetes.Interface) *installGantry {
	inst := &kubeComponentInstaller{
		fileOrURL:        fileOrURL,
		httpClient:       httpClient,
		logger:           logger,
		kubeResourcesCli: kubeResourcesCli,
		kubeCli:          kubeCli,
		namespace:        gantryNamespace,
		controllerName:   gantryDaemonSetName,
		waitTimeout:      5 * time.Minute,
		pollInterval:     5 * time.Second,
		tempPrefix:       "gantry",
	}

	// When no explicit manifests path/URL is provided, fall back to the
	// manifests embedded in the binary from deploy/gantry/.
	if fileOrURL == "" {
		inst.embeddedFS = gantrydeploy.Manifests
	}

	return &installGantry{kubeComponentInstaller: inst, daemonSetName: gantryDaemonSetName}
}

// run resolves and applies the gantry manifests, then waits for the gantry
// DaemonSet to finish rolling out. It does not reuse kubeComponentInstaller.run
// because that waits for a single controller pod; a DaemonSet needs a
// rollout-aware readiness check.
func (i *installGantry) run(ctx context.Context) error {
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
		return fmt.Errorf("waiting for %s daemonset rollout: %w", i.daemonSetName, err)
	}

	return nil
}

// waitForDaemonSetRollout polls the gantry DaemonSet until the controller has
// observed the latest spec and every scheduled pod is updated and ready, or
// until the timeout elapses.
func (i *installGantry) waitForDaemonSetRollout(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, i.timeout())
	defer cancel()

	ticker := time.NewTicker(i.interval())
	defer ticker.Stop()

	for {
		ds, err := i.kubeCli.AppsV1().DaemonSets(i.namespace).Get(ctx, i.daemonSetName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting daemonset %s/%s: %w", i.namespace, i.daemonSetName, err)
		}

		if daemonSetRolledOut(ds) {
			i.logger.Info("gantry daemonset rolled out",
				"namespace", i.namespace,
				"name", i.daemonSetName,
				"numberReady", ds.Status.NumberReady,
				"desiredNumberScheduled", ds.Status.DesiredNumberScheduled,
			)

			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for daemonset %s/%s rollout (ready %d/%d)",
				i.namespace, i.daemonSetName, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
		case <-ticker.C:
		}
	}
}

// daemonSetRolledOut reports whether a DaemonSet has finished rolling out: the
// controller has observed the latest spec, all scheduled pods run the updated
// revision, and every scheduled pod is ready. DesiredNumberScheduled of 0 is
// treated as not-yet-ready so callers keep waiting until at least one node has
// scheduled the agent (rather than returning success before the DaemonSet
// controller has computed its desired count).
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
