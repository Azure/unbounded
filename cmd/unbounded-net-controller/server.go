// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/authn"
	"github.com/Azure/unbounded/internal/net/certmanager"
	"github.com/Azure/unbounded/internal/net/html"
	"github.com/Azure/unbounded/internal/net/metrics"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	webhookpkg "github.com/Azure/unbounded/internal/net/webhook"
)

const (
	aggregatedNodeStatusWebSocketPath = "/apis/status.net.unbounded-cloud.io/v1alpha1/status/nodews"
	aggregatedNodeStatusPushPath      = "/apis/status.net.unbounded-cloud.io/v1alpha1/status/push"
	aggregatedStatusJSONPath          = "/apis/status.net.unbounded-cloud.io/v1alpha1/status/json"
	nodeIdentityTokenHeader           = "X-Unbounded-Node-Token"
)

// maxConcurrentNodeWS limits the number of simultaneous node WebSocket connections.
const maxConcurrentNodeWS = 50000

// wsFrame carries a WebSocket message along with its frame type so the
// handler can distinguish binary (protobuf) from text (JSON) frames.
type wsFrame struct {
	msgType websocket.MessageType
	data    []byte
}

type nodeStatusWSIdentity struct {
	NodeName string `json:"nodeName"`
	NodeInfo *struct {
		Name string `json:"name"`
	} `json:"nodeInfo"`
	Status *struct {
		NodeInfo *struct {
			Name string `json:"name"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

func extractNodeNameFromWSMessage(data []byte) string {
	var identity nodeStatusWSIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return ""
	}

	if identity.Status != nil && identity.Status.NodeInfo != nil {
		if identity.Status.NodeInfo.Name != "" {
			return identity.Status.NodeInfo.Name
		}
	}

	if identity.NodeInfo != nil && identity.NodeInfo.Name != "" {
		return identity.NodeInfo.Name
	}

	if identity.NodeName != "" {
		return identity.NodeName
	}

	return ""
}

// authorizeDirectStatusRequest validates a node-scoped HMAC token.
func authorizeDirectStatusRequest(tokenIssuer *authn.TokenIssuer, r *http.Request) (*authn.Claims, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		return nil, fmt.Errorf("bearer token is required")
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")

	claims, err := tokenIssuer.Validate(token)
	if err != nil {
		klog.V(3).Infof("HMAC token validation failed: %v", err)
		return nil, err
	}

	if claims.Role != authn.RoleNode {
		return nil, fmt.Errorf("token role %q is not authorized for node status", claims.Role)
	}

	if claims.NodeName == "" {
		return nil, fmt.Errorf("node token has no node claim")
	}

	return claims, nil
}

func startServer(ctx context.Context, healthPort int, requireDashboardAuth bool, health *healthState, webhookServer *webhookpkg.Server, certMgr *certmanager.CertManager, tokenIssuer *authn.TokenIssuer, tokenCfg tokenEndpointConfig) {
	mux := webhookServer.Mux()

	// Register webhook handlers (validate, mutate-nodes, aggregated API discovery).
	webhookServer.RegisterHandlers(ctx)

	// Create dashboard authorizer for SAR-based access control.
	dashAuthorizer := newDashboardAuthorizer(health.clientset)

	// Create cluster status cache and start its rebuild loop.
	clusterStatusCache := NewClusterStatusCache(health)

	health.clusterStatusCache = clusterStatusCache
	go clusterStatusCache.Run(context.Background())

	broadcaster := NewWSBroadcaster(health)
	go broadcaster.Run(context.Background())
	// Node status changes patch the pre-built cache in-place and notify the broadcaster.
	health.statusCache.SetOnChange(func(nodeName string, status *NodeStatusResponse) {
		statusCopy := *status

		// Set StatusSource from the cache entry's source (ws, push, etc.)
		if statusCopy.StatusSource == "" {
			if cached, ok := health.statusCache.Get(nodeName); ok && cached.Source != "" {
				statusCopy.StatusSource = cached.Source
			}
		}

		clusterStatusCache.PatchNode(nodeName, statusCopy)
		clusterStatusCache.MarkDirty()
		broadcaster.Notify()
	})

	// Node WebSocket connection semaphore.
	wsSemaphore := make(chan struct{}, maxConcurrentNodeWS)

	// Register handlers on the unified mux.
	metrics.Register(mux)
	registerProbeHandlers(mux, health)
	registerStatusHandlers(mux, health, requireDashboardAuth, webhookServer, dashAuthorizer, tokenIssuer)
	registerPushHandlers(mux, health, webhookServer, wsSemaphore, tokenIssuer)
	registerDashboardHandlers(mux, health, broadcaster, requireDashboardAuth, webhookServer, dashAuthorizer, tokenIssuer)
	registerTokenEndpoints(mux, health, webhookServer, tokenIssuer, tokenCfg)
	startStatusCacheCleanupLoop(health)

	if healthPort > 0 {
		serveUnifiedServer(ctx, healthPort, mux, certMgr, webhookServer)
	}
}

func registerProbeHandlers(mux *http.ServeMux, health *healthState) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		if health.isHealthy(ctx) {
			w.WriteHeader(http.StatusOK)

			if _, err := w.Write([]byte("ok")); err != nil {
				klog.V(4).Infof("healthz write failed: %v", err)
			}

			return
		}

		w.WriteHeader(http.StatusServiceUnavailable)

		if _, err := w.Write([]byte("cannot connect to kubernetes api")); err != nil {
			klog.V(4).Infof("healthz write failed: %v", err)
		}
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		ready, reason := health.readinessStatus(ctx)
		if ready {
			w.WriteHeader(http.StatusOK)

			if _, err := w.Write([]byte("ok")); err != nil {
				klog.V(4).Infof("readyz write failed: %v", err)
			}

			return
		}

		w.WriteHeader(http.StatusServiceUnavailable)

		if _, err := w.Write([]byte(reason)); err != nil {
			klog.V(4).Infof("readyz write failed: %v", err)
		}
	})
}

// serveStatusJSON handles the status JSON response logic, shared between
// the direct /status/json and aggregated API paths.
func serveStatusJSON(health *healthState, w http.ResponseWriter, r *http.Request) {
	if !health.isLeader.Load() {
		http.Error(w, "not the leader", http.StatusServiceUnavailable)
		return
	}

	debugParam := r.URL.Query().Get("debug")
	if debugParam != "" {
		w.Header().Set("Content-Type", "application/json")

		result := make(map[string]interface{})

		if debugParam == "allocators" || debugParam == "all" {
			if health.siteController != nil {
				sc := health.siteController.DebugState()
				result["allocators"] = sc.Allocators
				result["allocatorsReady"] = sc.AllocatorsReady
				result["workqueueLength"] = sc.WorkqueueLength
			}
		}

		if debugParam == "informers" || debugParam == "all" {
			informers := make(map[string]interface{})

			if health.siteController != nil {
				sc := health.siteController.DebugState()
				informers["siteController"] = map[string]interface{}{
					"hasSynced":      sc.HasSynced,
					"siteCount":      sc.SiteCount,
					"informerCounts": sc.InformerCounts,
				}
			}

			if health.siteInformer != nil {
				informers["sites"] = len(health.siteInformer.GetStore().List())
			}

			if health.gatewayPoolInformer != nil {
				informers["gatewayPools"] = len(health.gatewayPoolInformer.GetStore().List())
			}

			if health.sitePeeringInformer != nil {
				informers["sitePeerings"] = len(health.sitePeeringInformer.GetStore().List())
			}

			if health.assignmentInformer != nil {
				informers["siteGatewayPoolAssignments"] = len(health.assignmentInformer.GetStore().List())
			}

			if health.poolPeeringInformer != nil {
				informers["gatewayPoolPeerings"] = len(health.poolPeeringInformer.GetStore().List())
			}

			if health.statusCache != nil {
				informers["nodeStatusCacheSize"] = health.statusCache.Len()
			}

			result["informers"] = informers
		}

		if err := json.NewEncoder(w).Encode(result); err != nil {
			klog.V(4).Infof("debug json encode failed: %v", err)
		}

		return
	}

	var status *ClusterStatusResponse
	if health.clusterStatusCache != nil {
		status = health.clusterStatusCache.Get()
	}

	if status == nil {
		http.Error(w, "status not yet available", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(status); err != nil {
		klog.V(4).Infof("status json encode failed: %v", err)
	}
}

func registerStatusHandlers(mux *http.ServeMux, health *healthState, requireDashboardAuth bool, webhookServer *webhookpkg.Server, dashAuthorizer *dashboardAuthorizer, tokenIssuer *authn.TokenIssuer) {
	mux.HandleFunc("/status/json", func(w http.ResponseWriter, r *http.Request) {
		if !authorizeDashboardOrAggregated(requireDashboardAuth, tokenIssuer, dashAuthorizer, webhookServer, r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		serveStatusJSON(health, w, r)
	})

	mux.HandleFunc("/status/node/", func(w http.ResponseWriter, r *http.Request) {
		if !authorizeDashboardOrAggregated(requireDashboardAuth, tokenIssuer, dashAuthorizer, webhookServer, r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		if !health.isLeader.Load() {
			http.Error(w, "not the leader", http.StatusServiceUnavailable)
			return
		}

		nodeName := strings.TrimPrefix(r.URL.Path, "/status/node/")
		if nodeName == "" {
			http.Error(w, "node name required", http.StatusBadRequest)
			return
		}

		forcePull := r.URL.Query().Get("live") == "true"
		if !forcePull {
			if cached, ok := health.statusCache.Get(nodeName); ok {
				age := time.Since(cached.ReceivedAt)
				if age < health.staleThreshold {
					result := cached.Status
					t := cached.ReceivedAt
					result.LastPushTime = &t

					w.Header().Set("Content-Type", "application/json")

					if err := json.NewEncoder(w).Encode(result); err != nil {
						klog.V(4).Infof("status json encode failed: %v", err)
					}

					return
				}

				if !health.pullEnabled.Load() {
					result := cached.Status
					t := cached.ReceivedAt
					result.LastPushTime = &t
					result.StatusSource = "stale-cache"
					result.FetchError = fmt.Sprintf("stale status (%s old), pull disabled", formatDurationAgo(age))

					w.Header().Set("Content-Type", "application/json")

					if err := json.NewEncoder(w).Encode(result); err != nil {
						klog.V(4).Infof("status json encode failed: %v", err)
					}

					return
				}
			} else if !health.pullEnabled.Load() {
				http.Error(w, "no cached status for node (pull disabled)", http.StatusNotFound)
				return
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		if health.nodeLister == nil {
			http.Error(w, "node informer not ready", http.StatusServiceUnavailable)
			return
		}

		node, err := health.nodeLister.Get(nodeName)
		if err != nil {
			http.Error(w, fmt.Sprintf("node not found: %v", err), http.StatusNotFound)
			return
		}

		var nodeIP string

		for _, addr := range node.Status.Addresses {
			if addr.Type == "InternalIP" {
				nodeIP = addr.Address
				break
			}
		}

		if nodeIP == "" {
			http.Error(w, "no InternalIP found for node", http.StatusInternalServerError)
			return
		}

		nodeStatus, err := fetchNodeStatus(ctx, nodeIP, health.nodeAgentHealthPort)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to fetch node status: %v", err), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(nodeStatus); err != nil {
			klog.V(4).Infof("status json encode failed: %v", err)
		}
	})
}

func registerPushHandlers(mux *http.ServeMux, health *healthState, webhookServer *webhookpkg.Server, wsSemaphore chan struct{}, tokenIssuer *authn.TokenIssuer) {
	statusPushHandler := func(source string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}

			if !health.isLeader.Load() {
				http.Error(w, "not the leader", http.StatusServiceUnavailable)
				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

			var bodyReader io.Reader = r.Body
			if r.Header.Get("Content-Encoding") == "gzip" {
				gz, err := gzip.NewReader(r.Body)
				if err != nil {
					http.Error(w, fmt.Sprintf("failed to decompress gzip body: %v", err), http.StatusBadRequest)
					return
				}

				defer func() { _ = gz.Close() }() //nolint:errcheck

				bodyReader = gz
			}

			bodyBytes, err := io.ReadAll(bodyReader)
			if err != nil {
				http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
				return
			}

			if expectedNode := authorizedNodeName(r); expectedNode != "" {
				actualNode := extractNodeNameFromStatusBody(r, bodyBytes)
				if actualNode == "" {
					http.Error(w, "nodeName is required", http.StatusBadRequest)
					return
				}

				if actualNode != expectedNode {
					http.Error(w, "node token cannot update another node", http.StatusForbidden)
					return
				}
			}

			ack, statusCode, ackErr := handleStatusPushBody(health, r, bodyBytes, source)
			if ackErr != nil {
				http.Error(w, ackErr.Error(), statusCode)
				return
			}

			isProto := isProtobufContentType(r)
			if isProto {
				w.Header().Set("Content-Type", "application/x-protobuf")

				pbAck := &statusproto.NodeStatusAck{
					Status:   ack.Status,
					Revision: ack.Revision,
					Reason:   ack.Reason,
				}

				data, marshalErr := proto.Marshal(pbAck)
				if marshalErr != nil {
					klog.V(4).Infof("status push proto ack marshal failed: %v", marshalErr)
					http.Error(w, "internal error", http.StatusInternalServerError)

					return
				}

				if statusCode != http.StatusOK {
					w.WriteHeader(statusCode)
				}

				if _, writeErr := w.Write(data); writeErr != nil {
					klog.V(4).Infof("status push write failed: %v", writeErr)
				}
			} else {
				w.Header().Set("Content-Type", "application/json")

				if statusCode != http.StatusOK {
					w.WriteHeader(statusCode)
				}

				if err := json.NewEncoder(w).Encode(ack); err != nil {
					klog.V(4).Infof("status push write failed: %v", err)
				}
			}
		}
	}

	if health.nodeTokenVerifier != nil {
		// Direct push path -- HMAC token auth.
		mux.HandleFunc("/status/push", func(w http.ResponseWriter, r *http.Request) {
			claims, err := authorizeDirectStatusRequest(tokenIssuer, r)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			statusPushHandler("push").ServeHTTP(w, withAuthorizedNodeName(r, claims.NodeName))
		})
	}

	nodeWSHandler := func(source string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !health.isLeader.Load() {
				http.Error(w, "not the leader", http.StatusServiceUnavailable)
				return
			}

			// Acquire semaphore slot for concurrent WS limit.
			select {
			case wsSemaphore <- struct{}{}:
			default:
				http.Error(w, "too many concurrent WebSocket connections", http.StatusServiceUnavailable)
				return
			}

			defer func() { <-wsSemaphore }()

			lastWSNodeName := ""
			nodeNameForLog := func() string {
				if strings.TrimSpace(lastWSNodeName) == "" {
					return "unknown"
				}

				return lastWSNodeName
			}

			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
				OriginPatterns:  []string{"*"},
				CompressionMode: websocket.CompressionContextTakeover,
			})
			if err != nil {
				klog.V(2).Infof("Node WebSocket accept error (source=%s, node=%s): %v", source, nodeNameForLog(), err)
				return
			}

			conn.SetReadLimit(2 * 1024 * 1024) // 2 MiB -- status payloads grow with cluster size
			websocketConnections.Inc()

			defer func() {
				websocketConnections.Dec()

				if err := conn.CloseNow(); err != nil {
					klog.V(4).Infof("Node WebSocket close failed (source=%s, node=%s): %v", source, nodeNameForLog(), err)
				}
			}()

			send := func(frameType websocket.MessageType, ackMsgType string, ack NodeStatusPushAck) {
				if frameType == websocket.MessageBinary {
					payload, marshalErr := marshalProtoAck(ackMsgType, ack)
					if marshalErr != nil {
						klog.V(4).Infof("Node WebSocket proto ack marshal failed (source=%s, node=%s): %v", source, nodeNameForLog(), marshalErr)
						return
					}

					if writeErr := conn.Write(r.Context(), websocket.MessageBinary, payload); writeErr != nil {
						klog.V(4).Infof("Node WebSocket ack write failed (source=%s, node=%s): %v", source, nodeNameForLog(), writeErr)
					}

					return
				}

				payload, marshalErr := json.Marshal(map[string]interface{}{"type": ackMsgType, "data": ack})
				if marshalErr != nil {
					klog.V(4).Infof("Node WebSocket ack marshal failed (source=%s, node=%s): %v", source, nodeNameForLog(), marshalErr)
					return
				}

				if writeErr := conn.Write(r.Context(), websocket.MessageText, payload); writeErr != nil {
					klog.V(4).Infof("Node WebSocket ack write failed (source=%s, node=%s): %v", source, nodeNameForLog(), writeErr)
				}
			}

			wsCtx, wsCancel := context.WithCancel(r.Context())
			defer wsCancel()
			defer func() {
				health.unregisterNodeWS(lastWSNodeName, wsCancel)
			}()

			recvCh := make(chan wsFrame)
			errCh := make(chan error, 1)

			go func() {
				defer close(recvCh)

				for {
					msgType, data, readErr := conn.Read(wsCtx)
					if readErr != nil {
						errCh <- readErr
						return
					}

					select {
					case recvCh <- wsFrame{msgType: msgType, data: data}:
					case <-wsCtx.Done():
						errCh <- wsCtx.Err()
						return
					}
				}
			}()

			// Keepalive: run pings in a separate goroutine so the main
			// select loop continues draining recvCh. conn.Ping requires
			// a concurrent Reader call to read the pong frame; blocking
			// the select loop during Ping would deadlock if the reader
			// goroutine is blocked sending on recvCh.
			pingResultCh := make(chan error, 1)

			var (
				keepaliveTicker *time.Ticker
				keepaliveCh     <-chan time.Time
			)

			keepaliveFailures := 0
			pingTimeout := 5 * time.Second

			if health.statusWSKeepaliveInterval > 0 {
				// Stagger the first keepalive ping with random jitter to avoid
				// all connections pinging simultaneously on startup.
				jitter := time.Duration(rand.Int63n(int64(health.statusWSKeepaliveInterval)))
				initialDelay := health.statusWSKeepaliveInterval + jitter
				firstPingTimer := time.NewTimer(initialDelay)

				keepaliveCh = firstPingTimer.C
				defer firstPingTimer.Stop()

				if t := health.statusWSKeepaliveInterval / 4; t > pingTimeout {
					pingTimeout = t
				}
			}

			startRegularKeepalive := func() {
				if keepaliveTicker == nil && health.statusWSKeepaliveInterval > 0 {
					keepaliveTicker = time.NewTicker(health.statusWSKeepaliveInterval)
					keepaliveCh = keepaliveTicker.C
				}
			}

			defer func() {
				if keepaliveTicker != nil {
					keepaliveTicker.Stop()
				}
			}()

			pingInFlight := false
			lastActivity := time.Now()

			for {
				select {
				case <-wsCtx.Done():
					return
				case readErr := <-errCh:
					if lastWSNodeName != "" {
						health.statusCache.UpdateSource(lastWSNodeName, "stale-cache")
					}
					// Log graceful close frames and expected disconnections at
					// V(4) to reduce noise during rolling restarts.
					logLevel := klog.Level(2)

					closeStatus := websocket.CloseStatus(readErr)
					if closeStatus == websocket.StatusNormalClosure || closeStatus == websocket.StatusGoingAway {
						logLevel = 4
					} else if wsCtx.Err() != nil {
						// Our own context was canceled (controller shutting down)
						logLevel = 4
					} else if strings.Contains(readErr.Error(), "EOF") {
						// TCP connection dropped without close frame (peer pod restarted)
						logLevel = 4
					}

					klog.V(logLevel).Infof("Node WebSocket read closed (source=%s, node=%s): %v", source, nodeNameForLog(), readErr)

					return
				case frame, ok := <-recvCh:
					if !ok {
						if lastWSNodeName != "" {
							health.statusCache.UpdateSource(lastWSNodeName, "stale-cache")
						}

						return
					}

					lastActivity = time.Now()

					if frame.msgType == websocket.MessageBinary {
						nodeName := extractNodeNameFromProtoMessage(frame.data)
						if expectedNode := authorizedNodeName(r); expectedNode != "" && nodeName != "" && nodeName != expectedNode {
							send(websocket.MessageBinary, "node_status_resync", NodeStatusPushAck{
								Status: "resync_required",
								Reason: "node token cannot update another node",
							})

							return
						}

						if nodeName != "" {
							if lastWSNodeName == "" {
								// First message identifies the node -- register and evict old connections.
								health.registerNodeWS(nodeName, wsCancel)
							}

							lastWSNodeName = nodeName
						}

						ackType, ack := handleProtoWSMessage(health, frame.data, source)
						send(websocket.MessageBinary, ackType, ack)
					} else {
						nodeName := extractNodeNameFromWSMessage(frame.data)
						if expectedNode := authorizedNodeName(r); expectedNode != "" && nodeName != "" && nodeName != expectedNode {
							send(websocket.MessageText, "node_status_resync", NodeStatusPushAck{
								Status: "resync_required",
								Reason: "node token cannot update another node",
							})

							return
						}

						if nodeName != "" {
							if lastWSNodeName == "" {
								health.registerNodeWS(nodeName, wsCancel)
							}

							lastWSNodeName = nodeName
						}

						ackType, ack := handleNodeStatusWSMessageWithSource(health, frame.data, source)
						send(websocket.MessageText, ackType, ack)
					}
				case <-keepaliveCh:
					// Skip ping if we received a message recently
					if time.Since(lastActivity) < health.statusWSKeepaliveInterval {
						keepaliveFailures = 0
						continue
					}

					if pingInFlight {
						continue // previous ping still in progress, skip
					}

					pingInFlight = true

					go func() {
						pingCtx, pingCancel := context.WithTimeout(wsCtx, pingTimeout)
						pingErr := conn.Ping(pingCtx)

						pingCancel()

						select {
						case pingResultCh <- pingErr:
						case <-wsCtx.Done():
						}
					}()
				case pingErr := <-pingResultCh:
					pingInFlight = false

					startRegularKeepalive() // switch from initial jittered timer to regular ticker

					nodeNameLog := lastWSNodeName
					if nodeNameLog == "" {
						nodeNameLog = "unknown"
					}

					if pingErr != nil {
						keepaliveFailures++
						klog.V(2).Infof("Node WebSocket keepalive failed (source=%s, node=%s): %v (consecutive failures=%d/%d)", source, nodeNameLog, pingErr, keepaliveFailures, health.statusWSKeepaliveFailureCount)

						if keepaliveFailures >= health.statusWSKeepaliveFailureCount {
							klog.V(2).Infof("Node WebSocket keepalive closing connection after reaching failure threshold (source=%s, node=%s, failures=%d, threshold=%d)", source, nodeNameLog, keepaliveFailures, health.statusWSKeepaliveFailureCount)

							if lastWSNodeName != "" {
								health.statusCache.UpdateSource(lastWSNodeName, "stale-cache")
							}

							if closeErr := conn.Close(websocket.StatusGoingAway, "keepalive failure threshold reached"); closeErr != nil {
								klog.V(4).Infof("Node WebSocket close failed after keepalive threshold (source=%s, node=%s): %v", source, nodeNameLog, closeErr)
							}

							return
						}

						continue
					}

					if keepaliveFailures > 0 {
						klog.V(2).Infof("Node WebSocket keepalive recovered (source=%s, node=%s), resetting failure count", source, nodeNameLog)

						keepaliveFailures = 0
					}
				}
			}
		}
	}

	if health.nodeTokenVerifier != nil {
		// Direct WebSocket path -- HMAC token auth.
		mux.HandleFunc("/status/nodews", func(w http.ResponseWriter, r *http.Request) {
			claims, err := authorizeDirectStatusRequest(tokenIssuer, r)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			nodeWSHandler("ws").ServeHTTP(w, withAuthorizedNodeName(r, claims.NodeName))
		})
	}

	// Aggregated API paths -- front-proxy cert auth via webhook server.
	if health.registerAggregatedAPIServer {
		mux.HandleFunc(aggregatedNodeStatusPushPath, func(w http.ResponseWriter, r *http.Request) {
			authorizedRequest, err := authorizeAggregatedNodeRequest(r, health, webhookServer)
			if err != nil {
				klog.V(3).Infof("aggregated push rejected: %v", err)
				http.Error(w, "forbidden", http.StatusForbidden)

				return
			}

			statusPushHandler("apiserver-push").ServeHTTP(w, authorizedRequest)
		})

		mux.HandleFunc(aggregatedNodeStatusWebSocketPath, func(w http.ResponseWriter, r *http.Request) {
			authorizedRequest, err := authorizeAggregatedNodeRequest(r, health, webhookServer)
			if err != nil {
				klog.V(3).Infof("aggregated websocket rejected: %v", err)
				http.Error(w, "forbidden", http.StatusForbidden)

				return
			}

			nodeWSHandler("apiserver-ws").ServeHTTP(w, authorizedRequest)
		})

		mux.HandleFunc(aggregatedStatusJSONPath, func(w http.ResponseWriter, r *http.Request) {
			if !webhookServer.IsTrustedAggregatedRequest(r) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}

			serveStatusJSON(health, w, r)
		})
	}
}

func authorizeAggregatedNodeRequest(
	r *http.Request,
	health *healthState,
	webhookServer *webhookpkg.Server,
) (*http.Request, error) {
	if !webhookServer.IsTrustedAggregatedRequest(r) {
		return nil, fmt.Errorf("request is not from the trusted front proxy")
	}

	remoteUser := strings.TrimSpace(r.Header.Get("X-Remote-User"))

	saID, ok := serviceAccountIDFromUsername(remoteUser)
	if !ok || saID != health.nodeServiceAccount {
		return nil, fmt.Errorf("unexpected user %q", remoteUser)
	}

	if health.nodeTokenVerifier == nil {
		return nil, fmt.Errorf("node token verifier is not configured")
	}

	serviceAccountToken := strings.TrimSpace(r.Header.Get(nodeIdentityTokenHeader))
	if serviceAccountToken == "" {
		return nil, fmt.Errorf("missing node identity token")
	}

	identity, err := health.nodeTokenVerifier.Verify(r.Context(), serviceAccountToken)
	if err != nil {
		return nil, fmt.Errorf("validate node identity token: %w", err)
	}

	if identity.Subject != remoteUser {
		return nil, fmt.Errorf("node identity subject %q does not match authenticated user %q", identity.Subject, remoteUser)
	}

	if err := authorizeNodeServiceAccount(identity, health.nodeServiceAccount); err != nil {
		return nil, err
	}

	return withAuthorizedNodeName(r, identity.NodeName), nil
}

func registerDashboardHandlers(mux *http.ServeMux, health *healthState, broadcaster *WSBroadcaster, requireDashboardAuth bool, webhookServer *webhookpkg.Server, dashAuthorizer *dashboardAuthorizer, tokenIssuer *authn.TokenIssuer) {
	statusUIHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" && r.URL.Path != "/status/" {
			http.NotFound(w, r)
			return
		}

		if !authorizeDashboardOrAggregated(requireDashboardAuth, tokenIssuer, dashAuthorizer, webhookServer, r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		page, err := html.ClusterStatusIndex()
		if err != nil {
			http.Error(w, "status ui not available", http.StatusInternalServerError)
			return
		}

		if _, err := w.Write(page); err != nil {
			klog.V(4).Infof("status html write failed: %v", err)
		}
	}
	mux.HandleFunc("/status", statusUIHandler)
	mux.HandleFunc("/status/", statusUIHandler)

	assetsFS, err := html.ClusterStatusFS()
	if err != nil {
		klog.Errorf("Failed to load status ui assets: %v", err)
	} else {
		mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(assetsFS))))
	}

	mux.HandleFunc("/status/ws", func(w http.ResponseWriter, r *http.Request) {
		if !authorizeDashboardOrAggregated(requireDashboardAuth, tokenIssuer, dashAuthorizer, webhookServer, r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		if !health.isLeader.Load() {
			http.Error(w, "not the leader", http.StatusServiceUnavailable)
			return
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// OriginPatterns allows WebSocket connections from any origin.
			// Origin checking is not needed because authentication is handled
			// via HMAC bearer tokens validated before this point.
			OriginPatterns:  []string{"*"},
			CompressionMode: websocket.CompressionContextTakeover,
		})
		if err != nil {
			klog.V(3).Infof("WebSocket accept error: %v", err)
			return
		}

		conn.SetReadLimit(2 * 1024 * 1024)

		ctx, cancel := context.WithCancel(r.Context())
		client := &WSClient{
			conn:                    conn,
			send:                    make(chan []byte, 16),
			ctx:                     ctx,
			cancel:                  cancel,
			nodeDetailSubscriptions: make(map[string]bool),
		}
		broadcaster.Register(client)

		var summary *ClusterSummary

		if health.clusterStatusCache != nil {
			if status := health.clusterStatusCache.Get(); status != nil {
				status.Seq = broadcaster.getSeq()
				summary = buildClusterSummary(status)
			}
		}

		if summary == nil {
			// Cache not yet built (startup); send a minimal summary so the
			// client gets an immediate response instead of blocking.
			summary = &ClusterSummary{
				Timestamp: time.Now(),
				Errors:    []string{"controller starting up, data not yet available"},
			}
		}

		broadcaster.sendToClient(client, WSMessage{Type: "cluster_summary", Data: summary})

		go client.writePump()

		client.readPump(broadcaster)
	})
}

func handleStatusPushRequest(health *healthState, bodyBytes []byte) (NodeStatusPushAck, int, error) {
	start := time.Now()
	ack, code, err := handleStatusPushRequestWithSource(health, bodyBytes, "push")

	nodeStatusPushDuration.WithLabelValues("http").Observe(time.Since(start).Seconds())

	if err != nil {
		nodeStatusPushesTotal.WithLabelValues("http", "error").Inc()
	} else {
		nodeStatusPushesTotal.WithLabelValues("http", "success").Inc()
	}

	return ack, code, err
}

type authorizedNodeContextKey struct{}

func withAuthorizedNodeName(r *http.Request, nodeName string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authorizedNodeContextKey{}, nodeName))
}

func authorizedNodeName(r *http.Request) string {
	nodeName, ok := r.Context().Value(authorizedNodeContextKey{}).(string)
	if !ok {
		return ""
	}

	return nodeName
}

func extractNodeNameFromStatusBody(r *http.Request, body []byte) string {
	if isProtobufContentType(r) {
		return extractNodeNameFromProtoMessage(body)
	}

	return extractNodeNameFromWSMessage(body)
}

// isProtobufContentType returns true when the request Content-Type indicates
// a protobuf payload.
func isProtobufContentType(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "application/x-protobuf") || strings.HasPrefix(ct, "application/protobuf")
}

// handleStatusPushBody dispatches an HTTP push request to the protobuf or JSON
// handler based on the request Content-Type.
func handleStatusPushBody(health *healthState, r *http.Request, bodyBytes []byte, source string) (NodeStatusPushAck, int, error) {
	if isProtobufContentType(r) {
		return handleProtoPushRequest(health, bodyBytes, source)
	}

	return handleStatusPushRequestWithSource(health, bodyBytes, source)
}

func handleStatusPushRequestWithSource(health *healthState, bodyBytes []byte, source string) (NodeStatusPushAck, int, error) {
	var envelope NodeStatusPushEnvelope
	if err := json.Unmarshal(bodyBytes, &envelope); err != nil {
		return NodeStatusPushAck{}, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err)
	}

	ack := NodeStatusPushAck{Status: "ok"}

	if envelope.Mode == "" {
		var nodeStatus NodeStatusResponse
		if err := json.Unmarshal(bodyBytes, &nodeStatus); err != nil {
			return NodeStatusPushAck{}, http.StatusBadRequest, fmt.Errorf("invalid request body: %v", err)
		}

		if nodeStatus.NodeInfo.Name == "" {
			return NodeStatusPushAck{}, http.StatusBadRequest, fmt.Errorf("nodeInfo.name is required")
		}

		ack.Revision = health.statusCache.StoreFull(nodeStatus.NodeInfo.Name, nodeStatus, source)
		klog.V(5).Infof("Received full status push from node %s", nodeStatus.NodeInfo.Name)

		return ack, http.StatusOK, nil
	}

	nodeName := envelope.NodeName
	if envelope.Status != nil && envelope.Status.NodeInfo.Name != "" {
		nodeName = envelope.Status.NodeInfo.Name
	}

	if nodeName == "" {
		return NodeStatusPushAck{}, http.StatusBadRequest, fmt.Errorf("nodeName is required")
	}

	switch envelope.Mode {
	case "full":
		if envelope.Status == nil {
			return NodeStatusPushAck{}, http.StatusBadRequest, fmt.Errorf("status is required for full mode")
		}

		if envelope.Status.NodeInfo.Name == "" {
			envelope.Status.NodeInfo.Name = nodeName
		}

		ack.Revision = health.statusCache.StoreFull(nodeName, *envelope.Status, source)
		klog.V(5).Infof("Received full status push from node %s", nodeName)

		return ack, http.StatusOK, nil
	case "delta":
		if len(envelope.Delta) == 0 {
			return NodeStatusPushAck{}, http.StatusBadRequest, fmt.Errorf("delta is required for delta mode")
		}

		rev, conflict, applyErr := health.statusCache.ApplyDelta(nodeName, envelope.BaseRevision, envelope.Delta, source)
		if applyErr != nil {
			return NodeStatusPushAck{}, http.StatusBadRequest, fmt.Errorf("failed to apply delta: %v", applyErr)
		}

		if conflict {
			return NodeStatusPushAck{Status: "resync_required", Revision: rev, Reason: "base revision mismatch"}, http.StatusTooManyRequests, nil
		}

		ack.Revision = rev
		klog.V(5).Infof("Received delta status push from node %s (base=%d new=%d)", nodeName, envelope.BaseRevision, rev)

		return ack, http.StatusOK, nil
	default:
		return NodeStatusPushAck{}, http.StatusBadRequest, fmt.Errorf("mode must be full or delta")
	}
}

func handleNodeStatusWSMessage(health *healthState, data []byte) (string, NodeStatusPushAck) {
	return handleNodeStatusWSMessageWithSource(health, data, "ws")
}

func handleNodeStatusWSMessageWithSource(health *healthState, data []byte, source string) (string, NodeStatusPushAck) {
	var message NodeStatusWSMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Reason: "invalid message"}
	}

	nodeName := message.NodeName
	if message.Status != nil && message.Status.NodeInfo.Name != "" {
		nodeName = message.Status.NodeInfo.Name
	}

	if nodeName == "" {
		return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Reason: "nodeName is required"}
	}

	switch message.Type {
	case "node_status_full":
		if message.Status == nil {
			return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Reason: "full message missing status"}
		}

		if message.Status.NodeInfo.Name == "" {
			message.Status.NodeInfo.Name = nodeName
		}

		rev := health.statusCache.StoreFull(nodeName, *message.Status, source)

		return "node_status_ack", NodeStatusPushAck{Status: "ok", Revision: rev}
	case "node_status_delta":
		if len(message.Delta) == 0 {
			return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Reason: "delta message missing delta"}
		}

		rev, conflict, applyErr := health.statusCache.ApplyDelta(nodeName, message.BaseRevision, message.Delta, source)
		if applyErr != nil {
			return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Reason: applyErr.Error()}
		}

		if conflict {
			return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Revision: rev, Reason: "base revision mismatch"}
		}

		return "node_status_ack", NodeStatusPushAck{Status: "ok", Revision: rev}
	default:
		return "node_status_resync", NodeStatusPushAck{Status: "resync_required", Reason: "unsupported message type"}
	}
}

func startStatusCacheCleanupLoop(health *healthState) {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			if health.nodeLister == nil {
				continue
			}

			nodeList, err := health.nodeLister.List(labels.Everything())
			if err != nil {
				klog.V(4).Infof("Status cache cleanup: failed to list nodes: %v", err)
				continue
			}

			validNodes := make(map[string]bool, len(nodeList))
			for _, node := range nodeList {
				validNodes[node.Name] = true
			}

			health.statusCache.CleanupStaleEntries(validNodes)
		}
	}()
}

// newTLSErrorFilter returns a log.Logger that suppresses TLS handshake errors
// from localhost. Kubelet HTTPS probes connect from 127.0.0.1 and do not verify
// the server certificate, producing harmless "tls: unknown certificate" errors
// on every probe cycle. Non-localhost TLS errors are forwarded to klog.
func newTLSErrorFilter() *log.Logger {
	return log.New(tlsErrorFilterWriter{}, "", 0)
}

type tlsErrorFilterWriter struct{}

func (tlsErrorFilterWriter) Write(p []byte) (int, error) {
	msg := string(p)
	if strings.Contains(msg, "TLS handshake error") {
		if strings.Contains(msg, "127.0.0.1:") || strings.Contains(msg, "[::1]:") {
			return len(p), nil
		}
	}

	klog.Warning(strings.TrimSpace(msg))

	return len(p), nil
}

// serveUnifiedServer starts a TLS server that serves probes, webhooks, status,
// push, and dashboard endpoints on a single port. The certificate is obtained
// from the CertManager, and the front-proxy client CAs from the webhook server
// are set on the TLS config so that aggregated API requests can present client
// certificates.
func serveUnifiedServer(ctx context.Context, port int, mux *http.ServeMux, certMgr *certmanager.CertManager, webhookServer *webhookpkg.Server) {
	addr := fmt.Sprintf(":%d", port)
	klog.Infof("Starting unified HTTPS server on %s", addr)

	httpMiddleware := metrics.NewHTTPMiddleware("unbounded_cni_controller")

	tlsConfig := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: certMgr.GetCertificateFunc(),
		ClientAuth:     tls.VerifyClientCertIfGiven,
		ClientCAs:      webhookServer.GetClientCAs(),
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           gzipHandler(httpMiddleware.Wrap("all", mux)),
		ErrorLog:          newTLSErrorFilter(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         tlsConfig,
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			klog.Errorf("Server shutdown error: %v", err)
		}
	}()

	listener, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		klog.Fatalf("Failed to listen on %s: %v", addr, err)
	}

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			klog.Errorf("Server error: %v", err)
		}
	}()
}

// gzipHandler wraps an http.Handler to compress responses when the client
// advertises gzip support via Accept-Encoding. WebSocket upgrades and
// requests without gzip support are passed through unmodified.
func gzipHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			next.ServeHTTP(w, r)
			return
		}

		gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}

		defer func() { _ = gz.Close() }() //nolint:errcheck

		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, Writer: gz}, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	Writer io.Writer
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}
