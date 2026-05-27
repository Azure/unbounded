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
	certificatesv1 "k8s.io/api/certificates/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Azure/unbounded/internal/version"
)

const (
	controllerName        = "agent-csr-approver"
	defaultSignerName     = "kubernetes.io/kube-apiserver-client"
	defaultDaemonGroup    = "unbounded-agent-daemons"
	defaultBootstrapGroup = "system:bootstrappers:unbounded-agent-daemons"
)

type config struct {
	metricsAddr             string
	probeAddr               string
	leaderElection          bool
	leaderElectionNamespace string
	signerName              string
	daemonGroup             string
	bootstrapGroup          string
}

func main() {
	var cfg config

	cmd := &cobra.Command{
		Use:   controllerName,
		Short: "Approve unbounded-agent daemon controller CSRs",
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
	cmd.Flags().StringVar(&cfg.signerName, "signer-name", defaultSignerName, "CSR signerName this approver handles")
	cmd.Flags().StringVar(&cfg.daemonGroup, "daemon-group", defaultDaemonGroup, "Additional daemon controller group required in approved CSRs")
	cmd.Flags().StringVar(&cfg.bootstrapGroup, "bootstrap-group", defaultBootstrapGroup, "Bootstrap token group allowed to request initial daemon controller certificates")

	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
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
		LeaderElectionID:              controllerName,
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

	if err := (&csrApproverReconciler{
		Client:     directClient,
		KubeClient: kubeClient,
		Evaluator: csrEvaluator{
			SignerName:     cfg.signerName,
			DaemonGroup:    cfg.daemonGroup,
			BootstrapGroup: cfg.bootstrapGroup,
		},
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup CSR approver controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	ctrl.Log.Info("starting agent-csr-approver", "signerName", cfg.signerName, "daemonGroup", cfg.daemonGroup)

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("start manager: %w", err)
	}

	return nil
}

func runtimeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(certificatesv1.AddToScheme(scheme))

	return scheme
}
