// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

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

	"github.com/Azure/unbounded/internal/racer"
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	namespace := flag.String("state-namespace", "racer-system", "namespace for durable state and leader election")
	probes := flag.String("health-listen", ":8081", "health probe listen address")
	bootstrap := flag.String("bootstrap-node", "", "print shell bootstrap identities for this Kubernetes Node and exit")
	bootstrapUniverse := flag.String("bootstrap-universe", "", "required mapped Site universe from the Pod universe label")
	bootstrapService := flag.String("bootstrap-service", "racer-controlplane", "controller Service name for bootstrap")
	bootstrapNamespace := flag.String("bootstrap-namespace", "racer-system", "controller Service namespace for bootstrap")
	bootstrapPort := flag.String("bootstrap-port", "8080", "controller Service port name or number")
	keyDir := flag.String("generate-key", "", "write a raw Ed25519 seed and public key into a new directory and exit")
	management := flag.String("reserved-management-ports", "9090", "comma-separated dataplane management ports; 9090 is always reserved")
	reviewQPS := flag.Float64("token-review-qps", 20, "TokenReview API requests per second on credential cache misses")
	reviewBurst := flag.Int("token-review-burst", 30, "TokenReview API request burst on credential cache misses")
	rotationInterval := flag.Duration("signing-rotation-interval", 24*time.Hour, "time between signing key activations")
	propagationDelay := flag.Duration("signing-propagation-delay", 10*time.Minute, "minimum signing trust propagation delay")
	logging := zap.Options{}
	logging.BindFlags(flag.CommandLine)
	flag.Parse()

	if *keyDir != "" {
		if err := generateKey(*keyDir); err != nil {
			log.Fatal(err)
		}

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

	if err := run(*listen, *namespace, *probes, reserved, *reviewQPS, *reviewBurst, rotationPolicy{*rotationInterval, *propagationDelay}); err != nil {
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

func run(listen, namespace, probes string, reserved reservedPorts, reviewQPS float64, reviewBurst int, policy rotationPolicy) error {
	if err := policy.validate(); err != nil {
		return err
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
			&corev1.Secret{}:    {Namespaces: map[string]cache.Config{namespace: {}}},
		}},
	})
	if err != nil {
		return err
	}
	// The manager cache and leader election have not started. Provision through
	// a direct client so every replica can initialize without waiting for either.
	var signingClient client.Client
	if managedSigningEnabled() {
		signingClient, err = client.New(kube, client.Options{Scheme: scheme})
		if err != nil {
			return err
		}
	}

	signingCtx, cancelSigning := context.WithTimeout(ctx, 30*time.Second)
	key, err := signerFromEnv(signingCtx, signingClient, namespace)

	cancelSigning()

	if err != nil {
		return err
	}

	config.signer = key

	var refreshSigning func(context.Context) error

	if managedSigningEnabled() {
		observer := &signingSecretReconciler{client: manager.GetClient(), server: config, namespace: namespace}

		refreshSigning = func(ctx context.Context) error {
			for _, name := range []string{configSigningSecret, peerSigningSecret} {
				var secret corev1.Secret
				if err := signingClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
					return err
				}

				if err := observer.observe(&secret); err != nil {
					return err
				}
			}

			return nil
		}
		if err := refreshSigning(ctx); err != nil {
			return err
		}

		if err := setupSigningController(manager, observer, signingClient, policy); err != nil {
			return err
		}
	}

	if err := setupController(ctx, manager, config, namespace, reserved); err != nil {
		return err
	}

	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/{universe}/{node}", config.control)
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	server := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      70 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	if err := manager.Add(&subscriptionServer{server: server, refreshSigning: refreshSigning}); err != nil {
		return err
	}

	return manager.Start(ctx)
}

// Standbys do not listen on the subscription port and therefore fail the
// Kubernetes readiness probe. Only the fenced leader serves configurations.
type subscriptionServer struct {
	server         *http.Server
	refreshSigning func(context.Context) error
}

func (*subscriptionServer) NeedLeaderElection() bool { return true }
func (s *subscriptionServer) Start(ctx context.Context) error {
	if s.refreshSigning != nil {
		if err := s.refreshSigning(ctx); err != nil {
			return err
		}
	}

	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-ctx.Done():
			if err := s.server.Close(); err != nil {
				log.Printf("close subscription server: %v", err)
			}
		case <-done:
		}
	}()

	log.Printf("control plane leader listening on %s", s.server.Addr)

	err := s.server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}
