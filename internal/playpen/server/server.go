// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/certmanager"
	webhookpkg "github.com/Azure/unbounded/internal/net/webhook"
	"github.com/Azure/unbounded/internal/playpen/kubevirt"
	"github.com/Azure/unbounded/internal/playpen/network"
)

type Config struct {
	Port int
}

type Server struct {
	kube    kubernetes.Interface
	webhook *webhookpkg.Server
	certMgr *certmanager.CertManager
	vm      *kubevirt.Manager
	network *network.Manager
	cfg     Config
	mux     *http.ServeMux

	sessionMu       sync.RWMutex
	redfishSessions map[string]string
}

func New(kube kubernetes.Interface, webhook *webhookpkg.Server, certMgr *certmanager.CertManager, vm *kubevirt.Manager, networkMgr *network.Manager, cfg Config) *Server {
	if cfg.Port == 0 {
		cfg.Port = 9443
	}

	return &Server{
		kube:            kube,
		webhook:         webhook,
		certMgr:         certMgr,
		vm:              vm,
		network:         networkMgr,
		cfg:             cfg,
		mux:             http.NewServeMux(),
		redfishSessions: make(map[string]string),
	}
}

func (s *Server) Run(ctx context.Context) error {
	s.webhook.RefreshAggregatedClientCAs(ctx)
	s.registerHandlers()

	go s.cleanupLoop(ctx)

	addr := fmt.Sprintf(":%d", s.cfg.Port)
	tlsConfig := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: s.certMgr.GetCertificateFunc(),
		ClientAuth:     tls.VerifyClientCertIfGiven,
		ClientCAs:      s.webhook.GetClientCAs(),
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
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
			klog.Errorf("playpen server shutdown: %v", err)
		}
	}()

	klog.Infof("Starting playpen HTTPS server on %s", addr)
	if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}

func (s *Server) registerHandlers() {
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	s.mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	s.mux.HandleFunc("/apis/"+GroupName, s.handleDiscoveryGroup)
	s.mux.HandleFunc("/apis/"+GroupVersion, s.handleDiscoveryVersion)
	s.mux.HandleFunc(AllocatePath, s.handleAllocate)
	s.mux.HandleFunc(DeallocatePath, s.handleDeallocate)
	s.mux.HandleFunc("/redfish/v1", s.handleRedfishRoot)
	s.mux.HandleFunc("/redfish/v1/", s.handleRedfish)
}

func (s *Server) handleDiscoveryGroup(w http.ResponseWriter, r *http.Request) {
	if !s.trustedAggregatedRequest(w, r) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"kind":       "APIGroup",
		"apiVersion": "v1",
		"name":       GroupName,
		"versions": []map[string]string{{
			"groupVersion": GroupVersion,
			"version":      Version,
		}},
		"preferredVersion": map[string]string{"groupVersion": GroupVersion, "version": Version},
	})
}

func (s *Server) handleDiscoveryVersion(w http.ResponseWriter, r *http.Request) {
	if !s.trustedAggregatedRequest(w, r) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"kind":         "APIResourceList",
		"apiVersion":   "v1",
		"groupVersion": GroupVersion,
		"resources": []map[string]any{
			{"name": "vms/allocate", "singularName": "", "namespaced": false, "kind": "PlaypenVMAllocation", "verbs": []string{"create"}},
			{"name": "vms/deallocate", "singularName": "", "namespaced": false, "kind": "PlaypenVMDeallocation", "verbs": []string{"create"}},
		},
	})
}

