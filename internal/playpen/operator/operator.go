// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	apiregclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/playpen/runner"
)

const (
	idempotencyKeyHeader = "Idempotency-Key"

	apiGroup        = "playpen.unbounded-cloud.io"
	apiVersion      = "v1alpha1"
	apiGroupVersion = apiGroup + "/" + apiVersion
	apiServiceName  = apiVersion + "." + apiGroup

	aggregatedAPIGroupPath   = "/apis/" + apiGroup
	aggregatedAPIVersionPath = aggregatedAPIGroupPath + "/" + apiVersion
	allocsPath               = aggregatedAPIVersionPath + "/allocs"
	deallocsPath             = aggregatedAPIVersionPath + "/deallocs"

	extensionAuthNamespace       = "kube-system"
	extensionAuthConfigMapName   = "extension-apiserver-authentication"
	extensionAuthClientCAKey     = "requestheader-client-ca-file"
	extensionAuthAllowedNamesKey = "requestheader-allowed-names"
	remoteUserHeader             = "X-Remote-User"
	remoteGroupHeader            = "X-Remote-Group"

	runnerAppName        = "playpen-runner"
	runnerComponent      = "runner"
	runnerHostPortLabel  = "playpen.unbounded-cloud.io/host-port"
	runnerContainerName  = "runner"
	runnerWireGuardPort  = "wireguard"
	runnerHTTPSPort      = "https"
	runnerKubeFQDNAnnot  = "kubernetes.azure.com/set-kube-service-host-fqdn"
	runnerDataMountPath  = "/var/lib/playpen-runner"
	runnerWGMountPath    = "/etc/playpen/wireguard"
	runnerKVMPath        = "/dev/kvm"
	runnerTunPath        = "/dev/net/tun"
	runnerHTTPSContainer = int32(8443)

	controlPlaneAppName       = "playpen-control-plane"
	controlPlaneComponent     = "control-plane"
	controlPlaneContainerName = "k3s"
	controlPlaneHelperName    = "helper"
	controlPlaneAPIPort       = "apiserver"
	controlPlaneKubeconfig    = "/etc/rancher/k3s/k3s.yaml"
	controlPlaneDataMountPath = "/var/lib/rancher/k3s"
	controlPlaneKubeMountPath = "/etc/rancher/k3s"
)

type Operator struct {
	Client       client.Client
	KubeClient   kubernetes.Interface
	APIRegClient apiregclient.Interface
	Config       Config
	Scheme       *runtime.Scheme

	aggregatedMu               sync.RWMutex
	aggregatedClientCAs        *x509.CertPool
	aggregatedClientAllowedCNs map[string]struct{}
}

type AllocRequest struct {
	IdempotencyKey     string `json:"idempotencyKey,omitempty"`
	ResourceType       string `json:"resourceType,omitempty"`
	KubernetesVersion  string `json:"kubernetesVersion,omitempty"`
	WireGuardPublicKey string `json:"wireGuardPublicKey,omitempty"`
	Architecture       string `json:"architecture,omitempty"`
}

type DeallocRequest struct {
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

type AllocResponse struct {
	ResourceType string               `json:"resourceType"`
	Pod          PodResponse          `json:"pod"`
	Endpoint     EndpointResponse     `json:"endpoint"`
	WireGuard    WireGuardResponse    `json:"wireGuard"`
	VXLAN        VXLANResponse        `json:"vxlan"`
	Network      NetworkResponse      `json:"network"`
	Redfish      map[string]string    `json:"redfish"`
	ControlPlane ControlPlaneResponse `json:"controlPlane,omitempty"`
}

type PodResponse struct {
	Namespace         string `json:"namespace"`
	Name              string `json:"name"`
	NodeName          string `json:"nodeName"`
	NodePublicIP      string `json:"nodePublicIP"`
	ResourceType      string `json:"resourceType"`
	Architecture      string `json:"architecture"`
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
}

type EndpointResponse struct {
	Host                  string `json:"host"`
	WireGuardUDPPort      int32  `json:"wireGuardUDPPort"`
	APIServerTCPPort      int32  `json:"apiServerTCPPort,omitempty"`
	ExternalTrafficPolicy string `json:"externalTrafficPolicy"`
}

type ControlPlaneResponse struct {
	KubernetesVersion string `json:"kubernetesVersion"`
	Kubeconfig        string `json:"kubeconfig"`
	APIServerURL      string `json:"apiServerURL"`
	GuestAPIServerURL string `json:"guestAPIServerURL"`
}

type WireGuardResponse struct {
	Interface       string `json:"interface"`
	ServerPublicKey string `json:"serverPublicKey"`
	ServerAddress   string `json:"serverAddress"`
	ClientAddress   string `json:"clientAddress"`
	ListenPort      int    `json:"listenPort"`
}

type VXLANResponse struct {
	Interface     string `json:"interface"`
	VNI           int    `json:"vni"`
	UDPPort       int    `json:"udpPort"`
	ServerAddress string `json:"serverAddress"`
	ClientAddress string `json:"clientAddress"`
}

type NetworkResponse struct {
	GuestMAC    string   `json:"guestMAC"`
	GuestIPv4   string   `json:"guestIPv4"`
	SubnetMask  string   `json:"subnetMask"`
	GatewayIPv4 string   `json:"gatewayIPv4"`
	DNS         []string `json:"dns"`
}

func (o *Operator) Run(ctx context.Context) error {
	cert, err := o.EnsureTLSSecret(ctx)
	if err != nil {
		return err
	}

	certPEM, err := o.servingCertPEM(ctx)
	if err != nil {
		return err
	}

	if err := o.injectAPIServiceCABundle(ctx, certPEM); err != nil {
		return err
	}

	o.refreshAggregatedClientCAs(ctx)

	server := &http.Server{
		Addr:              o.Config.ListenAddr,
		Handler:           o.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequestClientCert,
			ClientCAs:    o.aggregatedClientCAs,
		},
	}

	errCh := make(chan error, 1)

	go func() {
		errCh <- server.ListenAndServeTLS("", "")
	}()

	ticker := time.NewTicker(o.Config.ReconcileInterval)
	defer ticker.Stop()

	o.ReconcileRunners(ctx) //nolint:errcheck // Periodic reconciliation below will retry transient startup failures.

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			server.Shutdown(shutdownCtx) //nolint:errcheck // Process is exiting after context cancellation.

			return nil
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}

			return err
		case <-ticker.C:
			o.refreshAggregatedClientCAs(ctx)
			o.ReconcileRunners(ctx) //nolint:errcheck // Next tick retries transient reconciliation failures.
		}
	}
}

func (o *Operator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(aggregatedAPIGroupPath, o.handleAPIGroupDiscovery)
	mux.HandleFunc(aggregatedAPIVersionPath, o.handleAPIVersionDiscovery)
	mux.HandleFunc(allocsPath, o.handleAllocs)
	mux.HandleFunc(deallocsPath, o.handleDeallocs)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	return mux
}

func (o *Operator) handleAPIGroupDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != aggregatedAPIGroupPath {
		http.NotFound(w, r)

		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	if !o.isTrustedAggregatedRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"kind":       "APIGroup",
		"apiVersion": "v1",
		"name":       apiGroup,
		"versions": []map[string]string{
			{"groupVersion": apiGroupVersion, "version": apiVersion},
		},
		"preferredVersion": map[string]string{"groupVersion": apiGroupVersion, "version": apiVersion},
	})
}

