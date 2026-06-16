// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command dashboard-example is a standalone Unbounded dashboard module used to
// exercise the dashboard mechanics end to end. It serves the module contract
// (manifest, summary, resources, details, actions, and a live SSE stream) over
// HTTP with in-memory state and no Kubernetes dependency, so it can be deployed
// next to the dashboard for testing and demonstration.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/dashboard/examplemodule"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	var (
		addr     string
		basePath string
	)

	rootCmd := &cobra.Command{
		Use:     "dashboard-example",
		Short:   "Example Unbounded dashboard module (prototype/testing)",
		Version: version.String(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), addr, basePath)
		},
	}

	rootCmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	flags := rootCmd.Flags()
	flags.StringVar(&addr, "addr", ":8090", "Address for the HTTP server to listen on")
	flags.StringVar(&basePath, "base-path", "/dashboard/v1", "Base path for the module contract endpoints")

	rootCmd.AddCommand(version.Command())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(parent context.Context, addr, basePath string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)

		if _, err := w.Write([]byte("ok")); err != nil {
			klog.V(4).Infof("dashboard-example: writing probe response: %v", err)
		}
	})

	examplemodule.New().Routes(mux, basePath)

	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		klog.Info("dashboard-example: shutting down")

		shutdownCtx, shCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shCancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			klog.Errorf("dashboard-example: shutdown error: %v", err)
		}
	}()

	klog.Infof("dashboard-example: listening on %s (base-path=%s)", addr, basePath)

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving HTTP: %w", err)
	}

	return nil
}
