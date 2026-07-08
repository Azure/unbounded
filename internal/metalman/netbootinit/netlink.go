// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

type NetworkConfigurator interface {
	LinkSetUp(name string) error
	AddrAdd(name string, ipNet *net.IPNet) error
	RouteAddDefault(name string, gateway net.IP) error
}

type realNetworkConfigurator struct{}

func (realNetworkConfigurator) LinkSetUp(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("find link %s: %w", name, err)
	}

	return netlink.LinkSetUp(link)
}

func (realNetworkConfigurator) AddrAdd(name string, ipNet *net.IPNet) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("find link %s: %w", name, err)
	}

	return netlink.AddrAdd(link, &netlink.Addr{IPNet: ipNet})
}

func (realNetworkConfigurator) RouteAddDefault(name string, gateway net.IP) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("find link %s: %w", name, err)
	}

	return netlink.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Gw:        gateway,
	})
}
