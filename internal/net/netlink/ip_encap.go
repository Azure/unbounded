// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

// LWTUNNEL_IP attribute constants for IPv4 lwt encapsulation.
// These match the LWTUNNEL_IP6_* constants numerically.
const (
	lwtunnelIPID  = 1 // LWTUNNEL_IP_ID
	lwtunnelIPDst = 2 // LWTUNNEL_IP_DST
	lwtunnelIPSrc = 3 // LWTUNNEL_IP_SRC
)

// IPEncap implements netlink.Encap for LWTUNNEL_ENCAP_IP (lightweight tunnel
// IPv4 encapsulation). It carries source and destination addresses used by
// external/flow-based tunnel devices such as vxlan0 in FlowBased mode.
type IPEncap struct {
	Src net.IP
	Dst net.IP
}

// Ensure IPEncap satisfies the netlink.Encap interface at compile time.
var _ netlink.Encap = (*IPEncap)(nil)

// Type returns the lightweight tunnel encap type for IPv4.
func (e *IPEncap) Type() int {
	return nl.LWTUNNEL_ENCAP_IP
}

// Encode serializes the encap into netlink TLV attributes.
func (e *IPEncap) Encode() ([]byte, error) {
	native := nl.NativeEndian()

	var final []byte

	// ID attribute (required by kernel, set to 0)
	resID := make([]byte, 12) // 4 header + 8 value
	native.PutUint16(resID[0:2], 12)
	native.PutUint16(resID[2:4], lwtunnelIPID)
	// value stays zero
	final = append(final, resID...)

	// DST attribute (4-byte IPv4)
	if e.Dst != nil {
		dst4 := e.Dst.To4()
		if dst4 == nil {
			return nil, fmt.Errorf("IPEncap.Dst is not IPv4")
		}

		resDst := make([]byte, 4+4) // 4 header + 4 value
		native.PutUint16(resDst[0:2], 8)
		native.PutUint16(resDst[2:4], lwtunnelIPDst)
		copy(resDst[4:], dst4)
		final = append(final, resDst...)
	}

	// SRC attribute (4-byte IPv4)
	if e.Src != nil {
		src4 := e.Src.To4()
		if src4 == nil {
			return nil, fmt.Errorf("IPEncap.Src is not IPv4")
		}

		resSrc := make([]byte, 4+4) // 4 header + 4 value
		native.PutUint16(resSrc[0:2], 8)
		native.PutUint16(resSrc[2:4], lwtunnelIPSrc)
		copy(resSrc[4:], src4)
		final = append(final, resSrc...)
	}

	return final, nil
}

// Decode deserializes netlink TLV attributes into the IPEncap fields.
func (e *IPEncap) Decode(buf []byte) error {
	attrs, err := nl.ParseRouteAttr(buf)
	if err != nil {
		return err
	}

	for _, attr := range attrs {
		switch attr.Attr.Type {
		case lwtunnelIPID:
			// ignore
		case lwtunnelIPDst:
			e.Dst = net.IP(attr.Value[:])
		case lwtunnelIPSrc:
			e.Src = net.IP(attr.Value[:])
		}
	}

	return nil
}

// String returns a human-readable representation matching `ip route` output.
func (e *IPEncap) String() string {
	return fmt.Sprintf("encap ip src %s dst %s", e.Src, e.Dst)
}

// Equal returns true when the other Encap is an IPEncap with the same addresses.
func (e *IPEncap) Equal(x netlink.Encap) bool {
	o, ok := x.(*IPEncap)
	if !ok {
		return false
	}

	if !e.Src.Equal(o.Src) {
		return false
	}

	if !e.Dst.Equal(o.Dst) {
		return false
	}

	return true
}
