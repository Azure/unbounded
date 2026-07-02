// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runner

import (
	"context"
	"fmt"
	"net/netip"
)

type NetworkManager struct {
	cmd Commander
	cfg Config
}

func NewNetworkManager(cmd Commander, cfg Config) *NetworkManager {
	return &NetworkManager{cmd: cmd, cfg: cfg}
}

func (m *NetworkManager) Setup(ctx context.Context, localAddress, remoteAddress string) error {
	if !m.cfg.ConfigureNetwork {
		return nil
	}

	localAddr, err := addressIP(localAddress)
	if err != nil {
		return fmt.Errorf("parse vxlan local address: %w", err)
	}

	remoteAddr, err := addressIP(remoteAddress)
	if err != nil {
		return fmt.Errorf("parse vxlan remote address: %w", err)
	}

	m.Teardown(ctx) //nolint:errcheck // Setup is idempotent and recreates network resources below.

	commands := [][]string{
		{"ip", "link", "add", m.cfg.BridgeName, "type", "bridge"},
		{"ip", "link", "set", m.cfg.BridgeName, "up"},
		{
			"ip", "link", "add", m.cfg.VXLAN.Interface,
			"type", "vxlan",
			"id", fmt.Sprint(m.cfg.VXLAN.VNI),
			"local", localAddr.String(),
			"remote", remoteAddr.String(),
			"dstport", fmt.Sprint(m.cfg.VXLAN.Port),
			"nolearning",
		},
		{"bridge", "fdb", "append", "00:00:00:00:00:00", "dev", m.cfg.VXLAN.Interface, "dst", remoteAddr.String()},
		{"ip", "link", "set", m.cfg.VXLAN.Interface, "master", m.cfg.BridgeName},
		{"ip", "link", "set", m.cfg.VXLAN.Interface, "up"},
		{"ip", "tuntap", "add", "dev", m.cfg.TapName, "mode", "tap"},
		{"ip", "link", "set", m.cfg.TapName, "master", m.cfg.BridgeName},
		{"ip", "link", "set", m.cfg.TapName, "up"},
	}

	for _, c := range commands {
		if err := m.cmd.Run(ctx, c[0], c[1:]...); err != nil {
			return fmt.Errorf("run %q: %w", joinCommand(c), err)
		}
	}

	return nil
}

func (m *NetworkManager) Teardown(ctx context.Context) error {
	if !m.cfg.ConfigureNetwork {
		return nil
	}

	for _, c := range [][]string{
		{"ip", "link", "delete", m.cfg.TapName},
		{"ip", "link", "delete", m.cfg.VXLAN.Interface},
		{"ip", "link", "delete", m.cfg.BridgeName},
	} {
		m.cmd.Run(ctx, c[0], c[1:]...) //nolint:errcheck // Teardown is best-effort for idempotency.
	}

	return nil
}

func addressIP(value string) (netip.Addr, error) {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix.Addr(), nil
	}

	return netip.ParseAddr(value)
}

func joinCommand(parts []string) string {
	result := ""

	for i, part := range parts {
		if i > 0 {
			result += " "
		}

		result += part
	}

	return result
}
