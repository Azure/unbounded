// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Command azure-machine-ops-controller is the machine-ops controller for
// Azure. It is also the reference for writing one for another provider: the
// provider in internal/azurevm implements the callbacks of pkg/machineops,
// and this file hands its registration to pkg/machineops/controller, which
// owns the MachineOperation, MachineClass and MachineAllocation reconcilers.
//
// A controller for another provider copies this command and replaces
// azureProviders. Apart from the provider itself, the only packages this file
// cannot import from outside this repository are internal/systemns, for the
// default namespace of the flags, and internal/version, for the version
// string. The README lists what else in this directory is tied to the
// repository.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/azure-machine-ops-controller/internal/azurevm"
	"github.com/Azure/unbounded/internal/systemns"
	"github.com/Azure/unbounded/internal/version"
	"github.com/Azure/unbounded/pkg/machineops"
	machineopscontroller "github.com/Azure/unbounded/pkg/machineops/controller"
)

const (
	controllerName    = "azure-machine-ops-controller"
	scopeFallbackName = "scope"

	errCapacityLeaderElection    = "--enable-capacity requires --leader-elect: the capacity controllers must run under a leader-election lease"
	errConsumerKindsCapacity     = "--allocation-consumer-kind requires --enable-capacity"
	errCapacityAPIServerEndpoint = "--enable-capacity requires --api-server-endpoint: allocated hosts bootstrap against it"
)

func main() {
	if err := newCommand(run).Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// newCommand builds the controller command. runController receives the
// parsed flags.
func newCommand(runController func(context.Context, config) error) *cobra.Command {
	var cfg config

	cmd := &cobra.Command{
		Use:   controllerName,
		Short: "Machine-ops controller for Azure",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return runController(ctx, cfg)
		},
		Version: version.Version + " (commit: " + version.GitCommit + ")",
	}

	cmd.Flags().StringVar(&cfg.metricsAddr, "metrics-bind-address", ":8080", "Address for metrics endpoint")
	cmd.Flags().StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "Address for health probes")
	cmd.Flags().BoolVar(&cfg.leaderElection, "leader-elect", true, "Enable leader election")
	cmd.Flags().StringVar(&cfg.leaderElectionNamespace, "leader-elect-namespace", systemns.Name(), "Namespace for the leader election lease")
	cmd.Flags().StringVar(&cfg.credentialSecretNamespace, "credential-secret-namespace", systemns.Name(), "Namespace containing MachineOperationCredential referenced Secrets")
	cmd.Flags().IntVar(&cfg.maxConcurrentReconciles, "max-concurrent-reconciles", 10, "Maximum concurrent MachineOperation reconciles")
	cmd.Flags().StringVar(&cfg.apiServerEndpoint, "api-server-endpoint", "", "Kubernetes API server endpoint used in host replacement bootstrap config")
	cmd.Flags().StringVar(&cfg.siteName, "site", "", "Limit this controller to Machines of one Site. Deployments must not have overlapping scopes")
	cmd.Flags().StringVar(&cfg.providerName, "provider", "", "Limit this controller to one of its providers. Deployments must not have overlapping scopes")
	cmd.Flags().BoolVar(&cfg.experimentalContainerIsolation, "experimental-container-isolation", false,
		"Enable the experimental container isolation mode for HostReplace: opt the replacement agent configuration in "+
			"(Experimental.ContainerIsolation); without it a HostReplace of a Machine that declares spec.isolation container fails")

	cmd.Flags().BoolVar(&cfg.enableCapacity, "enable-capacity", false,
		"Run the MachineClass and MachineAllocation capacity controllers for providers that declare capacity. "+
			"Requires --leader-elect, the capacity RBAC bindings and the allocation admission policies")
	cmd.Flags().StringVar(&cfg.providerConfigNamespace, "provider-config-namespace", systemns.Name(),
		"Namespace containing MachineClass provider-config and bootstrap files ConfigMaps; used with --enable-capacity")
	cmd.Flags().StringArrayVar(&cfg.allocationConsumerKinds, "allocation-consumer-kind", nil,
		"Cluster-scoped consumer kind, as group/version/Kind, whose termination and loss the MachineAllocation controller observes, "+
			"for example karpenter.sh/v1/NodeClaim. Repeatable; requires --enable-capacity. Empty means consumer loss is never observed")

	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	return cmd
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

	experimentalContainerIsolation bool

	enableCapacity          bool
	providerConfigNamespace string
	allocationConsumerKinds []string
}

func run(ctx context.Context, cfg config) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	capacity, err := capacityOptions(cfg)
	if err != nil {
		return err
	}

	providers, err := configuredProviders(cfg.providerName)
	if err != nil {
		return err
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

	if err := addControllers(mgr, cfg, providers, capacity); err != nil {
		return err
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	ctrl.Log.Info("starting "+controllerName,
		"site", cfg.siteName,
		"provider", cfg.providerName,
		"capacity", cfg.enableCapacity,
		"allocationConsumerKinds", cfg.allocationConsumerKinds,
		"leaderElectionID", leaderElectionID(cfg))

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("start manager: %w", err)
	}

	return nil
}

