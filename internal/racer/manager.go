// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer implements the Racer server using controller-runtime directly.
// Constructors compose only; serving requires locally validated replicated state.
package racer

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
)

type Application struct {
	Topology    *TopologyReconciler
	Keyring     *KeyringReconciler
	Server      *Server
	Lifecycle   *Lifecycle
	Replication *Replication
}

// Assemble performs no Kubernetes calls, I/O, cryptography, or goroutine startup.
// c supplies indexed discovery (configured by SetupWithManager in production);
// reader must bypass the cache for authorization and durable-state validation.
func Assemble(cfg Config, c client.Client, reader client.Reader) *Application {
	publications := NewPublications()
	publications.maxAge = cfg.snapshotMaxAge()
	lifecycle := newLifecycle(publications)
	trust := &Trust{maxAge: cfg.snapshotMaxAge()}
	issuer := &Issuer{APIReader: reader, Config: cfg, Trust: trust}
	bootstrap := &Bootstrap{Client: c, APIReader: reader, Config: cfg, Issuer: issuer}
	// Serialize credential admission/pruning with topology's authoritative read
	// and publication commit. Informer ordering alone cannot provide this gate.
	catalogGate := newCatalogGate()
	issuer.CatalogGate = catalogGate
	replication := &Replication{Config: cfg, Client: c, APIReader: reader, Publications: publications, Trust: trust, Lifecycle: lifecycle, CatalogGate: catalogGate}

	return &Application{
		Topology: &TopologyReconciler{
			Client:       c,
			APIReader:    reader,
			Config:       cfg,
			Publications: publications,
			Accepted:     make(AcceptedMembers),
			CatalogGate:  catalogGate,
			Trust:        trust,
		},
		Keyring: &KeyringReconciler{
			Client:      c,
			APIReader:   reader,
			Config:      cfg,
			Lifecycle:   lifecycle,
			CatalogGate: catalogGate,
			Trust:       trust,
		},
		Server: &Server{
			Config:       cfg,
			Trust:        trust,
			Bootstrap:    bootstrap,
			Publications: publications,
			Lifecycle:    lifecycle,
			Replication:  replication,
		},
		Lifecycle:   lifecycle,
		Replication: replication,
	}
}

func (a *Application) SetupWithManager(mgr ctrl.Manager) error {
	a.Lifecycle.waitForCacheSync = mgr.GetCache().WaitForCacheSync
	if err := mgr.Add(a.Lifecycle); err != nil {
		return fmt.Errorf("register serving lifecycle: %w", err)
	}

	if err := mgr.Add(a.Replication); err != nil {
		return fmt.Errorf("register replica observer: %w", err)
	}

	if err := mgr.Add(&publisherLifetime{replication: a.Replication}); err != nil {
		return fmt.Errorf("register publisher lifetime: %w", err)
	}

	if err := a.Topology.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register topology: %w", err)
	}

	if err := a.Keyring.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register keyring: %w", err)
	}

	if err := mgr.Add(a.Server); err != nil {
		return fmt.Errorf("register HTTPS server: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("register liveness: %w", err)
	}

	return mgr.AddReadyzCheck("racer", a.Server.Ready)
}

// Run uses standard manager leader election. On leadership loss the process
// exits; Lease release-on-cancel stays disabled to avoid overlapping writers.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}

	if err := racerv1.AddToScheme(scheme); err != nil {
		return err
	}

	// controller-runtime disables default client-side QPS throttling here;
	// serving admission bounds concurrency and API priority/fairness governs load.
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return err
	}

	if err := cfg.validateReplication(); err != nil {
		return err
	}

	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	options := managerOptions(cfg, scheme)
	options.LeaderElectionResourceLockInterface = &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Namespace: cfg.Namespace, Name: "racer-controller"},
		Client:     kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: cfg.PodName + "/" + cfg.PodUID},
	}

	mgr, err := ctrl.NewManager(restConfig, options)
	if err != nil {
		return err
	}

	app := Assemble(cfg, mgr.GetClient(), mgr.GetAPIReader())
	// Recovery validates permanent configuration and counters before starting any
	// manager runnable. The leader revalidates authoritatively for every commit.
	if _, _, err := readVersion(ctx, mgr.GetAPIReader(), cfg); err != nil {
		return fmt.Errorf("recover Racer installation: %w", err)
	}

	if err := app.SetupWithManager(mgr); err != nil {
		return err
	}

	return mgr.Start(ctx)
}

func managerOptions(cfg Config, scheme *runtime.Scheme) ctrl.Options {
	return ctrl.Options{
		Scheme:                        scheme,
		LeaderElection:                true,
		LeaderElectionID:              "racer-controller",
		LeaderElectionNamespace:       cfg.Namespace,
		LeaderElectionReleaseOnCancel: false,
		Metrics:                       metricsserver.Options{BindAddress: cfg.MetricsAddress},
		HealthProbeBindAddress:        cfg.ProbeAddress,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}:       {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
			&corev1.Secret{}:    {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
			&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
			&appsv1.DaemonSet{}: {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
		}},
	}
}
