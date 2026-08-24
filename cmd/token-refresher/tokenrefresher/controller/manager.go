// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
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
	utilruntime.Must(unboundedv1alpha3.AddToScheme(scheme))
}

func RunManager(ctx context.Context, cfg Config) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	restCfg := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Cache:                  tokenRefresherCacheOptions(),
		Metrics:                metricsserver.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress: cfg.ProbeAddr,
		LeaderElection:         cfg.EnableLeaderElection,
		LeaderElectionID:       "token-refresher-controller",
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	kubeClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create kubernetes clientset: %w", err)
	}

	if err := mgr.GetFieldIndexer().IndexField(ctx, &unboundedv1alpha3.Machine{}, machineSiteField, machineSiteIndex); err != nil {
		return fmt.Errorf("index Machine Site label: %w", err)
	}

	if err := (&TokenReconciler{
		Client:     mgr.GetClient(),
		APIReader:  mgr.GetAPIReader(),
		KubeClient: kubeClient,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup token controller: %w", err)
	}

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

func tokenRefresherCacheOptions() cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {
				Namespaces: map[string]cache.Config{
					metav1.NamespaceSystem: {
						LabelSelector: labels.NewSelector().Add(*mustRequirement(unboundedv1alpha3.MachineSiteLabelKey)),
					},
				},
			},
		},
	}
}

func mustRequirement(key string) *labels.Requirement {
	requirement, err := labels.NewRequirement(key, selection.Exists, nil)
	if err != nil {
		panic(err)
	}

	return requirement
}
