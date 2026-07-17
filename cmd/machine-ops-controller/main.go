// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops/providers/azurevm"
	"github.com/Azure/unbounded/internal/machineops/providers/ociinstance"
	"github.com/Azure/unbounded/internal/unbounded"
	"github.com/Azure/unbounded/internal/version"
	"github.com/Azure/unbounded/pkg/machineops"
	machineopscontroller "github.com/Azure/unbounded/pkg/machineops/controller"
)

const (
	controllerName    = "machine-ops-controller"
	scopeFallbackName = "scope"

	errSiteProviderPair = "--site and --provider must be set together"
)

func main() {
	var cfg config

	cmd := &cobra.Command{
		Use:   controllerName,
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
	cmd.Flags().StringVar(&cfg.leaderElectionNamespace, "leader-elect-namespace", unbounded.SystemNamespace(), "Namespace for the leader election lease")
	cmd.Flags().StringVar(&cfg.credentialSecretNamespace, "credential-secret-namespace", unbounded.SystemNamespace(), "Namespace containing MachineOperationCredential referenced Secrets")
	cmd.Flags().IntVar(&cfg.maxConcurrentReconciles, "max-concurrent-reconciles", 10, "Maximum concurrent MachineOperation reconciles")
	cmd.Flags().StringVar(&cfg.apiServerEndpoint, "api-server-endpoint", "", "Kubernetes API server endpoint used in host replacement bootstrap config")
	cmd.Flags().StringVar(&cfg.siteName, "site", "", "Site name this controller should operate on")
	cmd.Flags().StringVar(&cfg.providerName, "provider", "", "Provider name this controller should operate on")

	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	metricsAddr               string
	probeAddr                 string
	leaderElection            bool
	leaderElectionNamespace   string
	credentialSecretNamespace string
	maxConcurrentReconciles   int
	apiServerEndpoint         string
	siteName                  string
	providerName              string
}

func run(ctx context.Context, cfg config) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	if (cfg.siteName == "") != (cfg.providerName == "") {
		return errors.New(errSiteProviderPair)
	}

	restConfig := ctrl.GetConfigOrDie()
	scheme := runtimeScheme()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: cfg.metricsAddr},
		HealthProbeBindAddress:        cfg.probeAddr,
		LeaderElection:                cfg.leaderElection,
		LeaderElectionID:              leaderElectionID(cfg),
		LeaderElectionNamespace:       cfg.leaderElectionNamespace,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	providers, err := configuredProviders(cfg.providerName)
	if err != nil {
		return err
	}

	if err := machineopscontroller.AddToManager(mgr, providers, machineopscontroller.Options{
		SiteName:                  cfg.siteName,
		ProviderName:              cfg.providerName,
		MaxConcurrentReconciles:   cfg.maxConcurrentReconciles,
		APIServerEndpoint:         cfg.apiServerEndpoint,
		CredentialSecretNamespace: cfg.credentialSecretNamespace,
	}); err != nil {
		return err
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	ctrl.Log.Info("starting machine-ops-controller", "site", cfg.siteName, "provider", cfg.providerName, "leaderElectionID", leaderElectionID(cfg))

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("start manager: %w", err)
	}

	return nil
}

func configuredProviders(providerName string) ([]*machineops.Provider, error) {
	factories := map[string]func() (*machineops.Provider, error){
		unboundedv1alpha3.ExternalProviderAzureVM: func() (*machineops.Provider, error) {
			return (&azurevm.Provider{}).Registration()
		},
		unboundedv1alpha3.ExternalProviderOCIInstance: func() (*machineops.Provider, error) { return (&ociinstance.Provider{}).Registration() },
	}

	if providerName != "" {
		factory, ok := factories[providerName]
		if !ok {
			return nil, fmt.Errorf("unknown machine-ops provider %q", providerName)
		}

		provider, err := factory()
		if err != nil {
			return nil, fmt.Errorf("register machine-ops provider %q: %w", providerName, err)
		}

		return []*machineops.Provider{provider}, nil
	}

	azureProvider, err := factories[unboundedv1alpha3.ExternalProviderAzureVM]()
	if err != nil {
		return nil, fmt.Errorf("register Azure VM machine-ops provider: %w", err)
	}

	ociProvider, err := factories[unboundedv1alpha3.ExternalProviderOCIInstance]()
	if err != nil {
		return nil, fmt.Errorf("register OCI instance machine-ops provider: %w", err)
	}

	return []*machineops.Provider{azureProvider, ociProvider}, nil
}

func leaderElectionID(cfg config) string {
	if cfg.siteName == "" && cfg.providerName == "" {
		return controllerName
	}

	// Scoped controllers must not contend for the same lease. Each
	// provider/site deployment needs its own active leader.
	return controllerName + "-" + safeNamePart(cfg.providerName+"-"+cfg.siteName)
}

func safeNamePart(value string) string {
	var b strings.Builder

	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}

	prefix := strings.Trim(b.String(), "-")
	if prefix == "" {
		prefix = scopeFallbackName
	}

	if len(prefix) > 40 {
		prefix = strings.TrimRight(prefix[:40], "-")
	}

	sum := sha256.Sum256([]byte(value))

	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(sum[:])[:10])
}

func runtimeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(unboundedv1alpha3.AddToScheme(scheme))

	return scheme
}