func (o *Operator) handleAPIVersionDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != aggregatedAPIVersionPath {
		http.NotFound(w, r)

		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	if !o.isTrustedAggregatedRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"kind":         "APIResourceList",
		"apiVersion":   "v1",
		"groupVersion": apiGroupVersion,
		"resources": []map[string]any{
			{"name": "allocs", "singularName": "alloc", "namespaced": false, "kind": "Alloc", "verbs": []string{"create"}},
			{"name": "deallocs", "singularName": "dealloc", "namespaced": false, "kind": "Dealloc", "verbs": []string{"create"}},
		},
	})
}

func (o *Operator) handleAllocs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	if !o.authorizeAggregatedRequest(w, r, "allocs") {
		return
	}

	var req AllocRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)

		return
	}

	idempotencyKey := requestIdempotencyKey(r, req.IdempotencyKey)
	if idempotencyKey == "" {
		http.Error(w, "Idempotency-Key header or idempotencyKey body field is required", http.StatusBadRequest)

		return
	}

	resourceType, err := normalizeResourceType(req.ResourceType)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	req.ResourceType = resourceType

	req.WireGuardPublicKey = strings.TrimSpace(req.WireGuardPublicKey)
	if resourceType == ResourceTypeRunner {
		if _, err := wgtypes.ParseKey(req.WireGuardPublicKey); err != nil {
			http.Error(w, "wireGuardPublicKey must be a valid WireGuard public key", http.StatusBadRequest)

			return
		}
	}

	arch, err := normalizeArchitecture(req.Architecture)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	req.Architecture = arch

	resp, status, err := o.Alloc(r.Context(), idempotencyKey, req)
	if err != nil {
		http.Error(w, err.Error(), status)

		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (o *Operator) handleDeallocs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	if !o.authorizeAggregatedRequest(w, r, "deallocs") {
		return
	}

	idempotencyKey := requestIdempotencyKey(r, "")
	if idempotencyKey == "" {
		var req DeallocRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid JSON request", http.StatusBadRequest)

			return
		}

		idempotencyKey = requestIdempotencyKey(r, req.IdempotencyKey)
	}

	if idempotencyKey == "" {
		http.Error(w, "Idempotency-Key header or idempotencyKey body field is required", http.StatusBadRequest)

		return
	}

	status, err := o.Dealloc(r.Context(), idempotencyKey)
	if err != nil {
		http.Error(w, err.Error(), status)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func requestIdempotencyKey(r *http.Request, bodyKey string) string {
	if headerKey := strings.TrimSpace(r.Header.Get(idempotencyKeyHeader)); headerKey != "" {
		return headerKey
	}

	return strings.TrimSpace(bodyKey)
}

func (o *Operator) Alloc(ctx context.Context, idempotencyKey string, req AllocRequest) (AllocResponse, int, error) {
	resourceType, err := normalizeResourceType(req.ResourceType)
	if err != nil {
		return AllocResponse{}, http.StatusBadRequest, err
	}

	req.ResourceType = resourceType

	arch, err := normalizeArchitecture(req.Architecture)
	if err != nil {
		return AllocResponse{}, http.StatusBadRequest, err
	}

	req.Architecture = arch

	kubernetesVersion := ""
	if resourceType == ResourceTypeControlPlane {
		kubernetesVersion, err = o.normalizeKubernetesVersion(req.KubernetesVersion)
		if err != nil {
			return AllocResponse{}, http.StatusBadRequest, err
		}

		req.KubernetesVersion = kubernetesVersion
	}

	if resourceType == ResourceTypeRunner && strings.TrimSpace(req.WireGuardPublicKey) == "" {
		return AllocResponse{}, http.StatusBadRequest, fmt.Errorf("wireGuardPublicKey is required for runner allocations")
	}

	keyHash := hashString(idempotencyKey)
	reqHash := hashString(strings.Join([]string{req.ResourceType, req.KubernetesVersion, req.WireGuardPublicKey, req.Architecture}, "\n"))

	managedPods, err := o.listManagedPods(ctx)
	if err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	for i := range managedPods {
		pod := &managedPods[i]
		if pod.Annotations[AnnotationIdempotencyKeyHash] != keyHash {
			continue
		}

		if pod.Annotations[AnnotationRequestHash] != reqHash {
			return AllocResponse{}, http.StatusConflict, fmt.Errorf("idempotency key was already used with a different request")
		}

		resp, err := o.responseForPod(ctx, pod)
		if err != nil {
			return AllocResponse{}, http.StatusServiceUnavailable, err
		}

		return resp, http.StatusOK, nil
	}

	if resourceType == ResourceTypeControlPlane {
		return o.allocControlPlane(ctx, keyHash, reqHash, req)
	}

	return o.allocRunner(ctx, keyHash, reqHash, req)
}

func (o *Operator) allocRunner(ctx context.Context, keyHash, reqHash string, req AllocRequest) (AllocResponse, int, error) {
	pods, err := o.listRunnerPods(ctx)
	if err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	var unavailable []string

	matchingArchitecture := false

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[AnnotationIdempotencyKeyHash] != "" {
			continue
		}

		if podArchitecture(pod) != req.Architecture {
			continue
		}

		matchingArchitecture = true

		if reason := runnerPodUnavailableReason(pod); reason != "" {
			unavailable = append(unavailable, reason)

			continue
		}

		serverPublicKey, err := serverWireGuardPublicKeyForPod(pod)
		if err != nil {
			unavailable = append(unavailable, err.Error())

			continue
		}

		endpointPort, externalTrafficPolicy, err := endpointForPod(pod)
		if err != nil {
			unavailable = append(unavailable, err.Error())

			continue
		}

		gatewayIP, nodePublicIP, err := o.gatewayForPod(ctx, pod)
		if err != nil {
			unavailable = append(unavailable, err.Error())

			continue
		}

		allocatedPod, claimed, err := o.patchClaim(ctx, pod, keyHash, reqHash, req.WireGuardPublicKey)
		if err != nil {
			if errors.Is(err, errRunnerAlreadyAllocated) {
				continue
			}

			if errors.Is(err, errIdempotencyRequestConflict) {
				return AllocResponse{}, http.StatusConflict, err
			}

			return AllocResponse{}, http.StatusInternalServerError, err
		}

		if !claimed {
			resp, err := o.responseForPod(ctx, allocatedPod)
			if err != nil {
				return AllocResponse{}, http.StatusServiceUnavailable, err
			}

			return resp, http.StatusOK, nil
		}

		resp, err := o.buildResponse(allocatedPod, gatewayIP, nodePublicIP, endpointPort, externalTrafficPolicy, serverPublicKey)
		if err != nil {
			return AllocResponse{}, http.StatusInternalServerError, err
		}

		return resp, http.StatusOK, nil
	}

	if len(unavailable) > 0 {
		return AllocResponse{}, http.StatusServiceUnavailable, fmt.Errorf("no idle playpen runner pods are available with hostPort endpoints on ExternalIP nodes: %s", strings.Join(unavailable, "; "))
	}

	if !matchingArchitecture {
		return AllocResponse{}, http.StatusServiceUnavailable, fmt.Errorf("no idle %s playpen runner pods are available", req.Architecture)
	}

	return AllocResponse{}, http.StatusServiceUnavailable, fmt.Errorf("no idle playpen runner pods are available")
}

