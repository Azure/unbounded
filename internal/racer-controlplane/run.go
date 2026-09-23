// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1alpha1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer"
)

// PrintBootstrap prints shell bootstrap identities for a Kubernetes Node.
func PrintBootstrap(name, expectedUniverse, namespace, service, port string) error {
	kube, err := ctrl.GetConfig()
	if err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}

	c, err := client.New(kube, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		return err
	}

	if err := validateBootstrapNode(&node, expectedUniverse); err != nil {
		return err
	}

	var controller corev1.Service
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: service}, &controller); err != nil {
		return err
	}

	address, err := bootstrapAddress(&controller, port, os.Getenv("POD_IP"))
	if err != nil {
		return err
	}

	fmt.Printf("export RACER_UNIVERSE=%s\nexport RACER_NODE=%s\nexport RACER_CONTROL_ADDRESS='%s'\n", identity("universe", racer.NodeUniverse(&node)), identity("node", string(node.UID)), address)

	return nil
}

func bootstrapAddress(service *corev1.Service, port, podIP string) (string, error) {
	ip, err := netip.ParseAddr(podIP)
	if err != nil || ip.Is4In6() || ip.Zone() != "" || !ip.IsGlobalUnicast() {
		return "", fmt.Errorf("POD_IP must be a usable primary Pod IP")
	}

	origin, err := resolveOrigin(service, port)
	if err != nil {
		return "", err
	}

	address := origin.address(podIP)
	if address == "" {
		return "", fmt.Errorf("controller Service has no ClusterIP matching POD_IP")
	}

	return address, nil
}

func validateBootstrapNode(node *corev1.Node, expectedUniverse string) error {
	return racer.ValidateBootstrapNode(node, expectedUniverse)
}

// Run starts the control plane until interrupted or terminated.
func Run(listen, enrollListen, namespace, probes, socketRoot string, reviewQPS float64, reviewBurst int, rotationInterval time.Duration) error {
	if rotationInterval <= 0 {
		return fmt.Errorf("ca-rotation-interval must be positive")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config := new(Server)

	scheme := runtime.NewScheme()
	if err := racerv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	if err := authenticationv1.AddToScheme(scheme); err != nil {
		return err
	}

	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}

	if err := appsv1.AddToScheme(scheme); err != nil {
		return err
	}

	if err := machina.AddToScheme(scheme); err != nil {
		return err
	}

	kube, err := ctrl.GetConfig()
	if err != nil {
		return err
	}

	reviewConfig, err := tokenReviewConfig(kube, reviewQPS, reviewBurst)
	if err != nil {
		return err
	}

	config.reviewClient, err = newTokenReviewClient(reviewConfig, scheme)
	if err != nil {
		return err
	}

	manager, err := ctrl.NewManager(kube, ctrl.Options{
		Scheme: scheme, LeaderElection: true, LeaderElectionID: "racer-controlplane", LeaderElectionNamespace: namespace,
		HealthProbeBindAddress: probes, Metrics: metricsserver.Options{BindAddress: "0"},
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}:       {Label: labels.SelectorFromSet(labels.Set{dataplaneLabel: "true"})},
			&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{namespace: {}}, Label: labels.SelectorFromSet(labels.Set{stateLabel: "commit"})},
		}},
	})
	if err != nil {
		return err
	}

	if err := setupController(ctx, manager, config, namespace, socketRoot); err != nil {
		return err
	}

	if err := setupStorageController(manager, config); err != nil {
		return err
	}

	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}

	if err := setupTLSControl(manager, config, listen, enrollListen, namespace, rotationInterval); err != nil {
		return err
	}

	return manager.Start(ctx)
}
