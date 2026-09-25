// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer scaffolds the Racer control plane using controller-runtime
// directly. Constructors compose only; operational methods fail closed until
// their contracts are implemented and tested.
package racer

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
)

type Application struct {
	Topology *TopologyReconciler
	Keyring  *KeyringReconciler
	Workload *WorkloadReconciler
	Server   *Server
}

// Assemble performs no Kubernetes calls, I/O, cryptography, or goroutine startup.
func Assemble(cfg Config, c client.Client, reader client.Reader) *Application {
	publications := NewPublications(cfg.Limits)
	issuer := &Issuer{APIReader: reader, Config: cfg}
	bootstrap := &Bootstrap{Client: c, APIReader: reader, Config: cfg, Issuer: issuer}

	return &Application{
		Topology: &TopologyReconciler{Client: c, APIReader: reader, Config: cfg, Publications: publications, Accepted: make(AcceptedMembers)},
		Keyring:  &KeyringReconciler{Client: c, APIReader: reader, Config: cfg, Issuer: issuer},
		Workload: &WorkloadReconciler{Client: c, Config: cfg},
		Server:   &Server{Config: cfg, APIReader: reader, Bootstrap: bootstrap, Publications: publications},
	}
}

func (a *Application) SetupWithManager(mgr ctrl.Manager) error {
	if err := a.Topology.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register topology: %w", err)
	}

	if err := a.Keyring.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register keyring: %w", err)
	}

	if err := a.Workload.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register workload: %w", err)
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

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                  scheme,
		LeaderElection:          true,
		LeaderElectionID:        "racer-controller",
		LeaderElectionNamespace: cfg.Namespace,
		Metrics:                 metricsserver.Options{BindAddress: cfg.MetricsAddress},
		HealthProbeBindAddress:  cfg.ProbeAddress,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}:       {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
			&corev1.Secret{}:    {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
			&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
			&appsv1.DaemonSet{}: {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
		}},
	})
	if err != nil {
		return err
	}

	app := Assemble(cfg, mgr.GetClient(), mgr.GetAPIReader())
	if err := app.SetupWithManager(mgr); err != nil {
		return err
	}

	return mgr.Start(ctx)
}