func (o *Operator) allocControlPlane(ctx context.Context, keyHash, reqHash string, req AllocRequest) (AllocResponse, int, error) {
	pods, err := o.listControlPlanePods(ctx)
	if err != nil {
		return AllocResponse{}, http.StatusInternalServerError, err
	}

	var unavailable []string

	matchingVersion := false

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[AnnotationIdempotencyKeyHash] != "" {
			continue
		}

		if podKubernetesVersion(pod) != req.KubernetesVersion {
			continue
		}

		matchingVersion = true

		if reason := runnerPodUnavailableReason(pod); reason != "" {
			unavailable = append(unavailable, reason)

			continue
		}

		endpointPort, externalTrafficPolicy, err := controlPlaneEndpointForPod(pod)
		if err != nil {
			unavailable = append(unavailable, err.Error())

			continue
		}

		gatewayIP, nodePublicIP, err := o.gatewayForPod(ctx, pod)
		if err != nil {
			unavailable = append(unavailable, err.Error())

			continue
		}

		if strings.TrimSpace(pod.Annotations[AnnotationControlPlaneKubeconfig]) == "" {
			unavailable = append(unavailable, fmt.Sprintf("control-plane pod %s has no kubeconfig annotation", pod.Name))

			continue
		}

		allocatedPod, claimed, err := o.patchClaim(ctx, pod, keyHash, reqHash, "")
		if err != nil {
			if errors.Is(err, errRunnerAlreadyAllocated) {
				continue
			}

			if errors.Is(err, errIdempotencyRequestConflict) {
				return AllocResponse{}, http.StatusConflict, err
			}

			return AllocResponse{}, http.StatusInternalServerError, err
		}

		if !claimed {
			resp, err := o.responseForPod(ctx, allocatedPod)
			if err != nil {
				return AllocResponse{}, http.StatusServiceUnavailable, err
			}

			return resp, http.StatusOK, nil
		}

		resp, err := o.buildControlPlaneResponse(allocatedPod, gatewayIP, nodePublicIP, endpointPort, externalTrafficPolicy)
		if err != nil {
			return AllocResponse{}, http.StatusInternalServerError, err
		}

		return resp, http.StatusOK, nil
	}

	if len(unavailable) > 0 {
		return AllocResponse{}, http.StatusServiceUnavailable, fmt.Errorf("no idle playpen control-plane pods are available with hostPort endpoints on ExternalIP nodes: %s", strings.Join(unavailable, "; "))
	}

	if !matchingVersion {
		return AllocResponse{}, http.StatusServiceUnavailable, fmt.Errorf("no idle %s playpen control-plane pods are available", req.KubernetesVersion)
	}

	return AllocResponse{}, http.StatusServiceUnavailable, fmt.Errorf("no idle playpen control-plane pods are available")
}

func (o *Operator) Dealloc(ctx context.Context, idempotencyKey string) (int, error) {
	keyHash := hashString(idempotencyKey)

	pods, err := o.listManagedPods(ctx)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	for i := range pods {
		pod := &pods[i]
		if pod.Annotations[AnnotationIdempotencyKeyHash] != keyHash {
			continue
		}

		if err := o.deleteRunnerPod(ctx, pod); err != nil {
			return http.StatusInternalServerError, err
		}

		return http.StatusNoContent, nil
	}

	return http.StatusNoContent, nil
}

func (o *Operator) ReconcileRunners(ctx context.Context) error {
	pods, err := o.listManagedPods(ctx)
	if err != nil {
		return err
	}

	now := time.Now()
	remaining := make([]corev1.Pod, 0, len(pods))

	for i := range pods {
		pod := &pods[i]
		if o.claimExpired(pod, now) {
			if err := o.deleteRunnerPod(ctx, pod); err != nil {
				return err
			}

			continue
		}

		remaining = append(remaining, *pod)
	}

	if err := o.reconcileRunnerPool(ctx, remaining); err != nil {
		return err
	}

	return o.reconcileControlPlanePool(ctx, remaining)
}

type runnerPoolTarget struct {
	architecture string
	count        int
}

func (o *Operator) reconcileRunnerPool(ctx context.Context, pods []corev1.Pod) error {
	targets := []runnerPoolTarget{
		{architecture: ArchitectureAMD64, count: o.Config.RunnerAMD64Count},
		{architecture: ArchitectureARM64, count: o.Config.RunnerARM64Count},
	}

	if o.Config.RunnerWireGuardHostPortStart <= 0 || o.Config.RunnerWireGuardHostPortEnd < o.Config.RunnerWireGuardHostPortStart {
		return fmt.Errorf("runner WireGuard hostPort range is invalid")
	}

	usedHostPorts := map[int32]struct{}{}
	idleCounts := map[string]int{}

	for i := range pods {
		pod := &pods[i]
		if podResourceType(pod) != ResourceTypeRunner {
			continue
		}

		hostPort := podWireGuardHostPort(pod)
		if hostPort != 0 {
			usedHostPorts[hostPort] = struct{}{}
		}

		if pod.DeletionTimestamp.IsZero() && pod.Annotations[AnnotationIdempotencyKeyHash] == "" && hostPort != 0 {
			idleCounts[podArchitecture(pod)]++
		}
	}

	for _, target := range targets {
		if target.count < 0 {
			return fmt.Errorf("desired %s runner count must be non-negative", target.architecture)
		}

		for idleCounts[target.architecture] < target.count {
			hostPort, ok := o.nextRunnerHostPort(usedHostPorts)
			if !ok {
				return fmt.Errorf("no free WireGuard hostPorts are available in range %d-%d", o.Config.RunnerWireGuardHostPortStart, o.Config.RunnerWireGuardHostPortEnd)
			}

			usedHostPorts[hostPort] = struct{}{}

			pod := o.runnerPod(target.architecture, hostPort)
			if err := o.Client.Create(ctx, pod); apierrors.IsAlreadyExists(err) {
				continue
			} else if err != nil {
				return err
			}

			idleCounts[target.architecture]++
		}
	}

	return nil
}

type controlPlanePoolTarget struct {
	kubernetesVersion string
	count             int
}

