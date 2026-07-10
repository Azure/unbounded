// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// NetworkSpec describes the pod-local L2 network playpen creates.
type NetworkSpec struct {
	BridgeName string
	TapName    string
	VXLANName  string
	RemoteIP   net.IP
	LocalIP    net.IP
	VNI        int
	Port       int
	MTU        int
}

// Network owns the configured TAP file descriptor and kernel links.
type Network struct {
	Spec    NetworkSpec
	TapFile *os.File
}

// BuildNetworkSpec converts a normalized Config into a pure network spec.
func BuildNetworkSpec(cfg Config) (NetworkSpec, error) {
	remote := net.ParseIP(cfg.VXLANRemote)
	if remote == nil {
		return NetworkSpec{}, fmt.Errorf("invalid vxlan remote %q", cfg.VXLANRemote)
	}

	var local net.IP
	if cfg.VXLANLocal != "" {
		local = net.ParseIP(cfg.VXLANLocal)
		if local == nil {
			return NetworkSpec{}, fmt.Errorf("invalid vxlan local %q", cfg.VXLANLocal)
		}
	}

	return NetworkSpec{
		BridgeName: cfg.BridgeName,
		TapName:    cfg.TapName,
		VXLANName:  cfg.VXLANName,
		RemoteIP:   remote,
		LocalIP:    local,
		VNI:        cfg.VXLANVNI,
		Port:       cfg.VXLANPort,
		MTU:        cfg.MTU,
	}, nil
}

// SetupNetwork creates br-playpen, vxlan0, and tap0 in the current network
// namespace. The returned TAP file must stay open while QEMU is running.
func SetupNetwork(cfg Config) (*Network, error) {
	spec, err := BuildNetworkSpec(cfg)
	if err != nil {
		return nil, err
	}

	if spec.LocalIP == nil {
		local, err := detectLocalIP(spec.RemoteIP)
		if err != nil {
			return nil, err
		}

		spec.LocalIP = local
	}

	bridge, err := ensureBridge(spec.BridgeName, spec.MTU)
	if err != nil {
		return nil, err
	}

	var tapFile *os.File

	cleanup := func() error {
		var errs []error

		if err := closeFile(tapFile, spec.TapName); err != nil {
			errs = append(errs, err)
		}

		for _, name := range []string{spec.VXLANName, spec.TapName, spec.BridgeName} {
			if err := deleteLink(name); err != nil {
				errs = append(errs, err)
			}
		}

		return errors.Join(errs...)
	}

	if err := replaceVXLAN(spec, bridge); err != nil {
		cleanupErr := cleanup()

		return nil, errors.Join(err, cleanupErr)
	}

	tapFile, err = createTAP(cfg.TUNPath, spec.TapName)
	if err != nil {
		cleanupErr := cleanup()

		return nil, errors.Join(err, cleanupErr)
	}

	if err := attachLinkToBridge(spec.TapName, bridge, spec.MTU); err != nil {
		cleanupErr := cleanup()

		return nil, errors.Join(err, cleanupErr)
	}

	if err := netlink.LinkSetUp(bridge); err != nil {
		cleanupErr := cleanup()

		return nil, errors.Join(fmt.Errorf("bring up bridge %s: %w", spec.BridgeName, err), cleanupErr)
	}

	return &Network{Spec: spec, TapFile: tapFile}, nil
}

