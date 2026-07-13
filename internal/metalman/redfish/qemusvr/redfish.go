// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package qemusvr implements a recording Redfish fixture backed by one libvirt
// domain. It is a Go reimplementation of hack/metalman-redfish-fixture.py used
// by the metalman smoke tests.
//
// The package is split into two layers: Server implements the Redfish semantics
// (routing, authentication, request validation, and JSONL recording), and
// Machine (see qemu.go) drives the underlying libvirt domain. Server talks to
// the machine layer only through the Backend interface, which is faked in tests.
//
// PXE overrides update the libvirt boot order directly. When the optional HTTP
// boundary arguments are supplied, a UefiHttp PATCH makes a prebuilt EFI disk
// visible at the next power-on. That disk is the HTTP smoke test's documented
// post-firmware boundary; the fixture does not pretend that OVMF implements
// native DHCP-free UEFI HTTP boot.
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

// Config holds the fixture's runtime configuration. It mirrors the command-line
// flags of the original Python fixture and configures both the Redfish server
// and the QEMU machine layers.
type Config struct {
	Bind            string
	Port            int
	Cert            string
	Key             string
	Domain          string
	MAC             string
	Record          string
	EFISource       string
	EFIActive       string
	Bridge          string
	CacheDir        string
	Username        string
	Password        string
	ManageBootOrder bool
}

// Backend performs the machine-level operations behind the Redfish semantics. It
// is implemented by Machine and faked in tests. bootURL and clientIP are derived
// from Redfish state by the Server and passed down.
type Backend interface {
	// PowerState reports "On" or "Off".
	PowerState() string
	// PowerOff forces the domain off (best-effort).
	PowerOff()
	// PowerOn starts the domain.
	PowerOn() error
	// Restart resets a running domain or starts a stopped one.
	Restart() error
	// SetBootOrder sets the libvirt boot order for "Pxe" or "Hdd".
	SetBootOrder(target string) error
	// StageEFIBoundary stages (enabled) or restores (disabled) the EFI boundary
	// disk.
	StageEFIBoundary(enabled bool, bootURL, clientIP string) error
	// FetchBootEntrypoint emulates firmware's initial HTTP fetch after power-on.
	FetchBootEntrypoint(bootURL, clientIP string) error
	// DetachEFIBoundary removes the staged boundary disk once firmware has
	// loaded. It is expected to be called asynchronously.
	DetachEFIBoundary()
}

// Server is the Redfish fixture state for a single libvirt domain.
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

// staticAddress validates that exactly one complete static IPv4 address has been
// accepted with DHCP disabled and returns its address.
func (s *Server) staticAddress() (string, error) {
	if !reflect.DeepEqual(s.nic["DHCPv4"], map[string]any{"DHCPEnabled": false}) {
		return "", errors.New("UefiHttp requested before DHCPv4 was disabled")
	}

	addresses, ok := s.nic["IPv4StaticAddresses"].([]any)
	if !ok || len(addresses) != 1 {
		return "", errors.New("UefiHttp requires exactly one accepted static IPv4 address")
	}

	address := asMap(addresses[0])
	if !nonEmptyString(address["Address"]) || !nonEmptyString(address["SubnetMask"]) {
		return "", errors.New("UefiHttp static IPv4 address is incomplete")
	}

	return asString(address["Address"]), nil
}

// patchSystem applies a PATCH to the system's Boot configuration.
func (s *Server) patchSystem(body map[string]any) error {
	boot := asMap(body["Boot"])
	target := asString(boot["BootSourceOverrideTarget"])

	var clientIP string

	if target == "UefiHttp" {
		if mode := asString(boot["BootSourceOverrideMode"]); mode != "UEFI" {
			return errors.New("UefiHttp override must explicitly select UEFI mode")
		}

		if enabled := asString(boot["BootSourceOverrideEnabled"]); enabled != "Continuous" {
			return errors.New("standard UefiHttp override must be continuous")
		}

		addr, err := s.staticAddress()
		if err != nil {
			return err
		}

		clientIP = addr
	}

	for k, v := range boot {
		s.boot[k] = v
	}

	switch target {
	case "UefiHttp":
		if err := s.backend.StageEFIBoundary(true, asString(s.boot["HttpBootUri"]), clientIP); err != nil {
			return err
		}
	case "Pxe", "Hdd":
		if err := s.backend.SetBootOrder(target); err != nil {
			return err
		}
	}

	if enabled := asString(boot["BootSourceOverrideEnabled"]); enabled == "Disabled" {
		if s.cfg.EFISource != "" {
			if err := s.backend.StageEFIBoundary(false, "", ""); err != nil {
				return err
			}
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
		if err := s.backend.PowerOn(); err != nil {
			return err
		}
		// OVMF cannot consume Redfish static NIC settings. Emulate only its
		// initial fetch after power-on; EFI boundary preparation must not
		// advance Metalman's boot status.
		if err := s.fetchBootEntrypoint(); err != nil {
			return err
		}
		// The staged disk substitutes only for firmware's initial HTTP fetch.
		// Remove it from the persistent domain after OVMF has loaded shim/GRUB
		// so the installer's reboot starts the written OS disk.
		go s.backend.DetachEFIBoundary()
	case "ForceRestart":
		if err := s.backend.Restart(); err != nil {
			return err
		}
	default:
		return errors.New("unsupported ResetType " + resetType)
	}

	return nil
}

// fetchBootEntrypoint drives the backend's firmware fetch and records it as a
// FIRMWARE_FETCH entry. The caller must hold s.mu.
func (s *Server) fetchBootEntrypoint() error {
	bootURL := asString(s.boot["HttpBootUri"])

	clientIP, err := s.staticAddress()
	if err != nil {
		return err
	}

	if err := s.backend.FetchBootEntrypoint(bootURL, clientIP); err != nil {
		return err
	}

	return s.appendRecord("FIRMWARE_FETCH", bootURL, map[string]any{"source": clientIP}, 200)
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
	defer f.Close() //nolint:errcheck // Best-effort close of append-only record file.

	_, err = f.Write(append(data, '\n'))

	return err
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