func (s *Server) handleAllocate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizedAggregatedRequest(w, r, "create", "vms/allocate") {
		return
	}

	var req AllocateRequest
	if !decodeBody(w, r, &req) {
		return
	}

	vmAlloc, err := s.vm.Allocate(r.Context(), kubevirt.AllocateRequest{
		NamePrefix:            req.NamePrefix,
		Site:                  req.Site,
		PodCIDR:               req.PodCIDR,
		VMImage:               req.VMImage,
		NetworkAttachmentName: req.NetworkAttachmentName,
		SSHAuthorizedKey:      req.SSHAuthorizedKey,
		TTLSeconds:            req.TTLSeconds,
		L2Tunnel: kubevirt.L2TunnelConfig{
			ClientUnderlayIP: req.ClientInternalIP,
		},
	})
	if err != nil {
		klog.Errorf("playpen allocate VM failed: %v", err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	netAlloc, err := s.network.Allocate(r.Context(), vmAlloc.ID, vmAlloc.ExpiresAt, network.AllocateRequest{
		Site:                     req.Site,
		GatewayPool:              req.GatewayPool,
		ClientWireGuardPublicKey: req.ClientWireGuardPublicKey,
		ClientInternalIP:         req.ClientInternalIP,
	}, vmAlloc.PodCIDR)
	if err != nil {
		klog.Errorf("playpen allocate network failed for %s: %v", vmAlloc.ID, err)
		_, _ = s.vm.Delete(context.Background(), vmAlloc.ID)
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, AllocateResponse{
		AllocationID:     vmAlloc.ID,
		Namespace:        vmAlloc.Namespace,
		VMName:           vmAlloc.VMName,
		NodeName:         netAlloc.NodeName,
		Site:             vmAlloc.Site,
		PodCIDR:          netAlloc.PodCIDR,
		ExpiresAt:        vmAlloc.ExpiresAt,
		MACAddress:       vmAlloc.MAC,
		Lease:            DHCPLease{IP: leaseIP(netAlloc.PodCIDR), Subnet: netAlloc.PodCIDR, Router: routerIP(netAlloc.PodCIDR), DNS: routerIP(netAlloc.PodCIDR)},
		Redfish:          RedfishAccess{URL: s.vm.RedfishURL(vmAlloc.ID), Username: vmAlloc.Username, Password: vmAlloc.Password, DeviceID: vmAlloc.ID},
		Tunnel:           tunnelInfoFromNetwork(netAlloc.Tunnel),
		L2Tunnel:         l2TunnelInfoFromKubeVirt(vmAlloc.L2Tunnel),
		RequiresEndpoint: netAlloc.RequiresEndpoint,
		GatewayPeers:     gatewayPeersFromNetwork(netAlloc.GatewayPeers),
	})
}

func (s *Server) handleDeallocate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizedAggregatedRequest(w, r, "create", "vms/deallocate") {
		return
	}

	var req DeallocateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.AllocationID == "" {
		writeErrorMessage(w, http.StatusBadRequest, "allocationID is required")
		return
	}

	vmDeleted, vmErr := s.vm.Delete(r.Context(), req.AllocationID)
	netDeleted, netErr := s.network.Delete(r.Context(), req.AllocationID)
	if err := errors.Join(vmErr, netErr); err != nil {
		klog.Errorf("playpen deallocate failed for %s: %v", req.AllocationID, err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, DeallocateResponse{AllocationID: req.AllocationID, Deleted: vmDeleted || netDeleted})
}

func (s *Server) handleRedfishRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/redfish/v1" {
		http.NotFound(w, r)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"Systems": map[string]string{"@odata.id": "/redfish/v1/Systems"}})
}

func (s *Server) handleRedfish(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/redfish/v1/")
	if path == "" {
		s.handleRedfishRoot(w, r)
		return
	}
	if path == "Systems" {
		s.handleRedfishSystems(w, r)
		return
	}
	if path == "SessionService/Sessions" {
		s.handleRedfishSessionCreate(w, r)
		return
	}

	parts := strings.Split(path, "/")
	if len(parts) == 3 && parts[0] == "SessionService" && parts[1] == "Sessions" {
		s.handleRedfishSessionDelete(w, r, parts[2])
		return
	}
	if len(parts) < 2 || parts[0] != "Systems" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}

	allocationID := parts[1]
	if _, ok := s.authorizedRedfish(w, r, allocationID); !ok {
		return
	}

	if len(parts) == 2 {
		s.handleRedfishSystem(w, r, allocationID)
		return
	}
	if len(parts) == 4 && parts[2] == "Actions" && parts[3] == "ComputerSystem.Reset" {
		s.handleRedfishReset(w, r, allocationID)
		return
	}

	http.NotFound(w, r)
}

