// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeCH is a minimal stand-in for the cloud-hypervisor VMM REST API. It tracks
// the guest state and records the sequence of mutating actions so tests can
// assert the manager drives the right endpoints.
type fakeCH struct {
	mu    sync.Mutex
	state string
	calls []string
}

func (f *fakeCH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.URL.Path {
	case "/api/v1/vmm.ping":
		w.WriteHeader(http.StatusOK)
	case "/api/v1/vm.info":
		if f.state == "" {
			// No guest created yet: cloud-hypervisor answers non-2xx.
			w.WriteHeader(http.StatusNotFound)
			return
		}

		writeJSON(w, http.StatusOK, chVMInfo{State: f.state})
	case "/api/v1/vm.create":
		f.calls = append(f.calls, "vm.create")
		f.state = "Created"

		w.WriteHeader(http.StatusNoContent)
	case "/api/v1/vm.boot":
		f.calls = append(f.calls, "vm.boot")
		f.state = "Running"

		w.WriteHeader(http.StatusNoContent)
	case "/api/v1/vm.shutdown":
		f.calls = append(f.calls, "vm.shutdown")
		f.state = "Shutdown"

		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeCH) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

// newFakeVMManager starts a fake cloud-hypervisor VMM on a unix socket and
// returns a vmManager wired to it plus the fake for assertions.
func newFakeVMManager(t *testing.T) (*vmManager, *fakeCH) {
	t.Helper()

	socket := filepath.Join(t.TempDir(), "api.sock")

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	fake := &fakeCH{}
	srv := &http.Server{Handler: fake, ReadHeaderTimeout: time.Second}

	go srv.Serve(listener) //nolint:errcheck // serve exits on Close

	t.Cleanup(func() {
		_ = srv.Close() //nolint:errcheck // best-effort
	})

	m := &vmManager{
		cfg:       DefaultConfig(),
		socket:    socket,
		serialLog: "/tmp/serial.log",
		client:    unixHTTPClient(socket),
	}

	return m, fake
}

func TestVMManagerPowerStateNoGuest(t *testing.T) {
	m, _ := newFakeVMManager(t)

	state, err := m.PowerState(context.Background())
	if err != nil {
		t.Fatalf("PowerState: %v", err)
	}

	if state != powerStateOff {
		t.Errorf("PowerState with no guest = %q, want %q", state, powerStateOff)
	}
}

func TestVMManagerPowerOnCreatesAndBoots(t *testing.T) {
	m, fake := newFakeVMManager(t)

	if err := m.powerOn(context.Background()); err != nil {
		t.Fatalf("powerOn: %v", err)
	}

	got := fake.recorded()
	if len(got) != 2 || got[0] != "vm.create" || got[1] != "vm.boot" {
		t.Fatalf("powerOn calls = %v, want [vm.create vm.boot]", got)
	}

	state, err := m.PowerState(context.Background())
	if err != nil {
		t.Fatalf("PowerState: %v", err)
	}

	if state != powerStateOn {
		t.Errorf("PowerState after powerOn = %q, want %q", state, powerStateOn)
	}
}

func TestVMManagerPowerOnIdempotent(t *testing.T) {
	m, fake := newFakeVMManager(t)

	if err := m.powerOn(context.Background()); err != nil {
		t.Fatalf("first powerOn: %v", err)
	}

	if err := m.powerOn(context.Background()); err != nil {
		t.Fatalf("second powerOn: %v", err)
	}

	// The second powerOn should be a no-op (guest already Running).
	got := fake.recorded()
	if len(got) != 2 {
		t.Errorf("calls after two powerOn = %v, want just create+boot once", got)
	}
}

func TestVMManagerPowerOff(t *testing.T) {
	m, fake := newFakeVMManager(t)

	if err := m.powerOn(context.Background()); err != nil {
		t.Fatalf("powerOn: %v", err)
	}

	if err := m.powerOff(context.Background()); err != nil {
		t.Fatalf("powerOff: %v", err)
	}

	got := fake.recorded()
	if len(got) == 0 || got[len(got)-1] != "vm.shutdown" {
		t.Fatalf("powerOff calls = %v, want trailing vm.shutdown", got)
	}

	state, err := m.PowerState(context.Background())
	if err != nil {
		t.Fatalf("PowerState: %v", err)
	}

	if state != powerStateOff {
		t.Errorf("PowerState after powerOff = %q, want %q", state, powerStateOff)
	}
}

func TestVMManagerPowerOffIdempotent(t *testing.T) {
	m, fake := newFakeVMManager(t)

	if err := m.powerOff(context.Background()); err != nil {
		t.Fatalf("powerOff with no guest: %v", err)
	}

	if got := fake.recorded(); len(got) != 0 {
		t.Errorf("powerOff with no guest issued %v, want none", got)
	}
}

func TestVMManagerResetForceRestart(t *testing.T) {
	m, fake := newFakeVMManager(t)

	if err := m.powerOn(context.Background()); err != nil {
		t.Fatalf("powerOn: %v", err)
	}

	if err := m.Reset(context.Background(), resetForceRestart); err != nil {
		t.Fatalf("Reset ForceRestart: %v", err)
	}

	// Expect: create, boot (initial powerOn), then shutdown, boot (restart).
	got := fake.recorded()
	want := []string{"vm.create", "vm.boot", "vm.shutdown", "vm.boot"}

	if len(got) != len(want) {
		t.Fatalf("ForceRestart calls = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ForceRestart calls = %v, want %v", got, want)
		}
	}
}

func TestVMManagerResetDispatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   ResetType
		want string
	}{
		{"on", resetOn, "vm.boot"},
		{"force off", resetForceOff, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, fake := newFakeVMManager(t)

			if err := m.Reset(context.Background(), tc.rt); err != nil {
				t.Fatalf("Reset %s: %v", tc.rt, err)
			}

			got := fake.recorded()

			if tc.want == "" {
				return
			}

			found := false

			for _, c := range got {
				if c == tc.want {
					found = true
				}
			}

			if !found {
				t.Errorf("Reset %s calls = %v, want to include %q", tc.rt, got, tc.want)
			}
		})
	}
}

func TestVMManagerResetUnsupported(t *testing.T) {
	m, _ := newFakeVMManager(t)

	if err := m.Reset(context.Background(), ResetType("GracefulShutdown")); err == nil {
		t.Fatal("expected error for unsupported ResetType")
	}
}