func (o *Operator) reconcileControlPlanePool(ctx context.Context, pods []corev1.Pod) error {
	targets := make([]controlPlanePoolTarget, 0, len(o.Config.ControlPlaneVersions))
	for _, version := range o.Config.ControlPlaneVersions {
		normalized, err := normalizeKubernetesVersionValue(version)
		if err != nil {
			return err
		}

		targets = append(targets, controlPlanePoolTarget{kubernetesVersion: normalized, count: o.Config.ControlPlaneCount})
	}

	if o.Config.ControlPlaneAPIServerHostPortStart <= 0 || o.Config.ControlPlaneAPIServerHostPortEnd < o.Config.ControlPlaneAPIServerHostPortStart {
		return fmt.Errorf("control-plane API server hostPort range is invalid")
	}

	usedHostPorts := map[int32]struct{}{}
	idleCounts := map[string]int{}

	for i := range pods {
		pod := &pods[i]
		if podResourceType(pod) != ResourceTypeControlPlane {
			continue
		}

		hostPort := podControlPlaneHostPort(pod)
		if hostPort != 0 {
			usedHostPorts[hostPort] = struct{}{}
		}

		if pod.DeletionTimestamp.IsZero() && pod.Annotations[AnnotationIdempotencyKeyHash] == "" && hostPort != 0 {
			idleCounts[podKubernetesVersion(pod)]++
		}
	}

	for _, target := range targets {
		if target.count < 0 {
			return fmt.Errorf("desired %s control-plane count must be non-negative", target.kubernetesVersion)
		}

		for idleCounts[target.kubernetesVersion] < target.count {
			hostPort, ok := o.nextControlPlaneHostPort(usedHostPorts)
			if !ok {
				return fmt.Errorf("no free control-plane API server hostPorts are available in range %d-%d", o.Config.ControlPlaneAPIServerHostPortStart, o.Config.ControlPlaneAPIServerHostPortEnd)
			}

			usedHostPorts[hostPort] = struct{}{}

			pod := o.controlPlanePod(target.kubernetesVersion, hostPort)
			if err := o.Client.Create(ctx, pod); apierrors.IsAlreadyExists(err) {
				continue
			} else if err != nil {
				return err
			}

			idleCounts[target.kubernetesVersion]++
		}
	}

	return nil
}

func (o *Operator) nextRunnerHostPort(used map[int32]struct{}) (int32, bool) {
	for port := o.Config.RunnerWireGuardHostPortStart; port <= o.Config.RunnerWireGuardHostPortEnd; port++ {
		if _, ok := used[port]; !ok {
			return port, true
		}
	}

	return 0, false
}

func (o *Operator) nextControlPlaneHostPort(used map[int32]struct{}) (int32, bool) {
	for port := o.Config.ControlPlaneAPIServerHostPortStart; port <= o.Config.ControlPlaneAPIServerHostPortEnd; port++ {
		if _, ok := used[port]; !ok {
			return port, true
		}
	}

	return 0, false
}

func (o *Operator) runnerPod(architecture string, hostPort int32) *corev1.Pod {
	privileged := true
	listenPort := int32(o.Config.Runner.WireGuard.ListenPort) //nolint:gosec // User-configured Kubernetes port.
	hostPathCharDevice := corev1.HostPathCharDev

	imagePullPolicy := corev1.PullPolicy(strings.TrimSpace(o.Config.RunnerImagePullPolicy))
	if imagePullPolicy == "" {
		imagePullPolicy = corev1.PullAlways
	}

	volumeMounts := []corev1.VolumeMount{
		{Name: "data", MountPath: runnerDataMountPath},
		{Name: "wireguard", MountPath: runnerWGMountPath},
		{Name: "tun", MountPath: runnerTunPath},
	}

	volumes := []corev1.Volume{
		{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "wireguard", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tun", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: runnerTunPath, Type: &hostPathCharDevice}}},
	}
	if o.Config.RunnerRequireKVM {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "kvm", MountPath: runnerKVMPath})
		volumes = append(volumes, corev1.Volume{Name: "kvm", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: runnerKVMPath, Type: &hostPathCharDevice}}})
	}

	var tolerations []corev1.Toleration
	if o.Config.RunnerControlPlaneToleration {
		tolerations = []corev1.Toleration{
			{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
			{Key: "node-role.kubernetes.io/master", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runnerPodName(architecture, hostPort),
			Namespace: o.Config.RunnerNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      runnerAppName,
				"app.kubernetes.io/component": runnerComponent,
				LabelResourceType:             ResourceTypeRunner,
				LabelArchitecture:             architecture,
				runnerHostPortLabel:           strconv.FormatInt(int64(hostPort), 10),
			},
			Annotations: map[string]string{
				runnerKubeFQDNAnnot: "true",
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: o.Config.RunnerServiceAccountName,
			RestartPolicy:      corev1.RestartPolicyAlways,
			Tolerations:        tolerations,
			NodeSelector: map[string]string{
				"kubernetes.io/arch": architecture,
			},
			Containers: []corev1.Container{
				{
					Name:            runnerContainerName,
					Image:           o.Config.RunnerImage,
					ImagePullPolicy: imagePullPolicy,
					Command:         []string{"/unbounded/bin/playpen-runner"},
					Args: []string{
						"--architecture=" + architecture,
						"--wireguard-private-key-file=" + runnerWGMountPath + "/privatekey",
						"--wireguard-listen-port=" + strconv.Itoa(o.Config.Runner.WireGuard.ListenPort),
					},
					Env: append([]corev1.EnvVar{
						{
							Name: "POD_NAME",
							ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "metadata.name",
							}},
						},
						{
							Name: "POD_NAMESPACE",
							ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "metadata.namespace",
							}},
						},
					}, kubernetesServiceEnv()...),
					Ports: []corev1.ContainerPort{
						{
							Name:          runnerHTTPSPort,
							Protocol:      corev1.ProtocolTCP,
							ContainerPort: runnerHTTPSContainer,
						},
						{
							Name:          runnerWireGuardPort,
							Protocol:      corev1.ProtocolUDP,
							ContainerPort: listenPort,
							HostPort:      hostPort,
						},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("4"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
						},
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
							Path:   "/healthz",
							Port:   intstr.FromString(runnerHTTPSPort),
							Scheme: corev1.URISchemeHTTPS,
						}},
						InitialDelaySeconds: 10,
						PeriodSeconds:       10,
					},
					SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
					VolumeMounts:    volumeMounts,
				},
			},
			Volumes: volumes,
		},
	}
}

func kubernetesServiceEnv() []corev1.EnvVar {
	var env []corev1.EnvVar

	for _, name := range []string{"KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT", "KUBERNETES_SERVICE_PORT_HTTPS"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			env = append(env, corev1.EnvVar{Name: name, Value: value})
		}
	}

	return env
}