// addControllers registers the MachineOperation controller and, when capacity
// is set, the MachineClass and MachineAllocation controllers for the same
// providers and scope.
func addControllers(
	mgr ctrl.Manager,
	cfg config,
	providers []*machineops.Provider,
	capacity *machineopscontroller.CapacityOptions,
) error {
	if err := machineopscontroller.AddToManager(mgr, providers, machineopscontroller.Options{
		SiteName:                  cfg.siteName,
		ProviderName:              cfg.providerName,
		MaxConcurrentReconciles:   cfg.maxConcurrentReconciles,
		APIServerEndpoint:         cfg.apiServerEndpoint,
		CredentialSecretNamespace: cfg.credentialSecretNamespace,

		ExperimentalContainerIsolation: cfg.experimentalContainerIsolation,
	}); err != nil {
		return err
	}

	if capacity == nil {
		return nil
	}

	if err := machineopscontroller.AddCapacityToManager(mgr, providers, *capacity); err != nil {
		return fmt.Errorf("add capacity controllers: %w", err)
	}

	return nil
}

// azureProviders returns every Azure provider this controller can serve.
// Each compute mode is its own provider with its own name: a provider for
// virtual machine scale sets would be registered here next to the one for
// single virtual machines, and --provider would select between them.
func azureProviders() ([]*machineops.Provider, error) {
	azureVM, err := (&azurevm.Provider{}).Registration()
	if err != nil {
		return nil, fmt.Errorf("register Azure VM machine-ops provider: %w", err)
	}

	return []*machineops.Provider{azureVM}, nil
}

// configuredProviders returns the providers to serve: every Azure provider,
// or only the one named providerName.
func configuredProviders(providerName string) ([]*machineops.Provider, error) {
	providers, err := azureProviders()
	if err != nil {
		return nil, err
	}

	if providerName == "" {
		return providers, nil
	}

	i := slices.IndexFunc(providers, func(provider *machineops.Provider) bool {
		return provider.Name() == providerName
	})
	if i < 0 {
		return nil, fmt.Errorf("unknown machine-ops provider %q", providerName)
	}

	return providers[i : i+1], nil
}

// capacityOptions returns the options for the capacity controllers, or nil
// when --enable-capacity is off. The capacity controllers share the scope and
// the credential Secret namespace of the MachineOperation controller, which
// the same manager must run because it executes HostAllocate and HostRelease.
func capacityOptions(cfg config) (*machineopscontroller.CapacityOptions, error) {
	if !cfg.enableCapacity {
		if len(cfg.allocationConsumerKinds) > 0 {
			return nil, errors.New(errConsumerKindsCapacity)
		}

		return nil, nil
	}

	if !cfg.leaderElection {
		return nil, errors.New(errCapacityLeaderElection)
	}

	// HostAllocate renders the host's bootstrap config, which needs the API
	// server endpoint. Without it every allocation would stall in a retry
	// loop that no status reports.
	if strings.TrimSpace(cfg.apiServerEndpoint) == "" {
		return nil, errors.New(errCapacityAPIServerEndpoint)
	}

	if strings.TrimSpace(cfg.providerConfigNamespace) == "" {
		return nil, errors.New("--provider-config-namespace is required with --enable-capacity")
	}

	if strings.TrimSpace(cfg.credentialSecretNamespace) == "" {
		return nil, errors.New("--credential-secret-namespace is required with --enable-capacity")
	}

	consumerKinds, err := parseConsumerKinds(cfg.allocationConsumerKinds)
	if err != nil {
		return nil, err
	}

	return &machineopscontroller.CapacityOptions{
		ProviderName:               cfg.providerName,
		SiteName:                   cfg.siteName,
		CredentialSecretNamespace:  cfg.credentialSecretNamespace,
		ProviderConfigNamespace:    cfg.providerConfigNamespace,
		EnableAllocationController: true,
		AllocationConsumerKinds:    consumerKinds,
	}, nil
}

// parseConsumerKinds parses each value as group/version/Kind, or version/Kind
// for the core group.
func parseConsumerKinds(values []string) ([]schema.GroupVersionKind, error) {
	var kinds []schema.GroupVersionKind

	for _, value := range values {
		parts := strings.Split(strings.TrimSpace(value), "/")
		if len(parts) == 2 {
			parts = append([]string{""}, parts...)
		}

		if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
			return nil, fmt.Errorf("--allocation-consumer-kind %q must be group/version/Kind", value)
		}

		kind := schema.GroupVersionKind{Group: parts[0], Version: parts[1], Kind: parts[2]}

		for _, existing := range kinds {
			if existing.GroupKind() == kind.GroupKind() {
				return nil, fmt.Errorf("--allocation-consumer-kind %q repeats %s", value, kind.GroupKind())
			}
		}

		kinds = append(kinds, kind)
	}

	return kinds, nil
}

func leaderElectionID(cfg config) string {
	if cfg.siteName == "" && cfg.providerName == "" {
		return controllerName
	}

	// Deployments with different scopes must not contend for the same lease:
	// each needs its own active leader. The scope may name a provider, a
	// Site, or both.
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
