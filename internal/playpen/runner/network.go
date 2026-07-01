// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runner

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type NetworkManager struct {
	cmd Commander
	cfg Config
}

func NewNetworkManager(cmd Commander, cfg Config) *NetworkManager {
	return &NetworkManager{cmd: cmd, cfg: cfg}
}

func (m *NetworkManager) Setup(ctx context.Context) error {
	if !m.cfg.ConfigureNetwork {
		return nil
	}

	serverAddr, err := addressIP(m.cfg.WireGuard.ServerAddress)
	if err != nil {
		return fmt.Errorf("parse wireguard server address: %w", err)
	}

	clientAddr, err := addressIP(m.cfg.WireGuard.ClientAddress)
	if err != nil {
		return fmt.Errorf("parse wireguard client address: %w", err)
	}

	_ = m.Teardown(ctx)

	commands := [][]string{
		{"ip", "link", "add", m.cfg.WireGuard.Interface, "type", "wireguard"},
		{"wg", "set", m.cfg.WireGuard.Interface,
			"private-key", m.cfg.WireGuard.PrivateKeyFile,
			"listen-port", fmt.Sprint(m.cfg.WireGuard.ListenPort)},
		{"ip", "addr", "add", m.cfg.WireGuard.ServerAddress, "dev", m.cfg.WireGuard.Interface},
		{"ip", "link", "set", m.cfg.WireGuard.Interface, "up"},
		{"ip", "link", "add", m.cfg.BridgeName, "type", "bridge"},
		{"ip", "link", "set", m.cfg.BridgeName, "up"},
		{"ip", "link", "add", m.cfg.VXLAN.Interface,
			"type", "vxlan",
			"id", fmt.Sprint(m.cfg.VXLAN.VNI),
			"dev", m.cfg.WireGuard.Interface,
			"local", serverAddr.String(),
			"remote", clientAddr.String(),
			"dstport", fmt.Sprint(m.cfg.VXLAN.Port),
			"nolearning"},
		{"bridge", "fdb", "append", "00:00:00:00:00:00", "dev", m.cfg.VXLAN.Interface, "dst", clientAddr.String()},
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

	if m.cfg.WireGuard.ClientPublicKey != "" {
		if err := m.ConfigurePeer(ctx, m.cfg.WireGuard.ClientPublicKey); err != nil {
			return err
		}
	}

	return nil
}

func (m *NetworkManager) ConfigurePeer(ctx context.Context, clientPublicKey string) error {
	if !m.cfg.ConfigureNetwork {
		return nil
	}

	clientPublicKey = strings.TrimSpace(clientPublicKey)
	if clientPublicKey == "" {
		return fmt.Errorf("wireguard client public key is required")
	}

	c := []string{"wg", "set", m.cfg.WireGuard.Interface,
		"peer", clientPublicKey,
		"allowed-ips", m.cfg.WireGuard.ClientAddress}
	if err := m.cmd.Run(ctx, c[0], c[1:]...); err != nil {
		return fmt.Errorf("run %q: %w", joinCommand(c), err)
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
		{"ip", "link", "delete", m.cfg.WireGuard.Interface},
	} {
		_ = m.cmd.Run(ctx, c[0], c[1:]...)
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

func ensureWireGuardPrivateKey(path string) (string, error) {
	if existing, err := os.ReadFile(path); err == nil {
		key, err := wgtypes.ParseKey(strings.TrimSpace(string(existing)))
		if err != nil {
			return "", fmt.Errorf("parse wireguard private key: %w", err)
		}

		return key.PublicKey().String(), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}

	if err := os.WriteFile(path, []byte(key.String()+"\n"), 0o600); err != nil {
		return "", err
	}

	return key.PublicKey().String(), nil
}
