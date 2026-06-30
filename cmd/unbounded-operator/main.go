// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/operator"
	"github.com/Azure/unbounded/internal/unbounded"
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
	cmd.Flags().StringVar(&cfg.leaderElectionNamespace, "leader-elect-namespace", unbounded.SystemNamespace, "Namespace for the leader election lease")
	cmd.Flags().StringVar(&cfg.defaultNamespace, "default-namespace", operator.DefaultNamespace, "Default namespace for site components")
	cmd.Flags().StringVar(&cfg.netNamespace, "net-namespace", operator.DefaultNetNamespace, "Default namespace for unbounded-net")
	cmd.Flags().StringVar(&cfg.netControllerImage, "net-controller-image", "", "Default unbounded-net controller image")
	cmd.Flags().StringVar(&cfg.netNodeImage, "net-node-image", "", "Default unbounded-net node image")
	cmd.Flags().StringVar(&cfg.machinaImage, "machina-image", "", "Default machina controller image")
	cmd.Flags().StringVar(&cfg.metalmanImage, "metalman-image", "", "Default metalman image")
	cmd.Flags().StringVar(&cfg.storageImage, "storage-supervisor-image", "", "Default unbounded-storage-supervisor image")
	cmd.Flags().StringVar(&cfg.apiServerEndpoint, "api-server-endpoint", "", "Kubernetes API server endpoint advertised by machina")
	cmd.Flags().BoolVar(&cfg.reapLegacyResources, "reap-legacy-resources", false, "Migrate operator-owned state out of the legacy unbounded-kube/unbounded-net namespaces and delete the operator-owned resources left behind (does not delete the namespaces)")
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	cmd.AddCommand(migrateLegacyCommand())

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
	reapLegacyResources     bool
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

	if cfg.reapLegacyResources {
		if err := (&operator.LegacyReaper{
			Client:          mgr.GetClient(),
			TargetNamespace: cfg.defaultNamespace,
			SkipSecretNames: map[string]struct{}{"unbounded-net-serving-cert": {}},
			CopyConfigMaps:  []string{"machina-config"},
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setup legacy reaper: %w", err)
		}
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

// migrateLegacyCommand runs the legacy-namespace migrate-then-reap routine once
// to completion and exits. It performs the same copy/rewrite/reap steps as the
// always-on reaper but as a bounded, scriptable operation (suitable for a
// migration Job). It never deletes the legacy Namespace objects.
func migrateLegacyCommand() *cobra.Command {
	var (
		targetNamespace  string
		legacyNamespaces []string
		timeout          time.Duration
	)

	cmd := &cobra.Command{
		Use:   "migrate-legacy",
		Short: "Consolidate operator-owned state out of the legacy namespaces (one-shot)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			if timeout > 0 {
				var timeoutCancel context.CancelFunc

				ctx, timeoutCancel = context.WithTimeout(ctx, timeout)
				defer timeoutCancel()
			}

			ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

			cli, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: runtimeScheme()})
			if err != nil {
				return fmt.Errorf("create client: %w", err)
			}

			reaper := &operator.LegacyReaper{
				Client:           cli,
				TargetNamespace:  targetNamespace,
				LegacyNamespaces: legacyNamespaces,
				SkipSecretNames:  map[string]struct{}{"unbounded-net-serving-cert": {}},
				CopyConfigMaps:   []string{"machina-config"},
				Interval:         2 * time.Second,
			}

			return reaper.RunToCompletion(ctrl.LoggerInto(ctx, ctrl.Log))
		},
	}

	cmd.Flags().StringVar(&targetNamespace, "target-namespace", unbounded.SystemNamespace, "Namespace components are consolidated into")
	cmd.Flags().StringSliceVar(&legacyNamespaces, "legacy-namespaces", operator.LegacyNamespaces, "Legacy namespaces to drain")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "Overall timeout for the migration")

	return cmd
}
