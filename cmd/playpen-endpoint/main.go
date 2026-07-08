// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var virtualMachinesGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}

type config struct {
	Namespace          string
	VMName             string
	DeviceID           string
	Username           string
	PasswordFile       string
	WGPrivateKeyFile   string
	WGAddress          string
	WGPeerPublicKey    string
	WGPeerAddress      string
	WGListenPort       int
	VXLANVNI           int
	VXLANPort          int
	VXLANLocal         string
	VXLANRemote        string
	RedfishAddr        string
	TLSCert            string
	TLSKey             string
	PXEInterface       string
	WireGuardInterface string
	VXLANInterface     string
}

type redfishServer struct {
	cfg        config
	password   string
	dynamic    dynamic.Interface
	restConfig *rest.Config
	httpClient *http.Client
	sessions   map[string]struct{}
	boot       bootConfig
	mu         sync.Mutex
}

type bootConfig struct {
	Target      string
	Enabled     string
	HTTPBootURI string
}

func main() {
	ctx := context.Background()
	cfg, err := loadConfig()
	if err != nil {
		fatal(err)
	}

	if err := setupNetwork(ctx, cfg); err != nil {
		fatal(err)
	}

	password, err := os.ReadFile(cfg.PasswordFile)
	if err != nil {
		fatal(err)
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		fatal(err)
	}

	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		fatal(err)
	}

	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		fatal(err)
	}

	h := &redfishServer{
		cfg:        cfg,
		password:   strings.TrimSpace(string(password)),
		dynamic:    dyn,
		restConfig: restConfig,
		httpClient: httpClient,
		sessions:   map[string]struct{}{},
		boot:       bootConfig{Target: "Pxe", Enabled: "Continuous"},
	}

	server := &http.Server{
		Addr:              cfg.RedfishAddr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}

	fatal(server.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey))
}

func loadConfig() (config, error) {
	cfg := config{
		Namespace:          env("PLAYPEN_NAMESPACE", "playpen"),
		VMName:             os.Getenv("PLAYPEN_VM_NAME"),
		DeviceID:           env("PLAYPEN_DEVICE_ID", "1"),
		Username:           env("PLAYPEN_REDFISH_USERNAME", "playpen"),
		PasswordFile:       os.Getenv("PLAYPEN_REDFISH_PASSWORD_FILE"),
		WGPrivateKeyFile:   os.Getenv("PLAYPEN_WG_PRIVATE_KEY_FILE"),
		WGAddress:          os.Getenv("PLAYPEN_WG_ADDRESS"),
		WGPeerPublicKey:    os.Getenv("PLAYPEN_WG_PEER_PUBLIC_KEY"),
		WGPeerAddress:      os.Getenv("PLAYPEN_WG_PEER_ADDRESS"),
		WGListenPort:       envInt("PLAYPEN_WG_LISTEN_PORT", 51820),
		VXLANVNI:           envInt("PLAYPEN_VXLAN_VNI", 12001),
		VXLANPort:          envInt("PLAYPEN_VXLAN_PORT", 4789),
		VXLANLocal:         os.Getenv("PLAYPEN_VXLAN_LOCAL"),
		VXLANRemote:        os.Getenv("PLAYPEN_VXLAN_REMOTE"),
		RedfishAddr:        os.Getenv("PLAYPEN_REDFISH_ADDR"),
		TLSCert:            os.Getenv("PLAYPEN_TLS_CERT"),
		TLSKey:             os.Getenv("PLAYPEN_TLS_KEY"),
		PXEInterface:       env("PLAYPEN_PXE_INTERFACE", "pxe0"),
		WireGuardInterface: env("PLAYPEN_WG_INTERFACE", "wg0"),
		VXLANInterface:     env("PLAYPEN_VXLAN_INTERFACE", "vxlan0"),
	}

	for name, value := range map[string]string{
		"PLAYPEN_VM_NAME":               cfg.VMName,
		"PLAYPEN_REDFISH_PASSWORD_FILE": cfg.PasswordFile,
		"PLAYPEN_WG_PRIVATE_KEY_FILE":   cfg.WGPrivateKeyFile,
		"PLAYPEN_WG_ADDRESS":            cfg.WGAddress,
		"PLAYPEN_WG_PEER_PUBLIC_KEY":    cfg.WGPeerPublicKey,
		"PLAYPEN_WG_PEER_ADDRESS":       cfg.WGPeerAddress,
		"PLAYPEN_VXLAN_LOCAL":           cfg.VXLANLocal,
		"PLAYPEN_VXLAN_REMOTE":          cfg.VXLANRemote,
		"PLAYPEN_REDFISH_ADDR":          cfg.RedfishAddr,
		"PLAYPEN_TLS_CERT":              cfg.TLSCert,
		"PLAYPEN_TLS_KEY":               cfg.TLSKey,
	} {
		if strings.TrimSpace(value) == "" {
			return config{}, fmt.Errorf("%s is required", name)
		}
	}

	return cfg, nil
}

