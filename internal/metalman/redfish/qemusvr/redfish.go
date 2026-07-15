// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package qemusvr implements a recording Redfish fixture backed by one
// Cloud Hypervisor virtual machine. It is a Go reimplementation of
// hack/metalman-redfish-fixture.py used by the metalman smoke tests.
//
// The package is split into two layers: Server implements the Redfish semantics
// (routing, authentication, request validation, and JSONL recording), and
// Machine (see qemu.go) launches and controls the cloud-hypervisor process
// directly. Server talks to the machine layer only through the Backend
// interface, which is faked in tests.
//
// PXE overrides record the boot preference; the CloudHv OVMF firmware boots the
// disk when it holds a bootable OS and otherwise falls back to network boot. A
// UefiHttp PATCH is translated into a dnsmasq configuration bound to the
// boundary bridge: the Redfish static-NIC address becomes a DHCP reservation and
// the HttpBootUri becomes the UEFI HTTP boot URL. The OVMF firmware then
// performs a genuine firmware-native UEFI HTTP boot, fetching the boot
// entrypoint over HTTP itself. The address and boot URL are delivered via a DHCP
// reservation rather than firmware static configuration because upstream OVMF
// HttpBootDxe always DHCPs; Metalman's Redfish behavior is exercised and
// asserted unchanged.
package qemusvr

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Config holds the fixture's runtime configuration. It configures both the
// Redfish server and the QEMU machine layers.
type Config struct {
	// Redfish server.
	Bind     string
	Port     int
	Cert     string
	Key      string
	Username string
	Password string
	Record   string

	// Machine identity. Domain is the Redfish system Id and the QEMU guest name.
	Domain string
	MAC    string

	// Cloud Hypervisor virtual machine definition.
	Disk               string // qcow2 disk image path
	MemoryMiB          int    // guest RAM in MiB (default 4096)
	VCPUs              int    // guest vCPU count (default 2)
	Firmware           string // CloudHv OVMF firmware blob (read-only)
	FirmwareSecureBoot string // CloudHv OVMF firmware blob used when SecureBoot is set
	SecureBoot         bool   // enroll a Secure Boot Platform Key via SMBIOS and use the secure-boot firmware
	StateDir           string // working directory for sockets and TPM state

	// Networking. When Bridge is set the fixture creates the bridge, assigns
	// BridgeAddress/BridgePrefix to it, brings it up, and installs outbound NAT
	// for the derived subnet. Each power-on attaches a fresh tap to the bridge.
	Bridge        string
	BridgeAddress string // host IP on the bridge (the guest's gateway)
	BridgePrefix  int    // CIDR prefix length for BridgeAddress (default 24)

	// HTTP boot. DnsmasqDir is the working directory for the UEFI HTTP boot
	// dnsmasq bound to the bridge.
	DnsmasqDir string

	ManageBootOrder bool
}

// Backend performs the machine-level operations behind the Redfish semantics. It
// is implemented by Machine and faked in tests. The static-NIC and HttpBootUri
// values are derived from Redfish state by the Server and passed down.
type Backend interface {
	// PowerState reports "On" or "Off".
	PowerState() string
	// PowerOff forces the domain off (best-effort).
	PowerOff()
	// PowerOn starts the domain.
	PowerOn() error
	// Restart resets a running domain or starts a stopped one.
	Restart() error
	// SetBootOrder sets the boot order for "Pxe" (network first) or "Hdd"
	// (disk first), applied at the next power-on.
	SetBootOrder(target string) error
	// ConfigureHTTPBoot programs a DHCP reservation plus UEFI HTTP boot URL so
	// stock OVMF performs a firmware-native HTTP boot at the next power-on.
	ConfigureHTTPBoot(mac, address, subnetMask, gateway string, dns []string, bootURL string) error
	// ClearHTTPBoot tears down the HTTP boot DHCP reservation.
	ClearHTTPBoot() error
}

// Server is the Redfish fixture state for a single QEMU virtual machine.
type Server struct {
	cfg     Config
	backend Backend
	mu      sync.Mutex
	boot    map[string]any
	nic     map[string]any
}

