// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/internal/net/apis/unboundednet/v1alpha1"
	"github.com/Azure/unbounded-kube/internal/net/metrics"
	unboundednetnetlink "github.com/Azure/unbounded-kube/internal/net/netlink"
	statusproto "github.com/Azure/unbounded-kube/internal/net/status/proto"
)

const routingTableRefreshBackstop = 30 * time.Second

var routeSubscribeWithOptions = netlink.RouteSubscribeWithOptions

// nodeHealthState holds the shared state for health and status endpoints
type nodeHealthState struct {
	cniConfigured   *bool
	statusServer    *nodeStatusServer      // Set once WireGuard state is available
	informersSynced []cache.InformerSynced // Informer HasSynced funcs for readiness
	mu              sync.RWMutex
}

func (h *nodeHealthState) setStatusServer(srv *nodeStatusServer) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.statusServer = srv
}

func (h *nodeHealthState) getStatusServer() *nodeStatusServer {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.statusServer
}

const (
	serviceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	hmacTokenEndpointPath   = "/apis/status.net.unbounded-kube.io/v1alpha1/token/node"
)

// hmacTokenManager manages the HMAC authentication token for the node agent.
// It requests tokens from the controller's aggregated API endpoint and
// refreshes them before expiry or on 401 responses.
type hmacTokenManager struct {
	mu          sync.Mutex
	token       string
	issuedAt    time.Time
	expiresAt   time.Time
	nodeName    string
	saTokenPath string
	tokenURL    string
	client      *http.Client
}

// hmacTokenResponse is the JSON response from the token endpoint.
type hmacTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	NodeName  string    `json:"nodeName"`
}

// newHMACTokenManager creates a token manager that requests HMAC tokens from
// the controller's aggregated API token endpoint via the Kubernetes API server.
func newHMACTokenManager(nodeName string) *hmacTokenManager {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))

	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	if port == "" {
		port = "443"
	}

	tokenURL := fmt.Sprintf("https://%s:%s%s", host, port, hmacTokenEndpointPath)

	pool := x509.NewCertPool()
	if data, err := os.ReadFile(serviceAccountCACertPath); err == nil {
		pool.AppendCertsFromPEM(data)
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    pool,
			},
		},
	}

	return &hmacTokenManager{
		nodeName:    nodeName,
		saTokenPath: serviceAccountTokenPath,
		tokenURL:    tokenURL,
		client:      client,
	}
}

// getToken returns the current HMAC token, requesting a new one if needed.
func (tm *hmacTokenManager) getToken() (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.token != "" && !tm.expiresAt.IsZero() {
		// Refresh when 75% of the token lifetime has elapsed (i.e., only
		// 25% of the lifetime remains).
		lifetime := tm.expiresAt.Sub(tm.issuedAt)

		refreshAt := tm.issuedAt.Add(lifetime * 3 / 4)
		if time.Now().Before(refreshAt) {
			return tm.token, nil
		}
	}

	if err := tm.requestToken(); err != nil {
		// If we have a cached token that hasn't expired yet, use it.
		if tm.token != "" && time.Now().Before(tm.expiresAt) {
			klog.V(2).Infof("HMAC token refresh failed, using cached token: %v", err)
			return tm.token, nil
		}

		return "", err
	}

	return tm.token, nil
}

// invalidate marks the current token as expired, forcing a re-request.
func (tm *hmacTokenManager) invalidate() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.token = ""
	tm.issuedAt = time.Time{}
	tm.expiresAt = time.Time{}
}

// requestToken calls the token endpoint to get a new HMAC token.
// Must be called with tm.mu held.
func (tm *hmacTokenManager) requestToken() error {
	saToken, err := os.ReadFile(tm.saTokenPath)
	if err != nil {
		return fmt.Errorf("read service account token: %w", err)
	}

	body, err := json.Marshal(map[string]string{
		"serviceAccountToken": strings.TrimSpace(string(saToken)),
	})
	if err != nil {
		return fmt.Errorf("marshal token request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, tm.tokenURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(saToken)))

	resp, err := tm.client.Do(req)
	if err != nil {
		return fmt.Errorf("token request failed: %w", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var tokenResp hmacTokenResponse
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return fmt.Errorf("unmarshal token response: %w", err)
	}

	if tokenResp.Token == "" {
		return fmt.Errorf("token endpoint returned empty token")
	}

	tm.token = tokenResp.Token
	tm.issuedAt = time.Now()
	tm.expiresAt = tokenResp.ExpiresAt
	klog.V(2).Infof("HMAC token acquired, expires at %s", tm.expiresAt.Format(time.RFC3339))

	return nil
}

func startHealthServer(port int, healthState *nodeHealthState) {
	mux := http.NewServeMux()

	metrics.Register(mux)

	// /healthz - liveness probe: is the process alive and serving?
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)

		if _, err := w.Write([]byte("ok")); err != nil {
			klog.V(4).Infof("healthz write failed: %v", err)
		}
	})

	// /readyz - readiness probe: informers synced and route manager ready
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		for _, synced := range healthState.informersSynced {
			if !synced() {
				w.WriteHeader(http.StatusServiceUnavailable)

				if _, err := w.Write([]byte("informer caches not synced")); err != nil {
					klog.V(4).Infof("readyz write failed: %v", err)
				}

				return
			}
		}

		w.WriteHeader(http.StatusOK)

		if _, err := w.Write([]byte("ok")); err != nil {
			klog.V(4).Infof("readyz write failed: %v", err)
		}
	})

	// /status/json - JSON status endpoint
	mux.HandleFunc("/status/json", func(w http.ResponseWriter, r *http.Request) {
		srv := healthState.getStatusServer()
		if srv != nil {
			srv.handleStatusJSON(w, r)
		} else {
			// Return minimal status until WireGuard state is available
			status := NodeStatusResponse{
				Timestamp: time.Now(),
				NodeInfo: NodeInfo{
					Name:      os.Getenv("NODE_NAME"),
					PodCIDRs:  []string{},
					BuildInfo: nodeAgentBuildInfo(),
				},
			}

			w.Header().Set("Content-Type", "application/json")

			if err := json.NewEncoder(w).Encode(status); err != nil {
				klog.V(4).Infof("status json encode failed: %v", err)
			}
		}
	})

	// /status - JSON status endpoint (same as /status/json)
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		srv := healthState.getStatusServer()
		if srv != nil {
			srv.handleStatusJSON(w, r)
			return
		}
		// Return minimal status until WireGuard state is available
		status := NodeStatusResponse{
			Timestamp: time.Now(),
			NodeInfo: NodeInfo{
				Name:      os.Getenv("NODE_NAME"),
				PodCIDRs:  []string{},
				BuildInfo: nodeAgentBuildInfo(),
			},
		}

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(status); err != nil {
			klog.V(4).Infof("status json encode failed: %v", err)
		}
	})

	addr := fmt.Sprintf(":%d", port)
	klog.Infof("Starting health server on %s", addr)

	httpMiddleware := metrics.NewHTTPMiddleware("unbounded_cni_node")

	server := &http.Server{
		Addr:              addr,
		Handler:           httpMiddleware.Wrap("all", mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		klog.Errorf("Health server error: %v", err)
	}
}

