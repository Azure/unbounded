// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Resolver extracts a request's client address. Forwarded headers are accepted
// only when the direct TCP peer belongs to an explicitly configured proxy CIDR.
type Resolver struct {
	TrustedProxies []netip.Prefix
}

func (r Resolver) FromRequest(req *http.Request) (string, error) {
	peer, err := remoteAddress(req.RemoteAddr)
	if err != nil {
		return "", err
	}

	if !r.isTrustedProxy(peer) {
		return peer.String(), nil
	}

	var forwarded []netip.Addr

	for _, value := range req.Header.Values("X-Forwarded-For") {
		for part := range strings.SplitSeq(value, ",") {
			addr, err := netip.ParseAddr(strings.TrimSpace(part))
			if err != nil {
				return "", fmt.Errorf("parse X-Forwarded-For address: %w", err)
			}

			forwarded = append(forwarded, addr.Unmap())
		}
	}

	for i := len(forwarded) - 1; i >= 0; i-- {
		if !r.isTrustedProxy(forwarded[i]) {
			return forwarded[i].String(), nil
		}
	}

	return peer.String(), nil
}

func (r Resolver) isTrustedProxy(addr netip.Addr) bool {
	for _, prefix := range r.TrustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

func remoteAddress(remoteAddr string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}

	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse remote address %q: %w", remoteAddr, err)
	}

	return addr.Unmap(), nil
}
