// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// standardBMCServer returns a TLS server that exposes no rack manager and no
// session service, so Dial falls back to basic auth. A non-empty device ID
// resolves without any further requests.
func standardBMCServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestPoolCachesByFingerprint(t *testing.T) {
	srv := standardBMCServer(t)
	url := srv.URL + "/7"
	pinned := tlsServerFingerprint(srv)

	pool := NewPool()
	t.Cleanup(pool.Close)

	c, err := pool.Get(t.Context(), url, pinned, "admin", "secret", "System.Embedded.1")
	require.NoError(t, err)

	cached, err := pool.Get(t.Context(), url, pinned, "admin", "secret", "System.Embedded.1")
	require.NoError(t, err)
	require.Same(t, c, cached)

	// A TOFU capture and a pinned fingerprint must not share a client.
	toufu, err := pool.Get(t.Context(), url, "", "admin", "secret", "System.Embedded.1")
	require.NoError(t, err)
	require.NotSame(t, c, toufu)

	repinned, err := pool.Get(t.Context(), url, pinned, "admin", "secret", "System.Embedded.1")
	require.NoError(t, err)
	require.NotSame(t, toufu, repinned)

	// A pin that does not match the server must force a new dial instead of
	// returning the client pinned to the previous certificate.
	_, err = pool.Get(t.Context(), url, strings.Repeat("00:", 31)+"00", "admin", "secret", "System.Embedded.1")
	require.ErrorContains(t, err, "TLS cert SHA256 mismatch")

	// The failed pin evicted the cached entry, so the next Get dials again.
	after, err := pool.Get(t.Context(), url, pinned, "admin", "secret", "System.Embedded.1")
	require.NoError(t, err)
	require.NotSame(t, repinned, after)
}

func TestPoolCachesByDeviceIDForStandardBMC(t *testing.T) {
	srv := standardBMCServer(t)
	url := srv.URL + "/7"
	fingerprint := tlsServerFingerprint(srv)

	pool := NewPool()
	t.Cleanup(pool.Close)

	c, err := pool.Get(t.Context(), url, fingerprint, "admin", "secret", "System.Embedded.1")
	require.NoError(t, err)
	require.Equal(t, "System.Embedded.1", c.deviceID)

	cached, err := pool.Get(t.Context(), url, fingerprint, "admin", "secret", "System.Embedded.1")
	require.NoError(t, err)
	require.Same(t, c, cached)

	other, err := pool.Get(t.Context(), url, fingerprint, "admin", "secret", "System.2")
	require.NoError(t, err)
	require.NotSame(t, c, other)
	require.Equal(t, "System.2", other.deviceID)

	rotated, err := pool.Get(t.Context(), url, fingerprint, "admin", "new-secret", "System.2")
	require.NoError(t, err)
	require.NotSame(t, other, rotated)
}

func TestPoolIgnoresDeviceIDForRackSlots(t *testing.T) {
	srv := rackServer(t, func(http.ResponseWriter, *http.Request) {})

	pool := NewPool()
	t.Cleanup(pool.Close)

	url := srv.URL + "/7"
	fingerprint := tlsServerFingerprint(srv)

	c, err := pool.Get(t.Context(), url, fingerprint, "admin", "secret", "ignored")
	require.NoError(t, err)
	require.NotNil(t, c.rack)

	again, err := pool.Get(t.Context(), url, fingerprint, "admin", "secret", "different")
	require.NoError(t, err)
	require.Same(t, c, again)
}
