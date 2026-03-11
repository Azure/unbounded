package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	machinav1alpha2 "github.com/project-unbounded/unbounded-kube/cmd/machina/machina/api/v1alpha2"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(machinav1alpha2.AddToScheme(scheme))
}

// RunManager runs the controller manager.
func RunManager(ctx context.Context, cfg Config) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress: cfg.ProbeAddr,
		LeaderElection:         cfg.EnableLeaderElection,
		LeaderElectionID:       "machina-controller",
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	// Build a standard kubernetes clientset so we can query core resources
	// (ConfigMaps, Services, Nodes) that are outside the controller-runtime
	// cache scope.
	restCfg := ctrl.GetConfigOrDie()

	kubeClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create kubernetes clientset: %w", err)
	}

	// Resolve cluster-level values once at startup. These rarely change and
	// are threaded into every bootstrap script invocation.
	clusterInfo, err := ResolveClusterInfo(ctx, kubeClient)
	if err != nil {
		return fmt.Errorf("resolve cluster info: %w", err)
	}

	// Setup Machine controller — handles both reachability and provisioning.
	if err := (&MachineReconciler{
		Client:                  mgr.GetClient(),
		Scheme:                  mgr.GetScheme(),
		ClusterInfo:             clusterInfo,
		MaxConcurrentReconciles: cfg.MaxConcurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup Machine controller: %w", err)
	}

	// Add health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	ctrl.Log.Info("Starting manager")

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("start manager: %w", err)
	}

	return nil
}
