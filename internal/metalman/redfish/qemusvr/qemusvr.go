// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package qemusvr implements a recording Redfish fixture backed by one libvirt
// domain. It is a Go reimplementation of hack/metalman-redfish-fixture.py used
// by the metalman smoke tests.
//
// PXE overrides update the libvirt boot order directly. When the optional HTTP
// boundary arguments are supplied, a UefiHttp PATCH makes a prebuilt EFI disk
// visible at the next power-on. That disk is the HTTP smoke test's documented
// post-firmware boundary; the fixture does not pretend that OVMF implements
// native DHCP-free UEFI HTTP boot.
package qemusvr

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
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
// flags of the original Python fixture.
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

// Runner executes external commands. It is abstracted so tests can stub out
// virsh, ip, curl, and the FAT tooling.
type Runner interface {
	// Run executes name with args and returns stdout and the process exit code.
	// err is non-nil only when the command could not be started or run to
	// completion (mirroring subprocess semantics where a non-zero exit is
	// reported through the exit code, not an error).
	Run(name string, args ...string) (stdout string, code int, err error)
}

// State is the mutable fixture state for a single libvirt domain.
type State struct {
	cfg    Config
	runner Runner
	mu     sync.Mutex
	boot   map[string]any
	nic    map[string]any
}

// NewState builds fixture state, prepares the record file, and, when an EFI
// source is configured, stages the blank boundary disk.
func NewState(cfg Config, runner Runner) (*State, error) {
	if runner == nil {
		runner = execRunner{}
	}

	mac := strings.ToLower(cfg.MAC)
	s := &State{
		cfg:    cfg,
		runner: runner,
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

	if cfg.EFISource != "" {
		if err := s.setEFIBoundary(false); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// virsh runs a virsh subcommand against the system libvirt instance.
func (s *State) virsh(args ...string) (string, int, error) {
	return s.runner.Run("virsh", append([]string{"--connect", "qemu:///system"}, args...)...)
}

// virshCheck runs virsh and returns an error on non-zero exit.
func (s *State) virshCheck(args ...string) (string, error) {
	stdout, code, err := s.virsh(args...)
	if err != nil {
		return stdout, err
	}

	if code != 0 {
		return stdout, fmt.Errorf("virsh %s exited with code %d", strings.Join(args, " "), code)
	}

	return stdout, nil
}

// authorized reports whether the Authorization header is acceptable. When no
// username is configured, all requests are authorized.
func (s *State) authorized(value string) bool {
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
func (s *State) staticAddress() (string, error) {
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

// withClientAddress attaches the static client IP to the HTTP boundary bridge,
// runs action, then removes the address.
func (s *State) withClientAddress(action func(clientIP string) error) error {
	if s.cfg.Bridge == "" {
		return errors.New("UefiHttp requested without an HTTP boundary bridge")
	}

	clientIP, err := s.staticAddress()
	if err != nil {
		return err
	}

	if _, code, err := s.runner.Run("ip", "address", "add", clientIP+"/32", "dev", s.cfg.Bridge); err != nil {
		return err
	} else if code != 0 {
		return fmt.Errorf("ip address add %s failed with code %d", clientIP, code)
	}

	defer func() {
		//nolint:errcheck // Best-effort removal of the temporary client address.
		_, _, _ = s.runner.Run("ip", "address", "delete", clientIP+"/32", "dev", s.cfg.Bridge)
	}()

	return action(clientIP)
}

// appendRecord writes a single JSONL record entry. The caller must hold s.mu.
func (s *State) appendRecord(method, path string, body any, status int) error {
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

// powerState returns "On" when the domain is running, otherwise "Off".
func (s *State) powerState() string {
	stdout, code, err := s.virsh("domstate", s.cfg.Domain)
	if err == nil && code == 0 && strings.Contains(stdout, "running") {
		return "On"
	}

	return "Off"
}

// setBootOrder rewrites the libvirt domain boot order when boot-order management
// is enabled.
func (s *State) setBootOrder(target string) error {
	if !s.cfg.ManageBootOrder {
		return nil
	}

	current, err := s.virshCheck("dumpxml", s.cfg.Domain)
	if err != nil {
		return err
	}

	devices := []string{"hd", "network"}
	if target == "Pxe" {
		devices = []string{"network", "hd"}
	}

	rewritten, err := rewriteBootOrder(current, devices)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "domain-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // Best-effort cleanup of temp file.

	if _, err := tmp.WriteString(rewritten); err != nil {
		_ = tmp.Close() //nolint:errcheck // Best-effort close on the error path.

		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	_, err = s.virshCheck("define", tmp.Name())

	return err
}

// fetchBootEntrypoint emulates firmware's initial HTTP fetch after power-on and
// records it as a FIRMWARE_FETCH entry. The caller must hold s.mu.
func (s *State) fetchBootEntrypoint() error {
	bootURL := asString(s.boot["HttpBootUri"])

	return s.withClientAddress(func(clientIP string) error {
		_, code, err := s.runner.Run("curl", "--fail", "--silent", "--show-error",
			"--interface", clientIP, "--output", "/dev/null", bootURL)
		if err != nil {
			return err
		}

		if code != 0 {
			return fmt.Errorf("curl %s failed with code %d", bootURL, code)
		}

		return s.appendRecord("FIRMWARE_FETCH", bootURL, map[string]any{"source": clientIP}, 200)
	})
}

// reset maps a Redfish ComputerSystem.Reset action to virsh.
func (s *State) reset(resetType string) error {
	switch resetType {
	case "ForceOff":
		_, _, _ = s.virsh("destroy", s.cfg.Domain) //nolint:errcheck // Best-effort power off.
	case "On":
		if _, err := s.virshCheck("start", s.cfg.Domain); err != nil {
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
		go s.detachEFIBoundary()
	case "ForceRestart":
		if s.powerState() == "On" {
			if _, err := s.virshCheck("reset", s.cfg.Domain); err != nil {
				return err
			}
		} else if _, err := s.virshCheck("start", s.cfg.Domain); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported ResetType %q", resetType)
	}

	return nil
}

// detachEFIBoundary removes the staged boundary disk after firmware has loaded.
func (s *State) detachEFIBoundary() {
	time.Sleep(60 * time.Second)

	//nolint:errcheck // Best-effort detach of the staged boundary disk.
	_, _, _ = s.virsh("detach-disk", s.cfg.Domain, "vdb", "--live", "--config")
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

// rewriteBootOrder replaces the <boot> children of the <os> element with one
// entry per device, preserving the rest of the domain XML.
func rewriteBootOrder(domainXML string, devices []string) (string, error) {
	dec := xml.NewDecoder(strings.NewReader(domainXML))

	var out strings.Builder

	enc := xml.NewEncoder(&out)
	inOS := false

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return "", err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			if inOS && t.Name.Local == "boot" {
				if err := dec.Skip(); err != nil {
					return "", err
				}

				continue
			}

			if t.Name.Local == "os" {
				inOS = true
			}
		case xml.EndElement:
			if inOS && t.Name.Local == "os" {
				for _, dev := range devices {
					start := xml.StartElement{
						Name: xml.Name{Local: "boot"},
						Attr: []xml.Attr{{Name: xml.Name{Local: "dev"}, Value: dev}},
					}
					if err := enc.EncodeToken(start); err != nil {
						return "", err
					}

					if err := enc.EncodeToken(xml.EndElement{Name: start.Name}); err != nil {
						return "", err
					}
				}

				inOS = false
			}
		}

		if err := enc.EncodeToken(tok); err != nil {
			return "", err
		}
	}

	if err := enc.Flush(); err != nil {
		return "", err
	}

	return out.String(), nil
}

// Handler returns an http.Handler serving the Redfish fixture API.
func (s *State) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.dispatch(w, r)
	})

	return mux
}

func (s *State) dispatch(w http.ResponseWriter, r *http.Request) {
	method := r.Method
	if !s.authorized(r.Header.Get("Authorization")) {
		reply(w, http.StatusUnauthorized, errorBody("invalid Redfish credentials"))

		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	body := map[string]any{}
	status := http.StatusOK

	var response any

	err := func() error {
		if method == http.MethodPatch || method == http.MethodPost {
			parsed, err := readBody(r)
			if err != nil {
				return err
			}

			body = parsed
		}

		var err error

		status, response, err = s.route(method, r.URL.Path, body)

		return err
	}()
	if err != nil {
		status = http.StatusInternalServerError
		response = errorBody(err.Error())
	}

	if recErr := s.appendRecord(method, r.URL.Path, body, status); recErr != nil {
		log.Printf("redfish: record append failed: %v", recErr)
	}

	reply(w, status, response)
}

// route resolves a request to a status and response body, mutating state as
// needed. The caller must hold s.mu.
func (s *State) route(method, path string, body map[string]any) (int, any, error) {
	system := "/redfish/v1/Systems/" + s.cfg.Domain

	switch {
	case method == http.MethodGet && path == "/redfish/v1/":
		return http.StatusOK, map[string]any{
			"Systems": map[string]any{"@odata.id": "/redfish/v1/Systems"},
		}, nil
	case method == http.MethodGet && path == "/redfish/v1/Systems":
		return http.StatusOK, map[string]any{
			"Members": []any{map[string]any{"@odata.id": system}},
		}, nil
	case method == http.MethodGet && path == system:
		return http.StatusOK, map[string]any{
			"Id":         s.cfg.Domain,
			"PowerState": s.powerState(),
			"Boot":       s.boot,
			"EthernetInterfaces": map[string]any{
				"@odata.id": system + "/EthernetInterfaces",
			},
		}, nil
	case method == http.MethodPatch && path == system:
		if err := s.patchSystem(body); err != nil {
			return 0, nil, err
		}

		return http.StatusNoContent, nil, nil
	case method == http.MethodGet && strings.HasSuffix(path, "/EthernetInterfaces"):
		return http.StatusOK, map[string]any{
			"Members": []any{map[string]any{"@odata.id": path + "/NIC.1"}},
		}, nil
	case method == http.MethodGet && strings.HasSuffix(path, "/EthernetInterfaces/NIC.1"):
		return http.StatusOK, s.nic, nil
	case method == http.MethodPatch && strings.HasSuffix(path, "/EthernetInterfaces/NIC.1"):
		if err := s.patchNIC(body); err != nil {
			return 0, nil, err
		}

		return http.StatusNoContent, nil, nil
	case method == http.MethodPost && strings.HasSuffix(path, "/Actions/ComputerSystem.Reset"):
		resetType := asString(body["ResetType"])
		if err := s.reset(resetType); err != nil {
			return 0, nil, err
		}

		return http.StatusNoContent, nil, nil
	case strings.HasSuffix(path, "/Bios/Settings"):
		// Standard EthernetInterface + ComputerSystem.HttpBootUri is the
		// contract under test. Report vendor BIOS settings unsupported.
		return http.StatusNotFound, errorBody("vendor BIOS settings unsupported"), nil
	case strings.HasPrefix(path, "/redfish/v1/SessionService/"):
		return http.StatusNotFound, errorBody("use basic authentication"), nil
	default:
		return http.StatusNotFound, errorBody("resource not found"), nil
	}
}

// patchSystem applies a PATCH to the system's Boot configuration.
func (s *State) patchSystem(body map[string]any) error {
	boot := asMap(body["Boot"])
	target := asString(boot["BootSourceOverrideTarget"])

	if target == "UefiHttp" {
		if mode := asString(boot["BootSourceOverrideMode"]); mode != "UEFI" {
			return errors.New("UefiHttp override must explicitly select UEFI mode")
		}

		if enabled := asString(boot["BootSourceOverrideEnabled"]); enabled != "Continuous" {
			return errors.New("standard UefiHttp override must be continuous")
		}

		if _, err := s.staticAddress(); err != nil {
			return err
		}
	}

	for k, v := range boot {
		s.boot[k] = v
	}

	switch target {
	case "UefiHttp":
		if err := s.setEFIBoundary(true); err != nil {
			return err
		}
	case "Pxe", "Hdd":
		if err := s.setBootOrder(target); err != nil {
			return err
		}
	}

	if enabled := asString(boot["BootSourceOverrideEnabled"]); enabled == "Disabled" {
		if s.cfg.EFISource != "" {
			if err := s.setEFIBoundary(false); err != nil {
				return err
			}
		}

		if err := s.setBootOrder("Hdd"); err != nil {
			return err
		}
	}

	return nil
}

// patchNIC applies a static IPv4 PATCH to the emulated EthernetInterface.
func (s *State) patchNIC(body map[string]any) error {
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
