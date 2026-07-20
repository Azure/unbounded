// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// vmPower is the subset of the guest lifecycle the Redfish server drives. The
// in-pod vmManager satisfies it; tests supply a fake.
type vmPower interface {
	PowerState(ctx context.Context) (string, error)
	Reset(ctx context.Context, rt ResetType) error
}

// bootOverride holds the Redfish boot source override state metalman sets via
// PATCH and reads back via GET. metalman's SetBootOverride, SetHTTPBootOverride,
// DisableBootOverride, and GetBootConfig operate on these fields. The client
// side reads them back (over the overlay) to steer the guest's network boot, so
// a UefiHttp override with an HttpBootUri actually drives an HTTP boot.
type bootOverride struct {
	target  string
	enabled string
	mode    string
	httpURI string
}

// defaultBootOverride is the state before any override is applied.
func defaultBootOverride() bootOverride {
	return bootOverride{target: "None", enabled: "Disabled"}
}

// redfishNICID is the single EthernetInterface member id the emulated guest
// exposes. The guest has exactly one virtio NIC (cfg.VMMAC).
const redfishNICID = "NIC.1"

// nicStaticAddress mirrors a Redfish IPv4StaticAddresses entry.
type nicStaticAddress struct {
	Address       string `json:"Address"`
	SubnetMask    string `json:"SubnetMask"`
	Gateway       string `json:"Gateway,omitempty"`
	AddressOrigin string `json:"AddressOrigin,omitempty"`
}

// nicConfig is the emulated EthernetInterface state metalman configures via
// SetStaticIPv4 (PATCH .../EthernetInterfaces/NIC.1). The client reads it back
// to program the guest's DHCP lease when driving a static-IP HTTP boot.
type nicConfig struct {
	dhcpEnabled    bool
	staticImagesV4 []nicStaticAddress
	nameServers    []string
}

// redfishServer is a minimal Redfish service that exposes the pod's guest VM as
// a single ComputerSystem so metalman can drive its power state and network
// boot. It implements the full set of endpoints metalman uses: service root,
// sessions, the Systems collection, GET/PATCH ComputerSystem (boot override),
// ComputerSystem.Reset, the EthernetInterfaces collection and member
// (GET/PATCH static IPv4), and the pending BIOS settings (GET/PATCH vendor HTTP
// boot attributes).
type redfishServer struct {
	vm       vmPower
	username string
	password string
	deviceID string
	mac      string

	mu       sync.Mutex
	sessions map[string]struct{}
	boot     bootOverride
	nic      nicConfig
	bios     map[string]any
}

// newRedfishServer builds a redfishServer for the given guest and credentials.
func newRedfishServer(cfg Config, vm vmPower) *redfishServer {
	return &redfishServer{
		vm:       vm,
		username: cfg.RedfishUsername,
		password: cfg.RedfishPassword,
		deviceID: cfg.RedfishDeviceID,
		mac:      normalizeMAC(cfg.VMMAC),
		sessions: make(map[string]struct{}),
		boot:     defaultBootOverride(),
		nic:      nicConfig{dhcpEnabled: true},
		bios:     make(map[string]any),
	}
}

func (s *redfishServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}

	// The service root is unauthenticated so a client can fetch it to pin the
	// server's TLS certificate before it holds any credentials (metalman's
	// trust-on-first-use fingerprint capture).
	if path == "/redfish/v1" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{
			"@odata.id": "/redfish/v1/",
			"Systems":   map[string]any{"@odata.id": "/redfish/v1/Systems"},
		})

		return
	}

	// Session creation is unauthenticated by definition (it establishes the
	// token from the supplied credentials).
	if path == "/redfish/v1/SessionService/Sessions" && r.Method == http.MethodPost {
		s.createSession(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/redfish/v1/SessionService/Sessions/") && r.Method == http.MethodDelete {
		s.deleteSession(w, r)
		return
	}

	if !s.authenticated(r) {
		w.Header().Set("WWW-Authenticate", "Basic realm=\"playpen\"")
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	systemPath := "/redfish/v1/Systems/" + s.deviceID

	switch {
	case path == "/redfish/v1/Systems" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"@odata.id": "/redfish/v1/Systems",
			"Members": []map[string]any{
				{"@odata.id": systemPath},
			},
			"Members@odata.count": 1,
		})
	case path == systemPath && r.Method == http.MethodGet:
		s.getSystem(w, r)
	case path == systemPath && r.Method == http.MethodPatch:
		s.patchSystem(w, r)
	case path == systemPath+"/Actions/ComputerSystem.Reset" && r.Method == http.MethodPost:
		s.reset(w, r)
	case path == systemPath+"/EthernetInterfaces" && r.Method == http.MethodGet:
		s.getEthernetInterfaces(w, r)
	case path == systemPath+"/EthernetInterfaces/"+redfishNICID && r.Method == http.MethodGet:
		s.getEthernetInterface(w, r)
	case path == systemPath+"/EthernetInterfaces/"+redfishNICID && r.Method == http.MethodPatch:
		s.patchEthernetInterface(w, r)
	case path == systemPath+"/Bios/Settings" && r.Method == http.MethodGet:
		s.getBIOSSettings(w, r)
	case path == systemPath+"/Bios/Settings" && r.Method == http.MethodPatch:
		s.patchBIOSSettings(w, r)
	default:
		http.NotFound(w, r)
	}
}

