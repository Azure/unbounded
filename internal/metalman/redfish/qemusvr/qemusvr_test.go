// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package qemusvr

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testDomain = "smoke-vm"

// fakeBackend records invocations and returns scripted results. It is the sole
// test fake for the Redfish server; the QEMU machine layer is not unit tested.
type fakeBackend struct {
	mu    sync.Mutex
	calls []string

	powerState string
	powerOnErr error
	restartErr error
	bootOrder  error
	configErr  error
	clearErr   error

	httpBoot httpBootArgs
}

// httpBootArgs captures the last ConfigureHTTPBoot invocation for assertions.
type httpBootArgs struct {
	mac        string
	address    string
	subnetMask string
	gateway    string
	dns        []string
	bootURL    string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{powerState: "Off"}
}

func (f *fakeBackend) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, name)
}

func (f *fakeBackend) called(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, c := range f.calls {
		if c == name {
			return true
		}
	}

	return false
}

func (f *fakeBackend) PowerState() string { f.record("PowerState"); return f.powerState }
func (f *fakeBackend) PowerOff()          { f.record("PowerOff") }

func (f *fakeBackend) PowerOn() error {
	f.record("PowerOn")

	return f.powerOnErr
}

func (f *fakeBackend) Restart() error {
	f.record("Restart")

	return f.restartErr
}

func (f *fakeBackend) SetBootOrder(target string) error {
	f.record("SetBootOrder:" + target)

	return f.bootOrder
}

func (f *fakeBackend) ConfigureHTTPBoot(mac, address, subnetMask, gateway string, dns []string, bootURL string) error {
	f.mu.Lock()
	f.httpBoot = httpBootArgs{
		mac:        mac,
		address:    address,
		subnetMask: subnetMask,
		gateway:    gateway,
		dns:        dns,
		bootURL:    bootURL,
	}
	f.mu.Unlock()
	f.record("ConfigureHTTPBoot")

	return f.configErr
}

func (f *fakeBackend) ClearHTTPBoot() error {
	f.record("ClearHTTPBoot")

	return f.clearErr
}

func newTestServer(t *testing.T, cfg Config, backend Backend) *Server {
	t.Helper()

	if cfg.Record == "" {
		cfg.Record = filepath.Join(t.TempDir(), "redfish.jsonl")
	}

	if cfg.Domain == "" {
		cfg.Domain = testDomain
	}

	if cfg.MAC == "" {
		cfg.MAC = "52:54:00:AB:CD:EF"
	}

	server, err := NewServer(cfg, backend)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	return server
}

