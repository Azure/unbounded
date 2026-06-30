// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(certificatesv1.AddToScheme(scheme))
	utilruntime.Must(unboundedv1alpha3.AddToScheme(scheme))
}

// RunManager runs the controller manager.
func RunManager(ctx context.Context, cfg Config) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  machinaCacheOptions(),
		Metrics:                metricsserver.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress: cfg.ProbeAddr,
		LeaderElection:         cfg.EnableLeaderElection,
		LeaderElectionID:       "machina-controller",
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	// Build a standard kubernetes clientset so we can query core resources
	// (ConfigMaps, Services, Nodes) that are outside the controller-runtime
	// cache scope.
	restCfg := ctrl.GetConfigOrDie()

	kubeClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create kubernetes clientset: %w", err)
	}

	// Resolve cluster-level values once at startup. These rarely change and
	// are threaded into every bootstrap script invocation.
	clusterInfo, err := ResolveClusterInfo(ctx, cfg, kubeClient)
	if err != nil {
		return fmt.Errorf("resolve cluster info: %w", err)
	}

	// Setup Machine controller — handles both reachability and provisioning.
	if err := setupMachineFieldIndexes(ctx, mgr.GetFieldIndexer()); err != nil {
		return fmt.Errorf("setup Machine field indexes: %w", err)
	}

	if err := (&MachineReconciler{
		Client:                      mgr.GetClient(),
		Scheme:                      mgr.GetScheme(),
		ClusterInfo:                 clusterInfo,
		MaxConcurrentReconciles:     cfg.MaxConcurrentReconciles,
		ProvisioningTimeoutDuration: cfg.ProvisioningTimeout,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup Machine controller: %w", err)
	}

	// Setup MachineConfiguration controller - manages versioned config snapshots.
	if err := (&MachineConfigurationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup MachineConfiguration controller: %w", err)
	}

	// Setup Machine configuration binding controller - resolves configurationRef
	// from explicit refs or MachineConfiguration selectors.
	if err := (&MachineConfigurationBindingReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup Machine configuration binding controller: %w", err)
	}

	daemonCSRApprover, err := NewDaemonCSRApprover(mgr.GetClient(), kubeClient)
	if err != nil {
		return fmt.Errorf("create daemon CSR approver controller: %w", err)
	}

	if err := daemonCSRApprover.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup daemon CSR approver controller: %w", err)
	}

	// Add health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	ctrl.Log.Info("Starting manager")

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("start manager: %w", err)
	}

	return nil
}

func machinaCacheOptions() cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			// Limit Secret caching to bootstrap tokens in kube-system and
			// Machina-managed secrets in the controller namespace.
			&corev1.Secret{}: {
				Namespaces: map[string]cache.Config{
					metav1.NamespaceSystem:         {},
					SecretNamespaceUnboundedSystem: {},
				},
			},
		},
	}
}

func setupMachineFieldIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &unboundedv1alpha3.Machine{}, machineNodeRefNameField, func(obj client.Object) []string {
		machine, ok := obj.(*unboundedv1alpha3.Machine)
		if !ok || machine.Spec.Kubernetes == nil || machine.Spec.Kubernetes.NodeRef == nil || machine.Spec.Kubernetes.NodeRef.Name == "" {
			return nil
		}

		return []string{machine.Spec.Kubernetes.NodeRef.Name}
	}); err != nil {
		return fmt.Errorf("index Machine node ref: %w", err)
	}

	return nil
}
