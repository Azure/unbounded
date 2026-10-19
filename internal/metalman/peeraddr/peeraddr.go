// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package peeraddr

import (
	"net"
	"net/http"
)

// Host returns the host part of r.RemoteAddr, the address of the direct TCP
// peer. If RemoteAddr has no port, it is returned unchanged.
func Host(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}
