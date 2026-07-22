// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/metalman/dhcp"
	"github.com/Azure/unbounded/internal/metalman/netboot"
)

const defaultHTTPReadHeaderTimeout = 10 * time.Second

const maxArtifactBackendAttempts = 3

// EdgeCmd runs Metalman's provisioning-network protocol edge.
func EdgeCmd() *cobra.Command {
	var (
		backendURL    string
		bindAddress   string
		httpPort      int
		tlsCertFile   string
		tlsKeyFile    string
		endpoint      string
		edgeTokenFile string
		dhcpEnabled   bool
		dhcpInterface string
		dhcpServerIP  string
		dhcpPort      int
		tftpEnabled   bool
		tftpBindAddr  string
		tftpPort      int
	)

	cmd := &cobra.Command{
		Use:   string(metalmanRoleEdge),
		Short: "Run the Metalman provisioning protocol edge",
		RunE: func(cmd *cobra.Command, _ []string) error {
			backend, err := parseEdgeBackendURL(backendURL)
			if err != nil {
				return err
			}

			if err := validateEdgeTLSFiles(tlsCertFile, tlsKeyFile); err != nil {
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
			if dhcpEnabled {
				decisionProvider, err := dhcp.NewHTTPDecisionProviderFromTokenFile(backend.String(), endpoint, edgeTokenFile, nil)
				if err != nil {
					return fmt.Errorf("creating DHCP backend: %w", err)
				}

				serverIP, err := edgeDHCPServerIP(dhcpServerIP, dhcpInterface)
				if err != nil {
					return err
				}

				dhcpServer := &dhcp.Server{Interface: dhcpInterface, Port: dhcpPort, DecisionProvider: decisionProvider, ServerIP: serverIP}

				go func() {
					if err := dhcpServer.Start(ctx); err != nil && ctx.Err() == nil {
						slog.ErrorContext(ctx, "Metalman edge DHCP server failed", "err", err)
						stop()
					}
				}()
			}

			if tftpEnabled {
				artifactBackend, err := netboot.NewHTTPArtifactBackend(backend.String(), nil)
				if err != nil {
					return fmt.Errorf("creating TFTP backend: %w", err)
				}

				tftpServer := &netboot.TFTPServer{BindAddr: tftpBindAddr, Port: tftpPort, Backend: artifactBackend}

				go func() {
					if err := tftpServer.Start(ctx); err != nil && ctx.Err() == nil {
						slog.ErrorContext(ctx, "Metalman edge TFTP server failed", "err", err)
						stop()
					}
				}()
			}

			go func() {
				<-ctx.Done()

				if err := server.Shutdown(context.Background()); err != nil {
					slog.Warn("shutting down Metalman edge HTTP server failed", "err", err)
				}
			}()

			PrintConfig("role", string(metalmanRoleEdge))
			PrintConfig("backend-url", backend.String())
			PrintService("HTTP", addr)
			PrintReady()
			slog.InfoContext(ctx, "starting Metalman edge", "addr", addr, "backend", backend.String())

			if err := serveEdgeHTTP(server, tlsCertFile, tlsKeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("serving edge HTTP: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&backendURL, "backend-url", "", "Metalman server base URL")
	cmd.Flags().StringVar(&bindAddress, "bind-address", "0.0.0.0", "IP address to bind the edge HTTP listener")
	cmd.Flags().IntVar(&httpPort, "http-port", 8880, "Port for the edge HTTP listener")
	cmd.Flags().StringVar(&tlsCertFile, "tls-cert-file", "", "TLS certificate file; requires --tls-key-file")
	cmd.Flags().StringVar(&tlsKeyFile, "tls-key-file", "", "TLS private key file; requires --tls-cert-file")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "NetbootEndpoint served by this edge")
	cmd.Flags().StringVar(&edgeTokenFile, "edge-token-file", "/var/run/secrets/metalman/token", "Audience-bound ServiceAccount token file")
	cmd.Flags().BoolVar(&dhcpEnabled, "dhcp-enabled", false, "Enable the DHCP protocol edge")
	cmd.Flags().StringVar(&dhcpInterface, "dhcp-interface", "", "Provisioning interface for direct DHCP; empty enables relay-only mode")
	cmd.Flags().StringVar(&dhcpServerIP, "dhcp-server-ip", "", "DHCP server IPv4 address; defaults to the interface or outbound address")
	cmd.Flags().IntVar(&dhcpPort, "dhcp-port", 67, "DHCP listener port")
	cmd.Flags().BoolVar(&tftpEnabled, "tftp-enabled", false, "Enable the TFTP protocol edge")
	cmd.Flags().StringVar(&tftpBindAddr, "tftp-bind-address", "0.0.0.0", "IP address to bind the TFTP listener")
	cmd.Flags().IntVar(&tftpPort, "tftp-port", 69, "TFTP listener port")

	if err := cmd.MarkFlagRequired("backend-url"); err != nil {
		panic(fmt.Sprintf("mark backend-url flag required: %v", err))
	}

	if err := cmd.MarkFlagRequired("endpoint"); err != nil {
		panic(fmt.Sprintf("mark endpoint flag required: %v", err))
	}

	return cmd
}

func validateEdgeTLSFiles(certFile, keyFile string) error {
	if (certFile == "") != (keyFile == "") {
		return errors.New("--tls-cert-file and --tls-key-file must be set together")
	}

	return nil
}

func serveEdgeHTTP(server *http.Server, certFile, keyFile string) error {
	if certFile != "" {
		return server.ListenAndServeTLS(certFile, keyFile)
	}

	return server.ListenAndServe()
}

func edgeDHCPServerIP(configured, iface string) (net.IP, error) {
	if configured != "" {
		ip := net.ParseIP(configured).To4()
		if ip == nil {
			return nil, errors.New("--dhcp-server-ip must be an IPv4 address")
		}

		return ip, nil
	}

	if iface != "" {
		return InterfaceIPv4(iface)
	}

	ip, err := OutboundIP()
	if err != nil || ip.To4() == nil {
		return nil, errors.New("detecting DHCP server IPv4 address; set --dhcp-server-ip")
	}

	return ip.To4(), nil
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
		backend:   backend,
		proxy:     proxy,
		transport: http.DefaultTransport,
	}
}

type edgeProxy struct {
	backend   *url.URL
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
	e.rewriteRequest(request)

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
		if _, err := io.Copy(w, response.Body); err != nil {
			slog.Warn("Metalman edge response copy failed", "path", r.URL.Path, "err", err)
		}

		response.Body.Close() //nolint:errcheck // The response body is no longer needed.

		return
	}

	start, end, ok := responseByteRange(response)
	if !ok {
		if _, err := io.Copy(w, response.Body); err != nil {
			slog.Warn("Metalman edge response copy failed", "path", r.URL.Path, "err", err)
		}

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
		e.rewriteRequest(request)
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

func (e *edgeProxy) rewriteRequest(request *http.Request) {
	request.URL.Scheme = e.backend.Scheme
	request.URL.Host = e.backend.Host
	request.URL.Path, request.URL.RawPath = joinURLPath(e.backend, request.URL)
	request.Host = e.backend.Host

	if e.backend.RawQuery == "" || request.URL.RawQuery == "" {
		request.URL.RawQuery = e.backend.RawQuery + request.URL.RawQuery
	} else {
		request.URL.RawQuery = e.backend.RawQuery + "&" + request.URL.RawQuery
	}
}

func joinURLPath(base, request *url.URL) (string, string) {
	if base.RawPath == "" && request.RawPath == "" {
		return singleJoiningSlash(base.Path, request.Path), ""
	}

	basePath := base.EscapedPath()
	requestPath := request.EscapedPath()

	return singleJoiningSlash(base.Path, request.Path), singleJoiningSlash(basePath, requestPath)
}

func singleJoiningSlash(left, right string) string {
	leftSlash := strings.HasSuffix(left, "/")
	rightSlash := strings.HasPrefix(right, "/")

	switch {
	case leftSlash && rightSlash:
		return left + right[1:]
	case !leftSlash && !rightSlash:
		return left + "/" + right
	default:
		return left + right
	}
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
