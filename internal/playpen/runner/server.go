// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runner

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/playpen/meta"
)

type RuntimeState struct {
	mu    sync.RWMutex
	ready bool
}

func NewRuntimeState(ready bool) *RuntimeState {
	return &RuntimeState{ready: ready}
}

func (s *RuntimeState) Ready() bool {
	if s == nil {
		return true
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.ready
}

func (s *RuntimeState) SetReady() {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.ready = true
}

func Run(ctx context.Context, cfg Config) error {
	if err := cfg.ApplyArchitectureDefaults(); err != nil {
		return err
	}

	if cfg.Redfish.DeviceID == "" {
		cfg.Redfish.DeviceID = "1"
	}

	if cfg.ConfigureNetwork {
		if err := ensureTLSCert(cfg); err != nil {
			return err
		}

		certPEM, err := readTLSCertPEM(cfg)
		if err != nil {
			return err
		}

		go publishRunnerMetadataUntilReady(ctx, cfg, certPEM)
	}

	cmd := OSCommander{}
	state := NewRuntimeState(!cfg.ConfigureNetwork)
	network := NewNetworkManager(cmd, cfg)
	if cfg.ConfigureNetwork {
		defer network.Teardown(context.Background()) //nolint:errcheck // Pod teardown also removes the network namespace.
	}

	vm := NewVMManager(cmd, cfg)
	defer vm.Stop() //nolint:errcheck // Best effort on process exit.

	server, err := NewServer(cfg, vm, state)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)

	if cfg.ConfigureNetwork {
		go func() {
			if err := configureClaimedNetwork(ctx, cfg, network, state); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- err
			}
		}()
	}

	go func() {
		errCh <- server.ListenAndServeTLS(filepath.Join(cfg.DataDir, "tls.crt"), filepath.Join(cfg.DataDir, "tls.key"))
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		server.Shutdown(shutdownCtx) //nolint:errcheck // Process is exiting after context cancellation.

		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}

		return err
	}
}

func configureClaimedNetwork(ctx context.Context, cfg Config, network *NetworkManager, state *RuntimeState) error {
	podIP, err := addressIP(cfg.PodIP)
	if err != nil {
		return fmt.Errorf("parse pod IP: %w", err)
	}

	remoteAddress, err := waitVXLANRemoteAddress(ctx, cfg)
	if err != nil {
		return err
	}

	if err := network.Setup(ctx, podIP.String(), remoteAddress); err != nil {
		return err
	}

	if err := markRunnerNetworkReady(ctx, cfg); err != nil {
		return err
	}

	state.SetReady()

	return nil
}

func NewServer(cfg Config, vm *VMManager, state *RuntimeState) (*http.Server, error) {
	if err := ensureTLSCert(cfg); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/redfish/v1/", NewRedfishHandler(vm, cfg.Redfish, cfg.DataDir))
	mux.HandleFunc("/playpen/v1/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, infoResponse(cfg))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !state.Ready() {
			http.Error(w, "runner has not been claimed", http.StatusServiceUnavailable)

			return
		}

		w.WriteHeader(http.StatusNoContent)
	})

	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}

func infoResponse(cfg Config) map[string]any {
	redfishURL := cfg.PublicRedfishURL
	if redfishURL == "" {
		redfishURL = defaultPublicRedfishURL(cfg)
	}

	serverAddress := ""
	serverAddr, err := addressIP(cfg.PodIP)
	if err != nil {
		serverAddress = strings.TrimSpace(cfg.PodIP)
	} else {
		serverAddress = serverAddr.String()
	}

	certPEM, _ := readTLSCertPEM(cfg) //nolint:errcheck // Optional metadata for clients using trusted cluster TLS.

	return map[string]any{
		"architecture": cfg.Architecture,
		"vxlan": map[string]any{
			"interface":     cfg.VXLAN.Interface,
			"device":        VXLANDevice,
			"vni":           cfg.VXLAN.VNI,
			"udpPort":       cfg.VXLAN.Port,
			"serverAddress": serverAddress,
			"clientAddress": "",
		},
		"network": map[string]any{
			"guestMAC":    cfg.Guest.MAC,
			"guestIPv4":   cfg.Guest.IPv4,
			"subnetMask":  cfg.Guest.SubnetMask,
			"gatewayIPv4": cfg.Guest.Gateway,
			"dns":         cfg.Guest.DNS,
		},
		"redfish": map[string]string{
			"url":      redfishURL,
			"username": cfg.Redfish.Username,
			"password": cfg.Redfish.Password,
			"certPEM":  certPEM,
			"deviceID": cfg.Redfish.DeviceID,
		},
	}
}