// NewServer builds Redfish state and prepares the record file.
func NewServer(cfg Config, backend Backend) (*Server, error) {
	mac := strings.ToLower(cfg.MAC)
	s := &Server{
		cfg:     cfg,
		backend: backend,
		boot: map[string]any{
			"BootSourceOverrideTarget":  "None",
			"BootSourceOverrideEnabled": "Disabled",
			"BootSourceOverrideMode":    "UEFI",
			"HttpBootUri":               "",
		},
		nic: map[string]any{
			"MACAddress":          mac,
			"PermanentMACAddress": mac,
			"DHCPv4":              map[string]any{"DHCPEnabled": true},
			"IPv4StaticAddresses": []any{},
			"StaticNameServers":   []any{},
		},
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Record), 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(cfg.Record, os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	_ = f.Close() //nolint:errcheck // Best-effort close after creating record file.

	return s, nil
}

// Handler returns an http.Handler serving the Redfish fixture API using
// method-and-pattern routing wrapped by the recording middleware.
func (s *Server) Handler() http.Handler {
	system := "/redfish/v1/Systems/{system}"

	mux := http.NewServeMux()
	mux.HandleFunc("GET /redfish/v1/{$}", s.wrap(s.handleServiceRoot))
	mux.HandleFunc("GET /redfish/v1/Systems", s.wrap(s.handleSystems))
	mux.HandleFunc("GET "+system, s.wrap(s.handleGetSystem))
	mux.HandleFunc("PATCH "+system, s.wrap(s.handlePatchSystem))
	mux.HandleFunc("GET "+system+"/EthernetInterfaces", s.wrap(s.handleEthernet))
	mux.HandleFunc("GET "+system+"/EthernetInterfaces/NIC.1", s.wrap(s.handleGetNIC))
	mux.HandleFunc("PATCH "+system+"/EthernetInterfaces/NIC.1", s.wrap(s.handlePatchNIC))
	mux.HandleFunc("POST "+system+"/Actions/ComputerSystem.Reset", s.wrap(s.handleReset))
	mux.HandleFunc(system+"/Bios/Settings", s.wrap(s.handleBios))
	mux.HandleFunc("/redfish/v1/SessionService/", s.wrap(s.handleSessionService))
	mux.HandleFunc("/", s.wrap(s.handleNotFound))

	return s.middleware(mux)
}

// redfishHandler resolves a request to a status and response body, mutating
// state as needed. The caller holds s.mu.
type redfishHandler func(r *http.Request, body map[string]any) (int, any, error)

// wrap adapts a redfishHandler to an http.HandlerFunc, converting handler errors
// to HTTP 500 responses and writing the reply.
func (s *Server) wrap(h redfishHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, response, err := h(r, bodyFrom(r.Context()))
		if err != nil {
			status = http.StatusInternalServerError
			response = errorBody(err.Error())
		}

		reply(w, status, response)
	}
}

// bodyKey identifies the parsed request body stored in the request context.
type bodyKey struct{}

func bodyFrom(ctx context.Context) map[string]any {
	if body, ok := ctx.Value(bodyKey{}).(map[string]any); ok {
		return body
	}

	return map[string]any{}
}

// statusRecorder captures the response status for recording.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(status int) {
	sr.status = status
	sr.ResponseWriter.WriteHeader(status)
}

// middleware enforces authentication, serializes requests, parses request
// bodies, and records every authorized request to the JSONL log.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r.Header.Get("Authorization")) {
			reply(w, http.StatusUnauthorized, errorBody("invalid Redfish credentials"))

			return
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		body := map[string]any{}
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		if r.Method == http.MethodPatch || r.Method == http.MethodPost {
			parsed, err := readBody(r)
			if err != nil {
				reply(sr, http.StatusInternalServerError, errorBody(err.Error()))
				s.record(r, body, sr.status)

				return
			}

			body = parsed
		}

		next.ServeHTTP(sr, r.WithContext(context.WithValue(r.Context(), bodyKey{}, body)))
		s.record(r, body, sr.status)
	})
}

// record appends a JSONL entry for the request, logging append failures.
func (s *Server) record(r *http.Request, body any, status int) {
	if err := s.appendRecord(r.Method, r.URL.Path, body, status); err != nil {
		log.Printf("redfish: record append failed: %v", err)
	}
}

func (s *Server) handleServiceRoot(_ *http.Request, _ map[string]any) (int, any, error) {
	return http.StatusOK, map[string]any{
		"Systems": map[string]any{"@odata.id": "/redfish/v1/Systems"},
	}, nil
}

