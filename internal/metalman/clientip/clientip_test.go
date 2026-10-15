// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package clientip

import (
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolverIgnoresForwardedForFromUntrustedPeer(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("X-Forwarded-For", "10.0.1.42")

	ip, err := (Resolver{}).FromRequest(req)
	require.NoError(t, err)
	require.Equal(t, "203.0.113.10", ip)
}

func TestResolverUsesFirstUntrustedAddressAfterTrustedProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.20, 192.0.2.11")

	resolver := Resolver{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}
	ip, err := resolver.FromRequest(req)
	require.NoError(t, err)
	require.Equal(t, "198.51.100.20", ip)
}

func TestResolverRejectsSpoofedLeftmostAddressThroughTrustedProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-For", "10.0.1.42, 198.51.100.20")

	resolver := Resolver{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}
	ip, err := resolver.FromRequest(req)
	require.NoError(t, err)
	require.Equal(t, "198.51.100.20", ip)
}

func TestResolverRejectsMalformedForwardedForFromTrustedProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Forwarded-For", "not-an-ip")

	resolver := Resolver{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}
	_, err := resolver.FromRequest(req)
	require.Error(t, err)
}