func (o *Operator) controlPlanePod(kubernetesVersion string, hostPort int32) *corev1.Pod {
	privileged := true
	apiPort := int32(6443)

	imagePullPolicy := corev1.PullPolicy(strings.TrimSpace(o.Config.RunnerImagePullPolicy))
	if imagePullPolicy == "" {
		imagePullPolicy = corev1.PullAlways
	}

	image := controlPlaneImage(o.Config.ControlPlaneImage, kubernetesVersion)
	guestServer := fmt.Sprintf("https://%s:%d", o.Config.Runner.Guest.Gateway, apiPort)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controlPlanePodName(kubernetesVersion, hostPort),
			Namespace: o.Config.RunnerNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      controlPlaneAppName,
				"app.kubernetes.io/component": controlPlaneComponent,
				LabelResourceType:             ResourceTypeControlPlane,
				LabelKubernetesVersion:        sanitizeLabelValue(kubernetesVersion),
				runnerHostPortLabel:           strconv.FormatInt(int64(hostPort), 10),
			},
			Annotations: map[string]string{
				AnnotationControlPlaneGuestServer: guestServer,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: o.Config.ControlPlaneServiceAccountName,
			RestartPolicy:      corev1.RestartPolicyAlways,
			Containers: []corev1.Container{
				{
					Name:            controlPlaneContainerName,
					Image:           image,
					ImagePullPolicy: imagePullPolicy,
					Command:         []string{"/bin/k3s"},
					Args: []string{
						"server",
						"--disable-agent",
						"--disable=traefik,servicelb,metrics-server,local-storage",
						"--write-kubeconfig=" + controlPlaneKubeconfig,
						"--write-kubeconfig-mode=0644",
						"--tls-san=" + o.Config.Runner.Guest.Gateway,
						"--advertise-address=127.0.0.1",
						"--bind-address=0.0.0.0",
					},
					Ports: []corev1.ContainerPort{
						{
							Name:          controlPlaneAPIPort,
							Protocol:      corev1.ProtocolTCP,
							ContainerPort: apiPort,
							HostPort:      hostPort,
						},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("4Gi"),
						},
					},
					LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
						Path:   "/readyz",
						Port:   intstr.FromString(controlPlaneAPIPort),
						Scheme: corev1.URISchemeHTTPS,
					}}, InitialDelaySeconds: 30, PeriodSeconds: 10},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
						Path:   "/readyz",
						Port:   intstr.FromString(controlPlaneAPIPort),
						Scheme: corev1.URISchemeHTTPS,
					}}, InitialDelaySeconds: 5, PeriodSeconds: 5},
					SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "data", MountPath: controlPlaneDataMountPath},
						{Name: "kubeconfig", MountPath: controlPlaneKubeMountPath},
					},
				},
				{
					Name:            controlPlaneHelperName,
					Image:           o.Config.RunnerImage,
					ImagePullPolicy: imagePullPolicy,
					Command:         []string{"/unbounded/bin/playpen-runner"},
					Args: []string{
						"control-plane",
						"--kubeconfig=" + controlPlaneKubeconfig,
						"--guest-server=" + guestServer,
						"--kubernetes-version=" + kubernetesVersion,
					},
					Env: append([]corev1.EnvVar{
						{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
						{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
					}, kubernetesServiceEnv()...),
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: "kubeconfig", MountPath: controlPlaneKubeMountPath}},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "kubeconfig", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
}

func runnerPodName(architecture string, hostPort int32) string {
	return fmt.Sprintf("playpen-runner-%s-%d", architecture, hostPort)
}

func controlPlanePodName(kubernetesVersion string, hostPort int32) string {
	return fmt.Sprintf("playpen-control-plane-%s-%d", strings.TrimPrefix(kubernetesVersion, "v"), hostPort)
}

func (o *Operator) EnsureTLSSecret(ctx context.Context) (tls.Certificate, error) {
	secret := &corev1.Secret{}

	key := types.NamespacedName{Namespace: o.Config.Namespace, Name: o.Config.TLSSecretName}
	if err := o.Client.Get(ctx, key, secret); err == nil {
		cert, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
		if err != nil {
			return tls.Certificate{}, err
		}

		return cert, nil
	} else if !apierrors.IsNotFound(err) {
		return tls.Certificate{}, err
	}

	serviceName := strings.TrimSpace(o.Config.ServiceName)
	if serviceName == "" {
		serviceName = "playpen-operator"
	}

	certPEM, keyPEM, err := selfSignedCert(serviceName, []string{
		serviceName,
		serviceName + "." + o.Config.Namespace,
		serviceName + "." + o.Config.Namespace + ".svc",
		serviceName + "." + o.Config.Namespace + ".svc.cluster.local",
		"localhost",
	})
	if err != nil {
		return tls.Certificate{}, err
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: o.Config.TLSSecretName, Namespace: o.Config.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}
	if err := o.Client.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return tls.Certificate{}, err
	}

	return tls.X509KeyPair(certPEM, keyPEM)
}

func (o *Operator) servingCertPEM(ctx context.Context) ([]byte, error) {
	secret := &corev1.Secret{}

	key := types.NamespacedName{Namespace: o.Config.Namespace, Name: o.Config.TLSSecretName}
	if err := o.Client.Get(ctx, key, secret); err != nil {
		return nil, err
	}

	certPEM := secret.Data[corev1.TLSCertKey]
	if len(certPEM) == 0 {
		return nil, fmt.Errorf("secret %s/%s is missing %s", o.Config.Namespace, o.Config.TLSSecretName, corev1.TLSCertKey)
	}

	return certPEM, nil
}

func (o *Operator) injectAPIServiceCABundle(ctx context.Context, caBundle []byte) error {
	if o.APIRegClient == nil || len(caBundle) == 0 {
		return nil
	}

	apiService, err := o.APIRegClient.ApiregistrationV1().APIServices().Get(ctx, apiServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		klog.Warningf("APIService %s not found; skipping caBundle injection", apiServiceName)

		return nil
	}

	if err != nil {
		return fmt.Errorf("get APIService %s: %w", apiServiceName, err)
	}

	if bytes.Equal(apiService.Spec.CABundle, caBundle) {
		return nil
	}

	apiService.Spec.CABundle = caBundle
	if _, err := o.APIRegClient.ApiregistrationV1().APIServices().Update(ctx, apiService, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update APIService %s caBundle: %w", apiServiceName, err)
	}

	return nil
}

func (o *Operator) refreshAggregatedClientCAs(ctx context.Context) {
	if o.KubeClient == nil {
		o.aggregatedMu.Lock()
		defer o.aggregatedMu.Unlock()

		o.aggregatedClientCAs = nil
		o.aggregatedClientAllowedCNs = nil

		return
	}

	cm, err := o.KubeClient.CoreV1().ConfigMaps(extensionAuthNamespace).Get(ctx, extensionAuthConfigMapName, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Failed to read %s/%s: %v", extensionAuthNamespace, extensionAuthConfigMapName, err)
		o.aggregatedMu.Lock()
		defer o.aggregatedMu.Unlock()

		o.aggregatedClientCAs = nil
		o.aggregatedClientAllowedCNs = nil

		return
	}

	pool := x509.NewCertPool()

	caPEM := []byte(cm.Data[extensionAuthClientCAKey])
	if len(caPEM) == 0 || !pool.AppendCertsFromPEM(caPEM) {
		klog.Warningf("ConfigMap %s/%s does not contain valid %q PEM data", extensionAuthNamespace, extensionAuthConfigMapName, extensionAuthClientCAKey)
		o.aggregatedMu.Lock()
		defer o.aggregatedMu.Unlock()

		o.aggregatedClientCAs = nil
		o.aggregatedClientAllowedCNs = nil

		return
	}

	allowedNames, err := parseRequestHeaderAllowedNames(cm.Data[extensionAuthAllowedNamesKey])
	if err != nil {
		klog.Warningf("Failed to parse %q from %s/%s: %v", extensionAuthAllowedNamesKey, extensionAuthNamespace, extensionAuthConfigMapName, err)
		o.aggregatedMu.Lock()
		defer o.aggregatedMu.Unlock()

		o.aggregatedClientCAs = nil
		o.aggregatedClientAllowedCNs = nil

		return
	}

	o.aggregatedMu.Lock()
	defer o.aggregatedMu.Unlock()

	o.aggregatedClientCAs = pool
	o.aggregatedClientAllowedCNs = allowedNames
}

func parseRequestHeaderAllowedNames(raw string) (map[string]struct{}, error) {
	if raw == "" {
		return nil, nil
	}

	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, err
	}

	if len(names) == 0 {
		return nil, nil
	}

	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}

		allowed[name] = struct{}{}
	}

	if len(allowed) == 0 {
		return nil, nil
	}

	return allowed, nil
}

