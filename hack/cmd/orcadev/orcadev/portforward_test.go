// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"net"
	"strings"
	"testing"
	"time"
)

// TestProbeTCP_Open spins up a real loopback listener on an
// OS-assigned port and verifies probeTCP detects it.
func TestProbeTCP_Open(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer ln.Close() //nolint:errcheck // best-effort

	host, port, _ := net.SplitHostPort(ln.Addr().String())

	if !probeTCP(host, port, 500*time.Millisecond) {
		t.Errorf("probeTCP(%s:%s) = false; want true (listener is up)", host, port)
	}
}

// TestProbeTCP_Closed binds an ephemeral port to learn one that's
// safe, then closes the listener and probes the now-closed port.
// The exact port could be reused by another process between the
// close and the probe, but that's vanishingly unlikely in test
// runtime.
func TestProbeTCP_Closed(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close() //nolint:errcheck // best-effort

	if probeTCP(host, port, 500*time.Millisecond) {
		t.Errorf("probeTCP(%s:%s) = true; want false (listener was closed)", host, port)
	}
}

// TestWaitForForwardingReader_Match verifies the stdout sentinel detection.
func TestWaitForForwardingReader_Match(t *testing.T) {
	t.Parallel()

	r := strings.NewReader("Forwarding from 127.0.0.1:8443 -> 8443\n")
	if _, err := waitForForwardingReader(r); err != nil {
		t.Errorf("waitForForwardingReader on sentinel input = %v; want nil", err)
	}
}

// TestWaitForForwardingReader_EOFBeforeSentinel verifies that EOF
// before the sentinel surfaces as an error including the captured
// output.
func TestWaitForForwardingReader_EOFBeforeSentinel(t *testing.T) {
	t.Parallel()

	r := strings.NewReader("error: services \"orca\" not found\n")

	_, err := waitForForwardingReader(r)
	if err == nil {
		t.Fatal("waitForForwardingReader on EOF input = nil; want error")
	}

	if !strings.Contains(err.Error(), "services \"orca\" not found") {
		t.Errorf("err should surface kubectl output; got %v", err)
	}
}