func setupNetwork(ctx context.Context, cfg config) error {
	commands := [][]string{
		{"ip", "link", "set", cfg.PXEInterface, "up"},
		{"ip", "link", "set", cfg.PXEInterface, "promisc", "on"},
		{"ip", "link", "add", "br0", "type", "bridge"},
		{"ip", "link", "set", "br0", "up"},
		{"ip", "link", "set", cfg.PXEInterface, "master", "br0"},
		{"ip", "link", "add", cfg.WireGuardInterface, "type", "wireguard"},
		{"wg", "set", cfg.WireGuardInterface, "private-key", cfg.WGPrivateKeyFile, "listen-port", strconv.Itoa(cfg.WGListenPort), "peer", cfg.WGPeerPublicKey, "allowed-ips", cfg.WGPeerAddress},
		{"ip", "addr", "add", cfg.WGAddress, "dev", cfg.WireGuardInterface},
		{"ip", "link", "set", cfg.WireGuardInterface, "up"},
		{"ip", "route", "add", cfg.WGPeerAddress, "dev", cfg.WireGuardInterface},
		{"ip", "link", "add", cfg.VXLANInterface, "type", "vxlan", "id", strconv.Itoa(cfg.VXLANVNI), "dev", cfg.WireGuardInterface, "local", cfg.VXLANLocal, "remote", cfg.VXLANRemote, "dstport", strconv.Itoa(cfg.VXLANPort), "nolearning"},
		{"bridge", "fdb", "append", "00:00:00:00:00:00", "dev", cfg.VXLANInterface, "dst", cfg.VXLANRemote},
		{"ip", "link", "set", cfg.VXLANInterface, "up"},
		{"ip", "link", "set", cfg.VXLANInterface, "promisc", "on"},
		{"ip", "link", "set", cfg.VXLANInterface, "master", "br0"},
	}

	for _, command := range commands {
		if err := run(ctx, command[0], command[1:]...); err != nil {
			return err
		}
	}

	return nil
}

