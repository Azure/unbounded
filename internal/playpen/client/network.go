// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"

	"github.com/Azure/unbounded/internal/playpen/operator"
)

// commander runs local network configuration commands.
type commander interface {
	Run(ctx context.Context, name string, args ...string) error
}

type osCommander struct{}

func (osCommander) Run(ctx context.Context, name string, args ...string) error {
	if os.Geteuid() != 0 {
		args = append([]string{"-n", name}, args...)
		name = "sudo"
	}

	cmd := exec.CommandContext(ctx, name, args...)

	out, err := cmd.CombinedOutput()
	if err != nil && len(out) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}

	return err
}

const defaultVXLANDevice = "unbounded0"

// TunnelConfig controls the local interface name for the playpen VXLAN layer.
type TunnelConfig struct {
	VXLANInterface string
}

type tunnel struct {
	cmd      commander
	metadata operator.AllocResponse
	cfg      TunnelConfig
}

func newTunnel(cmd commander, metadata operator.AllocResponse, cfg TunnelConfig) *tunnel {
	return &tunnel{cmd: cmd, metadata: metadata, cfg: cfg}
}

func (t *tunnel) Setup(ctx context.Context) error {
	if err := t.validate(); err != nil {
		return err
	}

	t.Teardown(ctx) //nolint:errcheck // Setup is idempotent and recreates network resources below.

	serverVXLAN, err := addressIP(t.metadata.VXLAN.ServerAddress)
	if err != nil {
		return fmt.Errorf("parse server VXLAN address: %w", err)
	}

	clientVXLAN, err := addressIP(t.metadata.VXLAN.ClientAddress)
	if err != nil {
		return fmt.Errorf("parse client VXLAN address: %w", err)
	}

	commands, err := t.setupCommands(serverVXLAN, clientVXLAN)
	if err != nil {
		return err
	}

	for _, c := range commands {
		if err := t.cmd.Run(ctx, c[0], c[1:]...); err != nil {
			return fmt.Errorf("run %q: %w", joinCommand(c), err)
		}
	}

	return nil
}

func (t *tunnel) setupCommands(serverVXLAN, clientVXLAN netip.Addr) ([][]string, error) {
	guestGatewayPrefix, _, err := guestNetworkPrefixes(t.metadata.Network)
	if err != nil {
		return nil, err
	}

	return [][]string{
		{
			"ip", "link", "add", t.cfg.VXLANInterface,
			"type", "vxlan",
			"id", fmt.Sprint(t.metadata.VXLAN.VNI),
			"dev", defaultVXLANDevice,
			"local", clientVXLAN.String(),
			"remote", serverVXLAN.String(),
			"dstport", fmt.Sprint(t.metadata.VXLAN.UDPPort),
			"nolearning",
		},
		{"bridge", "fdb", "append", "00:00:00:00:00:00", "dev", t.cfg.VXLANInterface, "dst", serverVXLAN.String()},
		{"ip", "link", "set", t.cfg.VXLANInterface, "up"},
		{"ip", "addr", "add", guestGatewayPrefix, "dev", t.cfg.VXLANInterface},
	}, nil
}

func (t *tunnel) Teardown(ctx context.Context) error {
	if t.cfg.VXLANInterface != "" {
		t.cmd.Run(ctx, "ip", "link", "delete", t.cfg.VXLANInterface) //nolint:errcheck // Teardown is best-effort for idempotency.
	}

	return nil
}

func (t *tunnel) validate() error {
	if t.cmd == nil {
		return fmt.Errorf("commander is required")
	}

	if strings.TrimSpace(t.cfg.VXLANInterface) == "" {
		return fmt.Errorf("vxlan interface name is required")
	}

	return nil
}

func tunnelConfigWithDefaults(cfg TunnelConfig, idempotencyKey string) TunnelConfig {
	suffix := shortHash(idempotencyKey)

	if cfg.VXLANInterface == "" {
		cfg.VXLANInterface = "ppvx" + suffix
	}

	return cfg
}

func addressIP(value string) (netip.Addr, error) {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix.Addr(), nil
	}

	return netip.ParseAddr(value)
}

func guestNetworkPrefixes(network operator.NetworkResponse) (string, string, error) {
	guestIP, err := netip.ParseAddr(network.GuestIPv4)
	if err != nil {
		return "", "", fmt.Errorf("parse guest IPv4 address %q: %w", network.GuestIPv4, err)
	}

	if !guestIP.Is4() {
		return "", "", fmt.Errorf("guest address %s is not IPv4", guestIP)
	}

	gatewayIP, err := netip.ParseAddr(network.GatewayIPv4)
	if err != nil {
		return "", "", fmt.Errorf("parse guest gateway IPv4 address %q: %w", network.GatewayIPv4, err)
	}

	if !gatewayIP.Is4() {
		return "", "", fmt.Errorf("guest gateway %s is not IPv4", gatewayIP)
	}

	mask := net.ParseIP(network.SubnetMask).To4()
	if mask == nil {
		return "", "", fmt.Errorf("parse guest subnet mask %q", network.SubnetMask)
	}

	ones, bits := net.IPMask(mask).Size()
	if bits != 32 {
		return "", "", fmt.Errorf("guest subnet mask %q is not contiguous", network.SubnetMask)
	}

	guestSubnet := netip.PrefixFrom(guestIP, ones).Masked()
	if !guestSubnet.Contains(gatewayIP) {
		return "", "", fmt.Errorf("guest gateway %s is outside guest subnet %s", gatewayIP, guestSubnet)
	}

	return netip.PrefixFrom(gatewayIP, ones).String(), guestSubnet.String(), nil
}

func shortHash(value string) string {
	h := sha256.Sum256([]byte(value))

	return hex.EncodeToString(h[:4])
}

func joinCommand(parts []string) string {
	return strings.Join(parts, " ")
}
