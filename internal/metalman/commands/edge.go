// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const defaultHTTPReadHeaderTimeout = 10 * time.Second

// EdgeCmd runs Metalman's provisioning-network protocol edge.
func EdgeCmd() *cobra.Command {
	var (
		backendURL  string
		bindAddress string
		httpPort    int
	)

	cmd := &cobra.Command{
		Use:   string(metalmanRoleEdge),
		Short: "Run the Metalman provisioning protocol edge",
		RunE: func(cmd *cobra.Command, _ []string) error {
			backend, err := parseEdgeBackendURL(backendURL)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			addr := fmt.Sprintf("%s:%d", bindAddress, httpPort)
			server := &http.Server{
				Addr:              addr,
				Handler:           newEdgeProxy(backend),
				ReadHeaderTimeout: defaultHTTPReadHeaderTimeout,
			}

			go func() {
				<-ctx.Done()
				_ = server.Shutdown(context.Background())
			}()

			PrintConfig("role", string(metalmanRoleEdge))
			PrintConfig("backend-url", backend.String())
			PrintService("HTTP", addr)
			PrintReady()
			slog.InfoContext(ctx, "starting Metalman edge", "addr", addr, "backend", backend.String())

			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("serving edge HTTP: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&backendURL, "backend-url", "", "Metalman server base URL")
	cmd.Flags().StringVar(&bindAddress, "bind-address", "0.0.0.0", "IP address to bind the edge HTTP listener")
	cmd.Flags().IntVar(&httpPort, "http-port", 8880, "Port for the edge HTTP listener")
	_ = cmd.MarkFlagRequired("backend-url")

	return cmd
}

func parseEdgeBackendURL(value string) (*url.URL, error) {
	backend, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("parsing --backend-url: %w", err)
	}
	if backend.Scheme != "http" && backend.Scheme != "https" {
		return nil, errors.New("--backend-url must use http or https")
	}
	if backend.Host == "" {
		return nil, errors.New("--backend-url must include a host")
	}

	return backend, nil
}

func newEdgeProxy(backend *url.URL) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(backend)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		slog.Warn("Metalman edge backend request failed", "err", err)
		http.Error(w, "Metalman backend unavailable", http.StatusBadGateway)
	}

	return proxy
}
