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

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
	projectsignal "github.com/Azure/unbounded/internal/signal"
	"github.com/Azure/unbounded/internal/version"
)

const controllerName = "project-signal-controller"

type config struct {
	namespace           string
	scriptNamespace     string
	authorityNamespace  string
	authorityName       string
	staticPackageDigest string
	runnerImage         string
	controllerImage     string
	adapterImage        string
	workflowDigest      string
	resolverImages      []string
	gateProtected       bool
	metricsAddress      string
	probeAddress        string
	leaderElect         bool
	maxConcurrentRuns   int
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
	cmd.Flags().StringVar(&cfg.scriptNamespace, "script-namespace", "project-signal", "Namespace containing immutable source script ConfigMaps")
	cmd.Flags().StringVar(&cfg.authorityNamespace, "authority-namespace", "project-signal-authority", "Operator-only namespace containing execution authority")
	cmd.Flags().StringVar(&cfg.authorityName, "authority-name", projectsignal.ExecutionAuthorityName, "Immutable execution authority ConfigMap name")
	cmd.Flags().StringVar(&cfg.staticPackageDigest, "static-package-digest", "", "Certified immutable static package SHA-256 digest")
	cmd.Flags().StringVar(&cfg.runnerImage, "runner-image", "", "Digest-pinned certification runner image")
	cmd.Flags().StringVar(&cfg.controllerImage, "controller-image", "", "Digest-pinned controller image recorded in evidence")
	cmd.Flags().StringVar(&cfg.adapterImage, "adapter-image", "", "Digest-pinned adapter image included in execution plans")
	cmd.Flags().StringVar(&cfg.workflowDigest, "workflow-template-digest", "", "SHA-256 digest of rendered WorkflowTemplate semantics")
	cmd.Flags().StringSliceVar(
		&cfg.resolverImages,
		"resolver-image",
		nil,
		"Digest-pinned resolver image included in phase-based execution plans; repeat for multiple resolvers",
	)
	cmd.Flags().BoolVar(
		&cfg.gateProtected,
		"qualification-gate-protected",
		false,
		"Allow protected qualification label projection after deployment verifies Node authorizer and NodeRestriction",
	)
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

	if cfg.namespace == "" || cfg.scriptNamespace == "" || cfg.authorityNamespace == "" || cfg.authorityName == "" {
		return errors.New("namespace, script namespace, authority namespace, and authority name are required")
	}

	if cfg.authorityNamespace == cfg.namespace || cfg.authorityNamespace == cfg.scriptNamespace {
		return errors.New("authority namespace must be distinct from workload and script namespaces")
	}

	if err := projectsignal.ValidateRunnerImage(cfg.runnerImage); err != nil {
		return err
	}

	if err := projectsignal.ValidateExecutionProvenance(
		cfg.controllerImage,
		cfg.adapterImage,
		cfg.workflowDigest,
	); err != nil {
		return err
	}

	for index, image := range cfg.resolverImages {
		if !signalv1.IsDigestPinnedImage(image) {
			return fmt.Errorf("resolver image %d must use an immutable sha256 digest", index)
		}
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}

	if err := signalv1.AddToScheme(scheme); err != nil {
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

	evidence, err := projectsignal.NewKubernetesEvidenceStore(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		cfg.namespace,
	)
	if err != nil {
		return err
	}

	if _, err := mgr.GetRESTMapper().RESTMapping(schema.GroupKind{Group: "argoproj.io", Kind: "Workflow"}, "v1alpha1"); err != nil {
		return fmt.Errorf("argo Workflows is required but Workflow.argoproj.io/v1alpha1 is unavailable: %w", err)
	}

	admission := &projectsignal.AdmissionReconciler{
		Client:              mgr.GetClient(),
		APIReader:           mgr.GetAPIReader(),
		Namespace:           cfg.namespace,
		ScriptNamespace:     cfg.scriptNamespace,
		AuthorityNamespace:  cfg.authorityNamespace,
		AuthorityName:       cfg.authorityName,
		SourceCommit:        version.GitCommit,
		StaticPackageDigest: cfg.staticPackageDigest,
		RunnerImage:         cfg.runnerImage,
		ControllerImage:     cfg.controllerImage,
		AdapterImage:        cfg.adapterImage,
		WorkflowDigest:      cfg.workflowDigest,
		ResolverImages:      cfg.resolverImages,
		Recorder:            mgr.GetEventRecorder(controllerName),
	}
	if err := admission.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register admission reconciler: %w", err)
	}

	runs := &projectsignal.RunReconciler{
		Client:              mgr.GetClient(),
		APIReader:           mgr.GetAPIReader(),
		Evidence:            evidence,
		MaxConcurrentRuns:   cfg.maxConcurrentRuns,
		ControllerImage:     cfg.controllerImage,
		AuthorityNamespace:  cfg.authorityNamespace,
		AuthorityName:       cfg.authorityName,
		SourceCommit:        version.GitCommit,
		StaticPackageDigest: cfg.staticPackageDigest,
		Recorder:            mgr.GetEventRecorder(controllerName),
	}
	if err := runs.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register run reconciler: %w", err)
	}

	qualification := &projectsignal.QualificationReconciler{
		Client:        mgr.GetClient(),
		APIReader:     mgr.GetAPIReader(),
		Evidence:      evidence,
		Namespace:     cfg.namespace,
		GateProtected: cfg.gateProtected,
	}
	if err := qualification.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register qualification reconciler: %w", err)
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
