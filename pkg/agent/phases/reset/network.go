// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// knownOverlayInterfaces lists the tunnel and overlay interfaces created by
// unbounded-net that must be removed during reset.
var knownOverlayInterfaces = []string{
	"geneve0",
	"vxlan0",
	"ipip0",
	"unbounded0",
	"cbr0",
}

type removeNetworkInterfaces struct {
	log *slog.Logger
}

// RemoveNetworkInterfaces returns a task that removes all network interfaces
// created by unbounded-net: WireGuard interfaces (wg*), and tunnel/overlay
// interfaces (geneve0, vxlan0, ipip0, unbounded0, cbr0).
func RemoveNetworkInterfaces(log *slog.Logger) phases.Task {
	return &removeNetworkInterfaces{log: log}
}

func (t *removeNetworkInterfaces) Name() string { return "remove-network-interfaces" }

// CleanupNetwork returns a task that removes network interfaces and policy
// routing state left by unbounded-net.
func CleanupNetwork(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		CleanupLocalDNSRules(log),
		RemoveNetworkInterfaces(log),
		CleanupRoutes(log),
	)
}

type cleanupLocalDNSRules struct {
	log *slog.Logger
}

// CleanupLocalDNSRules removes raw-table rules owned by LocalDNS.
func CleanupLocalDNSRules(log *slog.Logger) phases.Task {
	return &cleanupLocalDNSRules{log: log}
}

func (t *cleanupLocalDNSRules) Name() string { return "cleanup-localdns-rules" }

func (t *cleanupLocalDNSRules) Do(ctx context.Context) error {
	if err := executil.RunCmd(ctx, t.log, executil.Systemctl(), "disable", "--now", goalstates.LocalDNSNetworkUnit); err != nil {
		path := filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSNetworkUnit)
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("disable LocalDNS unit: %w", err)
		}
	}

	// Enumerate successfully before concluding absence: permission/tool failure
	// must not be mistaken for an absent table or interface.
	tables, err := executil.OutputCmd(ctx, t.log, "nft", "list", "tables")
	if err != nil {
		return fmt.Errorf("inspect LocalDNS tables: %w", err)
	}

	if strings.Contains(tables, "table ip "+goalstates.LocalDNSNFTTable+"\n") {
		if err := executil.RunCmd(ctx, t.log, func(ctx context.Context) *exec.Cmd {
			return exec.CommandContext(ctx, "nft")
		}, "delete", "table", "ip", goalstates.LocalDNSNFTTable); err != nil {
			return fmt.Errorf("remove LocalDNS nftables table: %w", err)
		}
	}

	links, err := executil.OutputCmd(ctx, t.log, "ip", "-d", "-o", "link", "show")
	if err != nil {
		return fmt.Errorf("inspect LocalDNS interface: %w", err)
	}

	for _, output := range strings.Split(links, "\n") {
		if !strings.Contains(output, ": "+goalstates.LocalDNSInterfaceName+":") {
			continue
		}

		if !strings.Contains(" "+output+" ", " dummy ") {
			return fmt.Errorf("refusing to remove non-dummy interface %s", goalstates.LocalDNSInterfaceName)
		}

		if err := executil.RunCmd(ctx, t.log, executil.Ip(), "link", "delete", goalstates.LocalDNSInterfaceName); err != nil {
			return fmt.Errorf("remove LocalDNS interface: %w", err)
		}
	}

	paths := []string{filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSNetworkUnit)}
	for _, prefix := range goalstates.KnownHostPrefixes(goalstates.HostPrefixFromAppliedConfig()) {
		paths = append(paths, goalstates.ResolveHostPaths(prefix).LocalDNSNetworkHelper)
	}

	for _, path := range paths {
		if err := removeFileIfExists(t.log, path); err != nil {
			return err
		}
	}

	return nil
}

func (t *removeNetworkInterfaces) Do(ctx context.Context) error {
	// Remove WireGuard interfaces (wg51820, wg51821, ...).
	wgIfaces, err := listWireGuardInterfaces(ctx, t.log)
	if err != nil {
		return err
	}

	for _, iface := range wgIfaces {
		t.log.Info("removing interface", "interface", iface)

		if err := deleteLink(ctx, t.log, iface); err != nil {
			return err
		}
	}

	// Remove tunnel and overlay interfaces.
	for _, iface := range knownOverlayInterfaces {
		if _, err := os.Stat(filepath.Join("/sys/class/net", iface)); err == nil {
			t.log.Info("removing interface", "interface", iface)

			if err := deleteLink(ctx, t.log, iface); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	return nil
}

type removeWireGuardKeys struct {
	log *slog.Logger
}

// RemoveWireGuardKeys returns a task that removes WireGuard private and public
// key files from /etc/wireguard.
func RemoveWireGuardKeys(log *slog.Logger) phases.Task {
	return &removeWireGuardKeys{log: log}
}

func (t *removeWireGuardKeys) Name() string { return "remove-wireguard-keys" }

func (t *removeWireGuardKeys) Do(_ context.Context) error {
	t.log.Info("removing WireGuard keys")

	for _, path := range []string{
		"/etc/wireguard/server.priv",
		"/etc/wireguard/server.pub",
	} {
		if err := removeFileIfExists(t.log, path); err != nil {
			return err
		}
	}

	return nil
}

// listWireGuardInterfaces returns the names of unbounded-managed WireGuard
// interfaces visible on the host.
func listWireGuardInterfaces(ctx context.Context, log *slog.Logger) ([]string, error) {
	out, err := executil.OutputCmd(ctx, log, "ip", "-o", "link", "show")
	if err != nil {
		return nil, fmt.Errorf("ip link show: %w", err)
	}

	var ifaces []string

	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		// Each line looks like: "2: wg51820: <...> ..."
		// The interface name is the second field, with a trailing colon.
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}

		name := strings.TrimRight(fields[1], ":")
		if isWireGuardInterface(name) {
			ifaces = append(ifaces, name)
		}
	}

	return ifaces, scanner.Err()
}

// isWireGuardInterface returns true if the interface name matches the
// unbounded-net WireGuard naming and table range (wg51820-wg51899).
func isWireGuardInterface(name string) bool {
	if !strings.HasPrefix(name, "wg") {
		return false
	}

	suffix := name[2:]
	if suffix == "" {
		return false
	}

	port, err := strconv.Atoi(suffix)
	if err != nil {
		return false
	}

	return port >= wireguardTableStart && port <= wireguardTableEnd
}

// deleteLink ignores only verified absence after a failed deletion.
func deleteLink(ctx context.Context, log *slog.Logger, name string) error {
	if err := executil.RunCmd(ctx, log, executil.Ip(), "link", "delete", name); err != nil {
		if _, statErr := os.Stat(filepath.Join("/sys/class/net", name)); errors.Is(statErr, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("delete interface %s: %w", name, err)
	}

	return nil
}
