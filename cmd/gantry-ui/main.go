// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/unbounded/internal/gantryui"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Gantry UI stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "build-image" {
		return runImageBuilder()
	}

	listen := flag.String("listen", "127.0.0.1:8088", "Loopback HTTP address; use a trusted tunnel for remote access")
	kubeconfig := flag.String("kubeconfig", "", "Kubeconfig path (uses normal client-go loading when omitted)")
	namespace := flag.String("namespace", "gantry-demo", "Existing namespace for UI-owned demo DaemonSets")
	pool := flag.String("node-pool", "flexpool", "Flex agentpool label value")
	architecture := flag.String("architecture", "amd64", "Worker image architecture: amd64 or arm64")
	registry := flag.String("registry", "", "ACR login server, for example example.azurecr.io")
	gantryNamespace := flag.String("gantry-namespace", "unbounded-system", "Namespace containing the Gantry agents")
	direct := flag.Bool("direct-agents", false, "Reach agent Pod IPs directly; recommended when running inside the cluster")
	interval := flag.Duration("interval", time.Second, "Minimum shared collection interval (at least 250ms); worker updates stream as observed")
	builderImage := flag.String("builder-image", "", "Digest-pinned image containing the Gantry image builder")
	builderBase := flag.String("builder-base", "", "Digest-pinned small runtime base in this ACR")
	builderAccount := flag.String("builder-service-account", "", "Workload-identity service account for image-build Jobs")
	builderRepository := flag.String("builder-repository", "gantry-demo/generated", "ACR repository for generated images")
	builderConcurrency := flag.Int("builder-concurrency", 1, "Maximum concurrent image-build Jobs")

	flag.Parse()

	host, _, err := net.SplitHostPort(*listen)
	if err != nil || (host != "localhost" && !net.ParseIP(host).IsLoopback()) {
		return errors.New("--listen must specify a loopback host and port")
	}

	for name, value := range map[string]string{"namespace": *namespace, "gantry-namespace": *gantryNamespace, "node-pool": *pool} {
		if problems := validation.IsDNS1123Label(value); len(problems) > 0 {
			return fmt.Errorf("invalid --%s: %s", name, problems[0])
		}
	}

	if *interval < 250*time.Millisecond {
		return errors.New("--interval must be at least 250ms")
	}

	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return err
	}

	catalog, err := gantryui.NewRegistry(*registry, *architecture, credential)
	if err != nil {
		return err
	}

	var builder *gantryui.ImageBuildConfig
	if *builderImage != "" || *builderBase != "" || *builderAccount != "" {
		builder = &gantryui.ImageBuildConfig{Image: *builderImage, Base: *builderBase, ServiceAccount: *builderAccount, Repository: *builderRepository, MaxConcurrent: *builderConcurrency}
		if err := builder.Validate(*registry); err != nil {
			return err
		}
	}

	loading := clientcmd.NewDefaultClientConfigLoadingRules()
	loading.ExplicitPath = *kubeconfig

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return err
	}

	config.QPS, config.Burst = 40, 64
	config.Timeout = 5 * time.Second

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app := gantryui.New(gantryui.Config{
		Client: client, Catalog: catalog, Registry: *registry, Namespace: *namespace,
		NodePool: *pool, Architecture: *architecture, GantryNamespace: *gantryNamespace,
		DirectAgents: *direct, Interval: *interval,
		Builder: builder,
	})
	server := &http.Server{
		Addr: *listen, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()

	slog.Info("Gantry web UI listening; selecting a new image creates a Flex DaemonSet",
		slog.String("address", "http://"+*listen), slog.String("registry", *registry), slog.String("namespace", *namespace))

	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return server.Shutdown(shutdown)
}
