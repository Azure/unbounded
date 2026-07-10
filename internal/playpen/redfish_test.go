// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	metalredfish "github.com/Azure/unbounded/internal/metalman/redfish"
)

func TestRedfishMetalmanCompatibility(t *testing.T) {
	cfg := normalizedTestConfig(t, ArchAMD64)
	cfg.BMCUsername = "admin"
	cfg.BMCPassword = "secret"
	cfg.BMCDeviceID = "playpen-system"

	var (
		argsMu sync.Mutex
		starts [][]string
	)

	manager := newVMManager(
		cfg,
		nil,
		Firmware{},
		func(_ string, args ...string) *exec.Cmd {
			argsMu.Lock()

			starts = append(starts, append([]string(nil), args...))
			argsMu.Unlock()

			cmd := exec.Command(os.Args[0], "-test.run=TestVMProcessHelper")

			cmd.Env = append(os.Environ(), "PLAYPEN_VM_HELPER=wait")

			return cmd
		},
		func(context.Context, Config) (tpmProcess, error) {
			return &fakeTPMProcess{closed: make(chan struct{})}, nil
		},
		time.Second,
	)

	t.Cleanup(func() { _ = manager.Close() })

	server := httptest.NewTLSServer(NewRedfishHandler(manager, cfg))
	t.Cleanup(server.Close)

	fingerprint, err := metalredfish.CaptureFingerprint(t.Context(), server.URL)
	if err != nil {
		t.Fatalf("CaptureFingerprint() error = %v", err)
	}

	wantFingerprint := certificateFingerprint(server)
	if fingerprint != wantFingerprint {
		t.Fatalf("fingerprint = %q, want %q", fingerprint, wantFingerprint)
	}

	client, err := metalredfish.Dial(t.Context(), server.URL, fingerprint, cfg.BMCUsername, cfg.BMCPassword, "")
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}

	t.Cleanup(client.Close)

	state, err := client.PowerState(t.Context())
	if err != nil {
		t.Fatalf("PowerState() error = %v", err)
	}

	if state != metalredfish.PowerOff {
		t.Fatalf("initial power state = %s, want %s", state, metalredfish.PowerOff)
	}

	if err := client.SetBootOverride(t.Context(), metalredfish.BootTargetPxe, metalredfish.BootOnce); err != nil {
		t.Fatalf("SetBootOverride() error = %v", err)
	}

	if err := client.Reset(t.Context(), metalredfish.ResetOn); err != nil {
		t.Fatalf("Reset(On) error = %v", err)
	}

	state, err = client.PowerState(t.Context())
	if err != nil {
		t.Fatalf("PowerState() after On error = %v", err)
	}

	if state != metalredfish.PowerOn {
		t.Fatalf("power state after On = %s, want %s", state, metalredfish.PowerOn)
	}

	boot, err := client.GetBootConfig(t.Context())
	if err != nil {
		t.Fatalf("GetBootConfig() error = %v", err)
	}

	if boot.Target != metalredfish.BootTargetPxe || boot.Enabled != metalredfish.BootDisabled {
		t.Fatalf("consumed one-time boot = %+v, want Pxe/Disabled", boot)
	}

	argsMu.Lock()
	startedArgs := strings.Join(starts[0], " ")
	argsMu.Unlock()

	if !strings.Contains(startedArgs, "virtio-net-pci,netdev=net0,mac="+cfg.MAC+",bootindex=1") {
		t.Fatalf("one-time PXE override was not applied to QEMU: %s", startedArgs)
	}

	if err := client.Reset(t.Context(), metalredfish.ResetForceOff); err != nil {
		t.Fatalf("Reset(ForceOff) error = %v", err)
	}

	if err := client.DisableBootOverride(t.Context()); err != nil {
		t.Fatalf("DisableBootOverride() error = %v", err)
	}
}

func TestRedfishAuthenticationAndValidation(t *testing.T) {
	cfg := normalizedTestConfig(t, ArchAMD64)
	cfg.BMCUsername = "admin"
	cfg.BMCPassword = "secret"
	manager := newVMManager(cfg, nil, Firmware{}, exec.Command, func(context.Context, Config) (tpmProcess, error) {
		return &fakeTPMProcess{closed: make(chan struct{})}, nil
	}, time.Second)
	server := httptest.NewServer(NewRedfishHandler(manager, cfg))
	t.Cleanup(server.Close)

	resp, err := server.Client().Get(server.URL + "/redfish/v1/Systems/1")
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, server.URL+"/redfish/v1/Systems/1", strings.NewReader(`{"Boot":{"BootSourceOverrideTarget":"Usb"}}`))
	if err != nil {
		t.Fatal(err)
	}

	request.SetBasicAuth("admin", "secret")
	request.Header.Set("Content-Type", "application/json")

	resp, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid boot target status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	if boot := manager.BootConfig(); boot.Target != BootTargetPxe || boot.Enabled != BootContinuous {
		t.Fatalf("invalid PATCH changed boot config: %+v", boot)
	}
}

func certificateFingerprint(server *httptest.Server) string {
	sum := sha256.Sum256(server.Certificate().Raw)
	parts := make([]string, len(sum))

	for i, value := range sum {
		parts[i] = fmt.Sprintf("%02x", value)
	}

	return strings.Join(parts, ":")
}