func (h *redfishServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}

	if r.Method == http.MethodGet && path == "/redfish/v1" {
		writeJSON(w, http.StatusOK, map[string]any{"@odata.id": "/redfish/v1/", "Systems": map[string]string{"@odata.id": "/redfish/v1/Systems"}})

		return
	}

	if r.Method == http.MethodPost && path == "/redfish/v1/SessionService/Sessions" {
		h.createSession(w, r)

		return
	}

	if r.Method == http.MethodDelete && strings.HasPrefix(path, "/redfish/v1/SessionService/Sessions/") {
		h.deleteSession(w, r)

		return
	}

	if !h.authenticated(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	switch {
	case r.Method == http.MethodGet && path == "/redfish/v1/Systems":
		writeJSON(w, http.StatusOK, map[string]any{"Members": []map[string]string{{"@odata.id": "/redfish/v1/Systems/" + h.cfg.DeviceID}}})
	case r.Method == http.MethodGet && path == "/redfish/v1/Systems/"+h.cfg.DeviceID:
		h.getSystem(w, r)
	case r.Method == http.MethodPatch && path == "/redfish/v1/Systems/"+h.cfg.DeviceID:
		h.patchSystem(w, r)
	case r.Method == http.MethodPost && path == "/redfish/v1/Systems/"+h.cfg.DeviceID+"/Actions/ComputerSystem.Reset":
		h.reset(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *redfishServer) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserName string `json:"UserName"`
		Password string `json:"Password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	if !h.credentialsMatch(body.UserName, body.Password) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	token, err := randomToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	h.mu.Lock()
	h.sessions[token] = struct{}{}
	h.mu.Unlock()
	w.Header().Set("X-Auth-Token", token)
	w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/"+token)
	writeJSON(w, http.StatusCreated, map[string]string{"Id": token})
}

func (h *redfishServer) deleteSession(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(strings.TrimRight(r.URL.Path, "/"), "/redfish/v1/SessionService/Sessions/")
	h.mu.Lock()
	delete(h.sessions, token)
	h.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (h *redfishServer) authenticated(r *http.Request) bool {
	if token := r.Header.Get("X-Auth-Token"); token != "" {
		h.mu.Lock()
		_, ok := h.sessions[token]
		h.mu.Unlock()
		if ok {
			return true
		}
	}

	user, pass, ok := r.BasicAuth()

	return ok && h.credentialsMatch(user, pass)
}

func (h *redfishServer) credentialsMatch(user, pass string) bool {
	return subtle.ConstantTimeCompare([]byte(user), []byte(h.cfg.Username)) == 1 && subtle.ConstantTimeCompare([]byte(pass), []byte(h.password)) == 1
}

func (h *redfishServer) getSystem(w http.ResponseWriter, r *http.Request) {
	powerState := "Off"
	vm, err := h.vm(r.Context())
	if err == nil {
		if created, _, _ := unstructured.NestedBool(vm.Object, "status", "created"); created {
			powerState = "On"
		}
	}

	h.mu.Lock()
	boot := h.boot
	h.mu.Unlock()

	bootData := map[string]any{"BootSourceOverrideTarget": boot.Target, "BootSourceOverrideEnabled": boot.Enabled}
	if boot.HTTPBootURI != "" {
		bootData["HttpBootUri"] = boot.HTTPBootURI
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.id":   "/redfish/v1/Systems/" + h.cfg.DeviceID,
		"@odata.type": "#ComputerSystem.v1_20_0.ComputerSystem",
		"Id":          h.cfg.DeviceID,
		"Name":        "Playpen KubeVirt VM",
		"PowerState":  powerState,
		"Boot":        bootData,
	})
}

func (h *redfishServer) patchSystem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Boot struct {
			BootSourceOverrideTarget  string `json:"BootSourceOverrideTarget"`
			BootSourceOverrideEnabled string `json:"BootSourceOverrideEnabled"`
			HttpBootUri               string `json:"HttpBootUri"`
		} `json:"Boot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	h.mu.Lock()
	if body.Boot.BootSourceOverrideTarget != "" {
		h.boot.Target = body.Boot.BootSourceOverrideTarget
	}

	if body.Boot.BootSourceOverrideEnabled != "" {
		h.boot.Enabled = body.Boot.BootSourceOverrideEnabled
	}

	if body.Boot.HttpBootUri != "" {
		h.boot.HTTPBootURI = body.Boot.HttpBootUri
	}
	boot := h.boot
	h.mu.Unlock()

	if err := h.applyBootOrder(r.Context(), boot.Target); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *redfishServer) reset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ResetType string `json:"ResetType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	var err error
	switch body.ResetType {
	case "On":
		err = h.subresource(r.Context(), "start", nil)
	case "ForceOff", "GracefulShutdown":
		err = h.subresource(r.Context(), "stop", map[string]any{"gracePeriod": 0})
	case "ForceRestart", "GracefulRestart", "PowerCycle":
		err = h.subresource(r.Context(), "restart", map[string]any{"gracePeriodSeconds": 0})
	default:
		err = fmt.Errorf("unsupported ResetType %q", body.ResetType)
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *redfishServer) applyBootOrder(ctx context.Context, target string) error {
	nicOrder, diskOrder := 1, 2
	if target == "Hdd" {
		nicOrder, diskOrder = 2, 1
	}

	patch := []map[string]any{
		{"op": "replace", "path": "/spec/template/spec/domain/devices/interfaces/1/bootOrder", "value": nicOrder},
		{"op": "replace", "path": "/spec/template/spec/domain/devices/disks/0/bootOrder", "value": diskOrder},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	_, err = h.dynamic.Resource(virtualMachinesGVR).Namespace(h.cfg.Namespace).Patch(ctx, h.cfg.VMName, types.JSONPatchType, data, metav1.PatchOptions{})

	return err
}

func (h *redfishServer) vm(ctx context.Context) (*unstructured.Unstructured, error) {
	return h.dynamic.Resource(virtualMachinesGVR).Namespace(h.cfg.Namespace).Get(ctx, h.cfg.VMName, metav1.GetOptions{})
}

func (h *redfishServer) subresource(ctx context.Context, name string, body map[string]any) error {
	var reader io.Reader = http.NoBody
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}

		reader = bytes.NewReader(data)
	}

	url := strings.TrimRight(h.restConfig.Host, "/") + "/apis/subresources.kubevirt.io/v1/namespaces/" + h.cfg.Namespace + "/virtualmachines/" + h.cfg.VMName + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, reader)
	if err != nil {
		return err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	if resp.StatusCode == http.StatusConflict && name == "start" {
		if _, err := h.vm(ctx); err == nil || !errors.IsNotFound(err) {
			return nil
		}
	}

	data, _ := io.ReadAll(resp.Body) //nolint:errcheck

	return fmt.Errorf("kubevirt %s returned %d: %s", name, resp.StatusCode, strings.TrimSpace(string(data)))
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) > 0 {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}

	return err
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}

	return value
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}

	return hex.EncodeToString(b[:]), nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value) //nolint:errcheck
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
