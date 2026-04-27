// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const defaultCAConfigMapName = "unbounded-net-serving-ca"

// proxyBackend manages a port-forward to the controller pod and provides
// an http.RoundTripper that auto-reconnects when the port-forward dies.
type proxyBackend struct {
	mu         sync.Mutex
	client     *kubernetes.Clientset
	cfg        *rest.Config
	ns         string
	deployName string
	remotePort int
	tlsConfig  *tls.Config

	backendPort int
	stopCh      chan struct{}
	transport   *http.Transport
}

// startPortForward establishes a new port-forward to a controller pod.
// Caller must hold pb.mu.
func (pb *proxyBackend) startPortForward(ctx context.Context) error {
	deploy, err := pb.client.AppsV1().Deployments(pb.ns).Get(ctx, pb.deployName, v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment %s: %w", pb.deployName, err)
	}

	selector := v1.FormatLabelSelector(deploy.Spec.Selector)

	pods, err := pb.client.CoreV1().Pods(pb.ns).List(ctx, v1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}

	if len(pods.Items) == 0 {
		return fmt.Errorf("no pods found for deployment %s in namespace %s", pb.deployName, pb.ns)
	}

	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	podName := pods.Items[0].Name

	reqURL := pb.client.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Namespace(pb.ns).
		Name(podName).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdy.RoundTripperFor(pb.cfg)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("failed to allocate ephemeral port: %w", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port //nolint:errcheck
	_ = ln.Close()                        //nolint:errcheck

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	pfPorts := []string{fmt.Sprintf("%d:%d", port, pb.remotePort)}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)

	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, pfPorts, stopCh, readyCh, nopWriter{}, nopWriter{})
	if err != nil {
		return err
	}

	pfErrCh := make(chan error, 1)

	go func() { pfErrCh <- fw.ForwardPorts() }()

	select {
	case <-readyCh:
	case err := <-pfErrCh:
		return fmt.Errorf("port-forward to %s failed: %w", podName, err)
	case <-time.After(30 * time.Second):
		close(stopCh)
		return fmt.Errorf("port-forward to %s timed out", podName)
	}

	if pb.stopCh != nil {
		close(pb.stopCh)
	}

	pb.stopCh = stopCh
	pb.backendPort = port
	pb.transport = &http.Transport{TLSClientConfig: pb.tlsConfig}

	_, _ = fmt.Fprintf(os.Stderr, "Port-forward established to pod %s (local :%d -> remote :%d)\n", podName, port, pb.remotePort) //nolint:errcheck

	return nil
}

// ensureConnected restarts the port-forward if needed.
func (pb *proxyBackend) ensureConnected(ctx context.Context) error {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	if pb.transport != nil {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", pb.backendPort), 500*time.Millisecond)
		if err == nil {
			_ = conn.Close() //nolint:errcheck
			return nil
		}

		_, _ = fmt.Fprintf(os.Stderr, "Port-forward lost, reconnecting...\n") //nolint:errcheck
	}

	return pb.startPortForward(ctx)
}

// RoundTrip implements http.RoundTripper with auto-reconnect on failure.
func (pb *proxyBackend) RoundTrip(req *http.Request) (*http.Response, error) {
	pb.mu.Lock()
	t := pb.transport
	port := pb.backendPort
	pb.mu.Unlock()

	if t == nil {
		if err := pb.ensureConnected(req.Context()); err != nil {
			return nil, err
		}

		pb.mu.Lock()
		t = pb.transport
		port = pb.backendPort
		pb.mu.Unlock()
	}

	req.URL.Host = fmt.Sprintf("127.0.0.1:%d", port)

	resp, err := t.RoundTrip(req)
	if err != nil {
		if reconnErr := pb.ensureConnected(req.Context()); reconnErr != nil {
			return nil, fmt.Errorf("port-forward reconnect failed: %w (original error: %v)", reconnErr, err)
		}

		pb.mu.Lock()
		t = pb.transport
		port = pb.backendPort
		pb.mu.Unlock()

		req.URL.Host = fmt.Sprintf("127.0.0.1:%d", port)

		return t.RoundTrip(req)
	}

	return resp, nil
}