// Close releases the TAP file descriptor and removes the links created for the
// current playpen pod.
func (n *Network) Close() error {
	var errs []error

	if err := closeFile(n.TapFile, n.Spec.TapName); err != nil {
		errs = append(errs, err)
	}

	for _, name := range []string{n.Spec.VXLANName, n.Spec.TapName, n.Spec.BridgeName} {
		if err := deleteLink(name); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func ensureBridge(name string, mtu int) (netlink.Link, error) {
	link, err := netlink.LinkByName(name)
	if err == nil {
		if link.Type() != "bridge" {
			return nil, fmt.Errorf("link %s already exists with type %s", name, link.Type())
		}

		if err := netlink.LinkSetMTU(link, mtu); err != nil {
			return nil, fmt.Errorf("set bridge %s mtu: %w", name, err)
		}

		return link, nil
	}

	if !isLinkNotFound(err) {
		return nil, fmt.Errorf("get bridge %s: %w", name, err)
	}

	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name, MTU: mtu}}
	if err := netlink.LinkAdd(bridge); err != nil {
		return nil, fmt.Errorf("create bridge %s: %w", name, err)
	}

	created, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("get created bridge %s: %w", name, err)
	}

	return created, nil
}

func replaceVXLAN(spec NetworkSpec, bridge netlink.Link) error {
	if err := deleteLink(spec.VXLANName); err != nil {
		return err
	}

	vxlan := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{
			Name: spec.VXLANName,
			MTU:  spec.MTU,
		},
		VxlanId:  spec.VNI,
		SrcAddr:  spec.LocalIP,
		Group:    spec.RemoteIP,
		Port:     spec.Port,
		Learning: true,
	}

	if err := netlink.LinkAdd(vxlan); err != nil {
		return fmt.Errorf("create vxlan %s: %w", spec.VXLANName, err)
	}

	return attachLinkToBridge(spec.VXLANName, bridge, spec.MTU)
}

func createTAP(tunPath, name string) (*os.File, error) {
	if err := deleteLink(name); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(tunPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", tunPath, err)
	}

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		closeErr := closeFile(file, tunPath)

		return nil, errors.Join(fmt.Errorf("create tap ifreq %s: %w", name, err), closeErr)
	}

	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)

	if err := unix.IoctlIfreq(int(file.Fd()), unix.TUNSETIFF, ifr); err != nil {
		closeErr := closeFile(file, tunPath)

		return nil, errors.Join(fmt.Errorf("create tap %s: %w", name, err), closeErr)
	}

	return file, nil
}

func attachLinkToBridge(name string, bridge netlink.Link, mtu int) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("get link %s: %w", name, err)
	}

	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		return fmt.Errorf("set link %s mtu: %w", name, err)
	}

	if err := netlink.LinkSetMaster(link, bridge); err != nil {
		return fmt.Errorf("enslave link %s to bridge %s: %w", name, bridge.Attrs().Name, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up link %s: %w", name, err)
	}

	return nil
}

func detectLocalIP(remote net.IP) (net.IP, error) {
	routes, err := netlink.RouteGet(remote)
	if err != nil {
		return nil, fmt.Errorf("detect local vxlan IP for remote %s: %w", remote, err)
	}

	for _, route := range routes {
		if route.Src != nil && route.Src.IsGlobalUnicast() {
			return route.Src, nil
		}
	}

	for _, route := range routes {
		if route.LinkIndex == 0 {
			continue
		}

		link, err := netlink.LinkByIndex(route.LinkIndex)
		if err != nil {
			continue
		}

		addrs, err := netlink.AddrList(link, familyForIP(remote))
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if addr.IP != nil && addr.IP.IsGlobalUnicast() {
				return addr.IP, nil
			}
		}
	}

	return nil, fmt.Errorf("could not detect local vxlan IP for remote %s; set --vxlan-local", remote)
}

func familyForIP(ip net.IP) int {
	if ip.To4() != nil {
		return netlink.FAMILY_V4
	}

	return netlink.FAMILY_V6
}

func deleteLink(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		if isLinkNotFound(err) {
			return nil
		}

		return fmt.Errorf("get link %s: %w", name, err)
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("delete link %s: %w", name, err)
	}

	return nil
}

func isLinkNotFound(err error) bool {
	var notFound netlink.LinkNotFoundError

	return errors.As(err, &notFound)
}

func closeFile(file *os.File, name string) error {
	if file == nil {
		return nil
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}

	return nil
}