func (o *Operator) authorizeAggregatedRequest(w http.ResponseWriter, r *http.Request, resource string) bool {
	if !o.isTrustedAggregatedRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)

		return false
	}

	user := strings.TrimSpace(r.Header.Get(remoteUserHeader))
	if user == "" {
		http.Error(w, "missing remote user", http.StatusForbidden)

		return false
	}

	if o.KubeClient == nil {
		http.Error(w, "authorization client is not configured", http.StatusInternalServerError)

		return false
	}

	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   user,
			Groups: remoteGroups(r),
			ResourceAttributes: &authzv1.ResourceAttributes{
				Verb:     "create",
				Group:    apiGroup,
				Resource: resource,
			},
		},
	}

	result, err := o.KubeClient.AuthorizationV1().SubjectAccessReviews().Create(r.Context(), sar, metav1.CreateOptions{})
	if err != nil {
		http.Error(w, "authorization check failed", http.StatusInternalServerError)

		return false
	}

	if !result.Status.Allowed || result.Status.Denied {
		http.Error(w, "forbidden", http.StatusForbidden)

		return false
	}

	return true
}

func remoteGroups(r *http.Request) []string {
	var groups []string

	for _, value := range r.Header.Values(remoteGroupHeader) {
		for _, group := range strings.Split(value, ",") {
			group = strings.TrimSpace(group)
			if group != "" {
				groups = append(groups, group)
			}
		}
	}

	return groups
}

func (o *Operator) isTrustedAggregatedRequest(r *http.Request) bool {
	o.aggregatedMu.RLock()
	clientCAs := o.aggregatedClientCAs
	allowedCNs := o.aggregatedClientAllowedCNs
	o.aggregatedMu.RUnlock()

	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || clientCAs == nil {
		return false
	}

	leaf := r.TLS.PeerCertificates[0]

	intermediates := x509.NewCertPool()
	for _, cert := range r.TLS.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         clientCAs,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return false
	}

	if len(allowedCNs) == 0 {
		return true
	}

	_, ok := allowedCNs[leaf.Subject.CommonName]

	return ok
}

func (o *Operator) listRunnerPods(ctx context.Context) (*corev1.PodList, error) {
	selector, err := labels.Parse(o.Config.RunnerLabelSelector)
	if err != nil {
		return nil, err
	}

	list := &corev1.PodList{}
	if err := o.Client.List(ctx, list, client.InNamespace(o.Config.RunnerNamespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}

	return list, nil
}

func (o *Operator) listManagedPods(ctx context.Context) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := o.Client.List(ctx, list, client.InNamespace(o.Config.RunnerNamespace)); err != nil {
		return nil, err
	}

	pods := make([]corev1.Pod, 0, len(list.Items))
	for _, pod := range list.Items {
		if isManagedPlaypenPod(&pod) {
			pods = append(pods, pod)
		}
	}

	return pods, nil
}

func (o *Operator) listControlPlanePods(ctx context.Context) (*corev1.PodList, error) {
	pods, err := o.listManagedPods(ctx)
	if err != nil {
		return nil, err
	}

	list := &corev1.PodList{}

	for _, pod := range pods {
		if podResourceType(&pod) == ResourceTypeControlPlane {
			list.Items = append(list.Items, pod)
		}
	}

	return list, nil
}

func isManagedPlaypenPod(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}

	if _, ok := pod.Labels[LabelResourceType]; ok {
		if _, err := normalizeResourceType(pod.Labels[LabelResourceType]); err == nil {
			return true
		}
	}

	switch pod.Labels["app.kubernetes.io/name"] {
	case runnerAppName, controlPlaneAppName:
		return true
	default:
		return false
	}
}

func runnerPodUnavailableReason(pod *corev1.Pod) string {
	if !pod.DeletionTimestamp.IsZero() {
		return fmt.Sprintf("runner pod %s is terminating", pod.Name)
	}

	if pod.Status.Phase != corev1.PodRunning {
		return fmt.Sprintf("runner pod %s is %s", pod.Name, pod.Status.Phase)
	}

	if len(pod.Status.ContainerStatuses) == 0 {
		return fmt.Sprintf("runner pod %s has no container status", pod.Name)
	}

	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Running == nil {
			return fmt.Sprintf("runner pod %s container %s is not running", pod.Name, status.Name)
		}

		if !status.Ready {
			return fmt.Sprintf("runner pod %s container %s is not ready", pod.Name, status.Name)
		}
	}

	return ""
}

var (
	errRunnerAlreadyAllocated     = errors.New("runner pod was already allocated")
	errIdempotencyRequestConflict = errors.New("idempotency key was already used with a different request")
)

func (o *Operator) patchClaim(ctx context.Context, pod *corev1.Pod, keyHash, reqHash, clientPublicKey string) (*corev1.Pod, bool, error) {
	var updated *corev1.Pod

	claimed := false

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1.Pod{}
		if err := o.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}, current); err != nil {
			return err
		}

		if current.Annotations[AnnotationIdempotencyKeyHash] == keyHash {
			if current.Annotations[AnnotationRequestHash] != reqHash {
				return errIdempotencyRequestConflict
			}

			updated = current
			claimed = false

			return nil
		}

		if current.Annotations[AnnotationIdempotencyKeyHash] != "" {
			return errRunnerAlreadyAllocated
		}

		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}

		if current.Labels == nil {
			current.Labels = map[string]string{}
		}

		allocationID := allocationIDForPod(pod)
		current.Annotations[AnnotationIdempotencyKeyHash] = keyHash
		current.Annotations[AnnotationRequestHash] = reqHash
		current.Annotations[AnnotationClientWireGuardPublicKey] = clientPublicKey
		current.Annotations[AnnotationClaimedAt] = time.Now().UTC().Format(time.RFC3339)
		current.Labels[LabelAllocated] = allocationID

		if err := o.Client.Update(ctx, current); err != nil {
			return err
		}

		updated = current
		claimed = true

		return nil
	})
	if err != nil {
		return nil, false, err
	}

	return updated, claimed, nil
}

func (o *Operator) responseForPod(ctx context.Context, pod *corev1.Pod) (AllocResponse, error) {
	if podResourceType(pod) == ResourceTypeControlPlane {
		gatewayIP, nodePublicIP, err := o.gatewayForPod(ctx, pod)
		if err != nil {
			return AllocResponse{}, err
		}

		endpointPort, externalTrafficPolicy, err := controlPlaneEndpointForPod(pod)
		if err != nil {
			return AllocResponse{}, err
		}

		return o.buildControlPlaneResponse(pod, gatewayIP, nodePublicIP, endpointPort, externalTrafficPolicy)
	}

	serverPublicKey, err := serverWireGuardPublicKeyForPod(pod)
	if err != nil {
		return AllocResponse{}, err
	}

	gatewayIP, nodePublicIP, err := o.gatewayForPod(ctx, pod)
	if err != nil {
		return AllocResponse{}, err
	}

	endpointPort, externalTrafficPolicy, err := endpointForPod(pod)
	if err != nil {
		return AllocResponse{}, err
	}

	return o.buildResponse(pod, gatewayIP, nodePublicIP, endpointPort, externalTrafficPolicy, serverPublicKey)
}

