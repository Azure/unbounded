// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	osSignal "os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
	projectsignal "github.com/Azure/unbounded/internal/signal"
	"github.com/Azure/unbounded/internal/version"
)

const controllerName = "project-signal-controller"

type config struct {
	namespace         string
	scriptNamespace   string
	metricsAddress    string
	probeAddress      string
	leaderElect       bool
	maxConcurrentRuns int
}

func main() {
	var cfg config

	cmd := &cobra.Command{
		Use:   controllerName,
		Short: "Project Signal node certification controller",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := osSignal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return run(ctx, cfg)
		},
		Version: version.Version + " (commit: " + version.GitCommit + ")",
	}

	cmd.Flags().StringVar(&cfg.namespace, "namespace", "project-signal", "Namespace for CertificationRuns and Workflows")
	cmd.Flags().StringVar(&cfg.scriptNamespace, "script-namespace", "project-signal", "Namespace containing mutable source script ConfigMaps")
	cmd.Flags().StringVar(&cfg.metricsAddress, "metrics-bind-address", ":8080", "Address for metrics")
	cmd.Flags().StringVar(&cfg.probeAddress, "health-probe-bind-address", ":8081", "Address for health probes")
	cmd.Flags().BoolVar(&cfg.leaderElect, "leader-elect", true, "Enable leader election")
	cmd.Flags().IntVar(&cfg.maxConcurrentRuns, "max-concurrent-runs", 4, "Maximum concurrent CertificationRun reconciles")
	cmd.CompletionOptions.DisableDefaultCmd = true

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config) error {
	ctrl.SetLogger(zap.New())

	if cfg.namespace == "" || cfg.scriptNamespace == "" {
		return errors.New("namespace and script namespace are required")
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}

	if err := signalv1.AddToScheme(scheme); err != nil {
		return err
	}

	if err := machinav1.AddToScheme(scheme); err != nil {
		return err
	}

	watchedNamespaces := map[string]cache.Config{cfg.namespace: {}}
	watchedNamespaces[cfg.scriptNamespace] = cache.Config{}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		Cache:                         cache.Options{DefaultNamespaces: watchedNamespaces},
		Metrics:                       metricsserver.Options{BindAddress: cfg.metricsAddress},
		HealthProbeBindAddress:        cfg.probeAddress,
		LeaderElection:                cfg.leaderElect,
		LeaderElectionID:              controllerName,
		LeaderElectionNamespace:       cfg.namespace,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	evidence, err := projectsignal.NewKubernetesEvidenceStore(mgr.GetClient(), cfg.namespace)
	if err != nil {
		return err
	}

	if _, err := mgr.GetRESTMapper().RESTMapping(schema.GroupKind{Group: "argoproj.io", Kind: "Workflow"}, "v1alpha1"); err != nil {
		return fmt.Errorf("argo Workflows is required but Workflow.argoproj.io/v1alpha1 is unavailable: %w", err)
	}

	admission := &projectsignal.AdmissionReconciler{
		Client:          mgr.GetClient(),
		APIReader:       mgr.GetAPIReader(),
		Namespace:       cfg.namespace,
		ScriptNamespace: cfg.scriptNamespace,
		Recorder:        mgr.GetEventRecorder(controllerName),
	}
	if err := admission.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register admission reconciler: %w", err)
	}

	runs := &projectsignal.RunReconciler{
		Client:            mgr.GetClient(),
		APIReader:         mgr.GetAPIReader(),
		Namespace:         cfg.namespace,
		Evidence:          evidence,
		MaxConcurrentRuns: cfg.maxConcurrentRuns,
		Recorder:          mgr.GetEventRecorder(controllerName),
	}
	if err := runs.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register run reconciler: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	ctrl.Log.Info("starting Project Signal controller", "version", version.String(), "namespace", cfg.namespace)

	return mgr.Start(ctx)
}
