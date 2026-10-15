// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseTrustedProxyCIDRs(t *testing.T) {
	prefixes, err := parseTrustedProxyCIDRs([]string{"192.0.2.7/24", "2001:db8::/32"})
	require.NoError(t, err)
	require.Equal(t, []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("2001:db8::/32"),
	}, prefixes)
}

func TestParseTrustedProxyCIDRsRejectsInvalidValue(t *testing.T) {
	_, err := parseTrustedProxyCIDRs([]string{"not-a-cidr"})
	require.Error(t, err)
}
