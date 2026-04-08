package app

import (
	"log/slog"
	"net/http"
	"time"

	"k8s.io/client-go/kubernetes"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	unboundedCNINamespace      = "unbounded-net"
	unboundedCNIControllerName = "unbounded-net-controller"
)

// installUnboundedCNI installs unbounded-net manifests and waits for the
// controller to become running.
type installUnboundedCNI struct {
	*kubeComponentInstaller
}

func newInstallUnboundedCNI(fileOrURL string, httpClient *http.Client, logger *slog.Logger, kubeResourcesCli client.Client, kubeCli kubernetes.Interface) *installUnboundedCNI {
	return &installUnboundedCNI{
		kubeComponentInstaller: &kubeComponentInstaller{
			fileOrURL:        fileOrURL,
			httpClient:       httpClient,
			logger:           logger,
			kubeResourcesCli: kubeResourcesCli,
			kubeCli:          kubeCli,
			namespace:        unboundedCNINamespace,
			controllerName:   unboundedCNIControllerName,
			waitTimeout:      5 * time.Minute,
			pollInterval:     5 * time.Second,
			tempPrefix:       "unbounded-net",
		},
	}
}
