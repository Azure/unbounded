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

// fakeRunner records invocations and returns scripted results.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	// stdout keyed by the command name; virsh keys use the first arg.
	stdout map[string]string
	code   map[string]int
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{stdout: map[string]string{}, code: map[string]int{}}
}

func (f *fakeRunner) Run(name string, args ...string) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, append([]string{name}, args...))

	key := name
	if name == "virsh" && len(args) >= 3 {
		key = "virsh:" + args[2] // skip --connect qemu:///system
	}

	return f.stdout[key], f.code[key], nil
}

func (f *fakeRunner) called(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, c := range f.calls {
		if c[0] == name {
			return true
		}
	}

	return false
}

func newTestState(t *testing.T, cfg Config, runner Runner) *State {
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

	state, err := NewState(cfg, runner)
	if err != nil {
		t.Fatalf("NewState: %v", err)
	}

	return state
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
	state := newTestState(t, Config{}, newFakeRunner())
	h := state.Handler()

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
	runner := newFakeRunner()
	runner.stdout["virsh:domstate"] = "running\n"
	state := newTestState(t, Config{}, runner)

	resp, data := do(t, state.Handler(), http.MethodGet, "/redfish/v1/Systems/"+testDomain, nil, "")
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
	state := newTestState(t, Config{Username: "smoke", Password: "secret"}, newFakeRunner())
	h := state.Handler()

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
	if got := len(readRecord(t, state.cfg.Record)); got != 1 {
		t.Fatalf("record entries = %d, want 1 (only the authorized request)", got)
	}
}

func TestEthernetInterfaceLifecycle(t *testing.T) {
	base := "/redfish/v1/Systems/" + testDomain + "/EthernetInterfaces"
	state := newTestState(t, Config{}, newFakeRunner())
	h := state.Handler()

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
	state := newTestState(t, Config{}, newFakeRunner())

	cases := []map[string]any{
		{"DHCPv4": map[string]any{"DHCPEnabled": true}, "IPv4StaticAddresses": []any{map[string]any{"Address": "1.2.3.4"}}},
		{"DHCPv4": map[string]any{"DHCPEnabled": false}, "IPv4StaticAddresses": []any{}},
	}
	for _, patch := range cases {
		resp, _ := do(t, state.Handler(), http.MethodPatch, base, patch, "")
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("patch %v status = %d, want 500", patch, resp.StatusCode)
		}
	}
}

func TestPatchBootPxeManagesBootOrder(t *testing.T) {
	runner := newFakeRunner()
	runner.stdout["virsh:dumpxml"] = `<domain><os><type>hvm</type><boot dev="hd"/></os></domain>`
	state := newTestState(t, Config{ManageBootOrder: true}, runner)

	patch := map[string]any{"Boot": map[string]any{"BootSourceOverrideTarget": "Pxe"}}

	resp, _ := do(t, state.Handler(), http.MethodPatch, "/redfish/v1/Systems/"+testDomain, patch, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("boot patch status = %d, want 204", resp.StatusCode)
	}

	if !runner.called("virsh") {
		t.Fatal("expected virsh to be called for boot order management")
	}
}

func TestPatchBootUefiHTTPRequiresStaticAddress(t *testing.T) {
	state := newTestState(t, Config{}, newFakeRunner())

	patch := map[string]any{"Boot": map[string]any{
		"BootSourceOverrideTarget":  "UefiHttp",
		"BootSourceOverrideEnabled": "Continuous",
		"BootSourceOverrideMode":    "UEFI",
		"HttpBootUri":               "http://server/bootx64.efi",
	}}

	resp, data := do(t, state.Handler(), http.MethodPatch, "/redfish/v1/Systems/"+testDomain, patch, "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("uefihttp patch status = %d, want 500", resp.StatusCode)
	}

	if !strings.Contains(string(data), "DHCPv4 was disabled") {
		t.Fatalf("unexpected error body: %s", data)
	}
}

func TestResetDispatch(t *testing.T) {
	cases := []struct {
		resetType string
		wantVirsh string
		wantCode  int
	}{
		{"ForceOff", "destroy", http.StatusNoContent},
		{"ForceRestart", "start", http.StatusNoContent},
		{"bogus", "", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.resetType, func(t *testing.T) {
			runner := newFakeRunner()
			state := newTestState(t, Config{}, runner)

			path := "/redfish/v1/Systems/" + testDomain + "/Actions/ComputerSystem.Reset"
			resp, _ := do(t, state.Handler(), http.MethodPost, path, map[string]any{"ResetType": tc.resetType}, "")

			if resp.StatusCode != tc.wantCode {
				t.Fatalf("reset %s status = %d, want %d", tc.resetType, resp.StatusCode, tc.wantCode)
			}

			if tc.wantVirsh != "" && !runner.called("virsh") {
				t.Fatalf("reset %s did not invoke virsh", tc.resetType)
			}
		})
	}
}

func TestBiosAndSessionServiceUnsupported(t *testing.T) {
	state := newTestState(t, Config{}, newFakeRunner())
	h := state.Handler()

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
	state := newTestState(t, Config{}, newFakeRunner())

	do(t, state.Handler(), http.MethodGet, "/redfish/v1/", nil, "")

	entries := readRecord(t, state.cfg.Record)
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

func TestRewriteBootOrder(t *testing.T) {
	in := `<domain><os><type>hvm</type><boot dev="hd"/><boot dev="network"/></os><devices/></domain>`

	out, err := rewriteBootOrder(in, []string{"network", "hd"})
	if err != nil {
		t.Fatalf("rewriteBootOrder: %v", err)
	}

	network := strings.Index(out, `dev="network"`)
	hd := strings.Index(out, `dev="hd"`)

	if network < 0 || hd < 0 {
		t.Fatalf("missing boot entries: %s", out)
	}

	if network > hd {
		t.Fatalf("expected network before hd: %s", out)
	}

	if strings.Count(out, "<boot") != 2 {
		t.Fatalf("expected exactly two boot entries: %s", out)
	}

	if !strings.Contains(out, "<type>hvm</type>") {
		t.Fatalf("rewrite dropped os content: %s", out)
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