// newControllerProxyCommand builds a command that proxies plain HTTP on
// localhost to the controller's HTTPS port via a Kubernetes port-forward.
// It fetches the controller CA from the unbounded-net-serving-ca ConfigMap
// so the proxy can verify the controller's TLS certificate.
func newControllerProxyCommand(rt *pluginRuntime, defaultOpenBrowser bool) *cobra.Command {
	var (
		deployName string
		localPort  int
		remotePort int
		addresses  []string
		noBrowser  bool
	)

	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "HTTP-to-HTTPS proxy to the controller (fetches CA from cluster)",
		Long: `Starts a local HTTP server that proxies requests to the controller's
HTTPS endpoint via a Kubernetes port-forward. The controller's CA
certificate is fetched from the unbounded-net-serving-ca ConfigMap
so the proxy verifies the controller's TLS certificate.

This avoids TLS errors when accessing the controller dashboard or
API from a browser or curl on localhost.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ns, err := rt.namespace()
			if err != nil {
				return err
			}

			client, err := rt.kubeClient()
			if err != nil {
				return err
			}

			cfg, err := rt.restConfig()
			if err != nil {
				return err
			}

			// Fetch the controller CA from the ConfigMap.
			cm, err := client.CoreV1().ConfigMaps(ns).Get(cmd.Context(), defaultCAConfigMapName, v1.GetOptions{})
			if err != nil {
				return fmt.Errorf("failed to get ConfigMap %s/%s: %w (is the controller running?)", ns, defaultCAConfigMapName, err)
			}

			caPEM := []byte(cm.Data["ca.crt"])
			if len(caPEM) == 0 {
				return fmt.Errorf("ConfigMap %s/%s has no ca.crt data", ns, defaultCAConfigMapName)
			}

			caPool := x509.NewCertPool()
			if !caPool.AppendCertsFromPEM(caPEM) {
				return fmt.Errorf("failed to parse CA certificate from ConfigMap %s/%s", ns, defaultCAConfigMapName)
			}

			backend := &proxyBackend{
				client:     client,
				cfg:        cfg,
				ns:         ns,
				deployName: deployName,
				remotePort: remotePort,
				tlsConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
					RootCAs:    caPool,
					ServerName: fmt.Sprintf("%s.%s.svc", deployName, ns),
				},
			}

			// Establish initial port-forward.
			if err := backend.ensureConnected(cmd.Context()); err != nil {
				return err
			}

			// Request an HMAC viewer token from the controller.
			viewerToken, tokenErr := requestViewerToken(cfg)
			if tokenErr != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to request viewer token: %v\n", tokenErr) //nolint:errcheck
			}

			var tokenMu sync.Mutex

			currentToken := viewerToken

			// Build a reverse proxy using the auto-reconnecting backend.
			backendURL, _ := url.Parse(fmt.Sprintf("https://127.0.0.1:%d", backend.backendPort)) //nolint:errcheck
			proxy := &httputil.ReverseProxy{
				Transport: backend,
				Rewrite: func(pr *httputil.ProxyRequest) {
					pr.SetURL(backendURL)
					pr.Out.URL.Host = fmt.Sprintf("127.0.0.1:%d", backend.backendPort)

					tokenMu.Lock()
					t := currentToken
					tokenMu.Unlock()

					if pr.Out.Header.Get("Authorization") == "" && t != "" {
						pr.Out.Header.Set("Authorization", "Bearer "+t)
					}
				},
			}

			listenAddr := fmt.Sprintf("%s:%d", addresses[0], localPort)
			statusURL := fmt.Sprintf("http://%s/status", listenAddr)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Proxying http://%s/status -> controller:%d\n", listenAddr, remotePort) //nolint:errcheck
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Open %s in your browser\n", statusURL)                                 //nolint:errcheck

			// Open browser unless --no-browser is set.
			if !noBrowser {
				openBrowser(statusURL)
			}

			// Start the HTTP proxy server.
			server := &http.Server{
				Addr:    listenAddr,
				Handler: proxy,
			}

			// Graceful shutdown on signal.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			go func() {
				<-sigCh
				backend.mu.Lock()
				if backend.stopCh != nil {
					close(backend.stopCh)
				}
				backend.mu.Unlock()

				_ = server.Close() //nolint:errcheck
			}()

			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("proxy server error: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&deployName, "deployment", defaultControllerDeploy, "Controller deployment name")
	cmd.Flags().IntVar(&localPort, "local-port", 9999, "Local HTTP port to listen on")
	cmd.Flags().IntVar(&remotePort, "remote-port", 9999, "Remote HTTPS port on the controller")
	cmd.Flags().StringSliceVar(&addresses, "address", []string{"127.0.0.1"}, "Addresses to listen on")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", !defaultOpenBrowser, "Do not open the status page in a browser")

	return cmd
}

// nopWriter discards all writes (used to silence port-forward output).
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// openBrowser opens the given URL in the default browser.
// Errors are silently ignored -- the URL is printed to stdout as a fallback.
func openBrowser(rawURL string) {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}

	_ = cmd.Start() //nolint:errcheck
}

const viewerTokenEndpointPath = "/apis/status.net.unbounded-cloud.io/v1alpha1/token/viewer"

// viewerTokenResponse is the JSON response from the viewer token endpoint.
type viewerTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
}

// requestViewerToken requests an HMAC viewer token from the controller's
// aggregated API endpoint via the Kubernetes API server. The API server
// authenticates the request using the kubeconfig credentials and proxies
// it to the controller.
func requestViewerToken(cfg *rest.Config) (string, error) {
	transport, err := rest.TransportFor(cfg)
	if err != nil {
		return "", fmt.Errorf("build transport: %w", err)
	}

	client := &http.Client{Transport: transport}

	tokenURL := strings.TrimRight(cfg.Host, "/") + viewerTokenEndpointPath

	req, err := http.NewRequest(http.MethodPost, tokenURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", fmt.Errorf("create token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request failed: %w", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResp viewerTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("unmarshal token response: %w", err)
	}

	if tokenResp.Token == "" {
		return "", fmt.Errorf("token endpoint returned empty token")
	}

	return tokenResp.Token, nil
}

// newDashboardCommand opens the controller dashboard in a browser.
// It is a convenience alias for 'controller proxy' that automatically
// opens the status page.
func newDashboardCommand(rt *pluginRuntime) *cobra.Command {
	cmd := newControllerProxyCommand(rt, true)
	cmd.Use = "dashboard"
	cmd.Short = "Open the controller dashboard in a browser"
	cmd.Long = `Opens the controller status dashboard in a browser by starting a
local HTTP-to-HTTPS proxy. Equivalent to 'controller proxy' but
automatically opens the status page.`

	return cmd
}
