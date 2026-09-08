// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const fstabPath = "/etc/fstab"

type disableSwap struct {
	log *slog.Logger
}

// DisableSwap returns a task that disables swap on the host. Kubernetes
// requires swap to be off so the kubelet memory management and pod QoS
// guarantees work correctly. The task runs swapoff -a and comments out any
// swap entries in /etc/fstab so swap stays disabled across reboots. It also
// masks any active systemd swap units, such as Fedora's zram swap unit, because
// those are generated outside of /etc/fstab.
func DisableSwap(log *slog.Logger) phases.Task {
	return &disableSwap{log: log}
}

func (d *disableSwap) Name() string { return "disable-swap" }

func (d *disableSwap) Do(ctx context.Context) error {
	if err := d.disableSystemdSwapUnits(ctx); err != nil {
		return err
	}

	if err := executil.RunCmd(ctx, d.log, swapoff(), "-a"); err != nil {
		return fmt.Errorf("swapoff -a: %w", err)
	}

	if err := commentOutSwapInFstab(fstabPath); err != nil {
		return fmt.Errorf("commenting out swap in %s: %w", fstabPath, err)
	}

	return nil
}

func (d *disableSwap) disableSystemdSwapUnits(ctx context.Context) error {
	out, err := executil.OutputCmdAt(ctx, d.log, slog.LevelDebug,
		"systemctl", "list-units", "--type=swap", "--all", "--no-legend", "--plain")
	if err != nil {
		d.log.DebugContext(ctx, "listing systemd swap units failed, continuing with swapoff", "err", err)

		return nil
	}

	units := parseSystemdSwapUnits(out)
	for _, unit := range zramSetupUnitsForSwapUnits(units) {
		if err := executil.RunCmdAt(ctx, d.log, slog.LevelInfo, executil.Systemctl(), "mask", "--now", unit); err != nil {
			return fmt.Errorf("masking systemd zram setup unit %s: %w", unit, err)
		}
	}

	for _, unit := range systemdSwapUnitsToMask(units) {
		if err := executil.RunCmdAt(ctx, d.log, slog.LevelInfo, executil.Systemctl(), "mask", "--now", unit); err != nil {
			return fmt.Errorf("masking systemd swap unit %s: %w", unit, err)
		}
	}

	return nil
}

func parseSystemdSwapUnits(out string) []string {
	seen := map[string]struct{}{}

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		unit := fields[0]
		if strings.HasSuffix(unit, ".swap") {
			seen[unit] = struct{}{}
		}
	}

	units := make([]string, 0, len(seen))
	for unit := range seen {
		units = append(units, unit)
	}

	sort.Strings(units)

	return units
}

func zramSetupUnitsForSwapUnits(swapUnits []string) []string {
	seen := map[string]struct{}{}

	for _, swapUnit := range swapUnits {
		device, ok := zramDeviceFromSwapUnit(swapUnit)
		if !ok {
			continue
		}

		seen["systemd-zram-setup@"+device+".service"] = struct{}{}
	}

	units := make([]string, 0, len(seen))
	for unit := range seen {
		units = append(units, unit)
	}

	sort.Strings(units)

	return units
}

func systemdSwapUnitsToMask(swapUnits []string) []string {
	units := make([]string, 0, len(swapUnits))
	for _, unit := range swapUnits {
		if isDiskBySwapUnit(unit) {
			continue
		}

		units = append(units, unit)
	}

	return units
}

func isDiskBySwapUnit(unit string) bool {
	return strings.HasPrefix(unit, `dev-disk-by\x2d`) && strings.HasSuffix(unit, ".swap")
}

func zramDeviceFromSwapUnit(unit string) (string, bool) {
	if !strings.HasSuffix(unit, ".swap") {
		return "", false
	}

	name := strings.TrimSuffix(unit, ".swap")
	if strings.HasPrefix(name, `dev-disk-by\x2dlabel-zram`) {
		device := strings.TrimPrefix(name, `dev-disk-by\x2dlabel-`)
		if device == "zram" {
			return "", false
		}

		return device, true
	}

	if !strings.HasPrefix(name, "dev-zram") {
		return "", false
	}

	device := strings.TrimPrefix(name, "dev-")
	if device == "" {
		return "", false
	}

	return device, true
}

// commentOutSwapInFstab reads the fstab file at the given path, comments out
// any uncommented lines containing "swap", and writes the result back. A backup
// of the original file is saved to <path>.bak before any modifications are made.
func commentOutSwapInFstab(path string) error {
	content, err := os.ReadFile(path) //#nosec G304 -- trusted path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// no fstab, nothing to do
			return nil
		}

		return fmt.Errorf("reading %s: %w", path, err)
	}

	lines := strings.Split(string(content), "\n")
	modified := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// skip empty lines and already-commented lines
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.Contains(trimmed, "swap") {
			lines[i] = "# " + line
			modified = true
		}
	}

	if !modified {
		return nil
	}

	// back up the original fstab before writing changes
	if err := utilio.WriteFile(path+".bak", content, 0o644); err != nil {
		return fmt.Errorf("backing up %s: %w", path, err)
	}

	newContent := []byte(strings.Join(lines, "\n"))

	return utilio.WriteFile(path, newContent, 0o644)
}

// swapoff returns a command factory for swapoff.
func swapoff() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "swapoff")
	}
}
