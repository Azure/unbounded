// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"log/slog"
	"net/http"
	"time"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machineopsdeploy "github.com/Azure/unbounded/deploy/machine-ops"
)

const (
	machineOpsNamespace      = "unbounded-kube"
	machineOpsControllerName = "machine-ops-controller"
)

// installMachineOps installs machine-ops-controller manifests and waits for
// the controller to become running.
type installMachineOps struct {
	*kubeComponentInstaller
}

func newInstallMachineOps(fileOrURL string, httpClient *http.Client, logger *slog.Logger, kubeResourcesCli client.Client, kubeCli kubernetes.Interface) *installMachineOps {
	inst := &kubeComponentInstaller{
		fileOrURL:        fileOrURL,
		httpClient:       httpClient,
		logger:           logger,
		kubeResourcesCli: kubeResourcesCli,
		kubeCli:          kubeCli,
		namespace:        machineOpsNamespace,
		controllerName:   machineOpsControllerName,
		waitTimeout:      5 * time.Minute,
		pollInterval:     5 * time.Second,
		tempPrefix:       "machine-ops",
	}

	// When no explicit manifests path/URL is provided, fall back to the
	// manifests embedded in the binary from deploy/machine-ops/.
	if fileOrURL == "" {
		inst.embeddedFS = machineopsdeploy.Manifests
	}

	return &installMachineOps{kubeComponentInstaller: inst}
}