// getSystem returns the guest as a Redfish ComputerSystem with its live power
// state and current boot source override.
func (s *redfishServer) getSystem(w http.ResponseWriter, r *http.Request) {
	state, err := s.vm.PowerState(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("power state: %v", err), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	boot := s.boot
	s.mu.Unlock()

	// Always advertise HttpBootUri (empty when unset) and the EthernetInterfaces
	// link so metalman's GetBootConfig reports HasHTTPBootURI=true and drives the
	// full Redfish-native HTTP boot path (SetStaticIPv4 on the EthernetInterface
	// plus SetHTTPBootOverride) rather than falling back to the vendor-BIOS path.
	bootJSON := map[string]any{
		"BootSourceOverrideTarget":  boot.target,
		"BootSourceOverrideEnabled": boot.enabled,
		"HttpBootUri":               boot.httpURI,
	}
	if boot.mode != "" {
		bootJSON["BootSourceOverrideMode"] = boot.mode
	}

	systemPath := "/redfish/v1/Systems/" + s.deviceID

	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.id":   systemPath,
		"@odata.type": "#ComputerSystem.v1_20_0.ComputerSystem",
		"Id":          s.deviceID,
		"Name":        "playpen guest",
		"PowerState":  state,
		"Boot":        bootJSON,
		"EthernetInterfaces": map[string]any{
			"@odata.id": systemPath + "/EthernetInterfaces",
		},
		"Bios": map[string]any{
			"@odata.id": systemPath + "/Bios",
		},
	})
}

// patchSystem applies a Redfish boot source override PATCH. playpen records the
// override so metalman's SetBootOverride/SetHTTPBootOverride/DisableBootOverride
// succeed and are reflected back by a subsequent GET (GetBootConfig). When the
// override selects UefiHttp with an HttpBootUri, the client-side boot reader
// picks it up and steers the guest's network boot to that URI.
func (s *redfishServer) patchSystem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Boot *struct {
			BootSourceOverrideTarget  *string `json:"BootSourceOverrideTarget"`
			BootSourceOverrideEnabled *string `json:"BootSourceOverrideEnabled"`
			BootSourceOverrideMode    *string `json:"BootSourceOverrideMode"`
			HTTPBootURI               *string `json:"HttpBootUri"`
		} `json:"Boot"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode body: %v", err), http.StatusBadRequest)
		return
	}

	if body.Boot != nil {
		s.mu.Lock()

		if body.Boot.BootSourceOverrideTarget != nil {
			s.boot.target = *body.Boot.BootSourceOverrideTarget
		}

		if body.Boot.BootSourceOverrideEnabled != nil {
			s.boot.enabled = *body.Boot.BootSourceOverrideEnabled
		}

		if body.Boot.BootSourceOverrideMode != nil {
			s.boot.mode = *body.Boot.BootSourceOverrideMode
		}

		if body.Boot.HTTPBootURI != nil {
			s.boot.httpURI = *body.Boot.HTTPBootURI
		}

		s.mu.Unlock()
	}

	w.WriteHeader(http.StatusNoContent)
}

// reset applies a Redfish ComputerSystem.Reset action to the guest.
func (s *redfishServer) reset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ResetType ResetType `json:"ResetType"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode body: %v", err), http.StatusBadRequest)
		return
	}

	if err := s.vm.Reset(r.Context(), body.ResetType); err != nil {
		http.Error(w, fmt.Sprintf("reset: %v", err), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// getEthernetInterfaces returns the single-member EthernetInterface collection
// for the guest NIC. metalman follows the System's EthernetInterfaces link here
// to enumerate members before matching one by MAC.
func (s *redfishServer) getEthernetInterfaces(w http.ResponseWriter, _ *http.Request) {
	base := "/redfish/v1/Systems/" + s.deviceID + "/EthernetInterfaces"
	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.id":           base,
		"@odata.type":         "#EthernetInterfaceCollection.EthernetInterfaceCollection",
		"Name":                "Ethernet Interface Collection",
		"Members":             []map[string]any{{"@odata.id": base + "/" + redfishNICID}},
		"Members@odata.count": 1,
	})
}