func waitVXLANRemoteAddress(ctx context.Context, cfg Config) (string, error) {
	if cfg.KubernetesClient == nil || cfg.PodName == "" || cfg.PodNamespace == "" {
		return "", fmt.Errorf("pod name, namespace, and Kubernetes client are required to wait for VXLAN metadata")
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		pod := &corev1.Pod{}
		key := types.NamespacedName{Namespace: cfg.PodNamespace, Name: cfg.PodName}
		if err := cfg.KubernetesClient.Get(ctx, key, pod); err != nil {
			return "", err
		}

		remoteAddress := strings.TrimSpace(pod.Annotations[meta.AnnotationVXLANRemoteAddress])
		if remoteAddress != "" {
			if _, err := addressIP(remoteAddress); err != nil {
				return "", fmt.Errorf("parse VXLAN remote address %q: %w", remoteAddress, err)
			}

			return remoteAddress, nil
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func markRunnerNetworkReady(ctx context.Context, cfg Config) error {
	if cfg.KubernetesClient == nil || cfg.PodName == "" || cfg.PodNamespace == "" {
		return fmt.Errorf("pod name, namespace, and Kubernetes client are required to publish runner network readiness")
	}

	pod := &corev1.Pod{}
	key := types.NamespacedName{Namespace: cfg.PodNamespace, Name: cfg.PodName}
	if err := cfg.KubernetesClient.Get(ctx, key, pod); err != nil {
		return err
	}

	base := pod.DeepCopy()
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}

	pod.Annotations[meta.AnnotationRunnerNetworkReady] = "true"

	return cfg.KubernetesClient.Patch(ctx, pod, client.MergeFrom(base))
}

func publishRunnerMetadata(ctx context.Context, cfg Config, redfishCertPEM string) error {
	if cfg.KubernetesClient == nil || cfg.PodName == "" || cfg.PodNamespace == "" {
		return fmt.Errorf("pod name, namespace, and Kubernetes client are required to publish runner metadata")
	}

	pod := &corev1.Pod{}

	key := types.NamespacedName{Namespace: cfg.PodNamespace, Name: cfg.PodName}
	if err := cfg.KubernetesClient.Get(ctx, key, pod); err != nil {
		return err
	}

	base := pod.DeepCopy()
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}

	pod.Annotations[meta.AnnotationRedfishCertPEM] = redfishCertPEM

	return cfg.KubernetesClient.Patch(ctx, pod, client.MergeFrom(base))
}

func publishRunnerMetadataUntilReady(ctx context.Context, cfg Config, redfishCertPEM string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		if err := publishRunnerMetadata(ctx, cfg, redfishCertPEM); err == nil {
			return
		} else {
			slog.Warn("publish runner metadata", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func ensureTLSCert(cfg Config) error {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}

	certPath := filepath.Join(cfg.DataDir, "tls.crt")

	keyPath := filepath.Join(cfg.DataDir, "tls.key")
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return nil
		}
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	notBefore := time.Now().Add(-time.Minute)
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(notBefore.UnixNano()),
		Subject: pkix.Name{
			CommonName: "playpen-runner",
		},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(30 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	for _, host := range certHosts(cfg) {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if host != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}

	certFile, err := os.OpenFile(certPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return errors.Join(err, certFile.Close())
	}

	if err := certFile.Close(); err != nil {
		return err
	}

	keyFile, err := os.OpenFile(keyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	if err := pem.Encode(keyFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		return errors.Join(err, keyFile.Close())
	}

	return keyFile.Close()
}

func readTLSCertPEM(cfg Config) (string, error) {
	data, err := os.ReadFile(filepath.Join(cfg.DataDir, "tls.crt"))
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func certHosts(cfg Config) []string {
	hosts := []string{"localhost", "127.0.0.1"}
	if host, _, err := net.SplitHostPort(cfg.ListenAddr); err == nil {
		hosts = append(hosts, strings.Trim(host, "[]"))
	}

	if addr, err := addressIP(cfg.PodIP); err == nil {
		hosts = append(hosts, addr.String())
	}

	if cfg.PublicRedfishURL != "" {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(cfg.PublicRedfishURL, "https://"), "http://")
		if host, _, err := net.SplitHostPort(trimmed); err == nil {
			hosts = append(hosts, host)
		} else if i := strings.Index(trimmed, "/"); i >= 0 {
			hosts = append(hosts, trimmed[:i])
		} else {
			hosts = append(hosts, trimmed)
		}
	}

	return hosts
}

func defaultPublicRedfishURL(cfg Config) string {
	host, err := addressIP(cfg.PodIP)
	if err != nil {
		return "https://" + cfg.ListenAddr
	}

	_, port, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil || port == "" {
		port = "8443"
	}

	return "https://" + net.JoinHostPort(host.String(), port)
}
