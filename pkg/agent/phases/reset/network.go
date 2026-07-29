// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reset

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
		t.log.Debug("LocalDNS network unit was not active", "error", err)
	}

	for _, chain := range []string{"OUTPUT", "PREROUTING"} {
		output, err := executil.OutputCmd(ctx, t.log, "iptables", "-w", "-t", "raw", "-S", chain)
		if err != nil {
			// Hosts without a LocalDNS deployment may not have iptables or a raw table.
			continue
		}

		for _, line := range strings.Split(output, "\n") {
			if !strings.Contains(line, "unbounded-localdns: skip conntrack") {
				continue
			}

			fields := strings.Fields(strings.ReplaceAll(line, `"`, ""))
			values := map[string]string{}

			for i := 0; i+1 < len(fields); i++ {
				switch fields[i] {
				case "-A", "-p", "-d":
					values[fields[i]] = strings.TrimSuffix(fields[i+1], "/32")
				}
			}

			if values["-A"] == "" || values["-p"] == "" || values["-d"] == "" {
				continue
			}

			args := []string{"-w", "-t", "raw", "-m", "comment", "--comment", "unbounded-localdns: skip conntrack", "-D", values["-A"], "-p", values["-p"], "-d", values["-d"], "--dport", "53", "-j", "NOTRACK"}
			if err := executil.RunCmd(ctx, t.log, executil.Iptables(), args...); err != nil {
				return fmt.Errorf("remove LocalDNS NOTRACK rule: %w", err)
			}
		}
	}

	if output, err := executil.OutputCmd(ctx, t.log, "ip", "-d", "-o", "link", "show", "dev", goalstates.LocalDNSInterfaceName); err == nil {
		if !strings.Contains(" "+output+" ", " dummy ") {
			return fmt.Errorf("refusing to remove non-dummy interface %s", goalstates.LocalDNSInterfaceName)
		}

		if err := executil.RunCmd(ctx, t.log, executil.Ip(), "link", "delete", goalstates.LocalDNSInterfaceName); err != nil {
			return fmt.Errorf("remove LocalDNS interface: %w", err)
		}
	}

	for _, path := range []string{
		filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSNetworkUnit),
		"/usr/local/libexec/unbounded-localdns-network",
	} {
		removeFileIfExists(t.log, path)
	}

	return nil
}

func (t *removeNetworkInterfaces) Do(ctx context.Context) error {
	// Remove WireGuard interfaces (wg51820, wg51821, ...).
	wgIfaces, err := listWireGuardInterfaces(ctx, t.log)
	if err != nil {
		t.log.Warn("failed to list WireGuard interfaces", "error", err)
	}

	for _, iface := range wgIfaces {
		t.log.Info("removing interface", "interface", iface)
		deleteLink(ctx, t.log, iface)
	}

	// Remove tunnel and overlay interfaces.
	for _, iface := range knownOverlayInterfaces {
		if linkExists(t.log, iface) {
			t.log.Info("removing interface", "interface", iface)
			deleteLink(ctx, t.log, iface)
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
		removeFileIfExists(t.log, path)
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

	return ifaces, nil
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

// linkExists checks whether a network interface exists by looking up its
// entry in /sys/class/net. This avoids shelling out and cleanly distinguishes
// "not found" from real errors.
func linkExists(log *slog.Logger, name string) bool {
	_, err := os.Stat(fmt.Sprintf("/sys/class/net/%s", name))
	if err == nil {
		return true
	}

	if errors.Is(err, os.ErrNotExist) {
		return false
	}

	log.Warn("failed to check interface existence", "interface", name, "error", err)

	return false
}

// deleteLink removes a network interface, logging a warning if the operation
// fails (e.g. the interface was already removed).
func deleteLink(ctx context.Context, log *slog.Logger, name string) {
	if err := executil.RunCmd(ctx, log, executil.Ip(), "link", "delete", name); err != nil {
		log.Warn("failed to delete interface (may already be gone)", "interface", name, "error", err)
	}
}