// getEthernetInterface returns the guest NIC as a Redfish EthernetInterface,
// reporting its MAC (so metalman can match it) and current IPv4 configuration.
func (s *redfishServer) getEthernetInterface(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	nic := s.nic
	s.mu.Unlock()

	statics := make([]map[string]any, 0, len(nic.staticImagesV4))

	for _, a := range nic.staticImagesV4 {
		entry := map[string]any{"Address": a.Address, "SubnetMask": a.SubnetMask}
		if a.Gateway != "" {
			entry["Gateway"] = a.Gateway
		}

		statics = append(statics, entry)
	}

	nameServers := nic.nameServers
	if nameServers == nil {
		nameServers = []string{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.id":           "/redfish/v1/Systems/" + s.deviceID + "/EthernetInterfaces/" + redfishNICID,
		"@odata.type":         "#EthernetInterface.v1_9_0.EthernetInterface",
		"Id":                  redfishNICID,
		"Name":                "playpen guest NIC",
		"MACAddress":          s.mac,
		"PermanentMACAddress": s.mac,
		"DHCPv4":              map[string]any{"DHCPEnabled": nic.dhcpEnabled},
		"IPv4StaticAddresses": statics,
		"StaticNameServers":   nameServers,
	})
}

// patchEthernetInterface applies metalman's SetStaticIPv4: it disables DHCPv4
// and records the static IPv4 addresses and name servers. The single emulated
// NIC accepts the PATCH regardless of the requested MAC (lenient matching) so a
// harness whose lease MAC differs from the guest MAC still succeeds. The client
// side reads this back to program the guest's static boot lease.
func (s *redfishServer) patchEthernetInterface(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DHCPv4 *struct {
			DHCPEnabled *bool `json:"DHCPEnabled"`
		} `json:"DHCPv4"`
		IPv4StaticAddresses []nicStaticAddress `json:"IPv4StaticAddresses"`
		StaticNameServers   []string           `json:"StaticNameServers"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode body: %v", err), http.StatusBadRequest)
		return
	}

	s.mu.Lock()

	if body.DHCPv4 != nil && body.DHCPv4.DHCPEnabled != nil {
		s.nic.dhcpEnabled = *body.DHCPv4.DHCPEnabled
	}

	if body.IPv4StaticAddresses != nil {
		s.nic.staticImagesV4 = body.IPv4StaticAddresses
	}

	if body.StaticNameServers != nil {
		s.nic.nameServers = body.StaticNameServers
	}

	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// getBIOSSettings returns the pending BIOS settings resource. metalman reads
// Attributes.UrlBootFile from here (GetBIOSHTTPBootURI).
func (s *redfishServer) getBIOSSettings(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	attrs := make(map[string]any, len(s.bios))

	for k, v := range s.bios {
		attrs[k] = v
	}

	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"@odata.id":   "/redfish/v1/Systems/" + s.deviceID + "/Bios/Settings",
		"@odata.type": "#Bios.v1_2_0.Bios",
		"Id":          "Settings",
		"Name":        "BIOS Pending Settings",
		"Attributes":  attrs,
	})
}

// patchBIOSSettings merges metalman's vendor BIOS attribute PATCH (UrlBootFile,
// static IPv4 attributes) into the pending settings. These are the best-effort
// vendor fallback metalman also writes; playpen records them for fidelity and
// so a subsequent GetBIOSHTTPBootURI reflects the value.
func (s *redfishServer) patchBIOSSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Attributes map[string]any `json:"Attributes"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode body: %v", err), http.StatusBadRequest)
		return
	}

	if body.Attributes != nil {
		s.mu.Lock()

		for k, v := range body.Attributes {
			s.bios[k] = v
		}

		s.mu.Unlock()
	}

	w.WriteHeader(http.StatusNoContent)
}

