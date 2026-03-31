package app

import (
	"log/slog"
	"net/http"
	"time"

	"k8s.io/client-go/kubernetes"

	machinadeploy "github.com/project-unbounded/unbounded-kube/deploy/machina"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	machinaNamespace      = "machina-system"
	machinaControllerName = "machina-controller"
)

// installMachina installs machina manifests and waits for the controller to
// become running.
type installMachina struct {
	*kubeComponentInstaller
}

func newInstallMachina(fileOrURL string, httpClient *http.Client, logger *slog.Logger, kubeResourcesCli client.Client, kubeCli kubernetes.Interface) *installMachina {
	inst := &kubeComponentInstaller{
		fileOrURL:        fileOrURL,
		httpClient:       httpClient,
		logger:           logger,
		kubeResourcesCli: kubeResourcesCli,
		kubeCli:          kubeCli,
		namespace:        machinaNamespace,
		controllerName:   machinaControllerName,
		waitTimeout:      5 * time.Minute,
		pollInterval:     5 * time.Second,
		tempPrefix:       "machina",
	}

	// When no explicit manifests path/URL is provided, fall back to the
	// manifests embedded in the binary from deploy/machina/.
	if fileOrURL == "" {
		inst.embeddedFS = machinadeploy.Manifests
	}

	return &installMachina{kubeComponentInstaller: inst}
}
