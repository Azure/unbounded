// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnetwork

import (
	"bytes"
	"net/netip"
	"strconv"
	"strings"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
)

func optInt(p *int32) string {
	if p == nil {
		return "-"
	}

	return strconv.Itoa(int(*p))
}

func optString(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

// runtimeLines renders the runtime description as tokenized lines, in the
// directive order from the design doc.
func runtimeLines(t *topology, src netip.Addr) [][]string {
	var lines [][]string

	for _, i := range t.ifaces {
		switch i.kind {
		case kindEthernet:
			lines = append(lines, []string{"ethernet", i.name, i.mac, optInt(i.mtu)})
		case kindBond:
			b := i.bond
			lines = append(lines, []string{
				"bond", i.name, bondMode(b), strconv.Itoa(int(bondMIIMonitor(b))),
				optString(b.TransmitHashPolicy), optString(b.LACPRate), optInt(i.mtu), strings.Join(b.Interfaces, ","),
			})
		}
	}

	for _, i := range t.ifaces {
		if i.kind == kindVLAN {
			lines = append(lines, []string{"vlan", i.name, i.vlan.Link, strconv.Itoa(int(i.vlan.ID)), optInt(i.mtu)})
		}
	}

	for _, i := range t.ifaces {
		for _, a := range i.addr.Addresses {
			lines = append(lines, []string{"address", i.name, a})
		}
	}

	for _, i := range t.ifaces {
		if i.addr.Gateway != "" {
			lines = append(lines, []string{"route", i.name, "default", i.addr.Gateway})
		}

		for _, r := range i.addr.Routes {
			lines = append(lines, []string{"route", i.name, r.To, r.Via})
		}
	}

	for _, d := range t.spec.DNS {
		lines = append(lines, []string{"dns", d})
	}

	if s, _ := t.sourceIface(src); s != nil {
		lines = append(lines, []string{"source", s.name, src.String()})
	}

	return lines
}

// RenderRuntime renders the line-oriented runtime description consumed by
// the local installer. It validates n against lease first.
func RenderRuntime(n *v1alpha3.HostNetworkSpec, lease v1alpha3.DHCPLease) ([]byte, error) {
	t, err := build(n, lease)
	if err != nil {
		return nil, err
	}

	var b bytes.Buffer
	for _, line := range runtimeLines(t, netip.MustParseAddr(lease.IPv4)) {
		b.WriteString(strings.Join(line, " "))
		b.WriteByte('\n')
	}

	return b.Bytes(), nil
}