func endpointForPod(pod *corev1.Pod) (int32, string, error) {
	if hostPort := podWireGuardHostPort(pod); hostPort != 0 {
		return hostPort, "HostPort", nil
	}

	return 0, "", fmt.Errorf("runner pod %s/%s has no WireGuard hostPort", pod.Namespace, pod.Name)
}

func controlPlaneEndpointForPod(pod *corev1.Pod) (int32, string, error) {
	if hostPort := podControlPlaneHostPort(pod); hostPort != 0 {
		return hostPort, "HostPort", nil
	}

	return 0, "", fmt.Errorf("control-plane pod %s/%s has no API server hostPort", pod.Namespace, pod.Name)
}

func (o *Operator) buildResponse(pod *corev1.Pod, gatewayIP, nodePublicIP string, endpointPort int32, externalTrafficPolicy, serverPublicKey string) (AllocResponse, error) {
	runnerCfg := o.Config.Runner

	serverWG, err := runnerAddressIP(runnerCfg.WireGuard.ServerAddress)
	if err != nil {
		return AllocResponse{}, err
	}

	clientWG, err := runnerAddressIP(runnerCfg.WireGuard.ClientAddress)
	if err != nil {
		return AllocResponse{}, err
	}

	redfishURL := runnerCfg.PublicRedfishURL
	if redfishURL == "" {
		redfishURL = defaultRunnerPublicRedfishURL(runnerCfg)
	}

	return AllocResponse{
		ResourceType: ResourceTypeRunner,
		Pod: PodResponse{
			Namespace:    pod.Namespace,
			Name:         pod.Name,
			NodeName:     pod.Spec.NodeName,
			NodePublicIP: nodePublicIP,
			ResourceType: ResourceTypeRunner,
			Architecture: podArchitecture(pod),
		},
		Endpoint: EndpointResponse{
			Host:                  gatewayIP,
			WireGuardUDPPort:      endpointPort,
			ExternalTrafficPolicy: externalTrafficPolicy,
		},
		WireGuard: WireGuardResponse{
			Interface:       runnerCfg.WireGuard.Interface,
			ServerPublicKey: serverPublicKey,
			ServerAddress:   runnerCfg.WireGuard.ServerAddress,
			ClientAddress:   runnerCfg.WireGuard.ClientAddress,
			ListenPort:      runnerCfg.WireGuard.ListenPort,
		},
		VXLAN: VXLANResponse{
			Interface:     runnerCfg.VXLAN.Interface,
			VNI:           runnerCfg.VXLAN.VNI,
			UDPPort:       runnerCfg.VXLAN.Port,
			ServerAddress: serverWG,
			ClientAddress: clientWG,
		},
		Network: NetworkResponse{
			GuestMAC:    runnerCfg.Guest.MAC,
			GuestIPv4:   runnerCfg.Guest.IPv4,
			SubnetMask:  runnerCfg.Guest.SubnetMask,
			GatewayIPv4: runnerCfg.Guest.Gateway,
			DNS:         runnerCfg.Guest.DNS,
		},
		Redfish: map[string]string{
			"url":                    redfishURL,
			"username":               runnerCfg.Redfish.Username,
			"password":               runnerCfg.Redfish.Password,
			"certPEM":                pod.Annotations[AnnotationRedfishCertPEM],
			"deviceID":               runnerCfg.Redfish.DeviceID,
			"systemURL":              redfishURL + "/redfish/v1/Systems/" + runnerCfg.Redfish.DeviceID,
			"serialConsoleStreamURI": "/redfish/v1/Systems/" + runnerCfg.Redfish.DeviceID + "/Oem/Unbounded/SerialConsole/Stream",
		},
	}, nil
}

func (o *Operator) buildControlPlaneResponse(pod *corev1.Pod, gatewayIP, nodePublicIP string, endpointPort int32, externalTrafficPolicy string) (AllocResponse, error) {
	version := podKubernetesVersion(pod)

	rawKubeconfig := strings.TrimSpace(pod.Annotations[AnnotationControlPlaneKubeconfig])
	if rawKubeconfig == "" {
		return AllocResponse{}, fmt.Errorf("control-plane pod %s/%s has no kubeconfig annotation", pod.Namespace, pod.Name)
	}

	hostServer := fmt.Sprintf("https://%s:%d", gatewayIP, endpointPort)

	guestServer := strings.TrimSpace(pod.Annotations[AnnotationControlPlaneGuestServer])
	if guestServer == "" {
		guestServer = fmt.Sprintf("https://%s:6443", o.Config.Runner.Guest.Gateway)
	}

	hostKubeconfig, err := rewriteKubeconfigServer(rawKubeconfig, hostServer, o.Config.Runner.Guest.Gateway)
	if err != nil {
		return AllocResponse{}, err
	}

	return AllocResponse{
		ResourceType: ResourceTypeControlPlane,
		Pod: PodResponse{
			Namespace:         pod.Namespace,
			Name:              pod.Name,
			NodeName:          pod.Spec.NodeName,
			NodePublicIP:      nodePublicIP,
			ResourceType:      ResourceTypeControlPlane,
			Architecture:      podArchitecture(pod),
			KubernetesVersion: version,
		},
		Endpoint: EndpointResponse{
			Host:                  gatewayIP,
			APIServerTCPPort:      endpointPort,
			ExternalTrafficPolicy: externalTrafficPolicy,
		},
		ControlPlane: ControlPlaneResponse{
			KubernetesVersion: version,
			Kubeconfig:        hostKubeconfig,
			APIServerURL:      hostServer,
			GuestAPIServerURL: guestServer,
		},
	}, nil
}

func rewriteKubeconfigServer(raw, server, tlsServerName string) (string, error) {
	cfg, err := clientcmd.Load([]byte(raw))
	if err != nil {
		return "", fmt.Errorf("parse control-plane kubeconfig: %w", err)
	}

	for _, cluster := range cfg.Clusters {
		cluster.Server = server
		cluster.TLSServerName = tlsServerName
	}

	data, err := clientcmd.Write(*cfg)
	if err != nil {
		return "", fmt.Errorf("write control-plane kubeconfig: %w", err)
	}

	return string(data), nil
}

func normalizeArchitecture(value string) (string, error) {
	switch arch := strings.ToLower(strings.TrimSpace(value)); arch {
	case "", ArchitectureAMD64:
		return ArchitectureAMD64, nil
	case ArchitectureARM64:
		return ArchitectureARM64, nil
	default:
		return "", fmt.Errorf("architecture must be %q or %q", ArchitectureAMD64, ArchitectureARM64)
	}
}

func normalizeResourceType(value string) (string, error) {
	switch resourceType := strings.ToLower(strings.TrimSpace(value)); resourceType {
	case "", strings.ToLower(ResourceTypeRunner):
		return ResourceTypeRunner, nil
	case strings.ToLower(ResourceTypeControlPlane), "control-plane", "controlplane":
		return ResourceTypeControlPlane, nil
	default:
		return "", fmt.Errorf("resourceType must be %q or %q", ResourceTypeRunner, ResourceTypeControlPlane)
	}
}

