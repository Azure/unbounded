// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/config"
)

type controllerRunFunc func(ctx context.Context, onReady func())

func runAsLeader(ctx context.Context, health *healthState, runFunc controllerRunFunc) {
	health.setLeader(true)
	runFunc(ctx, func() { health.setControllerReady(ctx) })
}

func runLeaderElection(ctx context.Context, cfg *config.Config, clientset kubernetes.Interface, health *healthState, runFunc controllerRunFunc) {
	// Get identity for leader election - prefer POD_NAME env var (required for hostNetwork),
	// fall back to hostname for local development
	identity := os.Getenv("POD_NAME")
	if identity == "" {
		var err error

		identity, err = os.Hostname()
		if err != nil {
			klog.Fatalf("Failed to get hostname: %v", err)
		}

		klog.Warningf("POD_NAME env var not set, using hostname for leader election identity: %s", identity)
	} else {
		klog.Infof("Using POD_NAME for leader election identity: %s", identity)
	}

	// Create leader election lock
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      cfg.LeaderElection.ResourceName,
			Namespace: cfg.LeaderElection.ResourceNamespace,
		},
		Client: clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}

	// Start leader election
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   cfg.LeaderElection.LeaseDuration,
		RenewDeadline:   cfg.LeaderElection.RenewDeadline,
		RetryPeriod:     cfg.LeaderElection.RetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.Info("Became leader, starting controller")
				runAsLeader(ctx, health, runFunc)
			},
			OnStoppedLeading: func() {
				klog.Info("Lost leadership, shutting down")
				health.setLeader(false)
				health.clearServiceEndpoints(context.Background())
				os.Exit(0)
			},
			OnNewLeader: func(newLeader string) {
				if newLeader == identity {
					return
				}

				klog.Infof("New leader elected: %s", newLeader)
			},
		},
	})
}
