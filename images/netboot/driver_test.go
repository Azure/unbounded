// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// driverBlock returns the driver function section of the installer init
// script.
func driverBlock(t *testing.T) string {
	t.Helper()

	content, err := os.ReadFile("assets/init")
	if err != nil {
		t.Fatal(err)
	}

	_, rest, ok := strings.Cut(string(content), "# BEGIN driver functions\n")
	if !ok {
		t.Fatal("init has no driver block")
	}

	block, _, ok := strings.Cut(rest, "# END driver functions\n")
	if !ok {
		t.Fatal("driver block is not terminated")
	}

	return block
}

// runLoadPCIDrivers runs load_pci_drivers against a fake PCI device tree with
// a modprobe stub that records each alias and fails for any alias in failing.
func runLoadPCIDrivers(t *testing.T, shell []string, pci string, failing ...string) (string, error) {
	t.Helper()

	log := filepath.Join(t.TempDir(), "modprobe.log")
	prelude := "set -u\n" +
		"UB_PCI='" + pci + "'\n" +
		"modprobe_stub() { echo \"$*\" >> '" + log + "'; case \"$1\" in " +
		strings.Join(append([]string{"__none__"}, failing...), "|") + ") return 1;; esac; }\n" +
		"UB_MODPROBE=modprobe_stub\n"

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	args := append(append([]string{}, shell[1:]...), "-c", prelude+driverBlock(t)+"load_pci_drivers; echo rc=$?")
	out, err := exec.CommandContext(ctx, shell[0], args...).CombinedOutput()

	b, readErr := os.ReadFile(log)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}

	if !strings.Contains(string(out), "rc=0") {
		t.Fatalf("load_pci_drivers did not return 0: %s", out)
	}

	return string(b), err
}

func writePCIDevice(t *testing.T, root, slot, alias string) {
	t.Helper()

	dir := filepath.Join(root, slot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if alias == "" {
		return
	}

	if err := os.WriteFile(filepath.Join(dir, "modalias"), []byte(alias+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPCIDrivers(t *testing.T) {
	const (
		megaraid = "pci:v00001000d000010E6sv000015D9sd00001D2Fbc01sc04i00"
		mlx      = "pci:v000015B3d00001021sv000015B3sd00000041bc02sc00i00"
	)

	for _, sh := range shells(t) {
		t.Run(sh[0], func(t *testing.T) {
			t.Run("loads every device alias", func(t *testing.T) {
				pci := t.TempDir()
				writePCIDevice(t, pci, "0000:d1:00.0", megaraid)
				writePCIDevice(t, pci, "0000:31:00.0", mlx)

				got, err := runLoadPCIDrivers(t, sh, pci)
				if err != nil {
					t.Fatal(err)
				}

				for _, want := range []string{megaraid, mlx} {
					if !strings.Contains(got, want+"\n") {
						t.Errorf("modprobe log %q lacks %q", got, want)
					}
				}
			})

			t.Run("continues past a failed alias", func(t *testing.T) {
				pci := t.TempDir()
				writePCIDevice(t, pci, "0000:01:00.0", mlx)
				writePCIDevice(t, pci, "0000:d1:00.0", megaraid)

				got, err := runLoadPCIDrivers(t, sh, pci, mlx)
				if err != nil {
					t.Fatal(err)
				}

				if !strings.Contains(got, megaraid+"\n") {
					t.Errorf("modprobe log %q lacks %q after a failure", got, megaraid)
				}
			})

			t.Run("skips devices without an alias", func(t *testing.T) {
				pci := t.TempDir()
				writePCIDevice(t, pci, "0000:00:00.0", "")
				writePCIDevice(t, pci, "0000:d1:00.0", megaraid)

				got, err := runLoadPCIDrivers(t, sh, pci)
				if err != nil {
					t.Fatal(err)
				}

				if got != megaraid+"\n" {
					t.Errorf("modprobe log = %q, want only %q", got, megaraid)
				}
			})

			t.Run("tolerates a missing device tree", func(t *testing.T) {
				got, err := runLoadPCIDrivers(t, sh, filepath.Join(t.TempDir(), "missing"))
				if err != nil {
					t.Fatal(err)
				}

				if got != "" {
					t.Errorf("modprobe log = %q, want empty", got)
				}
			})
		})
	}
}
