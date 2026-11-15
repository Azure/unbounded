// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"net/http"
	"testing"
)

// The provisioning network is unauthenticated, so a stalled client must not
// pin a handler goroutine and its connection indefinitely.
func TestNewHTTPServerTimeoutBounds(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.NewServeMux())

	if srv.ReadHeaderTimeout != httpReadHeaderTimeout || srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want %v", srv.ReadHeaderTimeout, httpReadHeaderTimeout)
	}

	if srv.ReadTimeout != httpReadTimeout || srv.ReadTimeout <= 0 {
		t.Errorf("ReadTimeout = %v, want %v", srv.ReadTimeout, httpReadTimeout)
	}

	if srv.IdleTimeout != httpIdleTimeout || srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v, want %v", srv.IdleTimeout, httpIdleTimeout)
	}

	if srv.MaxHeaderBytes != httpMaxHeaderBytes || srv.MaxHeaderBytes <= 0 {
		t.Errorf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, httpMaxHeaderBytes)
	}

	// ISO downloads can take far longer than any fixed deadline.
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 so long ISO streams are not truncated", srv.WriteTimeout)
	}
}