// nodeStatusPushAck is the JSON acknowledgment returned by the controller for push updates.
// Kept for backward-compatible JSON fallback parsing during protobuf rollout.
type nodeStatusPushAck struct {
	Status   string `json:"status"`
	Revision uint64 `json:"revision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

const (
	statusWSAPIServerModeNever         = "never"
	statusWSAPIServerModeFallback      = "fallback"
	statusWSAPIServerModePreferred     = "preferred"
	defaultAggregatedNodeStatusWSURL   = "wss://$(KUBERNETES_SERVICE_HOST)/apis/status.net.unbounded-kube.io/v1alpha1/status/nodews"
	defaultAggregatedNodeStatusPushURL = "https://$(KUBERNETES_SERVICE_HOST)/apis/status.net.unbounded-kube.io/v1alpha1/status/push"
	directRecoveryProbeMultiplier      = 3
	serviceAccountCACertPath           = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	legacyAggregatedStatusAPIPath      = "/apis/net.unbounded-kube.io/v1alpha1/status/"
	currentAggregatedStatusAPIPath     = "/apis/status.net.unbounded-kube.io/v1alpha1/status/"
	statusWSModeNone                   = int32(0)
	statusWSModeDirect                 = int32(1)
	statusWSModeFallback               = int32(2)
)

// newStatusPushHTTPClient creates an HTTP client for controller connections.
// The client must trust two CAs: the controller's self-signed CA (for direct
// HTTPS connections) and the cluster CA (for API server fallback connections).
// Both are loaded into a single CA pool.
func newStatusPushHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone() //nolint:errcheck
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	pool := x509.NewCertPool()
	loaded := false

	// Load the controller's self-signed CA for direct connections.
	if data, err := os.ReadFile("/var/run/secrets/unbounded-net/ca.crt"); err == nil {
		if pool.AppendCertsFromPEM(data) {
			klog.V(2).Info("Loaded controller CA from /var/run/secrets/unbounded-net/ca.crt")

			loaded = true
		}
	}

	// Also load the cluster CA for API server fallback connections.
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"); err == nil {
		if pool.AppendCertsFromPEM(data) {
			klog.V(2).Info("Loaded cluster CA from service account ca.crt")

			loaded = true
		}
	}

	if loaded {
		tlsConfig.RootCAs = pool
	} else {
		klog.Warningf("No CA certificates found for TLS verification")
	}

	transport.TLSClientConfig = tlsConfig

	return &http.Client{Timeout: timeout, Transport: transport}
}

func expandKubernetesServiceHost(rawURL string) string {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	if host == "" {
		return rawURL
	}

	expanded := strings.ReplaceAll(rawURL, "$(KUBERNETES_SERVICE_HOST)", host)
	expanded = strings.ReplaceAll(expanded, "${KUBERNETES_SERVICE_HOST}", host)

	return expanded
}

func parseStatusWSAPIServerMode(mode string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(mode))
	if normalized == "" {
		normalized = statusWSAPIServerModeFallback
	}

	switch normalized {
	case statusWSAPIServerModeNever, statusWSAPIServerModeFallback, statusWSAPIServerModePreferred:
		return normalized, nil
	default:
		return "", fmt.Errorf("invalid status websocket API server mode %q (expected never, fallback, preferred)", mode)
	}
}

// normalizeAggregatedStatusAPIURL rewrites legacy aggregated API group URLs to the current status group.
func normalizeAggregatedStatusAPIURL(rawURL string) string {
	if strings.Contains(rawURL, legacyAggregatedStatusAPIPath) {
		return strings.Replace(rawURL, legacyAggregatedStatusAPIPath, currentAggregatedStatusAPIPath, 1)
	}

	return rawURL
}

func resolveStatusPushAPIServerURL(cfg *config) string {
	if cfg.StatusWSAPIServerURL != "" {
		normalizedWSURL := normalizeAggregatedStatusAPIURL(cfg.StatusWSAPIServerURL)
		if normalizedWSURL != cfg.StatusWSAPIServerURL {
			klog.V(2).Infof("Status push: rewrote legacy aggregated API URL from %q to %q", cfg.StatusWSAPIServerURL, normalizedWSURL)
		}

		pushURL := strings.Replace(normalizedWSURL, "/status/nodews", "/status/push", 1)
		if strings.HasPrefix(pushURL, "wss://") {
			pushURL = "https://" + strings.TrimPrefix(pushURL, "wss://")
		} else if strings.HasPrefix(pushURL, "ws://") {
			pushURL = "http://" + strings.TrimPrefix(pushURL, "ws://")
		}

		return expandKubernetesServiceHost(pushURL)
	}

	return expandKubernetesServiceHost(defaultAggregatedNodeStatusPushURL)
}

// resolveDirectStatusPushURL resolves the direct controller push endpoint.
func resolveDirectStatusPushURL(cfg *config) string {
	if cfg.StatusPushURL != "" {
		return cfg.StatusPushURL
	}

	host := os.Getenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST")
	port := os.Getenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT")

	if host == "" {
		return ""
	}

	if port == "" {
		port = "9999"
	}

	return fmt.Sprintf("https://%s:%s/status/push", host, port)
}

// resolveDirectStatusWebSocketURL resolves the direct controller websocket endpoint.
func resolveDirectStatusWebSocketURL(cfg *config) string {
	if cfg.StatusWSURL != "" {
		return cfg.StatusWSURL
	}

	host := os.Getenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST")
	port := os.Getenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT")

	if host == "" {
		return ""
	}

	if port == "" {
		port = "9999"
	}

	return fmt.Sprintf("wss://%s:%s/status/nodews", host, port)
}

// statusDirectRecoveryProbeInterval returns how often fallback websocket sessions probe direct recovery.
func statusDirectRecoveryProbeInterval(statusPushInterval time.Duration) time.Duration {
	if statusPushInterval <= 0 {
		statusPushInterval = 10 * time.Second
	}

	return statusPushInterval * directRecoveryProbeMultiplier
}

// isAPIServerStatusWebSocketURL reports whether the websocket endpoint is the aggregated API server path.
func isAPIServerStatusWebSocketURL(endpointURL string) bool {
	return strings.Contains(endpointURL, "/apis/")
}

func isStatusAPIServerFallbackAllowed(startedAt, directDownSince, now time.Time, startupDelay time.Duration, hasDirectPath bool) bool {
	if !hasDirectPath || startupDelay <= 0 {
		return true
	}

	if directDownSince.IsZero() {
		return false
	}

	if now.Before(directDownSince) {
		return false
	}

	return now.Sub(directDownSince) >= startupDelay
}

func resolveStatusWebSocketURLs(cfg *config, allowAPIServerFallback bool) []string {
	if cfg.StatusWSURL != "" {
		return []string{cfg.StatusWSURL}
	}

	mode, err := parseStatusWSAPIServerMode(cfg.StatusWSAPIServerMode)
	if err != nil {
		klog.Warningf("Status websocket: %v; using fallback mode", err)

		mode = statusWSAPIServerModeFallback
	}

	if !allowAPIServerFallback {
		mode = statusWSAPIServerModeNever
	}

	urls := make([]string, 0, 2)
	host := os.Getenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST")

	port := os.Getenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT")
	if host != "" {
		if port == "" {
			port = "9999"
		}

		urls = append(urls, fmt.Sprintf("wss://%s:%s/status/nodews", host, port))
	}

	apiserverURL := cfg.StatusWSAPIServerURL
	if apiserverURL == "" {
		apiserverURL = defaultAggregatedNodeStatusWSURL
	}

	apiserverURL = normalizeAggregatedStatusAPIURL(apiserverURL)
	apiserverURL = expandKubernetesServiceHost(apiserverURL)

	switch mode {
	case statusWSAPIServerModeNever:
		// Keep direct controller websocket only.
	case statusWSAPIServerModePreferred:
		prioritized := make([]string, 0, 2)
		if apiserverURL != "" {
			prioritized = append(prioritized, apiserverURL)
		}

		prioritized = append(prioritized, urls...)
		urls = prioritized
	default:
		if apiserverURL != "" {
			urls = append(urls, apiserverURL)
		}
	}

	seen := make(map[string]struct{}, len(urls))

	uniqueURLs := make([]string, 0, len(urls))
	for _, endpointURL := range urls {
		if endpointURL == "" {
			continue
		}

		if _, exists := seen[endpointURL]; exists {
			continue
		}

		seen[endpointURL] = struct{}{}
		uniqueURLs = append(uniqueURLs, endpointURL)
	}

	return uniqueURLs
}

func resolveFallbackStatusWebSocketURL(cfg *config, directWSURL string) string {
	urls := resolveStatusWebSocketURLs(cfg, true)
	for _, endpointURL := range urls {
		if endpointURL == "" || endpointURL == directWSURL {
			continue
		}

		return endpointURL
	}

	return ""
}

func computeStatusDelta(prev, curr *NodeStatusResponse) (map[string]json.RawMessage, error) {
	if prev == nil {
		return nil, nil
	}

	prevRaw, err := json.Marshal(prev)
	if err != nil {
		return nil, err
	}

	currRaw, err := json.Marshal(curr)
	if err != nil {
		return nil, err
	}

	var prevMap map[string]json.RawMessage
	if err := json.Unmarshal(prevRaw, &prevMap); err != nil {
		return nil, err
	}

	var currMap map[string]json.RawMessage
	if err := json.Unmarshal(currRaw, &currMap); err != nil {
		return nil, err
	}

	delta := make(map[string]json.RawMessage)
	if nodeInfo, ok := currMap["nodeInfo"]; ok {
		delta["nodeInfo"] = nodeInfo
	}

	for key, value := range currMap {
		if key == "nodeInfo" {
			continue
		}

		prevValue, exists := prevMap[key]
		if !exists || !bytes.Equal(prevValue, value) {
			delta[key] = value
		}
	}
	// NOTE: don't emit "null" for keys present in prev but missing in curr.
	// Go json.Marshal omits nil slices/pointers with omitempty, so a missing
	// key in currMap usually means the field is nil/empty, not intentionally
	// cleared. Emitting null would wipe out the controller's cached data.

	if len(delta) == 1 {
		return nil, nil
	}

	return delta, nil
}

func stripPeerStats(status *NodeStatusResponse) *NodeStatusResponse {
	clone := *status

	clone.Peers = make([]WireGuardPeerStatus, 0, len(status.Peers))
	for _, peer := range status.Peers {
		peerCopy := peer
		peerCopy.Tunnel.RxBytes = 0
		peerCopy.Tunnel.TxBytes = 0
		peerCopy.Tunnel.LastHandshake = time.Time{}
		clone.Peers = append(clone.Peers, peerCopy)
	}

	return &clone
}

func peerStatsSnapshot(status *NodeStatusResponse) map[string][2]int64 {
	snapshot := make(map[string][2]int64, len(status.Peers))
	for _, peer := range status.Peers {
		key := peer.Name + "|" + peer.Tunnel.PublicKey + "|" + peer.Tunnel.Interface
		snapshot[key] = [2]int64{peer.Tunnel.RxBytes, peer.Tunnel.TxBytes}
	}

	return snapshot
}

const (
	nodeErrorTypeDirectPush      = "directPush"
	nodeErrorTypeDirectWebSocket = "directWebsocket"
	nodeErrorTypeFallbackPush    = "fallbackPush"
	nodeErrorTypeFallbackWS      = "fallbackWebsocket"
)

func appendNodeError(healthState *nodeHealthState, errorType, message string) {
	trimmedType := strings.TrimSpace(errorType)

	trimmedMessage := strings.TrimSpace(message)
	if trimmedType == "" || trimmedMessage == "" {
		return
	}

	srv := healthState.getStatusServer()
	if srv == nil || srv.state == nil {
		return
	}

	srv.state.mu.Lock()
	defer srv.state.mu.Unlock()

	now := time.Now()

	// If the same error already exists, refresh its timestamp instead of appending.
	errors := srv.state.nodeErrors
	for i := range errors {
		if errors[i].Type == trimmedType && errors[i].Message == trimmedMessage {
			errors[i].Timestamp = now
			return
		}
	}

	const maxNodeErrors = 20

	errors = append(errors, NodeError{Type: trimmedType, Message: trimmedMessage, Timestamp: now})
	if len(errors) > maxNodeErrors {
		errors = append([]NodeError(nil), errors[len(errors)-maxNodeErrors:]...)
	}

	srv.state.nodeErrors = errors
}

// filterExpiredNodeErrors returns a copy of errors with entries older than
// maxAge removed. Errors without a timestamp are kept.
func filterExpiredNodeErrors(errors []NodeError, now time.Time, maxAge time.Duration) []NodeError {
	if len(errors) == 0 {
		return nil
	}

	result := make([]NodeError, 0, len(errors))
	for _, e := range errors {
		if !e.Timestamp.IsZero() && now.Sub(e.Timestamp) > maxAge {
			continue
		}

		result = append(result, e)
	}

	if len(result) == 0 {
		return nil
	}

	return result
}

func clearNodeErrorsByTypes(healthState *nodeHealthState, errorTypes ...string) {
	if len(errorTypes) == 0 {
		return
	}

	typeSet := make(map[string]struct{}, len(errorTypes))
	for _, errorType := range errorTypes {
		trimmed := strings.TrimSpace(errorType)
		if trimmed == "" {
			continue
		}

		typeSet[trimmed] = struct{}{}
	}

	if len(typeSet) == 0 {
		return
	}

	srv := healthState.getStatusServer()
	if srv == nil || srv.state == nil {
		return
	}

	srv.state.mu.Lock()
	defer srv.state.mu.Unlock()

	if len(srv.state.nodeErrors) == 0 {
		return
	}

	filtered := make([]NodeError, 0, len(srv.state.nodeErrors))
	for _, nodeError := range srv.state.nodeErrors {
		if _, remove := typeSet[nodeError.Type]; remove {
			continue
		}

		filtered = append(filtered, nodeError)
	}

	if len(filtered) == 0 {
		srv.state.nodeErrors = nil
		return
	}

	srv.state.nodeErrors = filtered
}

func startStatusWebSocketPusher(
	ctx context.Context,
	cfg *config,
	healthState *nodeHealthState,
	wsConnected *atomic.Bool,
	wsMode *atomic.Int32,
	fallbackWSEnabled *atomic.Bool,
	apiPushEnabled *atomic.Bool,
	closeFallbackWS *atomic.Bool,
) {
	if !cfg.StatusWSEnabled {
		klog.Info("Status websocket push is disabled")

		if wsConnected != nil {
			wsConnected.Store(false)
		}

		if wsMode != nil {
			wsMode.Store(statusWSModeNone)
		}

		return
	}

	// Reuse the push client's TLS trust setup so wss://KUBERNETES_SERVICE_HOST
	// can validate the cluster CA in fallback/preferred API server modes.
	// Keep timeout disabled for long-lived websocket connections.
	dialHTTPClient := newStatusPushHTTPClient(0)
	directWSURL := resolveDirectStatusWebSocketURL(cfg)

	// SA token reader for aggregated API server fallback paths.
	var (
		cachedSAToken string
		saTokenReadAt time.Time
	)

	const saTokenRefreshInterval = 5 * time.Minute

	getSAToken := func() string {
		if cachedSAToken != "" && time.Since(saTokenReadAt) < saTokenRefreshInterval {
			return cachedSAToken
		}

		data, err := os.ReadFile(serviceAccountTokenPath)
		if err != nil {
			klog.V(4).Infof("Status websocket: failed to read service account token: %v", err)
			return cachedSAToken
		}

		cachedSAToken = strings.TrimSpace(string(data))
		saTokenReadAt = time.Now()

		return cachedSAToken
	}

	// HMAC token manager for direct controller connections.
	hmacMgr := newHMACTokenManager(cfg.NodeName)
	getToken := func() string {
		token, err := hmacMgr.getToken()
		if err != nil {
			klog.V(2).Infof("Status websocket: failed to get HMAC token: %v", err)
			return ""
		}

		return token
	}

	criticalEvery := cfg.CriticalDeltaEvery
	if criticalEvery <= 0 {
		criticalEvery = time.Second
	}

	statsEvery := cfg.StatsDeltaEvery
	if statsEvery <= 0 {
		statsEvery = 15 * time.Second
	}

	fullSyncEvery := cfg.FullSyncEvery
	if fullSyncEvery <= 0 {
		fullSyncEvery = 2 * time.Minute
	}
	// criticalEvery and statsEvery are maximum publish rates -- we only send when
	// a snapshot actually changes; changes are batched until the next tick.
	// fullSyncEvery forces a complete status push regardless of changes.

	directBackoff := time.Second
	fallbackBackoff := time.Second

	var (
		nextDirectAttemptAt   time.Time
		nextFallbackAttemptAt time.Time
	)

	for {
		select {
		case <-ctx.Done():
			if wsConnected != nil {
				wsConnected.Store(false)
			}

			return
		default:
		}

		now := time.Now()

		allowAPIServerFallback := directWSURL == ""
		if !allowAPIServerFallback && fallbackWSEnabled != nil {
			allowAPIServerFallback = fallbackWSEnabled.Load()
		}

		fallbackWSURL := resolveFallbackStatusWebSocketURL(cfg, directWSURL)
		if directWSURL == "" && (!allowAPIServerFallback || fallbackWSURL == "") {
			if wsConnected != nil {
				wsConnected.Store(false)
			}

			if wsMode != nil {
				wsMode.Store(statusWSModeNone)
			}

			if allowAPIServerFallback && fallbackWSURL != "" {
				klog.Warning("Status websocket: no websocket endpoint configured, retrying")
			} else {
				klog.V(2).Infof("Status websocket: waiting %s before enabling API server fallback endpoint", cfg.StatusWSAPIServerStartupDelay)
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}

			continue
		}

		type dialAttempt struct {
			url      string
			isDirect bool
			timeout  time.Duration
			headers  http.Header
		}

		type dialResult struct {
			url      string
			isDirect bool
			conn     *websocket.Conn
			err      error
		}

		attempts := make([]dialAttempt, 0, 2)

		if directWSURL != "" && (nextDirectAttemptAt.IsZero() || !now.Before(nextDirectAttemptAt)) {
			h := http.Header{}
			if token := getToken(); token != "" {
				h.Set("Authorization", "Bearer "+token)
			}

			attempts = append(attempts, dialAttempt{url: directWSURL, isDirect: true, timeout: 5 * time.Second, headers: h})
		}

		if allowAPIServerFallback && fallbackWSURL != "" && (nextFallbackAttemptAt.IsZero() || !now.Before(nextFallbackAttemptAt)) {
			h := http.Header{}
			if token := getSAToken(); token != "" {
				h.Set("Authorization", "Bearer "+token)
			}

			attempts = append(attempts, dialAttempt{url: fallbackWSURL, isDirect: false, timeout: 5 * time.Second, headers: h})
		}

		if len(attempts) == 0 {
			nextAttemptAt := now.Add(time.Second)
			if !nextDirectAttemptAt.IsZero() && nextDirectAttemptAt.Before(nextAttemptAt) {
				nextAttemptAt = nextDirectAttemptAt
			}

			if !nextFallbackAttemptAt.IsZero() && nextFallbackAttemptAt.Before(nextAttemptAt) {
				nextAttemptAt = nextFallbackAttemptAt
			}

			wait := time.Until(nextAttemptAt)
			if wait < 100*time.Millisecond {
				wait = 100 * time.Millisecond
			}

			select {
			case <-ctx.Done():
				if wsConnected != nil {
					wsConnected.Store(false)
				}

				return
			case <-time.After(wait):
			}

			continue
		}

		resultsCh := make(chan dialResult, len(attempts))
		// Use a context detached from the parent for dialing so the
		// connection survives parent cancellation long enough to send
		// a graceful close frame during shutdown.
		connCtx, connCancel := context.WithCancel(context.WithoutCancel(ctx))

		for _, attempt := range attempts {
			attempt := attempt

			go func() {
				dialCtx, cancel := context.WithTimeout(connCtx, attempt.timeout)
				defer cancel()

				candidateConn, _, dialErr := websocket.Dial(dialCtx, attempt.url, &websocket.DialOptions{
					HTTPHeader:      attempt.headers,
					HTTPClient:      dialHTTPClient,
					CompressionMode: websocket.CompressionContextTakeover,
				})
				resultsCh <- dialResult{url: attempt.url, isDirect: attempt.isDirect, conn: candidateConn, err: dialErr}
			}()
		}

		var (
			conn          *websocket.Conn
			wsURL         string
			directErr     error
			fallbackErr   error
			directTried   bool
			fallbackTried bool
			successes     []dialResult
		)

		for range attempts {
			result := <-resultsCh
			if result.isDirect {
				directTried = true

				if result.err != nil {
					directErr = result.err
				}
			} else {
				fallbackTried = true

				if result.err != nil {
					fallbackErr = result.err
				}
			}

			if result.err == nil && result.conn != nil {
				successes = append(successes, result)
			}
		}

		for _, success := range successes {
			if success.isDirect {
				conn = success.conn
				wsURL = success.url

				break
			}
		}

		if conn == nil && len(successes) > 0 {
			conn = successes[0].conn
			wsURL = successes[0].url
		}

		for _, success := range successes {
			if conn != nil && success.conn == conn {
				continue
			}

			_ = success.conn.Close(websocket.StatusNormalClosure, "alternate endpoint not selected") //nolint:errcheck
		}

		if directTried {
			if conn != nil && wsURL == directWSURL {
				directBackoff = time.Second
				nextDirectAttemptAt = time.Time{}
			} else {
				nextDirectAttemptAt = now.Add(directBackoff)
				directBackoff = nextExponentialBackoff(directBackoff, 15*time.Second)
			}
		}

		if fallbackTried {
			if conn != nil && wsURL == fallbackWSURL {
				fallbackBackoff = time.Second
				nextFallbackAttemptAt = time.Time{}
			} else {
				nextFallbackAttemptAt = now.Add(fallbackBackoff)
				fallbackBackoff = nextExponentialBackoff(fallbackBackoff, 15*time.Second)
			}
		}

		if conn == nil {
			if wsConnected != nil {
				wsConnected.Store(false)
			}

			if wsMode != nil {
				wsMode.Store(statusWSModeNone)
			}

			klog.V(2).Infof("Status websocket: connect failed (direct=%v fallback=%v)", directErr, fallbackErr)
			connCancel()

			continue
		}

		if wsConnected != nil {
			wsConnected.Store(true)
		}
		// Clear any push/WS errors from before the connection succeeded
		clearNodeErrorsByTypes(healthState, nodeErrorTypeDirectPush, nodeErrorTypeDirectWebSocket, nodeErrorTypeFallbackPush, nodeErrorTypeFallbackWS)

		if wsMode != nil {
			if wsURL == directWSURL {
				wsMode.Store(statusWSModeDirect)

				if fallbackWSEnabled != nil {
					fallbackWSEnabled.Store(false)
				}

				if apiPushEnabled != nil {
					apiPushEnabled.Store(false)
				}

				if closeFallbackWS != nil {
					closeFallbackWS.Store(false)
				}
			} else {
				wsMode.Store(statusWSModeFallback)

				if apiPushEnabled != nil {
					apiPushEnabled.Store(false)
				}
			}
		}

		klog.Infof("Status websocket connected: %s", wsURL)
		connectedViaAPIServer := isAPIServerStatusWebSocketURL(wsURL)
		directRecoveryBackoff := time.Second

		var (
			directRecoveryTimer *time.Timer
			directRecoveryCh    <-chan time.Time
		)

		if connectedViaAPIServer && directWSURL != "" {
			directRecoveryTimer = time.NewTimer(directRecoveryBackoff)
			directRecoveryCh = directRecoveryTimer.C
		}

		var (
			lastSentStatus       *NodeStatusResponse
			lastCriticalSnapshot *NodeStatusResponse
			revision             atomic.Uint64
			resyncRequired       atomic.Bool
			lastAckTimeNs        atomic.Int64
		)

		lastAckTimeNs.Store(time.Now().UnixNano())

		readCtx, readCancel := context.WithCancel(connCtx)

		go func() {
			defer readCancel()

			for {
				_, data, readErr := conn.Read(readCtx)
				if readErr != nil {
					klog.V(2).Infof("Status websocket: read loop closed: %v", readErr)
					return
				}

				lastAckTimeNs.Store(time.Now().UnixNano())

				var ack statusproto.NodeStatusAck
				if err := proto.Unmarshal(data, &ack); err != nil {
					klog.V(4).Infof("Status websocket: failed to unmarshal protobuf ack, trying JSON fallback: %v", err)
					// Fallback: try JSON for backward compatibility during rollout.
					var envelope struct {
						Type string            `json:"type"`
						Data nodeStatusPushAck `json:"data"`
					}
					if jsonErr := json.Unmarshal(data, &envelope); jsonErr != nil {
						continue
					}

					switch envelope.Type {
					case "node_status_ack":
						if envelope.Data.Revision > 0 {
							revision.Store(envelope.Data.Revision)
						}
					case "node_status_resync":
						if envelope.Data.Revision > 0 {
							revision.Store(envelope.Data.Revision)
						}

						resyncRequired.Store(true)
					}

					continue
				}

				switch ack.Status {
				case "ok":
					if ack.Revision > 0 {
						revision.Store(ack.Revision)
					}
				case "resync_required":
					if ack.Revision > 0 {
						revision.Store(ack.Revision)
					}

					resyncRequired.Store(true)
				}
			}
		}()

		sendFull := func() error {
			srv := healthState.getStatusServer()
			if srv == nil {
				return fmt.Errorf("status server not ready")
			}

			status := srv.getNodeStatus()
			if len(status.NodeErrors) > 0 {
				// When the websocket is established, publish a clean snapshot so
				// controller-side problem lists drop startup transport errors immediately.
				filtered := make([]NodeError, 0, len(status.NodeErrors))
				for _, nodeError := range status.NodeErrors {
					switch nodeError.Type {
					case nodeErrorTypeDirectPush, nodeErrorTypeDirectWebSocket, nodeErrorTypeFallbackPush, nodeErrorTypeFallbackWS:
						continue
					default:
						filtered = append(filtered, nodeError)
					}
				}

				status.NodeErrors = filtered
			}

			msg := &statusproto.NodeStatusMessage{
				Type:     "node_status_full",
				NodeName: status.NodeInfo.Name,
				Status:   nodeStatusToProto(status),
			}

			payload, err := proto.Marshal(msg)
			if err != nil {
				return err
			}

			if err := conn.Write(connCtx, websocket.MessageBinary, payload); err != nil {
				return err
			}
			// A successful websocket status write confirms controller communication is healthy.
			clearNodeErrorsByTypes(healthState, nodeErrorTypeDirectPush, nodeErrorTypeDirectWebSocket, nodeErrorTypeFallbackPush, nodeErrorTypeFallbackWS)

			lastSentStatus = status
			lastCriticalSnapshot = stripPeerStats(status)

			resyncRequired.Store(false)

			return nil
		}

		// Wait for the status server to be ready and send initial full status.
		// The status server might not be registered yet if reconciliation is still
		// running concurrently.
		initialSendOk := false

		for attempt := 0; attempt < 30; attempt++ {
			if err := sendFull(); err != nil {
				if healthState.getStatusServer() == nil {
					// Status server not ready yet; wait briefly and retry.
					select {
					case <-ctx.Done():
						break
					case <-time.After(500 * time.Millisecond):
					}

					continue
				}
				// Status server is ready but send failed -- connection issue.
				klog.V(2).Infof("Status websocket: initial full send failed: %v", err)

				break
			}

			initialSendOk = true

			break
		}

		if !initialSendOk {
			if wsConnected != nil {
				wsConnected.Store(false)
			}

			_ = conn.Close(websocket.StatusInternalError, "initial full send failed") //nolint:errcheck

			connCancel()

			continue
		}

		criticalTicker := time.NewTicker(criticalEvery)
		statsTicker := time.NewTicker(statsEvery)
		fullSyncTicker := time.NewTicker(fullSyncEvery)

		var (
			keepaliveTicker *time.Ticker
			keepaliveCh     <-chan time.Time
		)

		keepaliveFailures := 0

		if cfg.StatusWSKeepaliveInterval > 0 {
			keepaliveTicker = time.NewTicker(cfg.StatusWSKeepaliveInterval)
			keepaliveCh = keepaliveTicker.C
		}

		fallbackCloseTicker := time.NewTicker(500 * time.Millisecond)

	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case <-readCtx.Done():
				break loop
			case <-criticalTicker.C:
				if resyncRequired.Load() || lastSentStatus == nil {
					if err := sendFull(); err != nil {
						klog.V(2).Infof("Status websocket: resync full send failed: %v", err)
						break loop
					}

					continue
				}

				srv := healthState.getStatusServer()
				if srv == nil {
					continue
				}

				current := srv.getNodeStatus()

				criticalSnapshot := stripPeerStats(current)
				if reflect.DeepEqual(lastCriticalSnapshot, criticalSnapshot) {
					continue
				}

				delta, err := computeStatusDelta(lastSentStatus, current)
				if err != nil || len(delta) == 0 {
					continue
				}

				message := &statusproto.NodeStatusMessage{
					Type:         "node_status_delta",
					NodeName:     current.NodeInfo.Name,
					BaseRevision: revision.Load(),
					Delta:        nodeStatusDeltaToProto(delta),
				}

				payload, err := proto.Marshal(message)
				if err != nil {
					continue
				}

				if err := conn.Write(connCtx, websocket.MessageBinary, payload); err != nil {
					klog.V(2).Infof("Status websocket: critical delta write failed: %v", err)
					break loop
				}

				lastSentStatus = current
				lastCriticalSnapshot = criticalSnapshot
			case <-statsTicker.C:
				// Always send a delta on the stats interval even if stats
				// appear unchanged, so the controller sees fresh timestamps.
				srv := healthState.getStatusServer()
				if srv == nil {
					continue
				}

				current := srv.getNodeStatus()

				delta, err := computeStatusDelta(lastSentStatus, current)
				if err != nil || len(delta) == 0 {
					// No computable delta -- fall back to full send.
					if err := sendFull(); err != nil {
						klog.V(2).Infof("Status websocket: stats fallback full send failed: %v", err)
						break loop
					}

					continue
				}

				wsMsg := &statusproto.NodeStatusMessage{
					Type:         "node_status_delta",
					NodeName:     current.NodeInfo.Name,
					BaseRevision: revision.Load(),
					Delta:        nodeStatusDeltaToProto(delta),
				}

				payload, err := proto.Marshal(wsMsg)
				if err != nil {
					continue
				}

				if err := conn.Write(connCtx, websocket.MessageBinary, payload); err != nil {
					klog.V(2).Infof("Status websocket: stats delta write failed: %v", err)
					break loop
				}

				lastSentStatus = current
				lastCriticalSnapshot = stripPeerStats(current)
			case <-fullSyncTicker.C:
				// Forced full status sync to ensure the controller has
				// complete status regardless of delta accumulation.
				if err := sendFull(); err != nil {
					klog.V(2).Infof("Status websocket: full sync send failed: %v", err)
					break loop
				}
			case <-keepaliveCh:
				// Skip ping if we received an ack recently
				lastAck := time.Unix(0, lastAckTimeNs.Load())
				if time.Since(lastAck) < cfg.StatusWSKeepaliveInterval {
					keepaliveFailures = 0
					continue
				}

				pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
				pingErr := conn.Ping(pingCtx)

				pingCancel()

				if pingErr != nil {
					keepaliveFailures++
					klog.V(2).Infof("Status websocket: keepalive ping failed (node=%s): %v (consecutive failures=%d/%d)", cfg.NodeName, pingErr, keepaliveFailures, cfg.StatusWSKeepaliveFailureCount)

					if keepaliveFailures >= cfg.StatusWSKeepaliveFailureCount {
						break loop
					}

					continue
				}

				if keepaliveFailures > 0 {
					klog.V(4).Infof("Status websocket: keepalive recovered (node=%s), resetting failure count", cfg.NodeName)

					keepaliveFailures = 0
				}
			case <-directRecoveryCh:
				if tryDirectRecoveryProbe(ctx, healthState, dialHTTPClient, getToken, directWSURL, cfg.NodeName) {
					klog.V(2).Info("Status websocket: direct connectivity probe succeeded while on API server websocket; reconnecting to prefer direct endpoint")
					break loop
				}

				directRecoveryBackoff = nextExponentialBackoff(directRecoveryBackoff, 30*time.Second)
				if directRecoveryTimer != nil {
					directRecoveryTimer.Reset(directRecoveryBackoff)
				}
			case <-fallbackCloseTicker.C:
				if wsURL == fallbackWSURL && closeFallbackWS != nil && closeFallbackWS.Load() {
					closeFallbackWS.Store(false)
					klog.V(2).Info("Status websocket: closing fallback websocket due to higher-priority direct transport recovery")

					break loop
				}
			}
		}

		criticalTicker.Stop()
		statsTicker.Stop()
		fullSyncTicker.Stop()
		fallbackCloseTicker.Stop()

		if keepaliveTicker != nil {
			keepaliveTicker.Stop()
		}

		if directRecoveryTimer != nil {
			directRecoveryTimer.Stop()
		}

		readCancel()

		if wsConnected != nil {
			wsConnected.Store(false)
		}

		if wsMode != nil {
			wsMode.Store(statusWSModeNone)
		}
		// Send a graceful WebSocket close frame. Use StatusNormalClosure
		// for clean shutdown and StatusGoingAway for reconnect scenarios.
		// conn.Close has its own 5s timeout for the close handshake.
		closeCode := websocket.StatusGoingAway
		closeReason := "reconnect"

		if ctx.Err() != nil {
			closeCode = websocket.StatusNormalClosure
			closeReason = "shutdown"
		}

		_ = conn.Close(closeCode, closeReason) //nolint:errcheck

		connCancel() // tear down the detached connection context after graceful close
		klog.V(4).Info("Status websocket disconnected")
	}
}

// tryDirectRecoveryProbe verifies direct connectivity while fallback websocket remains active.
// It returns true only after a successful direct websocket or direct push probe.
func tryDirectRecoveryProbe(
	ctx context.Context,
	healthState *nodeHealthState,
	dialHTTPClient *http.Client,
	getToken func() string,
	directWSURL string,
	nodeName string,
) bool {
	if directWSURL != "" {
		headers := http.Header{}
		if token := getToken(); token != "" {
			headers.Set("Authorization", "Bearer "+token)
		}

		probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
		conn, _, err := websocket.Dial(probeCtx, directWSURL, &websocket.DialOptions{
			HTTPHeader:      headers,
			HTTPClient:      dialHTTPClient,
			CompressionMode: websocket.CompressionContextTakeover,
		})

		probeCancel()

		if err == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "direct recovery probe successful") //nolint:errcheck

			klog.V(2).Infof("Status websocket: direct websocket recovery probe succeeded for %s", directWSURL)
			clearNodeErrorsByTypes(healthState, nodeErrorTypeDirectPush, nodeErrorTypeDirectWebSocket)

			return true
		}

		klog.V(4).Infof("Status websocket: direct websocket recovery probe failed for %s: %v", directWSURL, err)
		// Don't append node errors for failed recovery probes -- the fallback
		// WS is working and these probe failures are expected during startup
		// or when the direct path is temporarily unavailable.
		return false
	}

	// No direct websocket URL means there is no higher-priority direct path to recover to.
	return false
}

func nextExponentialBackoff(current, max time.Duration) time.Duration {
	if current <= 0 {
		current = time.Second
	}

	if max <= 0 {
		max = 30 * time.Second
	}

	next := current * 2
	if next > max {
		return max
	}

	return next
}

func websocketHostScopeKey(wsURLs []string) string {
	hosts := make([]string, 0, len(wsURLs))
	for _, endpointURL := range wsURLs {
		parsed, err := url.Parse(endpointURL)
		if err != nil || parsed.Host == "" {
			hosts = append(hosts, endpointURL)
			continue
		}

		hosts = append(hosts, parsed.Host)
	}

	return strings.Join(hosts, ",")
}

// startStatusPusher periodically pushes this node's status to the controller.
// It acts as fallback when websocket transport is unavailable.
func startStatusPusher(
	ctx context.Context,
	cfg *config,
	healthState *nodeHealthState,
	wsConnected *atomic.Bool,
	wsMode *atomic.Int32,
	fallbackWSEnabled *atomic.Bool,
	apiPushEnabled *atomic.Bool,
	closeFallbackWS *atomic.Bool,
) {
	if !cfg.StatusPushEnabled {
		klog.Info("Status push is disabled")
		return
	}

	directPushURL := resolveDirectStatusPushURL(cfg)

	apiserverPushURL := resolveStatusPushAPIServerURL(cfg)

	wsAPIServerMode, modeErr := parseStatusWSAPIServerMode(cfg.StatusWSAPIServerMode)
	if modeErr != nil {
		klog.Warningf("Status push: %v; defaulting mode to fallback", modeErr)

		wsAPIServerMode = statusWSAPIServerModeFallback
	}

	if directPushURL == "" && wsAPIServerMode == statusWSAPIServerModeNever {
		klog.Warning("Status push: no direct controller push URL and API server mode is never; disabling status push")
		return
	}

	klog.Infof("Status push enabled: directURL=%q, apiserverURL=%q, interval=%s, apiserverInterval=%s, mode=%s, delta=%t", directPushURL, apiserverPushURL, cfg.StatusPushInterval, cfg.StatusPushAPIServerInterval, wsAPIServerMode, cfg.StatusPushDelta)

	if directPushURL == "" {
		if fallbackWSEnabled != nil {
			fallbackWSEnabled.Store(true)
		}

		if apiPushEnabled != nil {
			apiPushEnabled.Store(true)
		}
	}

	client := newStatusPushHTTPClient(5 * time.Second)

	// SA token reader for aggregated API server fallback pushes.
	var (
		cachedSAToken string
		saTokenReadAt time.Time
	)

	const saTokenRefreshInterval = 5 * time.Minute

	getSAToken := func() string {
		if cachedSAToken != "" && time.Since(saTokenReadAt) < saTokenRefreshInterval {
			return cachedSAToken
		}

		data, err := os.ReadFile(serviceAccountTokenPath)
		if err != nil {
			klog.V(4).Infof("Status push: failed to read service account token: %v", err)
			return cachedSAToken // return stale token if available
		}

		cachedSAToken = strings.TrimSpace(string(data))
		saTokenReadAt = time.Now()

		return cachedSAToken
	}

	// HMAC token manager for direct controller pushes.
	hmacMgr := newHMACTokenManager(cfg.NodeName)
	getHMACToken := func() string {
		token, err := hmacMgr.getToken()
		if err != nil {
			klog.V(2).Infof("Status push: failed to get HMAC token: %v", err)
			return ""
		}

		return token
	}

	var lastAckRevision uint64

	forceFullPush := true

	var (
		lastSentStatus            *NodeStatusResponse
		pushStateMu               sync.Mutex
		lastAPIServerPushUnix     atomic.Int64
		lastDirectPushSuccessUnix atomic.Int64
	)

	startedAt := time.Now()
	lastDirectPushSuccessUnix.Store(startedAt.UnixNano())

	var directPushDownSince time.Time

	// pushInFlight prevents overlapping pushes.
	var pushInFlight atomic.Bool

	ticker := time.NewTicker(cfg.StatusPushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			currentWSMode := statusWSModeNone
			if wsMode != nil {
				currentWSMode = wsMode.Load()
			}

			if currentWSMode == statusWSModeDirect {
				if fallbackWSEnabled != nil {
					fallbackWSEnabled.Store(false)
				}

				if apiPushEnabled != nil {
					apiPushEnabled.Store(false)
				}

				if closeFallbackWS != nil {
					closeFallbackWS.Store(false)
				}

				pushStateMu.Lock()
				directPushDownSince = time.Time{}
				pushStateMu.Unlock()
				// Clear any push errors from before the WS connected
				clearNodeErrorsByTypes(healthState, nodeErrorTypeDirectPush, nodeErrorTypeFallbackPush)

				continue
			}

			if pushInFlight.Load() {
				klog.V(3).Info("Status push: previous push still in flight, skipping this tick")
				continue
			}

			srv := healthState.getStatusServer()
			if srv == nil {
				klog.V(5).Info("Status push: status server not ready yet, skipping")
				continue
			}

			// Collect status and prepare the request body synchronously.
			collectStart := time.Now()
			nodeStatus := srv.getNodeStatus()
			collectDuration := time.Since(collectStart)

			pushStateMu.Lock()
			currentForceFull := forceFullPush
			currentRevision := lastAckRevision
			previousStatus := lastSentStatus
			pushStateMu.Unlock()

			mode := "full"

			protoMsg := &statusproto.NodeStatusMessage{
				Type:     "node_status_full",
				NodeName: nodeStatus.NodeInfo.Name,
				Status:   nodeStatusToProto(nodeStatus),
			}
			if cfg.StatusPushDelta && !currentForceFull {
				delta, deltaErr := computeStatusDelta(previousStatus, nodeStatus)
				if deltaErr != nil {
					klog.V(3).Infof("Status push: failed to compute delta: %v", deltaErr)
				} else if len(delta) > 0 {
					mode = "delta"
					protoMsg.Type = "node_status_delta"
					protoMsg.BaseRevision = currentRevision
					protoMsg.Status = nil
					protoMsg.Delta = nodeStatusDeltaToProto(delta)
				}
			}

			marshalStart := time.Now()

			data, err := proto.Marshal(protoMsg)
			if err != nil {
				klog.V(3).Infof("Status push: failed to marshal protobuf status: %v", err)
				continue
			}

			// Gzip-compress the protobuf body to reduce bandwidth
			var compressed bytes.Buffer

			gz, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
			if err != nil {
				klog.V(3).Infof("Status push: failed to init gzip writer: %v", err)
				continue
			}

			if _, err := gz.Write(data); err != nil {
				_ = gz.Close() //nolint:errcheck

				klog.V(3).Infof("Status push: failed to gzip status: %v", err)

				continue
			}

			if err := gz.Close(); err != nil {
				klog.V(3).Infof("Status push: failed to finalize gzip: %v", err)
				continue
			}

			prepareDuration := time.Since(marshalStart)

			if collectDuration > 2*time.Second {
				klog.Warningf("Status push: getNodeStatus() took %v (marshal+gzip: %v, body: %d bytes, mode=%s)", collectDuration, prepareDuration, compressed.Len(), mode)
			} else {
				klog.V(4).Infof("Status push: collected in %v, prepared in %v (%d bytes, mode=%s)", collectDuration, prepareDuration, compressed.Len(), mode)
			}

			// Copy the compressed data so the goroutine owns it
			body := compressed.Bytes()

			// Send HTTP POST in background so slow network doesn't block the ticker.
			// The ticker loop stays responsive and can fire the next push on time.
			pushInFlight.Store(true)

			go func(mode string, statusCopy *NodeStatusResponse) {
				defer pushInFlight.Store(false)

				postStart := time.Now()

				isAPIServerPushAllowed := func() bool {
					if wsAPIServerMode == statusWSAPIServerModeNever || apiserverPushURL == "" {
						return false
					}

					if apiPushEnabled != nil && !apiPushEnabled.Load() {
						return false
					}

					if wsMode != nil && wsMode.Load() == statusWSModeFallback {
						return false
					}

					pushStateMu.Lock()
					currentDirectPushDownSince := directPushDownSince
					pushStateMu.Unlock()

					if !isStatusAPIServerFallbackAllowed(startedAt, currentDirectPushDownSince, time.Now(), cfg.StatusWSAPIServerStartupDelay, directPushURL != "") {
						return false
					}

					interval := cfg.StatusPushAPIServerInterval
					if interval <= 0 {
						interval = 30 * time.Second
					}

					last := lastAPIServerPushUnix.Load()
					if last == 0 {
						return true
					}

					return time.Since(time.Unix(0, last)) >= interval
				}

				postTo := func(targetURL, targetLabel string) (bool, bool) {
					pushStart := time.Now()

					req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
					if err != nil {
						klog.V(2).Infof("Status push: failed to create %s request: %v", targetLabel, err)

						if targetLabel == "direct" {
							pushStateMu.Lock()
							if directPushDownSince.IsZero() {
								directPushDownSince = time.Now()
							}
							pushStateMu.Unlock()

							errorType := nodeErrorTypeDirectPush
							if targetLabel == "apiserver" {
								errorType = nodeErrorTypeFallbackPush
							}

							appendNodeError(healthState, errorType, fmt.Sprintf("request build failed: %v", err))
						}

						return false, false
					}

					req.Header.Set("Content-Type", "application/x-protobuf")
					req.Header.Set("Content-Encoding", "gzip")
					// Direct paths use HMAC token; aggregated API paths use SA token.
					if targetLabel == "direct" {
						if token := getHMACToken(); token != "" {
							req.Header.Set("Authorization", "Bearer "+token)
						}
					} else {
						if token := getSAToken(); token != "" {
							req.Header.Set("Authorization", "Bearer "+token)
						}
					}

					resp, err := client.Do(req)
					if err != nil {
						klog.V(2).Infof("Status push: %s request failed after %v: %v", targetLabel, time.Since(postStart).Round(time.Millisecond), err)

						if targetLabel == "direct" {
							pushStateMu.Lock()
							if directPushDownSince.IsZero() {
								directPushDownSince = time.Now()
							}
							pushStateMu.Unlock()

							fallbackSince := time.Unix(0, lastDirectPushSuccessUnix.Load())
							if cfg.StatusWSAPIServerStartupDelay <= 0 || time.Since(fallbackSince) >= cfg.StatusWSAPIServerStartupDelay {
								if fallbackWSEnabled != nil {
									fallbackWSEnabled.Store(true)
								}

								if wsMode != nil && wsMode.Load() != statusWSModeFallback && apiPushEnabled != nil {
									apiPushEnabled.Store(true)
								}
							}

							errorType := nodeErrorTypeDirectPush
							if targetLabel == "apiserver" {
								errorType = nodeErrorTypeFallbackPush
							}

							appendNodeError(healthState, errorType, fmt.Sprintf("request failed: %v", err))
						}

						return false, false
					}

					defer func() { _ = resp.Body.Close() }() //nolint:errcheck

					var ack statusproto.NodeStatusAck

					respBody, readErr := io.ReadAll(resp.Body)
					if readErr != nil {
						klog.V(4).Infof("Status push: failed to read %s response body: %v", targetLabel, readErr)
					} else if protoErr := proto.Unmarshal(respBody, &ack); protoErr != nil {
						// Fallback: try JSON for backward compatibility during rollout.
						var jsonAck nodeStatusPushAck
						if json.Unmarshal(respBody, &jsonAck) == nil {
							ack.Revision = jsonAck.Revision
							ack.Status = jsonAck.Status
							ack.Reason = jsonAck.Reason
						}
					}

					if resp.StatusCode == http.StatusTooManyRequests {
						pushStateMu.Lock()
						forceFullPush = true
						lastAckRevision = ack.Revision
						pushStateMu.Unlock()
						klog.V(3).Infof("Status push: controller requested full resync via %s (mode=%s, revision=%d)", targetLabel, mode, ack.Revision)

						return false, true
					}

					// Invalidate HMAC token on 401 so next attempt re-requests.
					if resp.StatusCode == http.StatusUnauthorized && targetLabel == "direct" {
						hmacMgr.invalidate()
						klog.V(2).Infof("Status push: 401 from direct endpoint, invalidated HMAC token")
					}

					if resp.StatusCode != http.StatusOK {
						klog.V(2).Infof("Status push: unexpected %s status %d in %v", targetLabel, resp.StatusCode, time.Since(postStart).Round(time.Millisecond))

						switch targetLabel {
						case "direct":
							pushStateMu.Lock()
							if directPushDownSince.IsZero() {
								directPushDownSince = time.Now()
							}
							pushStateMu.Unlock()

							fallbackSince := time.Unix(0, lastDirectPushSuccessUnix.Load())
							if cfg.StatusWSAPIServerStartupDelay <= 0 || time.Since(fallbackSince) >= cfg.StatusWSAPIServerStartupDelay {
								if fallbackWSEnabled != nil {
									fallbackWSEnabled.Store(true)
								}

								if wsMode != nil && wsMode.Load() != statusWSModeFallback && apiPushEnabled != nil {
									apiPushEnabled.Store(true)
								}
							}

							errorType := nodeErrorTypeDirectPush
							if targetLabel == "apiserver" {
								errorType = nodeErrorTypeFallbackPush
							}

							appendNodeError(healthState, errorType, fmt.Sprintf("unexpected HTTP status: %d", resp.StatusCode))
						}

						return false, false
					}

					pushStateMu.Lock()
					if ack.Revision > 0 {
						lastAckRevision = ack.Revision
					}

					lastSentStatus = statusCopy
					forceFullPush = false
					pushStateMu.Unlock()
					clearNodeErrorsByTypes(healthState, nodeErrorTypeDirectPush, nodeErrorTypeFallbackPush)

					switch targetLabel {
					case "apiserver":
						lastAPIServerPushUnix.Store(time.Now().UnixNano())
					case "direct":
						pushStateMu.Lock()
						directPushDownSince = time.Time{}
						pushStateMu.Unlock()
						lastDirectPushSuccessUnix.Store(time.Now().UnixNano())

						if fallbackWSEnabled != nil {
							fallbackWSEnabled.Store(false)
						}

						if apiPushEnabled != nil {
							apiPushEnabled.Store(false)
						}

						if closeFallbackWS != nil {
							closeFallbackWS.Store(true)
						}
					}

					klog.V(5).Infof("Status push: %s OK in %v", targetLabel, time.Since(postStart).Round(time.Millisecond))
					nodeStatusPushTotal.WithLabelValues("http", "success").Inc()
					nodeStatusPushDuration.WithLabelValues("http").Observe(time.Since(pushStart).Seconds())
					nodeStatusPushBytes.WithLabelValues("http", "gzip").Observe(float64(len(body)))

					return true, false
				}

				if wsAPIServerMode == statusWSAPIServerModePreferred {
					if isAPIServerPushAllowed() {
						if ok, _ := postTo(apiserverPushURL, "apiserver"); ok {
							return
						}
					}

					if directPushURL != "" {
						klog.V(2).Info("Status push: falling back to direct endpoint after API server push attempt")

						_, _ = postTo(directPushURL, "direct")
					}

					return
				}

				if directPushURL != "" {
					if ok, resync := postTo(directPushURL, "direct"); ok || resync {
						return
					}
				}

				if fallbackWSEnabled != nil && fallbackWSEnabled.Load() && wsMode != nil && wsMode.Load() != statusWSModeFallback && apiPushEnabled != nil {
					apiPushEnabled.Store(true)
				}

				if isAPIServerPushAllowed() {
					klog.V(2).Info("Status push: falling back to API server endpoint after direct push failure")

					_, _ = postTo(apiserverPushURL, "apiserver")
				}
			}(mode, nodeStatus)
		}
	}
}

// nodeStatusServer provides HTTP endpoints for node status information
// It is started after WireGuard state is available
type nodeStatusServer struct {
	state     *wireGuardState
	cfg       *config
	pubKey    string
	clientset kubernetes.Interface

	// Informers for route annotation context (best-effort, may be nil).
	siteInformer        cache.SharedIndexInformer
	sliceInformer       cache.SharedIndexInformer
	gatewayPoolInformer cache.SharedIndexInformer
	sitePeeringInformer cache.SharedIndexInformer

	routingTableRefreshMu sync.Mutex
	routingTableCacheMu   sync.RWMutex
	routingTableCache     RoutingTableInfo
	routingTableCachedAt  time.Time
	routingTableDirty     atomic.Bool
}

func (s *nodeStatusServer) handleStatusJSON(w http.ResponseWriter, _ *http.Request) {
	defer func() {
		if r := recover(); r != nil {
			klog.Errorf("PANIC in handleStatusJSON: %v", r)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	}()

	status := s.getNodeStatus()

	w.Header().Set("Content-Type", "application/json")

	data, err := json.Marshal(status)
	if err != nil {
		klog.Errorf("status json marshal failed: %v", err)
		http.Error(w, fmt.Sprintf("json marshal error: %v", err), http.StatusInternalServerError)

		return
	}

	if _, err := w.Write(data); err != nil {
		klog.V(4).Infof("status json write failed: %v", err)
	}
}

// startRouteChangeWatcher subscribes to kernel route notifications and marks
// the routing-table cache dirty so getNodeStatus can refresh on demand.
func (s *nodeStatusServer) startRouteChangeWatcher(ctx context.Context) {
	s.routingTableDirty.Store(true)

	updates := make(chan netlink.RouteUpdate, 64)

	done := make(chan struct{})
	if err := routeSubscribeWithOptions(updates, done, netlink.RouteSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.V(3).Infof("Route change watcher error: %v", err)
			s.routingTableDirty.Store(true)
		},
	}); err != nil {
		klog.Warningf("Failed to start route change watcher, falling back to periodic route polling: %v", err)
		return
	}

	go func() {
		defer close(done)

		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-updates:
				if !ok {
					return
				}

				s.routingTableDirty.Store(true)
			}
		}
	}()
}

// getNodeStatus collects all status information about this node
func (s *nodeStatusServer) getNodeStatus() *NodeStatusResponse {
	// Snapshot state under the lock - copy all fields we need, then release.
	// Expensive operations (WireGuard GetDevice, collectRoutingTable) happen outside the lock.
	lockStart := time.Now()

	s.state.mu.Lock()
	lockWait := time.Since(lockStart)

	status := &NodeStatusResponse{
		Timestamp: time.Now(),
		NodeInfo: NodeInfo{
			Name:      s.cfg.NodeName,
			SiteName:  s.state.siteName,
			IsGateway: s.state.isGatewayNode,
			PodCIDRs:  s.state.nodePodCIDRs,
			BuildInfo: nodeAgentBuildInfo(),
			WireGuard: &WireGuardStatusInfo{
				Interface: fmt.Sprintf("wg%d", s.cfg.WireGuardPort),
				PublicKey: s.pubKey,
			},
		},
		NodeErrors: filterExpiredNodeErrors(s.state.nodeErrors, time.Now(), time.Minute),
	}

	// Append link stats warnings as node errors if any interfaces have incrementing counters.
	if s.state.linkStatsMonitor != nil {
		for _, w := range s.state.linkStatsMonitor.GetWarnings() {
			status.NodeErrors = append(status.NodeErrors, NodeError{Type: "link-stats", Message: w})
		}
	}

	// Append kube-proxy health warning if the health check is failing.
	if s.state.kubeProxyMonitor != nil {
		if w := s.state.kubeProxyMonitor.GetWarning(); w != "" {
			status.NodeErrors = append(status.NodeErrors, NodeError{Type: "kube-proxy", Message: w})
		}
	}

	// Use cached node IPs (fetched once at startup, no API call)
	status.NodeInfo.InternalIPs = s.state.nodeInternalIPs
	status.NodeInfo.ExternalIPs = s.state.nodeExternalIPs

	// Snapshot WireGuard manager reference
	wgManager := s.state.wireguardManager

	// Build maps of public key -> peer metadata for lookup.
	peerNameMap := make(map[string]string, len(s.state.peers))
	peerSiteNameMap := make(map[string]string, len(s.state.peers))
	peerTunnelProtoMap := make(map[string]string, len(s.state.peers))
	peerOverlayIPsMap := make(map[string][]string, len(s.state.peers))
	peerHealthCheckEnabledMap := make(map[string]bool, len(s.state.meshPeerHealthCheckEnabled))
	peerSkipPodCIDRMap := make(map[string]bool, len(s.state.peers))
	gatewaySkipPodCIDRByName := make(map[string]bool, len(s.state.gatewayPeers))

	gatewayTunnelProtoByName := make(map[string]string, len(s.state.gatewayPeers))
	for _, p := range s.state.peers {
		peerNameMap[p.WireGuardPublicKey] = p.Name
		peerSiteNameMap[p.WireGuardPublicKey] = p.SiteName
		peerTunnelProtoMap[p.WireGuardPublicKey] = p.TunnelProtocol

		overlayIPs := getHealthIPsFromPodCIDRs(p.PodCIDRs)
		if len(overlayIPs) > 0 {
			peerOverlayIPsMap[p.WireGuardPublicKey] = overlayIPs
		}

		peerSkipPodCIDRMap[p.WireGuardPublicKey] = p.SkipPodCIDRRoutes
	}

	for key, value := range s.state.meshPeerHealthCheckEnabled {
		peerHealthCheckEnabledMap[key] = value
	}

	for _, p := range s.state.gatewayPeers {
		gatewaySkipPodCIDRByName[p.Name] = p.SkipPodCIDRRoutes
		gatewayTunnelProtoByName[p.Name] = p.TunnelProtocol
	}

	// Snapshot gateway state: copy maps so we can iterate outside the lock
	type gwSnapshot struct {
		ifaceName          string
		overlayIP          string
		overlayIPs         []string
		healthCheckEnabled bool
		skipPodCIDR        bool
		gatewayName        string
		siteName           string
		routes             []string
		routeDistances     map[string]int
		wgManager          *unboundednetnetlink.WireGuardManager
	}

	gwSnapshots := make([]gwSnapshot, 0, len(s.state.gatewayHealthEndpoints))
	for ifaceName, overlayIP := range s.state.gatewayHealthEndpoints {
		overlayIPs := getHealthIPsFromPodCIDRs(s.state.gatewayPodCIDRs[ifaceName])

		if overlayIP != "" {
			present := false

			for _, ip := range overlayIPs {
				if ip == overlayIP {
					present = true
					break
				}
			}

			if !present {
				overlayIPs = append([]string{overlayIP}, overlayIPs...)
			}
		}

		snap := gwSnapshot{
			ifaceName:          ifaceName,
			overlayIP:          overlayIP,
			overlayIPs:         overlayIPs,
			healthCheckEnabled: s.state.gatewayPeerHealthCheckEnabled[ifaceName],
			skipPodCIDR:        gatewaySkipPodCIDRByName[s.state.gatewayNames[ifaceName]],
			gatewayName:        s.state.gatewayNames[ifaceName],
			siteName:           s.state.gatewaySiteNames[ifaceName],
			routes:             s.state.gatewayRoutes[ifaceName],
			routeDistances:     copyStringIntMap(s.state.gatewayRouteDistances[ifaceName]),
			wgManager:          s.state.gatewayWireguardManagers[ifaceName],
		}
		gwSnapshots = append(gwSnapshots, snap)
	}

	s.state.mu.Unlock()

	snapshotDuration := time.Since(lockStart) // includes lock wait + snapshot

	// === Expensive operations outside the lock ===
	expensiveStart := time.Now()

	// Collect healthcheck status (if manager is available)
	healthCheckMgr := s.state.healthCheckManager
	if healthCheckMgr != nil {
		allStatuses := healthCheckMgr.GetAllPeerStatuses()
		status.HealthCheck = &HealthCheckStatus{
			Healthy:   true,
			PeerCount: len(allStatuses),
			CheckedAt: time.Now(),
		}
		unhealthyCount := 0

		for _, ps := range allStatuses {
			if ps.State != 1 { // StateUp
				unhealthyCount++
			}
		}

		if unhealthyCount > 0 {
			status.HealthCheck.Healthy = false
			status.HealthCheck.Summary = fmt.Sprintf("%d of %d peers unhealthy", unhealthyCount, len(allStatuses))
		} else {
			status.HealthCheck.Summary = "all peers healthy"
		}
	}

	// Get WireGuard device info if available (netlink syscall)
	if wgManager != nil {
		if device, err := wgManager.GetDevice(); err == nil {
			status.NodeInfo.WireGuard.ListenPort = device.ListenPort
			status.NodeInfo.WireGuard.PeerCount = len(device.Peers)

			// Map WireGuard peers to our peer status (mesh = intra-site peers)
			for _, wgPeer := range device.Peers {
				pubKey := wgPeer.PublicKey.String()
				hcEnabled := peerHealthCheckEnabledMap[pubKey]
				peer := WireGuardPeerStatus{
					PeerType:          "site",
					SkipPodCIDRRoutes: peerSkipPodCIDRMap[pubKey],
					Tunnel: PeerTunnelStatus{
						Interface:     fmt.Sprintf("wg%d", s.cfg.WireGuardPort),
						PublicKey:     pubKey,
						LastHandshake: wgPeer.LastHandshakeTime,
						RxBytes:       wgPeer.ReceiveBytes,
						TxBytes:       wgPeer.TransmitBytes,
					},
				}

				// Convert AllowedIPs to strings
				for _, ip := range wgPeer.AllowedIPs {
					peer.Tunnel.AllowedIPs = append(peer.Tunnel.AllowedIPs, ip.String())
				}

				// Set endpoint if available
				if wgPeer.Endpoint != nil {
					peer.Tunnel.Endpoint = wgPeer.Endpoint.String()
				}

				// Find the peer name and overlay IP from our peer info
				if name, ok := peerNameMap[pubKey]; ok {
					peer.Name = name
				}

				if siteName, ok := peerSiteNameMap[pubKey]; ok {
					peer.SiteName = siteName
				}

				if tunnelProto, ok := peerTunnelProtoMap[pubKey]; ok && tunnelProto != "" {
					peer.Tunnel.Protocol = tunnelProto
				} else {
					peer.Tunnel.Protocol = "WireGuard"
				}

				overlayIPs := peerOverlayIPsMap[pubKey]
				if len(overlayIPs) > 0 {
					peer.PodCIDRGateways = append([]string(nil), overlayIPs...)
				}

				if hcEnabled && healthCheckMgr != nil {
					// Try to find healthcheck status by peer name
					if name, ok := peerNameMap[pubKey]; ok {
						if ps, err := healthCheckMgr.GetPeerStatus(name); err == nil && ps != nil {
							peer.HealthCheck = &HealthCheckPeerStatus{
								Enabled: true,
								Status:  ps.State.String(),
								Uptime:  formatDuration(time.Since(ps.Since)),
								RTT:     formatRTTMillis(ps.LastRTT),
							}
						} else {
							peer.HealthCheck = &HealthCheckPeerStatus{Enabled: true, Status: "down"}
						}
					}
				}

				status.Peers = append(status.Peers, peer)
			}
		}
	}

	// Collect gateway peer status (WireGuard GetDevice calls outside lock)
	addedPeerNames := make(map[string]bool)

	for _, gw := range gwSnapshots {
		// Get WireGuard peer info for this gateway interface (netlink syscall)
		if gw.wgManager != nil {
			if device, err := gw.wgManager.GetDevice(); err == nil && len(device.Peers) > 0 {
				wgPeer := device.Peers[0] // Each gateway interface has one peer

				peer := WireGuardPeerStatus{
					Name:              gw.gatewayName,
					PeerType:          "gateway",
					SiteName:          gw.siteName,
					SkipPodCIDRRoutes: gw.skipPodCIDR,
					PodCIDRGateways:   []string{gw.overlayIP},
					RouteDistances:    copyStringIntMap(gw.routeDistances),
					Tunnel: PeerTunnelStatus{
						Protocol:      gatewayTunnelProtoByName[gw.gatewayName],
						Interface:     gw.ifaceName,
						PublicKey:     wgPeer.PublicKey.String(),
						LastHandshake: wgPeer.LastHandshakeTime,
						RxBytes:       wgPeer.ReceiveBytes,
						TxBytes:       wgPeer.TransmitBytes,
					},
				}
				if wgPeer.Endpoint != nil {
					peer.Tunnel.Endpoint = wgPeer.Endpoint.String()
				}

				for _, ip := range wgPeer.AllowedIPs {
					peer.Tunnel.AllowedIPs = append(peer.Tunnel.AllowedIPs, ip.String())
				}
				// Merge healthcheck status by matching gateway name
				if len(gw.overlayIPs) > 0 {
					peer.PodCIDRGateways = append([]string(nil), gw.overlayIPs...)
				}

				if gw.healthCheckEnabled && healthCheckMgr != nil {
					if ps, err := healthCheckMgr.GetPeerStatus(gw.gatewayName); err == nil && ps != nil {
						peer.HealthCheck = &HealthCheckPeerStatus{
							Enabled: true,
							Status:  ps.State.String(),
							Uptime:  formatDuration(time.Since(ps.Since)),
							RTT:     formatRTTMillis(ps.LastRTT),
						}
					} else {
						peer.HealthCheck = &HealthCheckPeerStatus{Enabled: true, Status: "down"}
					}
				}

				status.Peers = append(status.Peers, peer)
				addedPeerNames[gw.gatewayName] = true
			}
		}
	}

	// Add tunnel peers (GENEVE, IPIP, None) to the peer list. These are peers
	// that don't use WireGuard -- they don't appear in wgctrl device output
	// since they use separate tunnel interfaces (or no interface for None).
	// Healthcheck works identically: probes travel over the tunnel
	// and the healthcheck manager tracks peers by name.
	s.state.mu.Lock()
	for _, p := range s.state.peers {
		if (p.TunnelProtocol != "GENEVE" && p.TunnelProtocol != "IPIP" && p.TunnelProtocol != "None" && p.TunnelProtocol != "VXLAN") || len(p.InternalIPs) == 0 {
			continue
		}

		if addedPeerNames[p.Name] {
			continue
		}

		var ifName string
		if s.cfg.TunnelDataplane == "ebpf" {
			// eBPF dataplane uses shared interfaces, not per-peer.
			ifName = ebpfTunnelInterfaceName(p.TunnelProtocol)
		} else {
			ifName = tunnelIfaceNameForPeer(p.TunnelProtocol, net.ParseIP(p.InternalIPs[0]))
		}

		if ifName == "" {
			if name, _, err := unboundednetnetlink.DetectDefaultRouteInterfaceFromCache(s.state.netlinkCache); err == nil {
				ifName = name
			}
		}

		hcEnabled := s.state.meshPeerHealthCheckEnabled[p.WireGuardPublicKey]

		peer := WireGuardPeerStatus{
			Name:              p.Name,
			PeerType:          "site",
			SiteName:          p.SiteName,
			SkipPodCIDRRoutes: p.SkipPodCIDRRoutes,
			Tunnel: PeerTunnelStatus{
				Protocol:  p.TunnelProtocol,
				Interface: ifName,
				Endpoint:  p.InternalIPs[0],
			},
		}
		for _, cidr := range p.PodCIDRs {
			gwIP := getGatewayIPFromCIDR(cidr)
			if gwIP != nil {
				peer.PodCIDRGateways = append(peer.PodCIDRGateways, gwIP.String())
			}

			peer.Tunnel.AllowedIPs = append(peer.Tunnel.AllowedIPs, cidr)
		}

		if hcEnabled && healthCheckMgr != nil {
			if ps, err := healthCheckMgr.GetPeerStatus(p.Name); err == nil && ps != nil {
				peer.HealthCheck = &HealthCheckPeerStatus{
					Enabled: true,
					Status:  ps.State.String(),
					Uptime:  formatDuration(time.Since(ps.Since)),
					RTT:     formatRTTMillis(ps.LastRTT),
				}
			} else {
				peer.HealthCheck = &HealthCheckPeerStatus{Enabled: true, Status: "down"}
			}
		}

		status.Peers = append(status.Peers, peer)
	}

	for _, gp := range s.state.gatewayPeers {
		if (gp.TunnelProtocol != "GENEVE" && gp.TunnelProtocol != "IPIP" && gp.TunnelProtocol != "None" && gp.TunnelProtocol != "VXLAN") || len(gp.InternalIPs) == 0 {
			continue
		}

		if addedPeerNames[gp.Name] {
			continue
		}

		var ifName string
		if s.cfg.TunnelDataplane == "ebpf" {
			ifName = ebpfTunnelInterfaceName(gp.TunnelProtocol)
		} else {
			ifName = tunnelIfaceNameForPeer(gp.TunnelProtocol, net.ParseIP(gp.InternalIPs[0]))
		}

		if ifName == "" {
			if name, _, err := unboundednetnetlink.DetectDefaultRouteInterfaceFromCache(s.state.netlinkCache); err == nil {
				ifName = name
			}
		}

		hcEnabled := s.state.gatewayPeerHealthCheckEnabled[ifName]

		peer := WireGuardPeerStatus{
			Name:              gp.Name,
			PeerType:          "gateway",
			SiteName:          gp.SiteName,
			SkipPodCIDRRoutes: gp.SkipPodCIDRRoutes,
			RouteDistances:    copyStringIntMap(gp.RouteDistances),
			Tunnel: PeerTunnelStatus{
				Protocol:  gp.TunnelProtocol,
				Interface: ifName,
				Endpoint:  gp.InternalIPs[0],
			},
		}
		for _, cidr := range gp.PodCIDRs {
			gwIP := getGatewayIPFromCIDR(cidr)
			if gwIP != nil {
				peer.PodCIDRGateways = append(peer.PodCIDRGateways, gwIP.String())
			}
		}

		peer.Tunnel.AllowedIPs = append(peer.Tunnel.AllowedIPs, gp.RoutedCidrs...)
		if hcEnabled && healthCheckMgr != nil {
			if ps, err := healthCheckMgr.GetPeerStatus(gp.Name); err == nil && ps != nil {
				peer.HealthCheck = &HealthCheckPeerStatus{
					Enabled: true,
					Status:  ps.State.String(),
					Uptime:  formatDuration(time.Since(ps.Since)),
					RTT:     formatRTTMillis(ps.LastRTT),
				}
			} else {
				peer.HealthCheck = &HealthCheckPeerStatus{Enabled: true, Status: "down"}
			}
		}

		status.Peers = append(status.Peers, peer)
	}
	s.state.mu.Unlock()

	// Collect routing table from kernel via netlink
	status.RoutingTable = s.collectRoutingTableFromKernel()

	// Annotate routes with Expected/Present/PeerDestinations/Info
	if len(status.RoutingTable.Routes) > 0 {
		annotateNodeRoutes(status, s.siteInformer, s.sliceInformer, s.gatewayPoolInformer, s.sitePeeringInformer)
	}

	// Add managed route info from the unified route manager
	if s.state.routeManager != nil {
		installed := s.state.routeManager.GetInstalledRoutes()
		status.RoutingTable.ManagedRouteCount = len(installed)
	}

	// Collect BPF trie entries when using eBPF tunnel dataplane
	if s.cfg.TunnelDataplane == "ebpf" {
		status.BpfEntries = s.collectBpfEntries()
	}

	expensiveDuration := time.Since(expensiveStart)

	totalDuration := time.Since(lockStart)
	if totalDuration > 2*time.Second {
		klog.Warningf("getNodeStatus() slow: total=%v (lock_wait=%v, snapshot=%v, expensive=%v)",
			totalDuration, lockWait, snapshotDuration-lockWait, expensiveDuration)
	} else {
		klog.V(4).Infof("getNodeStatus() timing: total=%v (lock_wait=%v, snapshot=%v, expensive=%v)",
			totalDuration, lockWait, snapshotDuration-lockWait, expensiveDuration)
	}

	return status
}

// collectRoutingTableFromKernel reads routes from the kernel via netlink.
// Includes routes on tunnel interfaces (wg*, gn*, ip*, vxlan*) and routes
// on any interface whose destination matches a managed route from the route
// manager (for tunnelProtocol: None where routes go on the default interface).
// The table is refreshed on kernel route-change notifications and periodically
// (backstop) to avoid high-frequency polling from status reads.
func (s *nodeStatusServer) collectRoutingTableFromKernel() RoutingTableInfo {
	info := RoutingTableInfo{
		Routes: []RouteEntry{},
	}

	now := time.Now()

	s.routingTableCacheMu.RLock()
	cachedAt := s.routingTableCachedAt

	dirty := s.routingTableDirty.Load()
	if !cachedAt.IsZero() && !dirty && now.Sub(cachedAt) < routingTableRefreshBackstop {
		cached := s.routingTableCache
		s.routingTableCacheMu.RUnlock()

		return cached
	}

	s.routingTableCacheMu.RUnlock()

	s.routingTableRefreshMu.Lock()
	defer s.routingTableRefreshMu.Unlock()

	now = time.Now()

	s.routingTableCacheMu.RLock()
	cachedAt = s.routingTableCachedAt

	dirty = s.routingTableDirty.Load()
	if !cachedAt.IsZero() && !dirty && now.Sub(cachedAt) < routingTableRefreshBackstop {
		cached := s.routingTableCache
		s.routingTableCacheMu.RUnlock()

		return cached
	}

	s.routingTableCacheMu.RUnlock()

	// Build a set of managed route prefixes from the route manager so we can
	// include routes on non-tunnel interfaces (e.g. eth0 with tunnelProtocol: None).
	managedPrefixes := make(map[string]bool)

	if s.state.routeManager != nil {
		for _, ir := range s.state.routeManager.GetInstalledRoutes() {
			managedPrefixes[ir.Prefix] = true
		}
	}

	collect := func(family int, familyLabel string) []RouteEntry {
		// Collect routes from the main table and, if configured, our dedicated table.
		// RouteList(nil, family) only returns routes from the main table, so we
		// explicitly request routes from our dedicated table via RouteListFiltered.
		var allRoutes []netlink.Route

		// Use the netlink cache for main table routes if available.
		if s.state.netlinkCache != nil {
			mainRoutes, err := s.state.netlinkCache.RouteList(nil, family)
			if err != nil {
				klog.V(4).Infof("Failed to list cached routes (family %d): %v", family, err)
			} else {
				for i := range mainRoutes {
					if mainRoutes[i].Table == 0 {
						mainRoutes[i].Table = unix.RT_TABLE_MAIN
					}
				}

				allRoutes = append(allRoutes, mainRoutes...)
			}
		} else {
			mainRoutes, err := netlink.RouteList(nil, family)
			if err != nil {
				klog.V(4).Infof("Failed to list kernel routes (family %d): %v", family, err)
			} else {
				for i := range mainRoutes {
					if mainRoutes[i].Table == 0 {
						mainRoutes[i].Table = unix.RT_TABLE_MAIN
					}
				}

				allRoutes = append(allRoutes, mainRoutes...)
			}
		}

		dedicatedTableID := s.state.routeTableID
		if dedicatedTableID != 0 && dedicatedTableID != unix.RT_TABLE_MAIN {
			tableRoutes, tErr := netlink.RouteListFiltered(family,
				&netlink.Route{Table: dedicatedTableID}, netlink.RT_FILTER_TABLE)
			if tErr != nil {
				klog.V(4).Infof("Failed to list kernel routes from table %d (family %d): %v",
					dedicatedTableID, family, tErr)
			} else {
				allRoutes = append(allRoutes, tableRoutes...)
			}
		}

		routes := allRoutes

		type nhKey struct {
			gateway string
			device  string
		}

		type destEntry struct {
			destination string
			table       int
			nexthops    map[nhKey]NextHop
			nhOrder     []nhKey
		}

		destMap := make(map[string]*destEntry)
		destOrder := []string{}

		for _, r := range routes {
			if r.Dst == nil {
				continue
			}
			// Skip kernel-generated address routes (proto kernel) -- these are
			// auto-created when IPs are assigned to interfaces and are not
			// routes we programmed.
			if r.Protocol == unix.RTPROT_KERNEL {
				continue
			}

			// Handle multipath (ECMP) routes -- these have r.MultiPath populated
			// instead of r.LinkIndex/r.Gw.
			if len(r.MultiPath) > 0 {
				type wgHop struct {
					devName string
					gwStr   string
				}

				var wgHops []wgHop

				for _, mp := range r.MultiPath {
					var (
						link    netlink.Link
						linkErr error
					)

					if s.state.netlinkCache != nil {
						link, linkErr = s.state.netlinkCache.LinkByIndex(mp.LinkIndex)
					} else {
						link, linkErr = netlink.LinkByIndex(mp.LinkIndex)
					}

					if linkErr != nil || link == nil || link.Attrs() == nil {
						continue
					}

					devName := link.Attrs().Name
					if !isManagedTunnelInterface(devName) && !managedPrefixes[r.Dst.String()] {
						continue
					}

					gwStr := ""
					if mp.Gw != nil {
						gwStr = mp.Gw.String()
					}

					wgHops = append(wgHops, wgHop{devName, gwStr})
				}

				if len(wgHops) == 0 {
					continue
				}

				prefix := r.Dst.String()

				table := r.Table
				if table == 0 {
					table = unix.RT_TABLE_MAIN
				}

				mapKey := fmt.Sprintf("%d:%s", table, prefix)

				de, exists := destMap[mapKey]
				if !exists {
					de = &destEntry{destination: prefix, table: table, nexthops: make(map[nhKey]NextHop)}
					destMap[mapKey] = de
					destOrder = append(destOrder, mapKey)
				}

				for _, wh := range wgHops {
					nk := nhKey{gateway: wh.gwStr, device: wh.devName}
					if _, nhExists := de.nexthops[nk]; !nhExists {
						nh := NextHop{
							Gateway: wh.gwStr, Device: wh.devName, Distance: r.Priority,
							RouteTypes: []RouteType{{Type: "kernel", Attributes: []string{"fib"}}},
						}
						if r.MTU > 0 {
							nh.MTU = r.MTU
						}

						de.nexthops[nk] = nh
						de.nhOrder = append(de.nhOrder, nk)
					}
				}

				continue
			}

			// Single-path route -- include if on a tunnel interface or if it is a
			// managed route (tunnelProtocol: None puts routes on the default interface).
			var (
				link    netlink.Link
				linkErr error
			)

			if s.state.netlinkCache != nil {
				link, linkErr = s.state.netlinkCache.LinkByIndex(r.LinkIndex)
			} else {
				link, linkErr = netlink.LinkByIndex(r.LinkIndex)
			}

			if linkErr != nil || link == nil || link.Attrs() == nil {
				continue
			}

			devName := link.Attrs().Name
			if !isManagedTunnelInterface(devName) && !managedPrefixes[r.Dst.String()] {
				continue
			}

			prefix := r.Dst.String()

			table := r.Table
			if table == 0 {
				table = unix.RT_TABLE_MAIN
			}

			mapKey := fmt.Sprintf("%d:%s", table, prefix)

			de, exists := destMap[mapKey]
			if !exists {
				de = &destEntry{destination: prefix, table: table, nexthops: make(map[nhKey]NextHop)}
				destMap[mapKey] = de
				destOrder = append(destOrder, mapKey)
			}

			gwStr := ""
			if r.Gw != nil {
				gwStr = r.Gw.String()
			}

			nk := nhKey{gateway: gwStr, device: devName}
			if _, nhExists := de.nexthops[nk]; !nhExists {
				nh := NextHop{
					Gateway:  gwStr,
					Device:   devName,
					Distance: r.Priority,
					RouteTypes: []RouteType{{
						Type:       "kernel",
						Attributes: []string{"fib"},
					}},
				}
				if r.MTU > 0 {
					nh.MTU = r.MTU
				}

				de.nexthops[nk] = nh
				de.nhOrder = append(de.nhOrder, nk)
			}
		}

		result := make([]RouteEntry, 0, len(destOrder))
		for _, mapKey := range destOrder {
			de := destMap[mapKey]

			nhs := make([]NextHop, 0, len(de.nhOrder))
			for _, nk := range de.nhOrder {
				nhs = append(nhs, de.nexthops[nk])
			}

			result = append(result, RouteEntry{
				Destination: de.destination,
				Family:      familyLabel,
				Table:       de.table,
				NextHops:    nhs,
			})
		}

		return result
	}

	v4Routes := collect(netlink.FAMILY_V4, "IPv4")
	v6Routes := collect(netlink.FAMILY_V6, "IPv6")

	if v4Routes == nil {
		v4Routes = []RouteEntry{}
	}

	if v6Routes == nil {
		v6Routes = []RouteEntry{}
	}

	info.Routes = append(v4Routes, v6Routes...)

	s.routingTableCacheMu.Lock()
	s.routingTableCache = info
	s.routingTableCachedAt = time.Now()
	s.routingTableCacheMu.Unlock()
	s.routingTableDirty.Store(false)

	return info
}

// isManagedTunnelInterface returns true for WireGuard (wg*), GENEVE (gn*),
// IPIP (ip*), VXLAN (vxlan*), and unbounded0 (eBPF dataplane) interfaces.
func isManagedTunnelInterface(name string) bool {
	return strings.HasPrefix(name, "wg") || strings.HasPrefix(name, "gn") ||
		strings.HasPrefix(name, "ip") || strings.HasPrefix(name, "vxlan") ||
		name == "unbounded0" || name == "geneve0"
}

// ebpfTunnelInterfaceName returns the shared tunnel interface name for a
// given protocol when using the eBPF dataplane. Unlike the netlink dataplane
// which creates per-peer interfaces, eBPF uses a single shared interface.
func ebpfTunnelInterfaceName(protocol string) string {
	switch unboundednetv1alpha1.TunnelProtocol(protocol) {
	case unboundednetv1alpha1.TunnelProtocolGENEVE:
		return "geneve0"
	case unboundednetv1alpha1.TunnelProtocolVXLAN:
		return "vxlan0"
	case unboundednetv1alpha1.TunnelProtocolIPIP:
		return "ipip0"
	case unboundednetv1alpha1.TunnelProtocolNone:
		return "" // resolved to default route interface by caller
	default:
		return "geneve0"
	}
}

// formatDuration returns a human-readable duration string.
func formatDuration(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}

	if d >= time.Minute {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}

	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// formatRTTMillis formats a duration as milliseconds with appropriate precision.
func formatRTTMillis(d time.Duration) string {
	ms := float64(d) / float64(time.Millisecond)
	if ms < 0.01 {
		return "0ms"
	}

	if ms < 1 {
		return fmt.Sprintf("%.2fms", ms)
	}

	if ms < 10 {
		return fmt.Sprintf("%.1fms", ms)
	}

	return fmt.Sprintf("%.0fms", ms)
}