// normalizeMAC lower-cases a MAC address for case-insensitive comparison,
// returning the input unchanged when it does not parse.
func normalizeMAC(mac string) string {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(mac))
	}

	return hw.String()
}

// createSession authenticates the supplied credentials and issues a session
// token returned in the X-Auth-Token header.
func (s *redfishServer) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserName string `json:"UserName"`
		Password string `json:"Password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode body: %v", err), http.StatusBadRequest)
		return
	}

	if !s.credentialsMatch(body.UserName, body.Password) {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	token, err := randomToken()
	if err != nil {
		http.Error(w, fmt.Sprintf("generate token: %v", err), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.sessions[token] = struct{}{}
	s.mu.Unlock()

	w.Header().Set("X-Auth-Token", token)
	w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/"+token)
	writeJSON(w, http.StatusCreated, map[string]any{
		"@odata.id": "/redfish/v1/SessionService/Sessions/" + token,
		"Id":        token,
		"Name":      "User Session",
	})
}

// deleteSession revokes a session token.
func (s *redfishServer) deleteSession(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/redfish/v1/SessionService/Sessions/")

	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// authenticated reports whether the request carries a valid session token or
// matching HTTP Basic credentials.
func (s *redfishServer) authenticated(r *http.Request) bool {
	if token := r.Header.Get("X-Auth-Token"); token != "" {
		s.mu.Lock()
		_, ok := s.sessions[token]
		s.mu.Unlock()

		if ok {
			return true
		}
	}

	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}

	return s.credentialsMatch(user, pass)
}

// credentialsMatch compares credentials in constant time.
func (s *redfishServer) credentialsMatch(user, pass string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.password)) == 1

	return userOK && passOK
}

// randomToken returns a 32-byte random hex string.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	return hex.EncodeToString(buf), nil
}

// writeJSON writes value as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(value) //nolint:errcheck // response write is best-effort
}

// startRedfishServer serves the Redfish API over HTTPS on the pod overlay
// address so the client (and, through the client's local forward, metalman) can
// reach it. It generates an in-memory self-signed certificate and logs its
// SHA-256 fingerprint so a trust-on-first-use client can pin it. The server is
// shut down when ctx is cancelled.
func startRedfishServer(ctx context.Context, cfg Config, vm vmPower) error {
	cert, fingerprint, err := selfSignedCert(cfg.OverlayRemoteIP)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(cfg.OverlayRemoteIP, fmt.Sprintf("%d", cfg.RedfishPort))

	srv := &http.Server{
		Addr:              addr,
		Handler:           newRedfishServer(cfg, vm),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		},
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen redfish %s: %w", addr, err)
	}

	fmt.Printf("redfish server listening on https://%s/redfish/v1/ (cert sha256 fingerprint %s)\n", addr, fingerprint)

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_ = srv.Shutdown(shutdownCtx) //nolint:errcheck // best-effort shutdown
	}()

	if err := srv.ServeTLS(listener, "", ""); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("serve redfish: %w", err)
	}

	return nil
}

// selfSignedCert generates an in-memory self-signed TLS certificate valid for
// localhost, 127.0.0.1, and the given overlay IP. It returns the certificate
// and the SHA-256 fingerprint of the DER-encoded leaf (colon-separated hex, the
// form metalman pins).
func selfSignedCert(overlayIP string) (tls.Certificate, string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate key: %w", err)
	}

	notBefore := time.Now().Add(-1 * time.Minute)

	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(notBefore.UnixNano()),
		Subject:               pkix.Name{CommonName: "playpen-redfish"},
		NotBefore:             notBefore,
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	hosts := []string{"localhost", "127.0.0.1"}
	if overlayIP != "" {
		hosts = append(hosts, overlayIP)
	}

	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("build keypair: %w", err)
	}

	return cert, certFingerprint(der), nil
}

// certFingerprint returns the SHA-256 fingerprint of a DER certificate as
// colon-separated uppercase hex.
func certFingerprint(der []byte) string {
	sum := sha256.Sum256(der)

	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = strings.ToUpper(hex.EncodeToString([]byte{b}))
	}

	return strings.Join(parts, ":")
}
