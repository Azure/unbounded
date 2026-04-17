// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded-kube/internal/net/controller"
)

// healthState tracks the health of the controller for health and status endpoints.
type healthState struct {
	clientset           kubernetes.Interface
	nodeAgentHealthPort int
	healthPort          int
	azureTenantID       string
	leaderElectionNS    string
	leaderElectionName  string
	isLeader            atomic.Bool
	podIP               string // from POD_IP env var, for EndpointSlice management
	nodeName            string // from NODE_NAME env var (downward API)

	// Informer-based listers for efficient lookups (set after leader election).
	nodeLister          corev1listers.NodeLister
	podLister           corev1listers.PodLister
	siteInformer        cache.SharedIndexInformer
	gatewayPoolInformer cache.SharedIndexInformer
	sitePeeringInformer cache.SharedIndexInformer
	assignmentInformer  cache.SharedIndexInformer
	poolPeeringInformer cache.SharedIndexInformer

	// Site controller reference for debug introspection.
	siteController *controller.SiteController

	// Push-based status cache.
	statusCache        *NodeStatusCache
	clusterStatusCache *ClusterStatusCache
	staleThreshold     time.Duration // from --status-stale-threshold flag
	tokenAuth          *tokenAuthenticator
	nodeServiceAccount string // expected service account in namespace:name format

	// Pull fallback toggle (controlled via dashboard WS message; default: disabled).
	pullEnabled atomic.Bool
	// registerAggregatedAPIServer controls serving aggregated API status push endpoints.
	registerAggregatedAPIServer bool
	// statusWSKeepaliveInterval controls websocket ping cadence for node status streams.
	statusWSKeepaliveInterval time.Duration
	// statusWSKeepaliveFailureCount controls sequential keepalive failures before disconnecting.
	statusWSKeepaliveFailureCount int
	// nodeMTU is the configured node MTU from the shared configmap (node.mtu).
	// Used to validate that no node's detected WireGuard MTU is lower than this value.
	nodeMTU int
	// maxPullConcurrency limits the number of concurrent HTTP pulls when
	// pull mode is enabled. Defaults to defaultMaxPullConcurrency.
	maxPullConcurrency int
	// kubeProxyMonitor checks the local kube-proxy health endpoint.
	kubeProxyMonitor *kubeProxyMonitor

	// nodeWSRegistry tracks the active WS cancel function per node name.
	// When a node reconnects, the previous connection is cancelled to avoid
	// duplicate connections consuming resources.
	nodeWSMu       sync.Mutex
	nodeWSRegistry map[string]context.CancelFunc
}

const defaultMaxPullConcurrency = 20

// registerNodeWS registers a WS connection for a node. If an existing
// connection is registered for the same node, its context is cancelled
// to force it to close (preventing duplicate connections).
func (h *healthState) registerNodeWS(nodeName string, cancel context.CancelFunc) {
	if nodeName == "" {
		return
	}

	h.nodeWSMu.Lock()
	defer h.nodeWSMu.Unlock()

	if h.nodeWSRegistry == nil {
		h.nodeWSRegistry = make(map[string]context.CancelFunc)
	}

	if prev, ok := h.nodeWSRegistry[nodeName]; ok {
		prev() // cancel the old connection
	}

	h.nodeWSRegistry[nodeName] = cancel
}

// unregisterNodeWS removes a node's WS registration. Only removes if the
// cancel function matches (to avoid unregistering a newer connection).
func (h *healthState) unregisterNodeWS(nodeName string, cancel context.CancelFunc) {
	if nodeName == "" {
		return
	}

	h.nodeWSMu.Lock()
	defer h.nodeWSMu.Unlock()

	if h.nodeWSRegistry == nil {
		return
	}
	// Only remove if it's still our registration (not replaced by a newer connection)
	if existing, ok := h.nodeWSRegistry[nodeName]; ok {
		// Compare by pointer identity -- Go func values aren't comparable,
		// but context.CancelFunc from the same WithCancel call is the same pointer.
		if fmt.Sprintf("%p", existing) == fmt.Sprintf("%p", cancel) {
			delete(h.nodeWSRegistry, nodeName)
		}
	}
}

