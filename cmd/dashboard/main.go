// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command dashboard is the Unbounded management dashboard server. It renders a
// server-side UI and JSON API over component modules discovered from a static
// registry. This is the prototype entrypoint: it wires the registry, the
// authorization mode, and the HTTP server together.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/dashboard/authz"
	"github.com/Azure/unbounded/internal/dashboard/registry"
	"github.com/Azure/unbounded/internal/dashboard/server"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	var (
		addr           string
		modulesPath    string
		authMode       string
		kubeconfigPath string
		insecureTLS    bool
	)

	rootCmd := &cobra.Command{
		Use:     "dashboard",
		Short:   "Unbounded management dashboard server",
		Version: version.String(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), runConfig{
				addr:           addr,
				modulesPath:    modulesPath,
				authMode:       authMode,
				kubeconfigPath: kubeconfigPath,
				insecureTLS:    insecureTLS,
			})
		},
	}

	rootCmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	flags := rootCmd.Flags()
	flags.StringVar(&addr, "addr", ":8080", "Address for the HTTP server to listen on")
	flags.StringVar(&modulesPath, "modules-config", "/etc/unbounded-dashboard/modules.yaml", "Path to the static module registry YAML")
	flags.StringVar(&authMode, "auth-mode", "none", "Authorization mode: none or sar (Kubernetes SubjectAccessReview)")
	flags.StringVar(&kubeconfigPath, "kubeconfig", "", "Path to kubeconfig (sar mode only; uses in-cluster config if empty)")
	flags.BoolVar(&insecureTLS, "module-insecure-tls", false, "Skip TLS verification when calling module backends (prototype: net controller serves a self-signed cert)")

	rootCmd.AddCommand(version.Command())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type runConfig struct {
	addr           string
	modulesPath    string
	authMode       string
	kubeconfigPath string
	insecureTLS    bool
}

func run(parent context.Context, cfg runConfig) error {
	reg, err := registry.Load(cfg.modulesPath)
	if err != nil {
		return err
	}

	klog.Infof("dashboard: loaded %d module(s) from %s", len(reg.Modules), cfg.modulesPath)

	authorizer, err := buildAuthorizer(cfg)
	if err != nil {
		return err
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}

	if cfg.insecureTLS {
		klog.Warning("dashboard: module-insecure-tls enabled, module TLS certificates are not verified (prototype)")

		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // prototype: self-signed module cert
		}
	}

	srv, err := server.New(server.Options{
		Registry:   reg,
		Authorizer: authorizer,
		HTTPClient: httpClient,
	})
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	httpServer := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		klog.Info("dashboard: shutting down")

		shutdownCtx, shCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shCancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			klog.Errorf("dashboard: shutdown error: %v", err)
		}
	}()

	klog.Infof("dashboard: listening on %s (auth-mode=%s)", cfg.addr, cfg.authMode)

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving HTTP: %w", err)
	}

	return nil
}

func buildAuthorizer(cfg runConfig) (authz.Authorizer, error) {
	switch cfg.authMode {
	case "none", "":
		klog.Warning("dashboard: auth-mode=none, all requests are authorized (prototype)")
		return authz.AllowAll{}, nil
	case "sar":
		restConfig, err := loadRESTConfig(cfg.kubeconfigPath)
		if err != nil {
			return nil, err
		}

		clientset, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return nil, fmt.Errorf("creating kubernetes client: %w", err)
		}

		return authz.NewSubjectAccessReview(clientset), nil
	default:
		return nil, fmt.Errorf("unknown auth-mode %q (want none or sar)", cfg.authMode)
	}
}

func loadRESTConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}

	return rest.InClusterConfig()
}
