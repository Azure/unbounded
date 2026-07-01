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
)

type RuntimeState struct {
	mu                       sync.RWMutex
	ready                    bool
	serverWireGuardPublicKey string
}

func NewRuntimeState(serverWireGuardPublicKey string, ready bool) *RuntimeState {
	return &RuntimeState{serverWireGuardPublicKey: serverWireGuardPublicKey, ready: ready}
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

func (s *RuntimeState) ServerWireGuardPublicKey() string {
	if s == nil {
		return ""
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.serverWireGuardPublicKey
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.Redfish.DeviceID == "" {
		cfg.Redfish.DeviceID = "1"
	}

	cmd := OSCommander{}
	serverPublicKey := ""
	if cfg.ConfigureNetwork {
		var err error
		serverPublicKey, err = ensureWireGuardPrivateKey(cfg.WireGuard.PrivateKeyFile)
		if err != nil {
			return err
		}
	}

	state := NewRuntimeState(serverPublicKey, !cfg.ConfigureNetwork || cfg.WireGuard.ClientPublicKey != "")
	network := NewNetworkManager(cmd, cfg)
	if err := network.Setup(ctx); err != nil {
		return err
	}
	defer network.Teardown(context.Background()) //nolint:errcheck // Pod teardown also removes the network namespace.

	vm := NewVMManager(cmd, cfg)
	defer vm.Stop() //nolint:errcheck // Best effort on process exit.

	server, err := NewServer(cfg, vm, state)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServeTLS(filepath.Join(cfg.DataDir, "tls.crt"), filepath.Join(cfg.DataDir, "tls.key"))
	}()
	if cfg.ConfigureNetwork && cfg.WireGuard.ClientPublicKey == "" && cfg.WireGuard.ClientPublicKeyFile != "" {
		go waitForClientPublicKey(ctx, cfg.WireGuard.ClientPublicKeyFile, network, state)
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)

		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}

		return err
	}
}

func NewServer(cfg Config, vm *VMManager, state *RuntimeState) (*http.Server, error) {
	if err := ensureTLSCert(cfg); err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/redfish/v1/", NewRedfishHandler(vm, cfg.Redfish))
	mux.HandleFunc("/playpen/v1/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, infoResponse(cfg, state.ServerWireGuardPublicKey()))
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

func infoResponse(cfg Config, serverPublicKey string) map[string]any {
	redfishURL := cfg.PublicRedfishURL
	if redfishURL == "" {
		redfishURL = "https://" + cfg.ListenAddr
	}

	serverWG, _ := addressIP(cfg.WireGuard.ServerAddress)
	clientWG, _ := addressIP(cfg.WireGuard.ClientAddress)

	return map[string]any{
		"wireGuard": map[string]any{
			"interface":       cfg.WireGuard.Interface,
			"serverPublicKey": serverPublicKey,
			"serverAddress":   cfg.WireGuard.ServerAddress,
			"clientAddress":   cfg.WireGuard.ClientAddress,
			"listenPort":      cfg.WireGuard.ListenPort,
		},
		"vxlan": map[string]any{
			"interface":     cfg.VXLAN.Interface,
			"vni":           cfg.VXLAN.VNI,
			"udpPort":       cfg.VXLAN.Port,
			"serverAddress": serverWG.String(),
			"clientAddress": clientWG.String(),
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
			"deviceID": cfg.Redfish.DeviceID,
		},
	}
}

func waitForClientPublicKey(ctx context.Context, path string, network *NetworkManager, state *RuntimeState) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		if clientPublicKey, err := readClientPublicKey(path); err == nil && clientPublicKey != "" {
			if err := network.ConfigurePeer(ctx, clientPublicKey); err == nil {
				state.SetReady()

				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func readClientPublicKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(data)), nil
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
		NotAfter:              notBefore.Add(24 * time.Hour),
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
		certFile.Close() //nolint:errcheck // Best effort after encode failure.
		return err
	}

	if err := certFile.Close(); err != nil {
		return err
	}

	keyFile, err := os.OpenFile(keyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	if err := pem.Encode(keyFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		keyFile.Close() //nolint:errcheck // Best effort after encode failure.
		return err
	}

	return keyFile.Close()
}

func certHosts(cfg Config) []string {
	hosts := []string{"localhost", "127.0.0.1"}
	if host, _, err := net.SplitHostPort(cfg.ListenAddr); err == nil {
		hosts = append(hosts, strings.Trim(host, "[]"))
	}

	if addr, err := addressIP(cfg.WireGuard.ServerAddress); err == nil {
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
