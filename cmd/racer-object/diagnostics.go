// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"syscall"

	"github.com/Azure/unbounded/internal/racer/object"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// diagnosticError retains the cause without printing paths, URLs, or credentials.
type diagnosticError struct {
	message  string
	category string
	cause    error
}

func (e *diagnosticError) Error() string { return e.message + " (category=" + e.category + ")" }
func (e *diagnosticError) Unwrap() error { return e.cause }

// message and fallback must be fixed strings, never external error text.
func safeDiagnostic(message, fallback string, cause error) error {
	return &diagnosticError{message: message, category: diagnosticCategory(cause, fallback), cause: cause}
}

func diagnosticCategory(err error, fallback string) string {
	// A TLS or certificate failure is a configuration error even though the
	// origin maps it to ErrUnavailable, so check it before the sentinels.
	if tlsFailure(err) {
		return "tls"
	}

	for _, entry := range []struct {
		cause    error
		category string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "timeout"},
		{os.ErrDeadlineExceeded, "timeout"},
		{os.ErrPermission, "permission"},
		{syscall.EADDRINUSE, "addressinuse"},
		{syscall.EADDRNOTAVAIL, "addressunavailable"},
		{syscall.ECONNREFUSED, "connectionrefused"},
		{syscall.ECONNRESET, "connectionreset"},
		{syscall.ENOSPC, "nospace"},
		{syscall.EROFS, "readonly"},
		{syscall.EMFILE, "resourceexhausted"},
		{syscall.ENFILE, "resourceexhausted"},
		{syscall.ENOTDIR, "notdirectory"},
		{syscall.ELOOP, "symlink"},
		{os.ErrNotExist, "notfound"},
		{os.ErrExist, "exists"},
		{net.ErrClosed, "closed"},
		{racersdk.ErrUnauthorized, "unauthorized"},
		{racersdk.ErrForbidden, "forbidden"},
		{racersdk.ErrNotFound, "notfound"},
		{racersdk.ErrVersionMismatch, "versionmismatch"},
		{racersdk.ErrRangeNotSatisfiable, "range"},
		{racersdk.ErrInvalidRequest, "invalid"},
		{racersdk.ErrUnavailable, "unavailable"},
		{os.ErrInvalid, "invalid"},
	} {
		if errors.Is(err, entry.cause) {
			return entry.category
		}
	}

	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}

	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return "dns"
	}

	if category := object.InvalidResponseCategory(err); category != "" {
		return category
	}

	return fallback
}

func tlsFailure(err error) bool {
	var (
		verification *tls.CertificateVerificationError
		authority    x509.UnknownAuthorityError
		hostname     x509.HostnameError
		invalid      x509.CertificateInvalidError
		header       tls.RecordHeaderError
		alert        tls.AlertError
	)

	return errors.As(err, &verification) || errors.As(err, &authority) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &header) || errors.As(err, &alert)
}

func logSidecarStartup(address net.Addr) {
	// Only a bound loopback TCP address is safe to include in the log.
	if tcp, ok := address.(*net.TCPAddr); ok && tcp.IP.IsLoopback() && tcp.Zone == "" && tcp.Port > 0 && tcp.Port <= 65535 {
		log.Printf("racer-object: mode=sidecar event=listening address=%s", tcp.AddrPort())
		return
	}

	log.Print("racer-object: mode=sidecar event=listening")
}

func diagnoseSidecarPanic() {
	if value := recover(); value != nil {
		if value != http.ErrAbortHandler {
			log.Print("racer-object: mode=sidecar event=request_failed category=panic")
		}

		// Abort the response without letting net/http format the panic value.
		panic(http.ErrAbortHandler)
	}
}

// net/http diagnostics contain peer addresses and may contain private text.
type safeHTTPLogWriter struct{}

func (safeHTTPLogWriter) Write(p []byte) (int, error) {
	category := "server"

	switch {
	case bytes.HasPrefix(p, []byte("http: panic serving ")):
		category = "panic"
	case bytes.HasPrefix(p, []byte("http: Accept error:")):
		category = "accept"
	case bytes.HasPrefix(p, []byte("http: TLS handshake error")):
		category = "tls"
	}

	log.Printf("racer-object: mode=sidecar event=server_error category=%s", category)

	return len(p), nil
}

func diagnoseOrigin(origin racersdk.Origin) racersdk.Origin {
	return func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		defer func() {
			if recover() != nil {
				log.Print("racer-object: mode=origin event=request_failed category=panic")
				panic(http.ErrAbortHandler)
			}
		}()

		metadata, body, err := origin(ctx, request)
		if err != nil {
			log.Printf("racer-object: mode=origin event=request_failed category=%s", diagnosticCategory(err, "upstream"))
		}

		return metadata, body, err
	}
}
