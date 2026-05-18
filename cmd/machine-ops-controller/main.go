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
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
	"github.com/Azure/unbounded/internal/machineops/providers/azurevm"
	"github.com/Azure/unbounded/internal/machineops/providers/ociinstance"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	var cfg config

	cmd := &cobra.Command{
		Use:   "machine-ops-controller",
		Short: "External MachineOperation controller",
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
	cmd.Flags().IntVar(&cfg.maxConcurrentReconciles, "max-concurrent-reconciles", 10, "Maximum concurrent MachineOperation reconciles")
	cmd.Flags().StringVar(&cfg.apiServerEndpoint, "api-server-endpoint", "", "Kubernetes API server endpoint used in host replacement bootstrap config")
	cmd.Flags().StringVar(&cfg.ociConfigFile, "oci-config-file", "", "Path to OCI config file for OCIInstance operations")
	cmd.Flags().StringVar(&cfg.ociConfigProfile, "oci-config-profile", "DEFAULT", "OCI config profile for OCIInstance operations")
	cmd.Flags().StringVar(&cfg.ociAuth, "oci-auth", "api_key", "OCI auth mode for OCIInstance operations: api_key or security_token")

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
	maxConcurrentReconciles int
	apiServerEndpoint       string
	ociConfigFile           string
	ociConfigProfile        string
	ociAuth                 string
}

func run(ctx context.Context, cfg config) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	restConfig := ctrl.GetConfigOrDie()
	scheme := runtimeScheme()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: cfg.metricsAddr},
		HealthProbeBindAddress:        cfg.probeAddr,
		LeaderElection:                cfg.leaderElection,
		LeaderElectionID:              "machine-ops-controller",
		LeaderElectionNamespace:       cfg.leaderElectionNamespace,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	directClient, err := client.New(restConfig, client.Options{Scheme: scheme, Mapper: mgr.GetRESTMapper()})
	if err != nil {
		return fmt.Errorf("create direct client: %w", err)
	}

	if err := (&machineops.MachineOperationReconciler{
		Client: directClient,
		Providers: []machineops.Provider{
			&azurevm.Provider{},
			&ociinstance.Provider{ConfigFile: cfg.ociConfigFile, ConfigProfile: cfg.ociConfigProfile, Auth: cfg.ociAuth},
		},
		MaxConcurrentReconciles: cfg.maxConcurrentReconciles,
		KubeClient:              kubeClient,
		APIServerEndpoint:       cfg.apiServerEndpoint,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup MachineOperation controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	ctrl.Log.Info("starting machine-ops-controller")

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("start manager: %w", err)
	}

	return nil
}

func runtimeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(unboundedv1alpha3.AddToScheme(scheme))

	return scheme
}
