// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const defaultHTTPReadHeaderTimeout = 10 * time.Second

const maxArtifactBackendAttempts = 3

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

	return &edgeProxy{
		proxy:     proxy,
		transport: http.DefaultTransport,
	}
}

type edgeProxy struct {
	proxy     *httputil.ReverseProxy
	transport http.RoundTripper
}

func (e *edgeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v1/netboot/sessions/") && strings.Contains(r.URL.Path, "/artifacts/") {
		e.serveArtifact(w, r)
		return
	}

	e.proxy.ServeHTTP(w, r)
}

func (e *edgeProxy) serveArtifact(w http.ResponseWriter, r *http.Request) {
	request := r.Clone(r.Context())
	e.proxy.Director(request)

	response, err := e.transport.RoundTrip(request)
	if err != nil {
		slog.Warn("Metalman edge artifact request failed", "err", err)
		http.Error(w, "Metalman backend unavailable", http.StatusBadGateway)
		return
	}

	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)

	remaining := response.ContentLength
	if remaining <= 0 || (response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent) {
		_, _ = io.Copy(w, response.Body)
		response.Body.Close() //nolint:errcheck // The response body is no longer needed.
		return
	}

	start, end, ok := responseByteRange(response)
	if !ok {
		_, _ = io.Copy(w, response.Body)
		response.Body.Close() //nolint:errcheck // The response body is no longer needed.
		return
	}

	written, copyErr := io.Copy(w, response.Body)
	response.Body.Close() //nolint:errcheck // The response body is no longer needed.
	start += written
	remaining -= written
	if copyErr == nil && remaining == 0 {
		return
	}

	for attempt := 2; attempt <= maxArtifactBackendAttempts && remaining > 0 && start <= end; attempt++ {
		request = r.Clone(r.Context())
		e.proxy.Director(request)
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		response, err = e.transport.RoundTrip(request)
		if err != nil {
			slog.Warn("Metalman edge artifact resume failed", "path", r.URL.Path, "err", err)
			continue
		}
		if response.StatusCode != http.StatusPartialContent || response.ContentLength != remaining {
			response.Body.Close() //nolint:errcheck // Invalid resume response.
			slog.Warn("Metalman edge artifact resume returned an invalid range", "path", r.URL.Path, "status", response.StatusCode)
			return
		}
		resumeStart, resumeEnd, valid := responseByteRange(response)
		if !valid || resumeStart != start || resumeEnd != end {
			response.Body.Close() //nolint:errcheck // Invalid resume response.
			slog.Warn("Metalman edge artifact resume returned mismatched bytes", "path", r.URL.Path)
			return
		}

		written, copyErr = io.Copy(w, response.Body)
		response.Body.Close() //nolint:errcheck // The response body is no longer needed.
		start += written
		remaining -= written
		if copyErr == nil && remaining == 0 {
			return
		}
	}

	slog.Warn("Metalman edge artifact transfer failed", "path", r.URL.Path, "err", copyErr)
}

func responseByteRange(response *http.Response) (int64, int64, bool) {
	if response.StatusCode == http.StatusOK {
		return 0, response.ContentLength - 1, true
	}

	value := response.Header.Get("Content-Range")
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, false
	}
	rangeAndSize := strings.SplitN(strings.TrimPrefix(value, "bytes "), "/", 2)
	if len(rangeAndSize) != 2 {
		return 0, 0, false
	}
	bounds := strings.SplitN(rangeAndSize[0], "-", 2)
	if len(bounds) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(bounds[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	end, err := strconv.ParseInt(bounds[1], 10, 64)
	if err != nil || start < 0 || end < start || end-start+1 != response.ContentLength {
		return 0, 0, false
	}

	return start, end, true
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}
