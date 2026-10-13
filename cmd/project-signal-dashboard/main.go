// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
	"github.com/Azure/unbounded/internal/signaldashboard"
)

func main() {
	var (
		address   string
		namespace string
	)

	flag.StringVar(&address, "address", ":8080", "HTTP listen address")
	flag.StringVar(&namespace, "namespace", "project-signal", "namespace containing Project Signal resources")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	config, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("load in-cluster Kubernetes configuration", "error", err)
		os.Exit(1)
	}

	kubeScheme := runtime.NewScheme()
	mustAddToScheme(logger, scheme.AddToScheme(kubeScheme))
	mustAddToScheme(logger, signalv1.AddToScheme(kubeScheme))

	kube, err := client.New(config, client.Options{Scheme: kubeScheme})
	if err != nil {
		logger.Error("create Kubernetes client", "error", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Error("create Kubernetes log client", "error", err)
		os.Exit(1)
	}

	dashboard := &signaldashboard.Server{
		Collector: &signaldashboard.Collector{
			Reader:    kube,
			PodLogs:   podLogs{client: clientset},
			Namespace: namespace,
		},
		Logger: logger,
	}
	server := signaldashboard.NewHTTPServer(address, dashboard.Handler())

	stopContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-stopContext.Done()

		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownContext); err != nil {
			logger.Error("shut down dashboard", "error", err)
		}
	}()

	logger.Info("starting Project Signal dashboard", "address", address, "namespace", namespace)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("serve dashboard", "error", err)
		os.Exit(1)
	}
}

func mustAddToScheme(logger *slog.Logger, err error) {
	if err != nil {
		logger.Error("register Kubernetes scheme", "error", err)
		os.Exit(1)
	}
}

type podLogs struct {
	client kubernetes.Interface
}

func (p podLogs) ReadPodLog(
	ctx context.Context,
	namespace, pod, container string,
	tailLines, limitBytes int64,
) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	return p.client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container:  container,
		TailLines:  &tailLines,
		LimitBytes: &limitBytes,
	}).DoRaw(ctx)
}
