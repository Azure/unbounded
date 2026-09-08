// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

			// Token manager refreshes the HMAC viewer token on demand.
			// Prime it with an initial token so a failure surfaces immediately.
			tokens := newViewerTokenManager(cfg)
			if _, err := tokens.getToken(); err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to request viewer token: %v\n", err) //nolint:errcheck
			}

			// Wrap the port-forward transport so that auth headers are
			// applied per request and a 401 forces a single token refresh
			// and retry.
			authTransport := &authRetryTransport{
				next:   backend,
				tokens: tokens,
				errOut: cmd.ErrOrStderr(),
			}

			// Build a reverse proxy using the auto-reconnecting backend.
			backendURL, _ := url.Parse(fmt.Sprintf("https://127.0.0.1:%d", backend.backendPort)) //nolint:errcheck
			proxy := &httputil.ReverseProxy{
				Transport: authTransport,
				Rewrite: func(pr *httputil.ProxyRequest) {
					pr.SetURL(backendURL)
					pr.Out.URL.Host = fmt.Sprintf("127.0.0.1:%d", backend.backendPort)
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

// authRetryTransport injects a viewer token into outgoing requests and,
// on a 401 response from the upstream, invalidates the cached token,
// fetches a fresh one, and replays the request once. Requests that
// already carry an Authorization header are passed through unchanged.
//
// Upgrade requests (e.g. websockets) are not retried because the response
// body cannot be safely read and replayed; ReverseProxy hijacks those
// connections directly.
type authRetryTransport struct {
	next   http.RoundTripper
	tokens *viewerTokenManager
	errOut io.Writer
}

// applyToken sets the Authorization header on req using the current
// viewer token. Errors are logged but not returned: a missing token
// will surface as a 401 from the upstream, which the retry path can
// react to.
func (a *authRetryTransport) applyToken(req *http.Request) {
	if req.Header.Get("Authorization") != "" {
		return
	}

	t, err := a.tokens.getToken()
	if err != nil {
		if a.errOut != nil {
			_, _ = fmt.Fprintf(a.errOut, "Warning: failed to refresh viewer token: %v\n", err) //nolint:errcheck
		}

		return
	}

	if t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
}

// RoundTrip implements http.RoundTripper.
func (a *authRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the body up front so we can replay it on a 401. Skip
	// websocket and other upgrade requests: they have no replay-safe
	// body and ReverseProxy handles them via hijack anyway.
	isUpgrade := strings.EqualFold(req.Header.Get("Connection"), "upgrade")
	hadAuth := req.Header.Get("Authorization") != ""

	if !isUpgrade && req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		buf, err := io.ReadAll(req.Body)
		if err != nil {
			_ = req.Body.Close() //nolint:errcheck

			return nil, fmt.Errorf("buffer request body for auth retry: %w", err)
		}

		_ = req.Body.Close() //nolint:errcheck

		req.Body = io.NopCloser(bytes.NewReader(buf))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(buf)), nil
		}
	}

	a.applyToken(req)

	resp, err := a.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	// Only retry once, only on 401, and only if we own the auth header
	// (so we don't clobber a caller-supplied token) and the request is
	// safely replayable.
	if resp.StatusCode != http.StatusUnauthorized || hadAuth || isUpgrade {
		return resp, nil
	}

	// Drain and close the failed response so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck
	_ = resp.Body.Close()                 //nolint:errcheck

	if a.errOut != nil {
		_, _ = fmt.Fprintf(a.errOut, "Viewer token rejected (401), refreshing and retrying...\n") //nolint:errcheck
	}

	a.tokens.invalidate()

	// Rebuild the body for the replay.
	if req.GetBody != nil {
		body, bErr := req.GetBody()
		if bErr != nil {
			return nil, fmt.Errorf("rebuild request body for auth retry: %w", bErr)
		}

		req.Body = body
	}

	// Strip our previous Authorization header so applyToken installs
	// the freshly issued token.
	req.Header.Del("Authorization")
	a.applyToken(req)

	return a.next.RoundTrip(req)
}

