// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Command rebound serves the NodeGrant API, which lets namespaces take
// atomic, time-limited grants on sets of nodes.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	serverhealthz "k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	aggregator "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
	"github.com/Azure/unbounded/internal/rebound/apiserver"
	"github.com/Azure/unbounded/internal/rebound/certs"
	"github.com/Azure/unbounded/internal/rebound/controller"
	"github.com/Azure/unbounded/internal/rebound/ledger"
	"github.com/Azure/unbounded/internal/unbounded"
	"github.com/Azure/unbounded/internal/version"
)

const (
	serviceName    = "rebound"
	apiServiceName = "v1alpha1." + rebound.GroupName
)

type config struct {
	namespace      string
	excluded       []string
	metricsAddr    string
	probeAddr      string
	leaderElection bool
	server         *apiserver.Options
}

func main() {
	cfg := config{server: apiserver.NewOptions()}

	cmd := &cobra.Command{
		Use:   "rebound",
		Short: "Serve the NodeGrant API for time-limited node grants",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return run(ctx, cfg)
		},
		Version:      version.Version + " (commit: " + version.GitCommit + ")",
		SilenceUsage: true,
	}

	flags := cmd.Flags()
	flags.StringVar(&cfg.namespace, "namespace", unbounded.SystemNamespace(), "Namespace holding the grant ledger, serving certificate, and leader election lease")
	flags.StringVar(&cfg.metricsAddr, "metrics-bind-address", ":8080", "Address for the metrics endpoint")
	flags.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "Address for health probes")
	flags.BoolVar(&cfg.leaderElection, "leader-elect", true, "Enable leader election for the controllers and certificate rotation")
	flags.StringSliceVar(&cfg.excluded, "excluded-namespaces", []string{"kube-system", unbounded.SystemNamespace()}, "Namespaces whose pods are never deleted from granted nodes")
	cfg.server.AddFlags(flags)

	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config) error {
	ctrl.SetLogger(zap.New())

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	agg, err := aggregator.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create aggregator client: %w", err)
	}

	// The controllers only read the ledger ConfigMap; do not cache every
	// ConfigMap in the cluster.
	ledgerOnly := cache.ByObject{
		Namespaces: map[string]cache.Config{cfg.namespace: {}},
		Field:      fields.OneTermEqualSelector("metadata.name", ledger.ConfigMapName),
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                        clientgoscheme.Scheme,
		Metrics:                       metricsserver.Options{BindAddress: cfg.metricsAddr},
		HealthProbeBindAddress:        cfg.probeAddr,
		LeaderElection:                cfg.leaderElection,
		LeaderElectionID:              "rebound",
		LeaderElectionNamespace:       cfg.namespace,
		LeaderElectionReleaseOnCancel: true,
		Cache:                         cache.Options{ByObject: map[client.Object]cache.ByObject{&corev1.ConfigMap{}: ledgerOnly}},
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	store := &ledger.Store{Client: kube, Namespace: cfg.namespace}
	if err := controller.Setup(mgr, controller.Options{Namespace: cfg.namespace, Store: store, ExemptNamespaces: cfg.excluded}); err != nil {
		return err
	}

	provider := certs.NewProvider(kube, cfg.namespace)
	rotator := &certs.Rotator{
		Client:         kube,
		Aggregator:     agg,
		Namespace:      cfg.namespace,
		ServiceName:    serviceName,
		APIServiceName: apiServiceName,
	}

	for _, r := range []interface {
		Start(context.Context) error
		NeedLeaderElection() bool
	}{provider, rotator} {
		if err := mgr.Add(r); err != nil {
			return fmt.Errorf("add runnable: %w", err)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}

	if err := mgr.AddReadyzCheck("serving-cert", provider.Check); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		if err := mgr.Start(ctx); err != nil {
			return fmt.Errorf("manager: %w", err)
		}

		return nil
	})

	g.Go(func() error {
		// The leader issues the serving certificate; every replica waits
		// for it before it can serve HTTPS.
		if err := provider.WaitReady(ctx); err != nil {
			return nil //nolint:nilerr // Shutting down before the certificate loaded.
		}

		server, err := cfg.server.NewServer(apiserver.ServerConfig{
			Client:    kube,
			Namespace: cfg.namespace,
			Cert:      provider,
			ReadyzChecks: []serverhealthz.HealthChecker{
				serverhealthz.NamedCheck("serving-cert", provider.Check),
			},
		})
		if err != nil {
			return fmt.Errorf("create API server: %w", err)
		}

		ctrl.Log.Info("starting rebound API server", "namespace", cfg.namespace, "maxTTL", cfg.server.MaxTTL)

		if err := server.Run(ctx); err != nil && ctx.Err() == nil {
			return fmt.Errorf("API server: %w", err)
		}

		return nil
	})

	return g.Wait()
}
