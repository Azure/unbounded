// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/Azure/unbounded/internal/racer/object"
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

func TestLogSidecarFailure(t *testing.T) {
	output := captureDiagnosticLog(t)

	logSidecarFailure(object.SidecarFailure{
		Method: "GET", Status: 503, Bucket: "my-bucket",
		Err: fmt.Errorf("%s: %w", diagnosticSecret, racersdk.ErrUnavailable),
	})
	logSidecarFailure(object.SidecarFailure{Method: "HEAD", Status: 502, Bucket: "b", Err: object.ErrMetadataMismatch})

	got := output.String()
	for _, want := range []string{
		`mode=sidecar event=response_failed method=GET status=503 category=unavailable bucket="my-bucket"`,
		`method=HEAD status=502 category=mismatch bucket="b"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}

	if strings.Contains(got, diagnosticSecret) {
		t.Fatal("sidecar failure log leaked cause text")
	}
}
