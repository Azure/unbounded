// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
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
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/racer"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version.String())
		return
	}

	showVersion := flag.Bool("version", false, "print version and exit")
	listen := flag.String("listen", ":8443", "mTLS control listen address")
	enrollListen := flag.String("enroll-listen", ":8444", "server-authenticated HTTPS enrollment listen address")
	namespace := flag.String("state-namespace", "racer-system", "namespace for durable state and leader election")
	probes := flag.String("health-listen", ":8081", "health probe listen address")
	bootstrap := flag.String("bootstrap-node", "", "print shell bootstrap identities for this Kubernetes Node and exit")
	bootstrapUniverse := flag.String("bootstrap-universe", "", "required mapped Site universe from the Pod universe label")
	bootstrapService := flag.String("bootstrap-service", "racer-controlplane", "controller Service name for bootstrap")
	bootstrapNamespace := flag.String("bootstrap-namespace", "racer-system", "controller Service namespace for bootstrap")
	bootstrapPort := flag.String("bootstrap-port", "8443", "controller Service port name or number")
	management := flag.String("reserved-management-ports", "9090,9443", "comma-separated dataplane reserved ports; 9090 and 9443 are always reserved")
	reviewQPS := flag.Float64("token-review-qps", 20, "TokenReview API requests per second on credential cache misses")
	reviewBurst := flag.Int("token-review-burst", 30, "TokenReview API request burst on credential cache misses")
	rotationInterval := flag.Duration("ca-rotation-interval", 30*24*time.Hour, "time between CA rotations")
	logging := zap.Options{}
	logging.BindFlags(flag.CommandLine)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logging)))

	if *bootstrap != "" {
		if err := printBootstrap(*bootstrap, *bootstrapUniverse, *bootstrapNamespace, *bootstrapService, *bootstrapPort); err != nil {
			log.Fatal(err)
		}

		return
	}

	reserved, err := parseReservedPorts(*management)
	if err != nil {
		log.Fatal(err)
	}

	if err := run(*listen, *enrollListen, *namespace, *probes, reserved, *reviewQPS, *reviewBurst, *rotationInterval); err != nil {
		log.Fatal(err)
	}
}

func printBootstrap(name, expectedUniverse, namespace, service, port string) error {
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

func run(listen, enrollListen, namespace, probes string, reserved reservedPorts, reviewQPS float64, reviewBurst int, rotationInterval time.Duration) error {
	if rotationInterval <= 0 {
		return fmt.Errorf("ca-rotation-interval must be positive")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	config := new(Server)

	scheme := runtime.NewScheme()
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

	if err := setupController(ctx, manager, config, namespace, reserved); err != nil {
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
