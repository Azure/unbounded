// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"context"
	"errors"
	"fmt"
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
	ClientIP string
	Gateway  string
	Prefix   string
	Iface    string
}

func parseIPParam(value string) ipConfig {
	fields := strings.Split(value, ":")
	field := func(idx int) string {
		if idx >= len(fields) {
			return ""
		}

		return fields[idx]
	}

	mask := field(3)

	prefix := mask
	if strings.Contains(mask, ".") {
		prefix = maskToCIDR(mask)
	}

	return ipConfig{
		ClientIP: field(0),
		Gateway:  field(2),
		Prefix:   prefix,
		Iface:    field(5),
	}
}

func maskToCIDR(mask string) string {
	cidr := 0

	for _, octet := range strings.Split(mask, ".") {
		switch octet {
		case "255":
			cidr += 8
		case "254":
			cidr += 7
		case "252":
			cidr += 6
		case "248":
			cidr += 5
		case "240":
			cidr += 4
		case "224":
			cidr += 3
		case "192":
			cidr += 2
		case "128":
			cidr++
		}
	}

	return strconv.Itoa(cidr)
}

func (i *Installer) configureStaticIP(ctx context.Context, selectedIface, value string) error {
	cfg := parseIPParam(value)

	iface := selectedIface
	if cfg.Iface != "" && pathExists(filepath.Join(i.SysfsRoot, "class/net", cfg.Iface)) {
		iface = cfg.Iface
	}

	i.Logger.Printf("configuring %s with %s/%s gw %s", iface, cfg.ClientIP, cfg.Prefix, cfg.Gateway)

	if err := retry(ctx, 5, 2*time.Second, "link up "+iface, i.Sleep, i.Logger, func() error {
		return i.Runner.Run(ctx, "ip", "link", "set", iface, "up")
	}); err != nil {
		return fmt.Errorf("failed to bring up %s: %w", iface, err)
	}

	if err := retry(ctx, 3, time.Second, "add address", i.Sleep, i.Logger, func() error {
		return i.Runner.Run(ctx, "ip", "addr", "add", cfg.ClientIP+"/"+cfg.Prefix, "dev", iface)
	}); err != nil {
		i.Logger.Printf("WARNING: failed to add address")
	}

	if cfg.Gateway != "" {
		if err := retry(ctx, 3, time.Second, "add default route", i.Sleep, i.Logger, func() error {
			return i.Runner.Run(ctx, "ip", "route", "add", "default", "via", cfg.Gateway, "dev", iface)
		}); err != nil {
			i.Logger.Printf("WARNING: failed to add default route")
		}
	}

	return nil
}
