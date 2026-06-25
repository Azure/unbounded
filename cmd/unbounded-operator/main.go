// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/operator"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	var cfg config

	cmd := &cobra.Command{
		Use:   "unbounded-operator",
		Short: "Controller for top-level Unbounded Site configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return run(ctx, cfg)
		},
		Version: version.Version + " (commit: " + version.GitCommit + ")",
	}

	cmd.Flags().StringVar(&cfg.metricsAddr, "metrics-bind-address", ":8080", "Address for metrics endpoint")
	cmd.Flags().StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "Address for health probes")
	cmd.Flags().BoolVar(&cfg.leaderElection, "leader-elect", true, "Enable leader election")
	cmd.Flags().StringVar(&cfg.leaderElectionNamespace, "leader-elect-namespace", "unbounded-kube", "Namespace for the leader election lease")
	cmd.Flags().StringVar(&cfg.defaultNamespace, "default-namespace", operator.DefaultNamespace, "Default namespace for site components")
	cmd.Flags().StringVar(&cfg.netNamespace, "net-namespace", operator.DefaultNetNamespace, "Default namespace for unbounded-net")
	cmd.Flags().StringVar(&cfg.netControllerImage, "net-controller-image", "", "Default unbounded-net controller image")
	cmd.Flags().StringVar(&cfg.netNodeImage, "net-node-image", "", "Default unbounded-net node image")
	cmd.Flags().StringVar(&cfg.machinaImage, "machina-image", "", "Default machina controller image")
	cmd.Flags().StringVar(&cfg.metalmanImage, "metalman-image", "", "Default metalman image")
	cmd.Flags().StringVar(&cfg.storageImage, "storage-supervisor-image", "", "Default unbounded-storage-supervisor image")
	cmd.Flags().StringVar(&cfg.apiServerEndpoint, "api-server-endpoint", "", "Kubernetes API server endpoint advertised by machina")
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	metricsAddr             string
	probeAddr               string
	leaderElection          bool
	leaderElectionNamespace string
	defaultNamespace        string
	netNamespace            string
	netControllerImage      string
	netNodeImage            string
	machinaImage            string
	metalmanImage           string
	storageImage            string
	apiServerEndpoint       string
}

func run(ctx context.Context, cfg config) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	scheme := runtimeScheme()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: cfg.metricsAddr},
		HealthProbeBindAddress:        cfg.probeAddr,
		LeaderElection:                cfg.leaderElection,
		LeaderElectionID:              "unbounded-operator",
		LeaderElectionNamespace:       cfg.leaderElectionNamespace,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	if err := (&operator.SiteReconciler{
		Client: mgr.GetClient(),
		Scheme: scheme,
		Config: operator.Config{
			DefaultNamespace:   cfg.defaultNamespace,
			NetNamespace:       cfg.netNamespace,
			NetControllerImage: cfg.netControllerImage,
			NetNodeImage:       cfg.netNodeImage,
			MachinaImage:       cfg.machinaImage,
			MetalmanImage:      cfg.metalmanImage,
			StorageImage:       cfg.storageImage,
			APIServerEndpoint:  cfg.apiServerEndpoint,
		},
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup Site controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	ctrl.Log.Info("starting unbounded-operator")

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("start manager: %w", err)
	}

	return nil
}

func runtimeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(rbacv1.AddToScheme(scheme))
	utilruntime.Must(admissionregistrationv1.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(apiregistrationv1.AddToScheme(scheme))
	utilruntime.Must(unboundedv1alpha3.AddToScheme(scheme))
	utilruntime.Must(unboundednetv1alpha1.AddToScheme(scheme))

	return scheme
}