func (s *Server) handleSystems(_ *http.Request, _ map[string]any) (int, any, error) {
	return http.StatusOK, map[string]any{
		"Members": []any{map[string]any{"@odata.id": s.systemPath()}},
	}, nil
}

func (s *Server) handleGetSystem(_ *http.Request, _ map[string]any) (int, any, error) {
	return http.StatusOK, map[string]any{
		"Id":         s.cfg.Domain,
		"PowerState": s.backend.PowerState(),
		"Boot":       s.boot,
		"EthernetInterfaces": map[string]any{
			"@odata.id": s.systemPath() + "/EthernetInterfaces",
		},
	}, nil
}

func (s *Server) handlePatchSystem(_ *http.Request, body map[string]any) (int, any, error) {
	if err := s.patchSystem(body); err != nil {
		return 0, nil, err
	}

	return http.StatusNoContent, nil, nil
}

func (s *Server) handleEthernet(r *http.Request, _ map[string]any) (int, any, error) {
	return http.StatusOK, map[string]any{
		"Members": []any{map[string]any{"@odata.id": r.URL.Path + "/NIC.1"}},
	}, nil
}

func (s *Server) handleGetNIC(_ *http.Request, _ map[string]any) (int, any, error) {
	return http.StatusOK, s.nic, nil
}

func (s *Server) handlePatchNIC(_ *http.Request, body map[string]any) (int, any, error) {
	if err := s.patchNIC(body); err != nil {
		return 0, nil, err
	}

	return http.StatusNoContent, nil, nil
}

func (s *Server) handleReset(_ *http.Request, body map[string]any) (int, any, error) {
	if err := s.reset(asString(body["ResetType"])); err != nil {
		return 0, nil, err
	}

	return http.StatusNoContent, nil, nil
}

func (s *Server) handleBios(_ *http.Request, _ map[string]any) (int, any, error) {
	// Standard EthernetInterface + ComputerSystem.HttpBootUri is the contract
	// under test. Report vendor BIOS settings unsupported.
	return http.StatusNotFound, errorBody("vendor BIOS settings unsupported"), nil
}

func (s *Server) handleSessionService(_ *http.Request, _ map[string]any) (int, any, error) {
	return http.StatusNotFound, errorBody("use basic authentication"), nil
}

func (s *Server) handleNotFound(_ *http.Request, _ map[string]any) (int, any, error) {
	return http.StatusNotFound, errorBody("resource not found"), nil
}

// systemPath returns the Redfish path of the fixture's single system.
func (s *Server) systemPath() string {
	return "/redfish/v1/Systems/" + s.cfg.Domain
}