// viewerTokenResponse is the JSON response from the viewer token endpoint.
type viewerTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
}

// requestViewerToken requests an HMAC viewer token from the controller's
// aggregated API endpoint via the Kubernetes API server. The API server
// authenticates the request using the kubeconfig credentials and proxies
// it to the controller. It returns the token and its expiry time.
func requestViewerToken(cfg *rest.Config) (string, time.Time, error) {
	transport, err := rest.TransportFor(cfg)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build transport: %w", err)
	}

	client := &http.Client{Transport: transport}

	tokenURL := strings.TrimRight(cfg.Host, "/") + viewerTokenEndpointPath

	req, err := http.NewRequest(http.MethodPost, tokenURL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token request failed: %w", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResp viewerTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", time.Time{}, fmt.Errorf("unmarshal token response: %w", err)
	}

	if tokenResp.Token == "" {
		return "", time.Time{}, fmt.Errorf("token endpoint returned empty token")
	}

	// expiresAt is optional; if missing or unparseable, callers will fall
	// back to treating the token as having no known expiry.
	var expiresAt time.Time

	if tokenResp.ExpiresAt != "" {
		if t, parseErr := time.Parse(time.RFC3339, tokenResp.ExpiresAt); parseErr == nil {
			expiresAt = t
		}
	}

	return tokenResp.Token, expiresAt, nil
}

// viewerTokenManager caches an HMAC viewer token and proactively refreshes
// it when 90% or more of its lifetime has elapsed. Refreshes happen
// synchronously on demand (when getToken is called), so an idle session
// will not trigger background renewals.
type viewerTokenManager struct {
	// fetch returns a fresh token and its expiry. It is injected so
	// tests can drive the manager without making network calls.
	fetch func() (string, time.Time, error)

	mu        sync.Mutex
	token     string
	issuedAt  time.Time
	expiresAt time.Time
}

// newViewerTokenManager returns a token manager bound to the given REST config.
func newViewerTokenManager(cfg *rest.Config) *viewerTokenManager {
	return &viewerTokenManager{
		fetch: func() (string, time.Time, error) { return requestViewerToken(cfg) },
	}
}

// refreshThreshold is the fraction of the token lifetime that must elapse
// before getToken will request a new token.
const refreshThreshold = 0.9

// getToken returns a cached token if it is still fresh, otherwise it
// requests a new one. A token is considered fresh when less than
// refreshThreshold of its lifetime has elapsed. If the controller has
// not provided an expiry, the token is reused indefinitely until a 401
// forces invalidation by the caller.
func (m *viewerTokenManager) getToken() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.token != "" && m.shouldReuseLocked() {
		return m.token, nil
	}

	token, expiresAt, err := m.fetch()
	if err != nil {
		// Fall back to the cached token if it has not yet expired. This
		// keeps the dashboard working through transient API server
		// hiccups even after we cross the refresh threshold.
		if m.token != "" && (m.expiresAt.IsZero() || time.Now().Before(m.expiresAt)) {
			return m.token, nil
		}

		return "", err
	}

	m.token = token
	m.issuedAt = time.Now()
	m.expiresAt = expiresAt

	return m.token, nil
}

// invalidate clears the cached token, forcing the next getToken call to
// request a fresh one. Used by the reactive 401 retry path.
func (m *viewerTokenManager) invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.token = ""
	m.issuedAt = time.Time{}
	m.expiresAt = time.Time{}
}

// shouldReuseLocked reports whether the cached token can be reused.
// Caller must hold m.mu.
func (m *viewerTokenManager) shouldReuseLocked() bool {
	// No expiry information: reuse until something else invalidates.
	if m.expiresAt.IsZero() || m.issuedAt.IsZero() {
		return true
	}

	lifetime := m.expiresAt.Sub(m.issuedAt)
	if lifetime <= 0 {
		return false
	}

	elapsed := time.Since(m.issuedAt)

	return float64(elapsed)/float64(lifetime) < refreshThreshold
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