func do(t *testing.T, h http.Handler, method, path string, body any, auth string) (*http.Response, []byte) {
	t.Helper()

	var reader io.Reader

	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}

		reader = bytes.NewReader(data)
	}

	req := httptest.NewRequest(method, path, reader)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	resp := rec.Result()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	return resp, data
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestServiceRootAndSystems(t *testing.T) {
	server := newTestServer(t, Config{}, newFakeBackend())
	h := server.Handler()

	resp, data := do(t, h, http.MethodGet, "/redfish/v1/", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("service root status = %d", resp.StatusCode)
	}

	var root struct {
		Systems struct {
			ODataID string `json:"@odata.id"`
		} `json:"Systems"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal root: %v", err)
	}

	if root.Systems.ODataID != "/redfish/v1/Systems" {
		t.Fatalf("unexpected Systems link %q", root.Systems.ODataID)
	}

	resp, data = do(t, h, http.MethodGet, "/redfish/v1/Systems", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("systems status = %d", resp.StatusCode)
	}

	if !strings.Contains(string(data), "/redfish/v1/Systems/"+testDomain) {
		t.Fatalf("systems collection missing member: %s", data)
	}
}

func TestGetSystemPowerState(t *testing.T) {
	backend := newFakeBackend()
	backend.powerState = "On"
	server := newTestServer(t, Config{}, backend)

	resp, data := do(t, server.Handler(), http.MethodGet, "/redfish/v1/Systems/"+testDomain, nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("system status = %d", resp.StatusCode)
	}

	var system struct {
		ID         string `json:"Id"`
		PowerState string `json:"PowerState"`
		Boot       struct {
			Target string `json:"BootSourceOverrideTarget"`
		} `json:"Boot"`
	}
	if err := json.Unmarshal(data, &system); err != nil {
		t.Fatalf("unmarshal system: %v", err)
	}

	if system.ID != testDomain || system.PowerState != "On" || system.Boot.Target != "None" {
		t.Fatalf("unexpected system: %+v", system)
	}
}

func TestAuthorization(t *testing.T) {
	server := newTestServer(t, Config{Username: "smoke", Password: "secret"}, newFakeBackend())
	h := server.Handler()

	resp, _ := do(t, h, http.MethodGet, "/redfish/v1/", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing auth: status = %d, want 401", resp.StatusCode)
	}

	resp, _ = do(t, h, http.MethodGet, "/redfish/v1/", nil, basicAuth("smoke", "wrong"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong auth: status = %d, want 401", resp.StatusCode)
	}

	resp, _ = do(t, h, http.MethodGet, "/redfish/v1/", nil, basicAuth("smoke", "secret"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid auth: status = %d, want 200", resp.StatusCode)
	}

	// The rejected request must not be recorded.
	if got := len(readRecord(t, server.cfg.Record)); got != 1 {
		t.Fatalf("record entries = %d, want 1 (only the authorized request)", got)
	}
}

func TestEthernetInterfaceLifecycle(t *testing.T) {
	base := "/redfish/v1/Systems/" + testDomain + "/EthernetInterfaces"
	server := newTestServer(t, Config{}, newFakeBackend())
	h := server.Handler()

	resp, data := do(t, h, http.MethodGet, base, nil, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), base+"/NIC.1") {
		t.Fatalf("collection status=%d body=%s", resp.StatusCode, data)
	}

	patch := map[string]any{
		"DHCPv4": map[string]any{"DHCPEnabled": false},
		"IPv4StaticAddresses": []any{
			map[string]any{"Address": "192.168.1.10", "SubnetMask": "255.255.255.0"},
		},
		"StaticNameServers": []any{"8.8.8.8"},
	}

	resp, _ = do(t, h, http.MethodPatch, base+"/NIC.1", patch, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("nic patch status = %d, want 204", resp.StatusCode)
	}

	resp, data = do(t, h, http.MethodGet, base+"/NIC.1", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("nic get status = %d", resp.StatusCode)
	}

	if !strings.Contains(string(data), "192.168.1.10") {
		t.Fatalf("nic did not retain static address: %s", data)
	}
}

func TestEthernetInterfacePatchRejectsDHCP(t *testing.T) {
	base := "/redfish/v1/Systems/" + testDomain + "/EthernetInterfaces/NIC.1"
	server := newTestServer(t, Config{}, newFakeBackend())

	cases := []map[string]any{
		{"DHCPv4": map[string]any{"DHCPEnabled": true}, "IPv4StaticAddresses": []any{map[string]any{"Address": "1.2.3.4"}}},
		{"DHCPv4": map[string]any{"DHCPEnabled": false}, "IPv4StaticAddresses": []any{}},
	}
	for _, patch := range cases {
		resp, _ := do(t, server.Handler(), http.MethodPatch, base, patch, "")
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("patch %v status = %d, want 500", patch, resp.StatusCode)
		}
	}
}

func TestPatchBootPxeManagesBootOrder(t *testing.T) {
	backend := newFakeBackend()
	server := newTestServer(t, Config{ManageBootOrder: true}, backend)

	patch := map[string]any{"Boot": map[string]any{"BootSourceOverrideTarget": "Pxe"}}

	resp, _ := do(t, server.Handler(), http.MethodPatch, "/redfish/v1/Systems/"+testDomain, patch, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("boot patch status = %d, want 204", resp.StatusCode)
	}

	if !backend.called("SetBootOrder:Pxe") {
		t.Fatal("expected SetBootOrder(Pxe) for boot order management")
	}
}

func TestPatchBootUefiHTTPRequiresStaticAddress(t *testing.T) {
	server := newTestServer(t, Config{}, newFakeBackend())

	patch := map[string]any{"Boot": map[string]any{
		"BootSourceOverrideTarget":  "UefiHttp",
		"BootSourceOverrideEnabled": "Continuous",
		"BootSourceOverrideMode":    "UEFI",
		"HttpBootUri":               "http://server/bootx64.efi",
	}}

	resp, data := do(t, server.Handler(), http.MethodPatch, "/redfish/v1/Systems/"+testDomain, patch, "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("uefihttp patch status = %d, want 500", resp.StatusCode)
	}

	if !strings.Contains(string(data), "DHCPv4 was disabled") {
		t.Fatalf("unexpected error body: %s", data)
	}
}

func TestPatchBootUefiHTTPConfiguresHTTPBoot(t *testing.T) {
	backend := newFakeBackend()
	server := newTestServer(t, Config{}, backend)
	h := server.Handler()

	nicBase := "/redfish/v1/Systems/" + testDomain + "/EthernetInterfaces/NIC.1"
	nicPatch := map[string]any{
		"DHCPv4": map[string]any{"DHCPEnabled": false},
		"IPv4StaticAddresses": []any{
			map[string]any{
				"Address":    "192.168.200.10",
				"SubnetMask": "255.255.255.0",
				"Gateway":    "192.168.200.1",
			},
		},
		"StaticNameServers": []any{"8.8.8.8"},
	}

	if resp, _ := do(t, h, http.MethodPatch, nicBase, nicPatch, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("nic patch status = %d, want 204", resp.StatusCode)
	}

	bootPatch := map[string]any{"Boot": map[string]any{
		"BootSourceOverrideTarget":  "UefiHttp",
		"BootSourceOverrideEnabled": "Continuous",
		"BootSourceOverrideMode":    "UEFI",
		"HttpBootUri":               "http://192.168.200.1:8882/bootx64.efi",
	}}

	if resp, data := do(t, h, http.MethodPatch, "/redfish/v1/Systems/"+testDomain, bootPatch, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("boot patch status = %d, want 204: %s", resp.StatusCode, data)
	}

	if !backend.called("ConfigureHTTPBoot") {
		t.Fatal("expected ConfigureHTTPBoot to be invoked")
	}

	got := backend.httpBoot
	want := httpBootArgs{
		mac:        "52:54:00:ab:cd:ef",
		address:    "192.168.200.10",
		subnetMask: "255.255.255.0",
		gateway:    "192.168.200.1",
		dns:        []string{"8.8.8.8"},
		bootURL:    "http://192.168.200.1:8882/bootx64.efi",
	}

	if got.mac != want.mac || got.address != want.address || got.subnetMask != want.subnetMask ||
		got.gateway != want.gateway || got.bootURL != want.bootURL ||
		strings.Join(got.dns, ",") != strings.Join(want.dns, ",") {
		t.Fatalf("ConfigureHTTPBoot args = %+v, want %+v", got, want)
	}
}

func TestPatchBootDisabledClearsHTTPBoot(t *testing.T) {
	backend := newFakeBackend()
	server := newTestServer(t, Config{}, backend)

	patch := map[string]any{"Boot": map[string]any{"BootSourceOverrideEnabled": "Disabled"}}

	if resp, _ := do(t, server.Handler(), http.MethodPatch, "/redfish/v1/Systems/"+testDomain, patch, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("boot patch status = %d, want 204", resp.StatusCode)
	}

	if !backend.called("ClearHTTPBoot") {
		t.Fatal("expected ClearHTTPBoot on disabled override")
	}
}

func TestResetDispatch(t *testing.T) {
	cases := []struct {
		resetType string
		wantCall  string
		wantCode  int
	}{
		{"ForceOff", "PowerOff", http.StatusNoContent},
		{"ForceRestart", "Restart", http.StatusNoContent},
		{"bogus", "", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.resetType, func(t *testing.T) {
			backend := newFakeBackend()
			server := newTestServer(t, Config{}, backend)

			path := "/redfish/v1/Systems/" + testDomain + "/Actions/ComputerSystem.Reset"
			resp, _ := do(t, server.Handler(), http.MethodPost, path, map[string]any{"ResetType": tc.resetType}, "")

			if resp.StatusCode != tc.wantCode {
				t.Fatalf("reset %s status = %d, want %d", tc.resetType, resp.StatusCode, tc.wantCode)
			}

			if tc.wantCall != "" && !backend.called(tc.wantCall) {
				t.Fatalf("reset %s did not invoke %s", tc.resetType, tc.wantCall)
			}
		})
	}
}

func TestBiosAndSessionServiceUnsupported(t *testing.T) {
	server := newTestServer(t, Config{}, newFakeBackend())
	h := server.Handler()

	system := "/redfish/v1/Systems/" + testDomain

	for _, path := range []string{system + "/Bios/Settings", "/redfish/v1/SessionService/Sessions"} {
		resp, _ := do(t, h, http.MethodGet, path, nil, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, resp.StatusCode)
		}
	}

	resp, _ := do(t, h, http.MethodGet, "/redfish/v1/nope", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", resp.StatusCode)
	}
}

func TestRecordFormat(t *testing.T) {
	server := newTestServer(t, Config{}, newFakeBackend())

	do(t, server.Handler(), http.MethodGet, "/redfish/v1/", nil, "")

	entries := readRecord(t, server.cfg.Record)
	if len(entries) != 1 {
		t.Fatalf("record entries = %d, want 1", len(entries))
	}

	entry := entries[0]
	for _, key := range []string{"time", "method", "path", "body", "status"} {
		if _, ok := entry[key]; !ok {
			t.Fatalf("record entry missing key %q: %v", key, entry)
		}
	}

	if entry["method"] != "GET" || entry["path"] != "/redfish/v1/" {
		t.Fatalf("unexpected record entry: %v", entry)
	}

	if body, ok := entry["body"].(map[string]any); !ok || len(body) != 0 {
		t.Fatalf("expected empty body object, got %v", entry["body"])
	}
}

func TestQemuArgsBootOrder(t *testing.T) {
	base := qemuSpec{
		name: "smoke-vm", memoryMiB: 4096, vcpus: 2,
		disk: "/tmp/disk.qcow2", ovmfCode: "/ovmf/code.fd", nvram: "/state/vars.fd",
		mac: "52:54:00:ab:cd:ef", tap: "tap-smoke",
		serialSock: "/state/console.sock", qgaSock: "/state/qga.sock",
		qmpSock: "/state/qmp.sock", tpmSock: "/state/tpm/swtpm.sock",
	}

	netFirst := base
	netFirst.bootNetwork = true

	args := strings.Join(qemuArgs(netFirst), " ")
	if !strings.Contains(args, "virtio-net-pci,netdev=net0,mac=52:54:00:ab:cd:ef,bootindex=1") {
		t.Fatalf("expected network bootindex=1: %s", args)
	}

	if !strings.Contains(args, "virtio-blk-pci,drive=disk0,bootindex=2") {
		t.Fatalf("expected disk bootindex=2: %s", args)
	}

	diskFirst := base

	args = strings.Join(qemuArgs(diskFirst), " ")
	if !strings.Contains(args, "virtio-blk-pci,drive=disk0,bootindex=1") {
		t.Fatalf("expected disk bootindex=1: %s", args)
	}

	if !strings.Contains(args, "virtio-net-pci,netdev=net0,mac=52:54:00:ab:cd:ef,bootindex=2") {
		t.Fatalf("expected network bootindex=2: %s", args)
	}
}

func TestQemuArgsSecureBoot(t *testing.T) {
	spec := qemuSpec{
		name: "smoke-vm", memoryMiB: 4096, vcpus: 2,
		disk: "/tmp/disk.qcow2", ovmfCode: "/ovmf/code.fd", nvram: "/state/vars.fd",
		mac: "52:54:00:ab:cd:ef", tap: "tap-http",
	}

	plain := strings.Join(qemuArgs(spec), " ")
	if strings.Contains(plain, "smm=on") || strings.Contains(plain, "property=secure,value=on") {
		t.Fatalf("did not expect secure boot options without SecureBoot: %s", plain)
	}

	spec.secureBoot = true

	secure := strings.Join(qemuArgs(spec), " ")
	if !strings.Contains(secure, "q35,accel=kvm,smm=on") {
		t.Fatalf("expected smm=on machine: %s", secure)
	}

	if !strings.Contains(secure, "driver=cfi.pflash01,property=secure,value=on") {
		t.Fatalf("expected secure pflash global: %s", secure)
	}
}

func TestSubnetCIDR(t *testing.T) {
	got, err := subnetCIDR("192.168.200.1", 24)
	if err != nil {
		t.Fatalf("subnetCIDR: %v", err)
	}

	if got != "192.168.200.0/24" {
		t.Fatalf("expected 192.168.200.0/24, got %s", got)
	}

	if _, err := subnetCIDR("not-an-ip", 24); err == nil {
		t.Fatalf("expected error for invalid address")
	}
}

func TestTapName(t *testing.T) {
	if got := tapName("virbr-smoke"); got != "tap-smoke" {
		t.Fatalf("expected tap-smoke, got %s", got)
	}

	if got := tapName("virbr-a-very-long-bridge-name"); len(got) > 15 {
		t.Fatalf("tap name exceeds 15 chars: %q", got)
	}
}

func readRecord(t *testing.T, path string) []map[string]any {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open record: %v", err)
	}
	defer f.Close() //nolint:errcheck // Test cleanup.

	var entries []map[string]any

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal record line: %v", err)
		}

		entries = append(entries, entry)
	}

	return entries
}