func (s *Server) handleRedfishSessionCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		UserName string `json:"UserName"`
		Password string `json:"Password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.UserName == "" {
		req.UserName, req.Password, _ = r.BasicAuth()
	}

	allocationID, err := s.vm.AllocationIDForRedfishCredentials(r.Context(), req.UserName, req.Password)
	if err != nil {
		writeRedfishError(w, err)
		return
	}

	token := uuid.NewString()
	s.sessionMu.Lock()
	s.redfishSessions[token] = allocationID
	s.sessionMu.Unlock()

	location := "/redfish/v1/SessionService/Sessions/" + token
	w.Header().Set("X-Auth-Token", token)
	w.Header().Set("Location", location)
	writeJSON(w, http.StatusCreated, map[string]any{"Id": token, "Name": "playpen redfish session"})
}

func (s *Server) handleRedfishSessionDelete(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Auth-Token") != token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.sessionMu.RLock()
	_, ok := s.redfishSessions[token]
	s.sessionMu.RUnlock()
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	s.sessionMu.Lock()
	delete(s.redfishSessions, token)
	s.sessionMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRedfishSystems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	allocationID, ok := s.authorizedRedfish(w, r, "")
	if !ok {
		return
	}
	members := []map[string]string{{"@odata.id": "/redfish/v1/Systems/" + allocationID}}

	writeJSON(w, http.StatusOK, map[string]any{"Members": members, "Members@odata.count": len(members)})
}

func (s *Server) handleRedfishSystem(w http.ResponseWriter, r *http.Request, allocationID string) {
	switch r.Method {
	case http.MethodGet:
		power, err := s.vm.PowerState(r.Context(), allocationID)
		if err != nil {
			writeRedfishError(w, err)
			return
		}

		boot, err := s.vm.BootConfig(r.Context(), allocationID)
		if err != nil {
			writeRedfishError(w, err)
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"Id":         allocationID,
			"Name":       allocationID,
			"PowerState": power,
			"Boot": map[string]string{
				"BootSourceOverrideTarget":  boot.Target,
				"BootSourceOverrideEnabled": boot.Enabled,
				"BootSourceOverrideMode":    boot.Mode,
				"HttpBootUri":               boot.HTTPBootURI,
			},
		})
	case http.MethodPatch:
		var req struct {
			Boot map[string]string `json:"Boot"`
		}
		if !decodeBody(w, r, &req) {
			return
		}

		cfg := kubevirt.BootConfig{
			Target:      req.Boot["BootSourceOverrideTarget"],
			Enabled:     req.Boot["BootSourceOverrideEnabled"],
			Mode:        req.Boot["BootSourceOverrideMode"],
			HTTPBootURI: req.Boot["HttpBootUri"],
		}
		if err := s.vm.SetBootConfig(r.Context(), allocationID, cfg); err != nil {
			writeRedfishError(w, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleRedfishReset(w http.ResponseWriter, r *http.Request, allocationID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ResetType string `json:"ResetType"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	switch req.ResetType {
	case "ForceOff":
		if err := s.vm.SetPower(r.Context(), allocationID, false); err != nil {
			writeRedfishError(w, err)
			return
		}
	case "On":
		if err := s.vm.SetPower(r.Context(), allocationID, true); err != nil {
			writeRedfishError(w, err)
			return
		}
	default:
		writeErrorMessage(w, http.StatusBadRequest, "unsupported ResetType")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) trustedAggregatedRequest(w http.ResponseWriter, r *http.Request) bool {
	if !s.webhook.IsTrustedAggregatedRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}

	return true
}

func (s *Server) authorizedAggregatedRequest(w http.ResponseWriter, r *http.Request, verb, resource string) bool {
	if !s.trustedAggregatedRequest(w, r) {
		return false
	}

	remoteUser := strings.TrimSpace(r.Header.Get("X-Remote-User"))
	if remoteUser == "" {
		writeErrorMessage(w, http.StatusBadRequest, "missing X-Remote-User header")
		return false
	}

	if !s.performSAR(r.Context(), remoteUser, r.Header.Values("X-Remote-Group"), verb, resource) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}

	return true
}

func (s *Server) performSAR(ctx context.Context, username string, groups []string, verb, resource string) bool {
	review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:   username,
		Groups: groups,
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Verb:     verb,
			Group:    GroupName,
			Resource: resource,
		},
	}}

	sarCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result, err := s.kube.AuthorizationV1().SubjectAccessReviews().Create(sarCtx, review, metav1.CreateOptions{})
	if err != nil {
		klog.V(2).Infof("playpen SAR failed for user %q verb=%s resource=%s: %v", username, verb, resource, err)
		return false
	}

	return result.Status.Allowed && !result.Status.Denied
}

