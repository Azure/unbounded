// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

// Commander runs local network configuration commands.
type Commander interface {
	Run(ctx context.Context, name string, args ...string) error
}

// OSCommander executes commands on the local host.
type OSCommander struct{}

func (OSCommander) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// TunnelConfig controls local interface names and WireGuard settings.
type TunnelConfig struct {
	WireGuardInterface  string
	VXLANInterface      string
	WireGuardListenPort int
	PersistentKeepalive int
	PrivateKeyFile      string
}

// Tunnel manages local network resources for a playpen.
type Tunnel struct {
	cmd                 Commander
	privateKey          string
	metadata            operator.AllocResponse
	cfg                 TunnelConfig
	createdPrivateKey   bool
	createdPrivateKeyAt string
}

func NewTunnel(cmd Commander, privateKey string, metadata operator.AllocResponse, cfg TunnelConfig) *Tunnel {
	return &Tunnel{cmd: cmd, privateKey: privateKey, metadata: metadata, cfg: cfg}
}

func (t *Tunnel) Setup(ctx context.Context) error {
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
	commands := [][]string{
		{"ip", "link", "add", t.cfg.WireGuardInterface, "type", "wireguard"},
		{
			"wg", "set", t.cfg.WireGuardInterface,
			"private-key", privateKeyFile,
			"listen-port", fmt.Sprint(t.cfg.WireGuardListenPort),
			"peer", t.metadata.WireGuard.ServerPublicKey,
			"endpoint", endpoint,
			"allowed-ips", singleIPCIDR(serverWG),
			"persistent-keepalive", fmt.Sprint(t.cfg.PersistentKeepalive),
		},
		{"ip", "addr", "add", t.metadata.WireGuard.ClientAddress, "dev", t.cfg.WireGuardInterface},
		{"ip", "link", "set", t.cfg.WireGuardInterface, "up"},
		{"ip", "route", "add", singleIPCIDR(serverWG), "dev", t.cfg.WireGuardInterface},
		{
			"ip", "link", "add", t.cfg.VXLANInterface,
			"type", "vxlan",
			"id", fmt.Sprint(t.metadata.VXLAN.VNI),
			"dev", t.cfg.WireGuardInterface,
			"local", clientWG.String(),
			"remote", serverWG.String(),
			"dstport", fmt.Sprint(t.metadata.VXLAN.UDPPort),
			"nolearning",
		},
		{"bridge", "fdb", "append", "00:00:00:00:00:00", "dev", t.cfg.VXLANInterface, "dst", serverWG.String()},
		{"ip", "link", "set", t.cfg.VXLANInterface, "up"},
	}

	for _, c := range commands {
		if err := t.cmd.Run(ctx, c[0], c[1:]...); err != nil {
			return fmt.Errorf("run %q: %w", joinCommand(c), err)
		}
	}

	return nil
}

func (t *Tunnel) Teardown(ctx context.Context) error {
	if t.cfg.VXLANInterface != "" {
		t.cmd.Run(ctx, "ip", "link", "delete", t.cfg.VXLANInterface) //nolint:errcheck // Teardown is best-effort for idempotency.
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

func (t *Tunnel) validate() error {
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

	if strings.TrimSpace(t.cfg.WireGuardInterface) == "" || strings.TrimSpace(t.cfg.VXLANInterface) == "" {
		return fmt.Errorf("wireguard and vxlan interface names are required")
	}

	return nil
}

func (t *Tunnel) privateKeyFile() (string, error) {
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
	if cfg.WireGuardInterface == "" {
		cfg.WireGuardInterface = "ppwg" + suffix
	}

	if cfg.VXLANInterface == "" {
		cfg.VXLANInterface = "ppvx" + suffix
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

func shortHash(value string) string {
	h := sha256.Sum256([]byte(value))

	return hex.EncodeToString(h[:4])
}

func joinCommand(parts []string) string {
	return strings.Join(parts, " ")
}