func (o *Operator) normalizeKubernetesVersion(value string) (string, error) {
	requested := strings.TrimSpace(value)
	if requested == "" {
		return latestConfiguredKubernetesVersion(o.Config.ControlPlaneVersions)
	}

	normalized, err := normalizeKubernetesVersionValue(requested)
	if err != nil {
		return "", err
	}

	for _, configured := range o.Config.ControlPlaneVersions {
		configuredVersion, err := normalizeKubernetesVersionValue(configured)
		if err != nil {
			return "", err
		}

		if normalized == configuredVersion {
			return normalized, nil
		}
	}

	return "", fmt.Errorf("kubernetes version %q is not configured for control-plane allocation", normalized)
}

type kubernetesVersionParts struct {
	major int
	minor int
	patch int
}

func latestConfiguredKubernetesVersion(versions []string) (string, error) {
	if len(versions) == 0 {
		return "", fmt.Errorf("no control-plane Kubernetes versions are configured")
	}

	latest := ""

	var latestParts kubernetesVersionParts

	for _, version := range versions {
		normalized, err := normalizeKubernetesVersionValue(version)
		if err != nil {
			return "", err
		}

		parts, err := parseKubernetesVersionParts(normalized)
		if err != nil {
			return "", err
		}

		if latest == "" || compareKubernetesVersionParts(parts, latestParts) > 0 {
			latest = normalized
			latestParts = parts
		}
	}

	return latest, nil
}

func parseKubernetesVersionParts(version string) (kubernetesVersionParts, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(version), "v")
	trimmed = strings.SplitN(trimmed, "-", 2)[0]
	trimmed = strings.SplitN(trimmed, "+", 2)[0]

	fields := strings.Split(trimmed, ".")
	if len(fields) != 3 {
		return kubernetesVersionParts{}, fmt.Errorf("kubernetes version %q must be major.minor.patch", version)
	}

	major, err := strconv.Atoi(fields[0])
	if err != nil {
		return kubernetesVersionParts{}, fmt.Errorf("parse kubernetes version %q major: %w", version, err)
	}

	minor, err := strconv.Atoi(fields[1])
	if err != nil {
		return kubernetesVersionParts{}, fmt.Errorf("parse kubernetes version %q minor: %w", version, err)
	}

	patch, err := strconv.Atoi(fields[2])
	if err != nil {
		return kubernetesVersionParts{}, fmt.Errorf("parse kubernetes version %q patch: %w", version, err)
	}

	return kubernetesVersionParts{major: major, minor: minor, patch: patch}, nil
}

func compareKubernetesVersionParts(a, b kubernetesVersionParts) int {
	if a.major != b.major {
		return a.major - b.major
	}

	if a.minor != b.minor {
		return a.minor - b.minor
	}

	return a.patch - b.patch
}

func normalizeKubernetesVersionValue(value string) (string, error) {
	version := strings.TrimSpace(value)
	if version == "" {
		return "", fmt.Errorf("kubernetes version is required")
	}

	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}

	return version, nil
}

func controlPlaneImage(template, kubernetesVersion string) string {
	template = strings.TrimSpace(template)
	if template == "" {
		template = "rancher/k3s:{version}-k3s1"
	}

	return strings.ReplaceAll(template, "{version}", kubernetesVersion)
}

func podArchitecture(pod *corev1.Pod) string {
	arch, err := normalizeArchitecture(pod.Labels[LabelArchitecture])
	if err != nil {
		return pod.Labels[LabelArchitecture]
	}

	return arch
}

func podResourceType(pod *corev1.Pod) string {
	resourceType, err := normalizeResourceType(pod.Labels[LabelResourceType])
	if err == nil && resourceType != ResourceTypeRunner {
		return resourceType
	}

	if pod.Labels["app.kubernetes.io/name"] == controlPlaneAppName {
		return ResourceTypeControlPlane
	}

	return ResourceTypeRunner
}

func podKubernetesVersion(pod *corev1.Pod) string {
	version, err := normalizeKubernetesVersionValue(pod.Labels[LabelKubernetesVersion])
	if err != nil {
		return pod.Labels[LabelKubernetesVersion]
	}

	return version
}

func serverWireGuardPublicKeyForPod(pod *corev1.Pod) (string, error) {
	serverPublicKey := strings.TrimSpace(pod.Annotations[AnnotationServerWireGuardPublicKey])
	if _, err := wgtypes.ParseKey(serverPublicKey); err != nil {
		return "", fmt.Errorf("runner pod %q has no valid server WireGuard public key", pod.Name)
	}

	return serverPublicKey, nil
}

func podWireGuardHostPort(pod *corev1.Pod) int32 {
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.Protocol == corev1.ProtocolUDP && port.HostPort != 0 && port.Name == "wireguard" {
				return port.HostPort
			}
		}
	}

	return 0
}

func podControlPlaneHostPort(pod *corev1.Pod) int32 {
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.Protocol == corev1.ProtocolTCP && port.HostPort != 0 && port.Name == controlPlaneAPIPort {
				return port.HostPort
			}
		}
	}

	return 0
}

func (o *Operator) gatewayForPod(ctx context.Context, pod *corev1.Pod) (string, string, error) {
	if pod.Spec.NodeName == "" {
		return "", "", fmt.Errorf("runner pod is not scheduled")
	}

	node := &corev1.Node{}
	if err := o.Client.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
		return "", "", err
	}

	nodePublicIP := nodeExternalIP(node)
	if nodePublicIP == "" {
		return "", "", fmt.Errorf("runner pod %s/%s is on node %s with no ExternalIP", pod.Namespace, pod.Name, pod.Spec.NodeName)
	}

	return nodePublicIP, nodePublicIP, nil
}

func nodeExternalIP(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeExternalIP && addr.Address != "" {
			return addr.Address
		}
	}

	return ""
}

func (o *Operator) deleteRunnerPod(ctx context.Context, pod *corev1.Pod) error {
	if err := o.Client.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return nil
}

func (o *Operator) claimExpired(pod *corev1.Pod, now time.Time) bool {
	if o.Config.PlaypenTTL <= 0 || pod.Annotations[AnnotationIdempotencyKeyHash] == "" {
		return false
	}

	claimedAt, err := time.Parse(time.RFC3339, pod.Annotations[AnnotationClaimedAt])
	if err != nil {
		return true
	}

	return !claimedAt.Add(o.Config.PlaypenTTL).After(now)
}

func selfSignedCert(commonName string, hosts []string) ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	notBefore := time.Now().Add(-time.Minute)
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(notBefore.UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if host != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	return certPEM, keyPEM, nil
}

func allocationIDForPod(pod *corev1.Pod) string {
	return hashString(pod.Namespace + "/" + pod.Name)[:16]
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))

	return hex.EncodeToString(sum[:])
}

func sanitizeLabelValue(value string) string {
	return strings.Trim(strings.NewReplacer("+", "-", "_", "-").Replace(value), "-.")
}

func runnerAddressIP(value string) (string, error) {
	if i := strings.Index(value, "/"); i >= 0 {
		value = value[:i]
	}

	if net.ParseIP(value) == nil {
		return "", fmt.Errorf("invalid IP address %q", value)
	}

	return value, nil
}

func defaultRunnerPublicRedfishURL(cfg runner.Config) string {
	host, err := runnerAddressIP(cfg.WireGuard.ServerAddress)
	if err != nil {
		return "https://" + cfg.ListenAddr
	}

	_, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil || port == "" {
		port = "8443"
	}

	return "https://" + net.JoinHostPort(host, port)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value) //nolint:errcheck // Response body write errors cannot be reported to the client.
}