// authorized reports whether the Authorization header is acceptable. When no
// username is configured, all requests are authorized.
func (s *Server) authorized(value string) bool {
	if s.cfg.Username == "" {
		return true
	}

	expected := "Basic " + base64.StdEncoding.EncodeToString(
		[]byte(s.cfg.Username+":"+s.cfg.Password),
	)

	return subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

// staticNIC captures the accepted static IPv4 configuration derived from the
// EthernetInterface PATCH.
type staticNIC struct {
	address    string
	subnetMask string
	gateway    string
	dns        []string
}

// staticConfig validates that exactly one complete static IPv4 address has been
// accepted with DHCP disabled and returns the full static configuration.
func (s *Server) staticConfig() (staticNIC, error) {
	if !reflect.DeepEqual(s.nic["DHCPv4"], map[string]any{"DHCPEnabled": false}) {
		return staticNIC{}, errors.New("UefiHttp requested before DHCPv4 was disabled")
	}

	addresses, ok := s.nic["IPv4StaticAddresses"].([]any)
	if !ok || len(addresses) != 1 {
		return staticNIC{}, errors.New("UefiHttp requires exactly one accepted static IPv4 address")
	}

	address := asMap(addresses[0])
	if !nonEmptyString(address["Address"]) || !nonEmptyString(address["SubnetMask"]) {
		return staticNIC{}, errors.New("UefiHttp static IPv4 address is incomplete")
	}

	return staticNIC{
		address:    asString(address["Address"]),
		subnetMask: asString(address["SubnetMask"]),
		gateway:    asString(address["Gateway"]),
		dns:        asStringSlice(s.nic["StaticNameServers"]),
	}, nil
}

// patchSystem applies a PATCH to the system's Boot configuration.
func (s *Server) patchSystem(body map[string]any) error {
	boot := asMap(body["Boot"])
	target := asString(boot["BootSourceOverrideTarget"])

	var nic staticNIC

	if target == "UefiHttp" {
		if mode := asString(boot["BootSourceOverrideMode"]); mode != "UEFI" {
			return errors.New("UefiHttp override must explicitly select UEFI mode")
		}

		if enabled := asString(boot["BootSourceOverrideEnabled"]); enabled != "Continuous" {
			return errors.New("standard UefiHttp override must be continuous")
		}

		accepted, err := s.staticConfig()
		if err != nil {
			return err
		}

		nic = accepted
	}

	for k, v := range boot {
		s.boot[k] = v
	}

	switch target {
	case "UefiHttp":
		if err := s.backend.ConfigureHTTPBoot(strings.ToLower(s.cfg.MAC), nic.address,
			nic.subnetMask, nic.gateway, nic.dns, asString(s.boot["HttpBootUri"])); err != nil {
			return err
		}
	case "Pxe", "Hdd":
		if err := s.backend.SetBootOrder(target); err != nil {
			return err
		}
	}

	if enabled := asString(boot["BootSourceOverrideEnabled"]); enabled == "Disabled" {
		if err := s.backend.ClearHTTPBoot(); err != nil {
			return err
		}

		if err := s.backend.SetBootOrder("Hdd"); err != nil {
			return err
		}
	}

	return nil
}

// patchNIC applies a static IPv4 PATCH to the emulated EthernetInterface.
func (s *Server) patchNIC(body map[string]any) error {
	if !reflect.DeepEqual(body["DHCPv4"], map[string]any{"DHCPEnabled": false}) {
		return errors.New("static EthernetInterface PATCH must disable DHCPv4")
	}

	addresses, ok := body["IPv4StaticAddresses"].([]any)
	if !ok || len(addresses) != 1 {
		return errors.New("static EthernetInterface PATCH must contain one IPv4 address")
	}

	for k, v := range body {
		s.nic[k] = v
	}

	return nil
}

// reset maps a Redfish ComputerSystem.Reset action to the backend.
func (s *Server) reset(resetType string) error {
	switch resetType {
	case "ForceOff":
		s.backend.PowerOff()
	case "On":
		// Power on and let firmware do the work. dnsmasq, programmed by the
		// preceding UefiHttp PATCH, answers OVMF's DHCP with the reserved
		// address and boot URL so firmware performs a native HTTP boot.
		if err := s.backend.PowerOn(); err != nil {
			return err
		}
	case "ForceRestart":
		if err := s.backend.Restart(); err != nil {
			return err
		}
	default:
		return errors.New("unsupported ResetType " + resetType)
	}

	return nil
}

// appendRecord writes a single JSONL record entry. The caller must hold s.mu.
func (s *Server) appendRecord(method, path string, body any, status int) error {
	entry := map[string]any{
		"time":   float64(time.Now().UnixNano()) / 1e9,
		"method": method,
		"path":   path,
		"body":   body,
		"status": status,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(s.cfg.Record, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	_, writeErr := f.Write(append(data, '\n'))

	closeErr := f.Close()

	if writeErr != nil {
		return writeErr
	}

	return closeErr
}

func readBody(r *http.Request) (map[string]any, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}

	if len(data) == 0 {
		return map[string]any{}, nil
	}

	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}

	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("request body must be a JSON object")
	}

	return object, nil
}

func reply(w http.ResponseWriter, status int, value any) {
	if value == nil {
		w.WriteHeader(status)

		return
	}

	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data) //nolint:errcheck // Best-effort write of response body.
}

func errorBody(message string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message}}
}

func nonEmptyString(v any) bool {
	str, ok := v.(string)

	return ok && str != ""
}

// asString returns v as a string, or "" when v is not a string.
func asString(v any) string {
	str, ok := v.(string)
	if !ok {
		return ""
	}

	return str
}

// asMap returns v as a JSON object, or nil when v is not an object.
func asMap(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}

	return m
}

// asStringSlice returns v as a slice of strings, skipping any non-string
// elements. It returns nil when v is not a JSON array.
func asStringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(items))

	for _, item := range items {
		if str, ok := item.(string); ok {
			out = append(out, str)
		}
	}

	return out
}