func (h *healthState) tokenAuthStatus() (bool, string) {
	if h.tokenAuth == nil {
		return false, "token authenticator not initialized"
	}

	if h.tokenAuth.tokenReviewer == nil {
		return false, "token reviewer not configured"
	}

	return true, "ok"
}

func (h *healthState) setInformers(nodeLister corev1listers.NodeLister, podLister corev1listers.PodLister, siteInformer, gatewayPoolInformer, sitePeeringInformer, assignmentInformer, poolPeeringInformer cache.SharedIndexInformer) {
	h.nodeLister = nodeLister
	h.podLister = podLister
	h.siteInformer = siteInformer
	h.gatewayPoolInformer = gatewayPoolInformer
	h.sitePeeringInformer = sitePeeringInformer
	h.assignmentInformer = assignmentInformer
	h.poolPeeringInformer = poolPeeringInformer
}

func (h *healthState) setLeader(leader bool) {
	wasLeader := h.isLeader.Swap(leader)
	if leader {
		leaderIsLeader.Set(1)
		klog.Info("Health: marked as leader")
	} else {
		leaderIsLeader.Set(0)
		klog.Info("Health: no longer leader")
	}

	if leader != wasLeader {
		leaderElectionTransitions.Inc()
	}
}

// isHealthy returns true if we can connect to the Kubernetes API server.
func (h *healthState) isHealthy(_ context.Context) bool {
	_, err := h.clientset.Discovery().ServerVersion()
	return err == nil
}

// isReady returns true if auth and Kubernetes API checks pass.
func (h *healthState) isReady(_ context.Context) bool {
	ready, _ := h.tokenAuthStatus()
	if !ready {
		return false
	}

	_, err := h.clientset.Discovery().ServerVersion()

	return err == nil
}

// getLeaderInfo returns information about the current leader pod.
func (h *healthState) getLeaderInfo(ctx context.Context) (*LeaderInfo, error) {
	lease, err := h.clientset.CoordinationV1().Leases(h.leaderElectionNS).Get(ctx, h.leaderElectionName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get leader lease: %w", err)
	}

	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return nil, fmt.Errorf("no leader currently elected")
	}

	leaderPodName := *lease.Spec.HolderIdentity

	var nodeName string
	if leaderPodName == os.Getenv("POD_NAME") {
		nodeName = h.nodeName
	}

	return &LeaderInfo{PodName: leaderPodName, NodeName: nodeName}, nil
}

// updateServiceEndpoints creates/updates the unbounded-net-controller EndpointSlice
// to point to the leader's IP on the HTTP health/status port.
func (h *healthState) updateServiceEndpoints(ctx context.Context) error {
	if err := h.clientset.CoreV1().Endpoints(h.leaderElectionNS).Delete(ctx, "unbounded-net-controller", metav1.DeleteOptions{}); err != nil {
		if !errors.IsNotFound(err) {
			klog.Warningf("Failed to clean up stale Endpoints resource: %v", err)
		}
	} else {
		klog.Info("Cleaned up stale v1/Endpoints resource for unbounded-net-controller")
	}

	port := int32(h.healthPort)
	protocol := corev1.ProtocolTCP
	portName := "https"
	addressType := discoveryv1.AddressTypeIPv4
	ready := true
	endpointSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unbounded-net-controller",
			Namespace: h.leaderElectionNS,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: "unbounded-net-controller",
			},
		},
		AddressType: addressType,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{h.podIP},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name:     &portName,
			Port:     &port,
			Protocol: &protocol,
		}},
	}

	_, err := h.clientset.DiscoveryV1().EndpointSlices(h.leaderElectionNS).Update(ctx, endpointSlice, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		_, err = h.clientset.DiscoveryV1().EndpointSlices(h.leaderElectionNS).Create(ctx, endpointSlice, metav1.CreateOptions{})
	}

	return err
}

// clearServiceEndpoints removes the EndpointSlice when losing leadership.
func (h *healthState) clearServiceEndpoints(ctx context.Context) {
	err := h.clientset.DiscoveryV1().EndpointSlices(h.leaderElectionNS).Delete(ctx, "unbounded-net-controller", metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		klog.Errorf("Failed to clear service endpoint slice: %v", err)
	}
}
