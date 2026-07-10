// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type fakeTPMProcess struct {
	closes atomic.Int64
	closed chan struct{}
	once   sync.Once
}

func (p *fakeTPMProcess) Close() error {
	p.closes.Add(1)
	p.once.Do(func() { close(p.closed) })

	return nil
}

func TestVMManagerConcurrentLifecycle(t *testing.T) {
	cfg := normalizedTestConfig(t, ArchAMD64)

	var starts atomic.Int64

	tpm := &fakeTPMProcess{closed: make(chan struct{})}
	manager := newVMManager(
		cfg,
		nil,
		Firmware{},
		func(_ string, args ...string) *exec.Cmd {
			starts.Add(1)

			cmd := exec.Command(os.Args[0], "-test.run=TestVMProcessHelper")

			cmd.Env = append(os.Environ(), "PLAYPEN_VM_HELPER=wait")

			return cmd
		},
		func(context.Context, Config) (tpmProcess, error) { return tpm, nil },
		time.Second,
	)

	var wg sync.WaitGroup

	errCh := make(chan error, 32)

	for range 32 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			errCh <- manager.Start(t.Context())
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
	}

	if got := starts.Load(); got != 1 {
		t.Fatalf("QEMU starts = %d, want 1", got)
	}

	if state := manager.PowerState(); state != PowerOn {
		t.Fatalf("PowerState() = %s, want %s", state, PowerOn)
	}

	if err := manager.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	if state := manager.PowerState(); state != PowerOff {
		t.Fatalf("PowerState() after stop = %s, want %s", state, PowerOff)
	}

	if got := tpm.closes.Load(); got != 1 {
		t.Fatalf("TPM closes = %d, want 1", got)
	}

	if err := manager.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
}

func TestVMManagerBootOverrideAppliedOnRestartAndOnceConsumed(t *testing.T) {
	cfg := normalizedTestConfig(t, ArchAMD64)

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

	if err := manager.Start(t.Context()); err != nil {
		t.Fatalf("initial Start() error = %v", err)
	}

	if err := manager.Stop(); err != nil {
		t.Fatalf("initial Stop() error = %v", err)
	}

	if err := manager.SetBootConfig(BootConfig{Target: BootTargetHdd, Enabled: BootOnce}); err != nil {
		t.Fatalf("SetBootConfig() error = %v", err)
	}

	if err := manager.Start(t.Context()); err != nil {
		t.Fatalf("restart Start() error = %v", err)
	}

	argsMu.Lock()
	if len(starts) != 2 {
		argsMu.Unlock()
		t.Fatalf("QEMU starts = %d, want 2", len(starts))
	}

	first := strings.Join(starts[0], " ")
	second := strings.Join(starts[1], " ")
	argsMu.Unlock()

	if !strings.Contains(first, "virtio-net-pci,netdev=net0,mac="+cfg.MAC+",bootindex=1") ||
		!strings.Contains(first, "virtio-blk-pci,drive=disk0,bootindex=2") {
		t.Fatalf("initial QEMU boot order is not PXE first: %s", first)
	}

	if !strings.Contains(second, "virtio-net-pci,netdev=net0,mac="+cfg.MAC+",bootindex=2") ||
		!strings.Contains(second, "virtio-blk-pci,drive=disk0,bootindex=1") {
		t.Fatalf("restart QEMU boot order is not HDD first: %s", second)
	}

	if boot := manager.BootConfig(); boot.Target != BootTargetHdd || boot.Enabled != BootDisabled {
		t.Fatalf("consumed boot config = %+v, want Hdd/Disabled", boot)
	}

	if err := manager.Stop(); err != nil {
		t.Fatalf("final Stop() error = %v", err)
	}
}

func TestVMManagerUnexpectedExitCleansUpTPM(t *testing.T) {
	cfg := normalizedTestConfig(t, ArchAMD64)
	tpm := &fakeTPMProcess{closed: make(chan struct{})}

	manager := newVMManager(
		cfg,
		nil,
		Firmware{},
		func(_ string, _ ...string) *exec.Cmd {
			cmd := exec.Command(os.Args[0], "-test.run=TestVMProcessHelper")

			cmd.Env = append(os.Environ(), "PLAYPEN_VM_HELPER=exit")

			return cmd
		},
		func(context.Context, Config) (tpmProcess, error) { return tpm, nil },
		time.Second,
	)

	if err := manager.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	select {
	case <-tpm.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("TPM was not closed after unexpected QEMU exit")
	}

	if state := manager.PowerState(); state != PowerOff {
		t.Fatalf("PowerState() = %s, want %s", state, PowerOff)
	}
}

func TestVMManagerStopKillsUnresponsiveQEMU(t *testing.T) {
	cfg := normalizedTestConfig(t, ArchAMD64)
	tpm := &fakeTPMProcess{closed: make(chan struct{})}
	ready := t.TempDir() + "/ready"

	manager := newVMManager(
		cfg,
		nil,
		Firmware{},
		func(_ string, _ ...string) *exec.Cmd {
			cmd := exec.Command(os.Args[0], "-test.run=TestVMProcessHelper")

			cmd.Env = append(os.Environ(), "PLAYPEN_VM_HELPER=ignore-term", "PLAYPEN_VM_HELPER_READY="+ready)

			return cmd
		},
		func(context.Context, Config) (tpmProcess, error) { return tpm, nil },
		50*time.Millisecond,
	)

	if err := manager.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)

	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat helper ready marker: %v", err)
		}

		if time.Now().After(deadline) {
			t.Fatal("unresponsive QEMU helper did not become ready")
		}

		time.Sleep(10 * time.Millisecond)
	}

	if err := manager.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	select {
	case <-tpm.closed:
	case <-time.After(time.Second):
		t.Fatal("TPM was not closed after forced QEMU shutdown")
	}
}

func TestVMProcessHelper(t *testing.T) {
	switch os.Getenv("PLAYPEN_VM_HELPER") {
	case "":
		return
	case "exit":
		return
	case "wait":
		signals := make(chan os.Signal, 1)

		signal.Notify(signals, syscall.SIGTERM)
		defer signal.Stop(signals)

		<-signals
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)

		if err := os.WriteFile(os.Getenv("PLAYPEN_VM_HELPER_READY"), []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}

		select {}
	default:
		t.Fatalf("unknown helper mode")
	}
}