func (s *Server) authorizedRedfish(w http.ResponseWriter, r *http.Request, allocationID string) (string, bool) {
	if token := r.Header.Get("X-Auth-Token"); token != "" {
		s.sessionMu.RLock()
		sessionAllocationID := s.redfishSessions[token]
		s.sessionMu.RUnlock()
		if sessionAllocationID != "" && (allocationID == "" || sessionAllocationID == allocationID) {
			return sessionAllocationID, true
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}

	user, pass, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="playpen"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	if allocationID == "" {
		matchedAllocationID, err := s.vm.AllocationIDForRedfishCredentials(r.Context(), user, pass)
		if err != nil {
			writeRedfishError(w, err)
			return "", false
		}

		return matchedAllocationID, true
	}

	expectedUser, expectedPass, err := s.vm.RedfishCredentials(r.Context(), allocationID)
	if err != nil {
		writeRedfishError(w, err)
		return "", false
	}
	if user != expectedUser || pass != expectedPass {
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}

	return allocationID, true
}

func (s *Server) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().UTC()
			if err := s.vm.DeleteExpired(ctx, now); err != nil {
				klog.Warningf("playpen VM cleanup failed: %v", err)
			}
			if err := s.network.DeleteExpired(ctx, now); err != nil {
				klog.Warningf("playpen network cleanup failed: %v", err)
			}
		}
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErrorMessage(w, http.StatusBadRequest, "failed to read request body")
		return false
	}
	defer func() { _ = r.Body.Close() }() //nolint:errcheck

	if len(body) == 0 {
		body = []byte("{}")
	}
	if err := json.Unmarshal(body, target); err != nil {
		writeErrorMessage(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}

	return true
}

func leaseIP(podCIDR string) string {
	ip, _, err := net.ParseCIDR(podCIDR)
	if err != nil || ip.To4() == nil {
		return ""
	}

	addr := ip.To4()
	addr[3] = 10

	return addr.String()
}

func routerIP(podCIDR string) string {
	ip, _, err := net.ParseCIDR(podCIDR)
	if err != nil || ip.To4() == nil {
		return ""
	}

	addr := ip.To4()
	addr[3] = 1

	return addr.String()
}

func tunnelInfoFromNetwork(tunnel network.TunnelInfo) TunnelInfo {
	return TunnelInfo{
		Mode:               tunnel.Mode,
		WireGuardAddress:   tunnel.WireGuardAddress,
		WireGuardPublicKey: tunnel.WireGuardPublicKey,
		VXLANVNI:           tunnel.VXLANVNI,
		VXLANPort:          tunnel.VXLANPort,
		EndpointRequired:   tunnel.EndpointRequired,
	}
}

func l2TunnelInfoFromKubeVirt(tunnel kubevirt.L2TunnelConfig) L2TunnelInfo {
	return L2TunnelInfo{
		Enabled:               tunnel.Enabled,
		Mode:                  tunnel.Mode,
		NetworkAttachmentName: tunnel.NetworkAttachmentName,
		EndpointNamespace:     tunnel.EndpointNamespace,
		EndpointPodName:       tunnel.EndpointPodName,
		EndpointUnderlayIP:    tunnel.EndpointUnderlayIP,
		ClientUnderlayIP:      tunnel.ClientUnderlayIP,
		VXLANVNI:              tunnel.VXLANVNI,
		VXLANPort:             tunnel.VXLANPort,
		BridgeInterface:       tunnel.BridgeInterface,
		AttachInterface:       tunnel.AttachInterface,
		VXLANInterface:        tunnel.VXLANInterface,
	}
}

func gatewayPeersFromNetwork(peers []network.GatewayPeer) []GatewayPeer {
	if len(peers) == 0 {
		return nil
	}

	out := make([]GatewayPeer, 0, len(peers))
	for _, peer := range peers {
		out = append(out, GatewayPeer{
			Name:               peer.Name,
			Site:               peer.Site,
			WireGuardPublicKey: peer.WireGuardPublicKey,
			InternalIPs:        peer.InternalIPs,
			Endpoints:          peer.Endpoints,
			PodCIDRs:           peer.PodCIDRs,
			RoutedCIDRs:        peer.RoutedCIDRs,
		})
	}

	return out
}

func writeRedfishError(w http.ResponseWriter, err error) {
	if apierrors.IsUnauthorized(err) {
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	if apierrors.IsNotFound(err) {
		writeError(w, http.StatusNotFound, err)
		return
	}

	writeError(w, http.StatusInternalServerError, err)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeErrorMessage(w, status, err.Error())
}

func writeErrorMessage(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, ErrorResponse{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		klog.V(4).Infof("playpen response write failed: %v", err)
	}
}
