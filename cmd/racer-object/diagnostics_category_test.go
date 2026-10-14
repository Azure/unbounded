// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestDiagnosticCategoryTLS(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"verification", &tls.CertificateVerificationError{Err: errors.New("private")}},
		{"authority", x509.UnknownAuthorityError{}},
		{"hostname", x509.HostnameError{Host: "private"}},
		{"invalid", x509.CertificateInvalidError{Reason: x509.Expired}},
		{"record-header", tls.RecordHeaderError{Msg: "private"}},
		{"alert", tls.AlertError(42)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Mirror the origin wrapping: ErrUnavailable plus the send failure.
			err := fmt.Errorf("%w: %w", racersdk.ErrUnavailable, &net.OpError{Op: "dial", Err: tc.err})
			if got := diagnosticCategory(err, "fallback"); got != "tls" {
				t.Fatalf("category = %q, want tls", got)
			}
		})
	}

	plain := fmt.Errorf("%w: %w", racersdk.ErrUnavailable, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED})
	if got := diagnosticCategory(plain, "fallback"); got != "connectionrefused" {
		t.Fatalf("non-TLS send failure category = %q", got)
	}
}
