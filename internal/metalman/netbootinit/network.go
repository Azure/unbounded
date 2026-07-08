// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (i *Installer) logInterfaces() {
	i.Logger.Printf("network interfaces:")

	ifaces, err := i.listInterfaces()
	if err != nil {
		return
	}

	for _, iface := range ifaces {
		i.Logger.Printf("  %s mac=%s state=%s", iface.Name, defaultString(iface.MAC, "unknown"), defaultString(iface.State, "unknown"))
	}
}

type netInterface struct {
	Name  string
	MAC   string
	State string
}

func (i *Installer) listInterfaces() ([]netInterface, error) {
	entries, err := os.ReadDir(filepath.Join(i.SysfsRoot, "class/net"))
	if err != nil {
		return nil, err
	}

	ifaces := make([]netInterface, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		base := filepath.Join(i.SysfsRoot, "class/net", name)
		mac := strings.TrimSpace(readFileString(filepath.Join(base, "address")))
		state := strings.TrimSpace(readFileString(filepath.Join(base, "operstate")))
		ifaces = append(ifaces, netInterface{Name: name, MAC: mac, State: state})
	}

	return ifaces, nil
}

func (i *Installer) selectInterface(ctx context.Context, bootMAC string) (string, error) {
	i.Logger.Printf("waiting for network interface")

	var iface string

	if bootMAC != "" {
		err := retry(ctx, 30, time.Second, "find network interface with MAC "+bootMAC, i.Sleep, i.Logger, func() error {
			name, ok := i.findInterfaceByMAC(bootMAC)
			if !ok {
				return fmt.Errorf("interface with MAC %s not found", bootMAC)
			}

			iface = name

			return nil
		})
		if err != nil {
			i.Logger.Printf("WARNING: no network interface found with MAC %s", bootMAC)
		}
	}

	if iface == "" {
		if err := retry(ctx, 30, time.Second, "find network interface", i.Sleep, i.Logger, func() error {
			name, ok := i.findFirstInterface()
			if !ok {
				return errors.New("no network interface found")
			}

			iface = name

			return nil
		}); err != nil {
			return "", errors.New("no network interface found")
		}

		i.Logger.Printf("WARNING: using first non-loopback network interface %s", iface)
	} else {
		i.Logger.Printf("selected network interface %s for MAC %s", iface, bootMAC)
	}

	return iface, nil
}

func (i *Installer) findInterfaceByMAC(mac string) (string, bool) {
	ifaces, err := i.listInterfaces()
	if err != nil {
		return "", false
	}

	for _, iface := range ifaces {
		if iface.Name == "lo" {
			continue
		}

		if normalizeMAC(iface.MAC) == mac {
			return iface.Name, true
		}
	}

	return "", false
}

func (i *Installer) findFirstInterface() (string, bool) {
	ifaces, err := i.listInterfaces()
	if err != nil {
		return "", false
	}

	for _, iface := range ifaces {
		if iface.Name != "lo" {
			return iface.Name, true
		}
	}

	return "", false
}

type ipConfig struct {
	Address *net.IPNet
	Gateway net.IP
	Iface   string
}

func parseIPParam(value string) (ipConfig, error) {
	if value == "dhcp" || value == "on" || value == "any" {
		return ipConfig{}, fmt.Errorf("unsupported ip parameter %q", value)
	}

	fields := strings.Split(value, ":")
	field := func(idx int) string {
		if idx >= len(fields) {
			return ""
		}

		return fields[idx]
	}

	clientIP := net.ParseIP(field(0)).To4()
	if clientIP == nil {
		return ipConfig{}, fmt.Errorf("invalid IPv4 client IP %q", field(0))
	}

	mask := field(3)
	prefix, err := strconv.Atoi(mask)
	if strings.Contains(mask, ".") {
		prefix, err = maskToCIDR(mask)
	}
	if err != nil || prefix < 0 || prefix > 32 {
		return ipConfig{}, fmt.Errorf("invalid network mask %q", mask)
	}

	var gateway net.IP
	if field(2) != "" {
		gateway = net.ParseIP(field(2)).To4()
		if gateway == nil {
			return ipConfig{}, fmt.Errorf("invalid IPv4 gateway IP %q", field(2))
		}
	}

	return ipConfig{
		Address: &net.IPNet{IP: clientIP, Mask: net.CIDRMask(prefix, 32)},
		Gateway: gateway,
		Iface:   field(5),
	}, nil
}

func maskToCIDR(mask string) (int, error) {
	ip := net.ParseIP(mask).To4()
	if ip == nil {
		return 0, fmt.Errorf("invalid IPv4 mask %q", mask)
	}

	ones, bits := net.IPMask(ip).Size()
	if bits != 32 {
		return 0, fmt.Errorf("invalid IPv4 mask %q", mask)
	}

	return ones, nil
}

func (i *Installer) configureStaticIP(ctx context.Context, selectedIface, value string) error {
	cfg, err := parseIPParam(value)
	if err != nil {
		return err
	}

	iface := selectedIface
	if cfg.Iface != "" && pathExists(filepath.Join(i.SysfsRoot, "class/net", cfg.Iface)) {
		iface = cfg.Iface
	}

	i.Logger.Printf("configuring %s with %s gw %s", iface, cfg.Address.String(), cfg.Gateway.String())

	if err := retry(ctx, 5, 2*time.Second, "link up "+iface, i.Sleep, i.Logger, func() error {
		return i.Network.LinkSetUp(iface)
	}); err != nil {
		return fmt.Errorf("failed to bring up %s: %w", iface, err)
	}

	if err := retry(ctx, 3, time.Second, "add address", i.Sleep, i.Logger, func() error {
		return i.Network.AddrAdd(iface, cfg.Address)
	}); err != nil {
		i.Logger.Printf("WARNING: failed to add address")
	}

	if cfg.Gateway != nil {
		if err := retry(ctx, 3, time.Second, "add default route", i.Sleep, i.Logger, func() error {
			return i.Network.RouteAddDefault(iface, cfg.Gateway)
		}); err != nil {
			i.Logger.Printf("WARNING: failed to add default route")
		}
	}

	return nil
}
