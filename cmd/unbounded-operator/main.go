// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	apiregistrationv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/clusterinfo"
	"github.com/Azure/unbounded/internal/operator"
	"github.com/Azure/unbounded/internal/unbounded"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	cmd := newCommand(run)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newCommand(runFn func(context.Context, config) error) *cobra.Command {
	var cfg config

	cmd := &cobra.Command{
		Use:   "unbounded-operator",
		Short: "Controller for top-level Unbounded Site configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("reap-legacy-resources") {
				reapLegacyResources, err := envBoolDefault("UNBOUNDED_REAP_LEGACY_RESOURCES", true)
				if err != nil {
					return err
				}

				cfg.reapLegacyResources = reapLegacyResources
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return runFn(ctx, cfg)
		},
		Version: version.Version + " (commit: " + version.GitCommit + ")",
	}

	cmd.Flags().StringVar(&cfg.metricsAddr, "metrics-bind-address", ":8080", "Address for metrics endpoint")
	cmd.Flags().StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "Address for health probes")
	cmd.Flags().BoolVar(&cfg.leaderElection, "leader-elect", true, "Enable leader election")
	cmd.Flags().StringVar(&cfg.leaderElectionNamespace, "leader-elect-namespace", unbounded.SystemNamespace(), "Namespace for the leader election lease")
	cmd.Flags().StringVar(&cfg.namespace, "namespace", unbounded.SystemNamespace(), "Namespace the operator reconciles components into and migrates legacy state to")
	cmd.Flags().StringVar(&cfg.metalmanImage, "metalman-image", "", "Default metalman image")
	cmd.Flags().StringVar(&cfg.apiServerEndpoint, "api-server-endpoint", os.Getenv("UNBOUNDED_API_SERVER_ENDPOINT"), "Kubernetes API server endpoint advertised by machina; overrides auto-discovery from kube-public/cluster-info or the KUBERNETES_SERVICE_HOST FQDN (defaults to $UNBOUNDED_API_SERVER_ENDPOINT)")
	cmd.Flags().BoolVar(&cfg.reapLegacyResources, "reap-legacy-resources", true, "Translate legacy net-group Sites, migrate state into unbounded-system, and reap the pre-consolidation namespaces (defaults to $UNBOUNDED_REAP_LEGACY_RESOURCES or true)")
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	return cmd
}

type config struct {
	metricsAddr             string
	probeAddr               string
	leaderElection          bool
	leaderElectionNamespace string
	namespace               string
	metalmanImage           string
	apiServerEndpoint       string
	reapLegacyResources     bool
}

// envBoolDefault returns the boolean value of the named environment variable, or
// fallback when it is unset. Set values must be valid booleans.
func envBoolDefault(name string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}

	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", name, err)
	}

	return value, nil
}

// resolveAPIServerEndpoint returns the Kubernetes API server endpoint advertised
// to provisioned machines. An explicit override (from --api-server-endpoint /
// $UNBOUNDED_API_SERVER_ENDPOINT) always wins; otherwise it is discovered from
// the standard kube-public/cluster-info ConfigMap, and failing that from the
// KUBERNETES_SERVICE_HOST FQDN (managed control planes such as AKS do not
// publish cluster-info; the kubernetes.azure.com/set-kube-service-host-fqdn pod
// annotation makes that env the public API FQDN). Machina and metalman cannot
// function without an endpoint, so an empty override with no discoverable value
// is a hard error.
func resolveAPIServerEndpoint(ctx context.Context, override string, clientset kubernetes.Interface) (string, error) {
	if override != "" {
		return override, nil
	}

	endpoint, err := clusterinfo.DiscoverURL(ctx, clientset)
	if err != nil {
		return "", fmt.Errorf("no API server endpoint configured (set --api-server-endpoint or $UNBOUNDED_API_SERVER_ENDPOINT) and endpoint discovery failed: %w", err)
	}

	return endpoint, nil
}

func run(ctx context.Context, cfg config) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	scheme := runtimeScheme()

	namespace := cfg.namespace
	if namespace == "" {
		namespace = unbounded.SystemNamespace()
	}

	restConfig := ctrl.GetConfigOrDie()

	// Resolve the API server endpoint advertised to provisioned machines before
	// wiring the reconciler/reaper: an explicit override wins, otherwise it is
	// discovered from kube-public/cluster-info. Fail hard if neither is
	// available since machina and metalman require it.
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create kubernetes clientset: %w", err)
	}

	apiServerEndpoint, err := resolveAPIServerEndpoint(ctx, cfg.apiServerEndpoint, clientset)
	if err != nil {
		return err
	}

	cfg.apiServerEndpoint = apiServerEndpoint

	// Install/upgrade the CRDs before starting the manager: the typed Site
	// informer cannot sync until the Site CRD is served, and the operator owns
	// CRD lifecycle so a cluster can be maintained by applying the operator
	// manifests alone. This runs on every start and is idempotent.
	bootstrapClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create bootstrap client: %w", err)
	}

	if err := operator.BootstrapCRDs(ctx, bootstrapClient); err != nil {
		return fmt.Errorf("bootstrap CRDs: %w", err)
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
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

	if err := mgr.Add(&operator.CRDMaintainer{Client: bootstrapClient}); err != nil {
		return fmt.Errorf("add CRD maintainer: %w", err)
	}

	if err := (&operator.SiteReconciler{
		Client:    mgr.GetClient(),
		Scheme:    scheme,
		Namespace: namespace,
		Registry:  operator.DefaultRegistry(),
		Config: operator.Config{
			MetalmanImage:     cfg.metalmanImage,
			APIServerEndpoint: cfg.apiServerEndpoint,
		},
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup Site controller: %w", err)
	}

	if cfg.reapLegacyResources {
		reaper := &operator.LegacyReaper{
			Client:            mgr.GetClient(),
			APIReader:         mgr.GetAPIReader(),
			TargetNamespace:   namespace,
			LegacyNamespaces:  operator.LegacyNamespaces,
			SkipSecretNames:   map[string]struct{}{"unbounded-net-serving-cert": {}},
			CopyConfigMaps:    []string{"machina-config", "unbounded-net-config"},
			APIServerEndpoint: cfg.apiServerEndpoint,
			Recorder:          mgr.GetEventRecorder("unbounded-operator-reaper"),
		}
		if err := reaper.SetupWithManager(mgr); err != nil {
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
