// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"net"
	"net/http"
	"net/http/httptest"
)

// testPeerHeader lets tests that drive a real httptest.Server choose the
// peer address the handler observes. It is honored only by
// newTestPeerServer, never by production code.
const testPeerHeader = "X-Test-Peer-IP"

// newTestPeerServer starts an httptest.Server whose requests appear to come
// from the address in testPeerHeader, simulating a direct TCP peer.
func newTestPeerServer(h http.Handler) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := r.Header.Get(testPeerHeader); ip != "" {
			r.RemoteAddr = net.JoinHostPort(ip, "1234")
			r.Header.Del(testPeerHeader)
		}

		h.ServeHTTP(w, r)
	}))
}
