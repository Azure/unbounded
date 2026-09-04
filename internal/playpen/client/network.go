// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// TunnelConfig controls local interface names and WireGuard settings.
type TunnelConfig struct {
	NetworkNamespace             string
	WireGuardInterface           string
	VXLANInterface               string
	ManagementHostInterface      string
	ManagementNamespaceInterface string
	ManagementHostAddress        string
	ManagementNamespaceAddress   string
	WireGuardListenPort          int
	PersistentKeepalive          int
	PrivateKeyFile               string
}

type tunnel struct {
	cmd                 commander
	privateKey          string
	metadata            operator.AllocResponse
	cfg                 TunnelConfig
	createdPrivateKey   bool
	createdPrivateKeyAt string
}

func newTunnel(cmd commander, privateKey string, metadata operator.AllocResponse, cfg TunnelConfig) *tunnel {
	return &tunnel{cmd: cmd, privateKey: privateKey, metadata: metadata, cfg: cfg}
}

func (t *tunnel) Setup(ctx context.Context) error {
	if err := t.validate(); err != nil {
		return err
	}

	t.Teardown(ctx) //nolint:errcheck // Setup is idempotent and recreates network resources below.

	serverWG, err := addressIP(t.metadata.WireGuard.ServerAddress)
	if err != nil {
		return fmt.Errorf("parse server wireguard address: %w", err)
	}

	clientWG, err := addressIP(t.metadata.WireGuard.ClientAddress)
	if err != nil {
		return fmt.Errorf("parse client wireguard address: %w", err)
	}

	privateKeyFile, err := t.privateKeyFile()
	if err != nil {
		return err
	}

	endpoint := net.JoinHostPort(t.metadata.Endpoint.Host, fmt.Sprint(t.metadata.Endpoint.WireGuardUDPPort))

	commands, err := t.setupCommands(privateKeyFile, endpoint, serverWG, clientWG)
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

func (t *tunnel) setupCommands(privateKeyFile, endpoint string, serverWG, clientWG netip.Addr) ([][]string, error) {
	hostManagementIP, err := addressIP(t.cfg.ManagementHostAddress)
	if err != nil {
		return nil, fmt.Errorf("parse management host address: %w", err)
	}

	namespaceManagementIP, err := addressIP(t.cfg.ManagementNamespaceAddress)
	if err != nil {
		return nil, fmt.Errorf("parse management namespace address: %w", err)
	}

	guestGatewayPrefix, guestSubnetCIDR, err := guestNetworkPrefixes(t.metadata.Network)
	if err != nil {
		return nil, err
	}

	commands := [][]string{
		{"ip", "netns", "add", t.cfg.NetworkNamespace},
		{"ip", "link", "add", t.cfg.ManagementHostInterface, "type", "veth", "peer", "name", t.cfg.ManagementNamespaceInterface},
		{"ip", "link", "set", t.cfg.ManagementNamespaceInterface, "netns", t.cfg.NetworkNamespace},
		{"ip", "addr", "add", t.cfg.ManagementHostAddress, "dev", t.cfg.ManagementHostInterface},
		{"ip", "link", "set", t.cfg.ManagementHostInterface, "up"},
		{"ip", "-n", t.cfg.NetworkNamespace, "link", "set", "lo", "up"},
		{"ip", "-n", t.cfg.NetworkNamespace, "addr", "add", t.cfg.ManagementNamespaceAddress, "dev", t.cfg.ManagementNamespaceInterface},
		{"ip", "-n", t.cfg.NetworkNamespace, "link", "set", t.cfg.ManagementNamespaceInterface, "up"},
		{"ip", "-n", t.cfg.NetworkNamespace, "route", "add", "default", "via", hostManagementIP.String(), "dev", t.cfg.ManagementNamespaceInterface},
		{"sysctl", "-w", "net.ipv4.ip_forward=1"},
		{"iptables", "-A", "FORWARD", "-i", t.cfg.ManagementHostInterface, "-j", "ACCEPT"},
		{"iptables", "-A", "FORWARD", "-o", t.cfg.ManagementHostInterface, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		{"iptables", "-t", "nat", "-A", "POSTROUTING", "-s", singleIPCIDR(namespaceManagementIP), "-j", "MASQUERADE"},
		{"ip", "link", "add", t.cfg.WireGuardInterface, "type", "wireguard"},
		{"ip", "link", "set", t.cfg.WireGuardInterface, "netns", t.cfg.NetworkNamespace},
		{
			"ip", "netns", "exec", t.cfg.NetworkNamespace,
			"wg", "set", t.cfg.WireGuardInterface,
			"private-key", privateKeyFile,
			"listen-port", fmt.Sprint(t.cfg.WireGuardListenPort),
			"peer", t.metadata.WireGuard.ServerPublicKey,
			"endpoint", endpoint,
			"allowed-ips", singleIPCIDR(serverWG),
			"persistent-keepalive", fmt.Sprint(t.cfg.PersistentKeepalive),
		},
		{"ip", "-n", t.cfg.NetworkNamespace, "addr", "add", t.metadata.WireGuard.ClientAddress, "dev", t.cfg.WireGuardInterface},
		{"ip", "-n", t.cfg.NetworkNamespace, "link", "set", t.cfg.WireGuardInterface, "up"},
		{"ip", "-n", t.cfg.NetworkNamespace, "route", "add", singleIPCIDR(serverWG), "dev", t.cfg.WireGuardInterface},
		{
			"ip", "-n", t.cfg.NetworkNamespace, "link", "add", t.cfg.VXLANInterface,
			"type", "vxlan",
			"id", fmt.Sprint(t.metadata.VXLAN.VNI),
			"dev", t.cfg.WireGuardInterface,
			"local", clientWG.String(),
			"remote", serverWG.String(),
			"dstport", fmt.Sprint(t.metadata.VXLAN.UDPPort),
			"nolearning",
		},
		{"ip", "netns", "exec", t.cfg.NetworkNamespace, "bridge", "fdb", "append", "00:00:00:00:00:00", "dev", t.cfg.VXLANInterface, "dst", serverWG.String()},
	}

	commands = append(commands, []string{"ip", "-n", t.cfg.NetworkNamespace, "link", "set", t.cfg.VXLANInterface, "up"})
	commands = append(
		commands,
		[]string{"ip", "-n", t.cfg.NetworkNamespace, "addr", "add", guestGatewayPrefix, "dev", t.cfg.VXLANInterface},
		[]string{"ip", "netns", "exec", t.cfg.NetworkNamespace, "sysctl", "-w", "net.ipv4.ip_forward=1"},
		[]string{"ip", "netns", "exec", t.cfg.NetworkNamespace, "iptables", "-A", "FORWARD", "-i", t.cfg.VXLANInterface, "-o", t.cfg.ManagementNamespaceInterface, "-j", "ACCEPT"},
		[]string{"ip", "netns", "exec", t.cfg.NetworkNamespace, "iptables", "-A", "FORWARD", "-i", t.cfg.ManagementNamespaceInterface, "-o", t.cfg.VXLANInterface, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		[]string{"ip", "netns", "exec", t.cfg.NetworkNamespace, "iptables", "-t", "nat", "-A", "POSTROUTING", "-s", guestSubnetCIDR, "-o", t.cfg.ManagementNamespaceInterface, "-j", "MASQUERADE"},
	)

	return commands, nil
}

func (t *tunnel) Teardown(ctx context.Context) error {
	if t.cfg.ManagementNamespaceAddress != "" {
		if namespaceManagementIP, err := addressIP(t.cfg.ManagementNamespaceAddress); err == nil {
			t.cmd.Run(ctx, "iptables", "-t", "nat", "-D", "POSTROUTING", "-s", singleIPCIDR(namespaceManagementIP), "-j", "MASQUERADE") //nolint:errcheck // Teardown is best-effort for idempotency.
		}
	}

	if t.cfg.ManagementHostInterface != "" {
		t.cmd.Run(ctx, "iptables", "-D", "FORWARD", "-o", t.cfg.ManagementHostInterface, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT") //nolint:errcheck // Teardown is best-effort for idempotency.
		t.cmd.Run(ctx, "iptables", "-D", "FORWARD", "-i", t.cfg.ManagementHostInterface, "-j", "ACCEPT")                                                        //nolint:errcheck // Teardown is best-effort for idempotency.
		t.cmd.Run(ctx, "ip", "link", "delete", t.cfg.ManagementHostInterface)                                                                                   //nolint:errcheck // Teardown is best-effort for idempotency.
	}

	if t.cfg.NetworkNamespace != "" {
		t.cmd.Run(ctx, "ip", "netns", "delete", t.cfg.NetworkNamespace) //nolint:errcheck // Teardown is best-effort for idempotency.
	}

	if t.cfg.WireGuardInterface != "" {
		t.cmd.Run(ctx, "ip", "link", "delete", t.cfg.WireGuardInterface) //nolint:errcheck // Teardown is best-effort for idempotency.
	}

	if t.createdPrivateKeyAt != "" {
		os.Remove(t.createdPrivateKeyAt) //nolint:errcheck // Temporary key cleanup is best-effort.
		t.createdPrivateKeyAt = ""
		t.createdPrivateKey = false
	}

	return nil
}

func (t *tunnel) validate() error {
	if t.cmd == nil {
		return fmt.Errorf("commander is required")
	}

	if strings.TrimSpace(t.privateKey) == "" {
		return fmt.Errorf("wireguard private key is required")
	}

	if strings.TrimSpace(t.metadata.WireGuard.ServerPublicKey) == "" {
		return fmt.Errorf("server wireguard public key is required")
	}

	if strings.TrimSpace(t.metadata.Endpoint.Host) == "" || t.metadata.Endpoint.WireGuardUDPPort == 0 {
		return fmt.Errorf("wireguard endpoint host and port are required")
	}

	if strings.TrimSpace(t.cfg.NetworkNamespace) == "" || strings.TrimSpace(t.cfg.WireGuardInterface) == "" || strings.TrimSpace(t.cfg.VXLANInterface) == "" {
		return fmt.Errorf("network namespace, wireguard interface, and vxlan interface names are required")
	}

	if strings.TrimSpace(t.cfg.ManagementHostInterface) == "" || strings.TrimSpace(t.cfg.ManagementNamespaceInterface) == "" {
		return fmt.Errorf("management host and namespace interface names are required")
	}

	if strings.TrimSpace(t.cfg.ManagementHostAddress) == "" || strings.TrimSpace(t.cfg.ManagementNamespaceAddress) == "" {
		return fmt.Errorf("management host and namespace addresses are required")
	}

	return nil
}

func (t *tunnel) privateKeyFile() (string, error) {
	if t.cfg.PrivateKeyFile != "" {
		return t.cfg.PrivateKeyFile, os.WriteFile(t.cfg.PrivateKeyFile, []byte(t.privateKey+"\n"), 0o600)
	}

	file, err := os.CreateTemp("", "playpen-wireguard-*")
	if err != nil {
		return "", err
	}

	path := file.Name()
	if _, err := file.WriteString(t.privateKey + "\n"); err != nil {
		err = errors.Join(err, file.Close(), os.Remove(path))

		return "", err
	}

	if err := file.Close(); err != nil {
		err = errors.Join(err, os.Remove(path))

		return "", err
	}

	t.createdPrivateKey = true
	t.createdPrivateKeyAt = path

	return path, nil
}

func tunnelConfigWithDefaults(cfg TunnelConfig, idempotencyKey string) TunnelConfig {
	suffix := shortHash(idempotencyKey)
	if cfg.NetworkNamespace == "" {
		cfg.NetworkNamespace = "ppns" + suffix
	}

	if cfg.WireGuardInterface == "" {
		cfg.WireGuardInterface = "ppwg" + suffix
	}

	if cfg.VXLANInterface == "" {
		cfg.VXLANInterface = "ppvx" + suffix
	}

	if cfg.ManagementHostInterface == "" {
		cfg.ManagementHostInterface = "ppmh" + suffix
	}

	if cfg.ManagementNamespaceInterface == "" {
		cfg.ManagementNamespaceInterface = "ppmn" + suffix
	}

	if cfg.ManagementHostAddress == "" || cfg.ManagementNamespaceAddress == "" {
		hostAddress, namespaceAddress := defaultManagementAddresses(idempotencyKey)
		if cfg.ManagementHostAddress == "" {
			cfg.ManagementHostAddress = hostAddress
		}

		if cfg.ManagementNamespaceAddress == "" {
			cfg.ManagementNamespaceAddress = namespaceAddress
		}
	}

	if cfg.PersistentKeepalive == 0 {
		cfg.PersistentKeepalive = 25
	}

	return cfg
}

func addressIP(value string) (netip.Addr, error) {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		return prefix.Addr(), nil
	}

	return netip.ParseAddr(value)
}

func singleIPCIDR(addr netip.Addr) string {
	if addr.Is4() {
		return netip.PrefixFrom(addr, 32).String()
	}

	return netip.PrefixFrom(addr, 128).String()
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

func defaultManagementAddresses(idempotencyKey string) (string, string) {
	h := sha256.Sum256([]byte(idempotencyKey))
	fourthOctet := int(h[3] & 0xfc)

	return fmt.Sprintf("169.254.%d.%d/30", h[2], fourthOctet+1), fmt.Sprintf("169.254.%d.%d/30", h[2], fourthOctet+2)
}

func shortHash(value string) string {
	h := sha256.Sum256([]byte(value))

	return hex.EncodeToString(h[:4])
}

func joinCommand(parts []string) string {
	return strings.Join(parts, " ")
}
