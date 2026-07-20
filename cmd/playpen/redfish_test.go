// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeVMPower is a test double for the guest lifecycle the Redfish server drives.
type fakeVMPower struct {
	mu     sync.Mutex
	state  string
	resets []ResetType
	err    error
}

func (f *fakeVMPower) PowerState(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.state, f.err
}

func (f *fakeVMPower) Reset(_ context.Context, rt ResetType) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}

	f.resets = append(f.resets, rt)

	switch rt {
	case resetOn, resetForceRestart:
		f.state = powerStateOn
	case resetForceOff:
		f.state = powerStateOff
	}

	return nil
}

func testRedfishServer(vm vmPower) *httptest.Server {
	cfg := DefaultConfig()
	return httptest.NewServer(newRedfishServer(cfg, vm))
}

func TestRedfishServiceRootUnauthenticated(t *testing.T) {
	srv := testRedfishServer(&fakeVMPower{state: powerStateOff})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/redfish/v1/")
	if err != nil {
		t.Fatalf("GET service root: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("service root status = %d, want 200", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if _, ok := body["Systems"]; !ok {
		t.Errorf("service root missing Systems: %v", body)
	}
}

func TestRedfishRequiresAuth(t *testing.T) {
	srv := testRedfishServer(&fakeVMPower{state: powerStateOff})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/redfish/v1/Systems")
	if err != nil {
		t.Fatalf("GET Systems: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Systems without auth status = %d, want 401", resp.StatusCode)
	}
}

func TestRedfishBasicAuth(t *testing.T) {
	srv := testRedfishServer(&fakeVMPower{state: powerStateOn})
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/redfish/v1/Systems", nil)
	req.SetBasicAuth("admin", "password")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET Systems: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Systems with basic auth status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Members []struct {
			ODataID string `json:"@odata.id"`
		} `json:"Members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Members) != 1 || !strings.HasSuffix(body.Members[0].ODataID, "/1") {
		t.Errorf("Systems Members = %+v, want single member ending /1", body.Members)
	}
}

func TestRedfishBadBasicAuth(t *testing.T) {
	srv := testRedfishServer(&fakeVMPower{state: powerStateOff})
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/redfish/v1/Systems", nil)
	req.SetBasicAuth("admin", "wrong")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET Systems: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Systems with wrong password status = %d, want 401", resp.StatusCode)
	}
}

func TestRedfishSessionAuth(t *testing.T) {
	srv := testRedfishServer(&fakeVMPower{state: powerStateOn})
	defer srv.Close()

	// Create a session.
	sessionBody, _ := json.Marshal(map[string]string{"UserName": "admin", "Password": "password"})

	resp, err := http.Post(srv.URL+"/redfish/v1/SessionService/Sessions", "application/json", bytes.NewReader(sessionBody))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session status = %d, want 201", resp.StatusCode)
	}

	token := resp.Header.Get("X-Auth-Token")
	if token == "" {
		t.Fatal("create session did not return X-Auth-Token")
	}

	if loc := resp.Header.Get("Location"); !strings.Contains(loc, token) {
		t.Errorf("Location %q does not contain token", loc)
	}

	// Use the token.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/redfish/v1/Systems/1", nil)
	req.Header.Set("X-Auth-Token", token)

	sysResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET system with token: %v", err)
	}
	defer sysResp.Body.Close()

	if sysResp.StatusCode != http.StatusOK {
		t.Fatalf("GET system with token status = %d, want 200", sysResp.StatusCode)
	}
}

func TestRedfishSessionBadCredentials(t *testing.T) {
	srv := testRedfishServer(&fakeVMPower{state: powerStateOff})
	defer srv.Close()

	sessionBody, _ := json.Marshal(map[string]string{"UserName": "admin", "Password": "nope"})

	resp, err := http.Post(srv.URL+"/redfish/v1/SessionService/Sessions", "application/json", bytes.NewReader(sessionBody))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("create session with bad creds status = %d, want 401", resp.StatusCode)
	}
}

func TestRedfishGetSystemPowerState(t *testing.T) {
	vm := &fakeVMPower{state: powerStateOn}

	srv := testRedfishServer(vm)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/redfish/v1/Systems/1", nil)
	req.SetBasicAuth("admin", "password")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET system: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		PowerState string `json:"PowerState"`
		ID         string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.PowerState != powerStateOn {
		t.Errorf("PowerState = %q, want %q", body.PowerState, powerStateOn)
	}

	if body.ID != "1" {
		t.Errorf("Id = %q, want 1", body.ID)
	}
}

func TestRedfishReset(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resetType ResetType
		wantState string
	}{
		{"power on", resetOn, powerStateOn},
		{"force off", resetForceOff, powerStateOff},
		{"force restart", resetForceRestart, powerStateOn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vm := &fakeVMPower{state: powerStateOff}

			srv := testRedfishServer(vm)
			defer srv.Close()

			body, _ := json.Marshal(map[string]ResetType{"ResetType": tc.resetType})

			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", bytes.NewReader(body))
			req.SetBasicAuth("admin", "password")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("reset: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("reset status = %d, want 204", resp.StatusCode)
			}

			if len(vm.resets) != 1 || vm.resets[0] != tc.resetType {
				t.Errorf("resets = %v, want [%s]", vm.resets, tc.resetType)
			}

			if vm.state != tc.wantState {
				t.Errorf("state after reset = %q, want %q", vm.state, tc.wantState)
			}
		})
	}
}

func TestRedfishPatchBootOverride(t *testing.T) {
	srv := testRedfishServer(&fakeVMPower{state: powerStateOff})
	defer srv.Close()

	// Apply a boot override the way metalman's SetBootOverride does.
	patch := bytes.NewReader([]byte(`{"Boot":{"BootSourceOverrideTarget":"Pxe","BootSourceOverrideEnabled":"Continuous"}}`))

	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/redfish/v1/Systems/1", patch)
	req.SetBasicAuth("admin", "password")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch status = %d, want 204", resp.StatusCode)
	}

	// A subsequent GET (metalman's GetBootConfig) must reflect the override.
	getReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/redfish/v1/Systems/1", nil)
	getReq.SetBasicAuth("admin", "password")

	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("get system: %v", err)
	}
	defer getResp.Body.Close()

	var body struct {
		Boot struct {
			BootSourceOverrideTarget  string `json:"BootSourceOverrideTarget"`
			BootSourceOverrideEnabled string `json:"BootSourceOverrideEnabled"`
		} `json:"Boot"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Boot.BootSourceOverrideTarget != "Pxe" {
		t.Errorf("BootSourceOverrideTarget = %q, want Pxe", body.Boot.BootSourceOverrideTarget)
	}

	if body.Boot.BootSourceOverrideEnabled != "Continuous" {
		t.Errorf("BootSourceOverrideEnabled = %q, want Continuous", body.Boot.BootSourceOverrideEnabled)
	}
}

func TestRedfishPatchDisableBootOverride(t *testing.T) {
	srv := testRedfishServer(&fakeVMPower{state: powerStateOff})
	defer srv.Close()

	// metalman's DisableBootOverride only sends BootSourceOverrideEnabled.
	patch := bytes.NewReader([]byte(`{"Boot":{"BootSourceOverrideEnabled":"Disabled"}}`))

	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/redfish/v1/Systems/1", patch)
	req.SetBasicAuth("admin", "password")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch status = %d, want 204", resp.StatusCode)
	}

	getReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/redfish/v1/Systems/1", nil)
	getReq.SetBasicAuth("admin", "password")

	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("get system: %v", err)
	}
	defer getResp.Body.Close()

	var body struct {
		Boot struct {
			BootSourceOverrideEnabled string `json:"BootSourceOverrideEnabled"`
		} `json:"Boot"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Boot.BootSourceOverrideEnabled != "Disabled" {
		t.Errorf("BootSourceOverrideEnabled = %q, want Disabled", body.Boot.BootSourceOverrideEnabled)
	}
}

func TestRedfishUnknownPath(t *testing.T) {
	srv := testRedfishServer(&fakeVMPower{state: powerStateOff})
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/redfish/v1/Chassis", nil)
	req.SetBasicAuth("admin", "password")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET unknown: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", resp.StatusCode)
	}
}

func TestRedfishGetSystemAdvertisesHTTPBootAndLinks(t *testing.T) {
	srv := testRedfishServer(&fakeVMPower{state: powerStateOff})
	defer srv.Close()

	getReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/redfish/v1/Systems/1", nil)
	getReq.SetBasicAuth("admin", "password")

	resp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("get system: %v", err)
	}
	defer resp.Body.Close()

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// metalman's GetBootConfig sets HasHTTPBootURI only when the HttpBootUri key
	// is present, so it must always be emitted (even empty) to drive the native
	// HTTP boot path.
	boot, ok := raw["Boot"].(map[string]any)
	if !ok {
		t.Fatalf("Boot missing or wrong type: %v", raw["Boot"])
	}

	if _, ok := boot["HttpBootUri"]; !ok {
		t.Errorf("Boot missing HttpBootUri key: %v", boot)
	}

	for _, link := range []string{"EthernetInterfaces", "Bios"} {
		sub, ok := raw[link].(map[string]any)
		if !ok {
			t.Fatalf("%s link missing or wrong type: %v", link, raw[link])
		}

		if _, ok := sub["@odata.id"]; !ok {
			t.Errorf("%s link missing @odata.id: %v", link, sub)
		}
	}
}

func TestRedfishEthernetInterfaces(t *testing.T) {
	srv := testRedfishServer(&fakeVMPower{state: powerStateOff})
	defer srv.Close()

	// The collection must have exactly one member (the guest NIC).
	collResp := doAuthed(t, http.MethodGet, srv.URL+"/redfish/v1/Systems/1/EthernetInterfaces", nil)
	defer collResp.Body.Close()

	if collResp.StatusCode != http.StatusOK {
		t.Fatalf("GET EthernetInterfaces status = %d, want 200", collResp.StatusCode)
	}

	var coll struct {
		Members []struct {
			ODataID string `json:"@odata.id"`
		} `json:"Members"`
	}
	if err := json.NewDecoder(collResp.Body).Decode(&coll); err != nil {
		t.Fatalf("decode collection: %v", err)
	}

	if len(coll.Members) != 1 || !strings.HasSuffix(coll.Members[0].ODataID, "/EthernetInterfaces/"+redfishNICID) {
		t.Fatalf("collection Members = %+v, want single %s", coll.Members, redfishNICID)
	}

	memberURL := srv.URL + "/redfish/v1/Systems/1/EthernetInterfaces/" + redfishNICID

	// The member reports the guest MAC so metalman can match it.
	memResp := doAuthed(t, http.MethodGet, memberURL, nil)
	defer memResp.Body.Close()

	var member struct {
		MACAddress string `json:"MACAddress"`
		DHCPv4     struct {
			DHCPEnabled bool `json:"DHCPEnabled"`
		} `json:"DHCPv4"`
	}
	if err := json.NewDecoder(memResp.Body).Decode(&member); err != nil {
		t.Fatalf("decode member: %v", err)
	}

	wantMAC := normalizeMAC(DefaultConfig().VMMAC)
	if member.MACAddress != wantMAC {
		t.Errorf("MACAddress = %q, want %q", member.MACAddress, wantMAC)
	}

	if !member.DHCPv4.DHCPEnabled {
		t.Errorf("DHCPEnabled = false initially, want true")
	}

	// metalman's SetStaticIPv4 disables DHCP and sets a static address.
	patch := `{"DHCPv4":{"DHCPEnabled":false},"IPv4StaticAddresses":[{"Address":"10.0.0.5","SubnetMask":"255.255.255.0","Gateway":"10.0.0.1"}],"StaticNameServers":["10.0.0.53"]}`

	patchResp := doAuthed(t, http.MethodPatch, memberURL, bytes.NewReader([]byte(patch)))
	defer patchResp.Body.Close()

	if patchResp.StatusCode != http.StatusNoContent {
		t.Fatalf("PATCH member status = %d, want 204", patchResp.StatusCode)
	}

	// The GET must reflect the static config.
	afterResp := doAuthed(t, http.MethodGet, memberURL, nil)
	defer afterResp.Body.Close()

	var after struct {
		DHCPv4 struct {
			DHCPEnabled bool `json:"DHCPEnabled"`
		} `json:"DHCPv4"`
		IPv4StaticAddresses []struct {
			Address    string `json:"Address"`
			SubnetMask string `json:"SubnetMask"`
			Gateway    string `json:"Gateway"`
		} `json:"IPv4StaticAddresses"`
		StaticNameServers []string `json:"StaticNameServers"`
	}
	if err := json.NewDecoder(afterResp.Body).Decode(&after); err != nil {
		t.Fatalf("decode after: %v", err)
	}

	if after.DHCPv4.DHCPEnabled {
		t.Errorf("DHCPEnabled = true after PATCH, want false")
	}

	if len(after.IPv4StaticAddresses) != 1 || after.IPv4StaticAddresses[0].Address != "10.0.0.5" {
		t.Errorf("IPv4StaticAddresses = %+v, want single 10.0.0.5", after.IPv4StaticAddresses)
	}

	if len(after.StaticNameServers) != 1 || after.StaticNameServers[0] != "10.0.0.53" {
		t.Errorf("StaticNameServers = %v, want [10.0.0.53]", after.StaticNameServers)
	}
}

func TestRedfishBiosSettings(t *testing.T) {
	srv := testRedfishServer(&fakeVMPower{state: powerStateOff})
	defer srv.Close()

	biosURL := srv.URL + "/redfish/v1/Systems/1/Bios/Settings"

	// metalman's SetBIOSHTTPBootURI PATCHes the UrlBootFile attribute.
	patch := `{"Attributes":{"UrlBootFile":"http://example.com/boot.efi"}}`

	patchResp := doAuthed(t, http.MethodPatch, biosURL, bytes.NewReader([]byte(patch)))
	defer patchResp.Body.Close()

	if patchResp.StatusCode != http.StatusNoContent {
		t.Fatalf("PATCH Bios/Settings status = %d, want 204", patchResp.StatusCode)
	}

	// metalman's GetBIOSHTTPBootURI reads it back.
	getResp := doAuthed(t, http.MethodGet, biosURL, nil)
	defer getResp.Body.Close()

	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET Bios/Settings status = %d, want 200", getResp.StatusCode)
	}

	var body struct {
		Attributes map[string]any `json:"Attributes"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Attributes["UrlBootFile"] != "http://example.com/boot.efi" {
		t.Errorf("UrlBootFile = %v, want http://example.com/boot.efi", body.Attributes["UrlBootFile"])
	}
}

// doAuthed performs an authenticated request and fails the test on transport error.
func doAuthed(t *testing.T, method, url string, body *bytes.Reader) *http.Response {
	t.Helper()

	var reqBody io.Reader
	if body != nil {
		reqBody = body
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	req.SetBasicAuth("admin", "password")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}

	return resp
}

func TestSelfSignedCert(t *testing.T) {
	cert, fingerprint, err := selfSignedCert("172.31.99.1")
	if err != nil {
		t.Fatalf("selfSignedCert: %v", err)
	}

	if len(cert.Certificate) == 0 {
		t.Fatal("expected a certificate")
	}

	// Fingerprint is colon-separated hex of 32 bytes: 32 hex pairs -> 95 chars.
	if got := strings.Count(fingerprint, ":"); got != 31 {
		t.Errorf("fingerprint colon count = %d, want 31 (%q)", got, fingerprint)
	}
}
