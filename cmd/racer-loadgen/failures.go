// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/Azure/unbounded/pkg/racersdk"
)

type failureReason uint8

const (
	failureOther failureReason = iota
	failureCanceled
	failureTimeout
	failureTransport
	failureIncomplete
	failureStatus
	failureSize
	failureDigest
	failureReasonCount
)

func (r failureReason) String() string {
	switch r {
	case failureCanceled:
		return "canceled"
	case failureTimeout:
		return "timeout"
	case failureTransport:
		return "transport"
	case failureIncomplete:
		return "incomplete"
	case failureStatus:
		return "http_status"
	case failureSize:
		return "size_mismatch"
	case failureDigest:
		return "digest_mismatch"
	default:
		return "other"
	}
}

// Preserve errors.Is/As and the original returned diagnostic. Logs use typed
// causes instead of raw error text, which may contain URLs or server secrets.
type pullFailure struct {
	err       error
	reason    failureReason
	kind      string
	status    int
	integrity *integrityEvidence
}

// Only populated after a full-size HTTP 200 response fails verification.
// Keep object identity out of metric labels and never retain payload or URLs.
type integrityEvidence struct {
	expectedDigest string
	actualDigest   string
	expectedSize   int64
	receivedSize   int64
	pages          *pageEvidence
}

// Validate at the logging boundary, including a length bound before parsing.
func safeSHA256(value string) string {
	if len(value) != len("sha256:")+64 {
		return "invalid"
	}

	d := digest.Digest(value)
	if d.Validate() != nil || d.Algorithm() != digest.SHA256 {
		return "invalid"
	}

	return value
}

func (e *pullFailure) Error() string { return e.err.Error() }
func (e *pullFailure) Unwrap() error { return e.err }

// Only render trusted values, not wrapper messages, URLs, DNS names, certificate
// contents, or server text. Unknown causes still expose their concrete type.
func failureDiagnostic(err error) string {
	if err == nil {
		return ""
	}

	var dns *net.DNSError
	if errors.As(err, &dns) {
		switch {
		case dns.IsNotFound:
			return "DNS lookup: no such host"
		case dns.IsTimeout:
			return "DNS lookup: timeout"
		case dns.IsTemporary:
			return "DNS lookup: temporary failure"
		default:
			return "DNS lookup failed"
		}
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}

	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		invalidCert      x509.CertificateInvalidError
		tlsRecord        tls.RecordHeaderError
	)

	switch {
	case errors.As(err, &unknownAuthority):
		return "TLS: certificate signed by unknown authority"
	case errors.As(err, &hostname):
		return "TLS: certificate hostname mismatch"
	case errors.As(err, &invalidCert):
		return fmt.Sprintf("TLS: invalid certificate (reason %d)", invalidCert.Reason)
	case errors.As(err, &tlsRecord):
		return "TLS: invalid record header"
	}

	for _, known := range []error{
		context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF, io.EOF,
		io.ErrClosedPipe, io.ErrShortWrite, net.ErrClosed,
		racersdk.ErrInvalidRequest, racersdk.ErrUnauthorized, racersdk.ErrForbidden,
		racersdk.ErrNotFound, racersdk.ErrVersionMismatch, racersdk.ErrRangeNotSatisfiable,
		racersdk.ErrUnavailable,
	} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}

	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "network timeout"
	}

	for range 64 {
		cause := errors.Unwrap(err)
		if cause == nil {
			break
		}

		err = cause
	}

	return fmt.Sprintf("unrecognized cause (%T); details redacted", err)
}

func classifyFailure(err error) failureReason {
	var timeout net.Error

	switch {
	case errors.Is(err, context.Canceled):
		return failureCanceled
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return failureTimeout
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return failureIncomplete
	}

	var failure *pullFailure
	if errors.As(err, &failure) && failure.reason < failureReasonCount {
		return failure.reason
	}

	return failureOther
}

const failureLogInterval = 30 * time.Second

// Fixed storage and one log per reason per interval per puller, shared across
// all workers. A transport storm cannot suppress the first integrity failure.
type failureLogs struct {
	mu    sync.Mutex
	slots [failureReasonCount]struct {
		next       time.Time
		suppressed uint64
	}
}

func (p *puller) reportPullFailure(err error, now time.Time, logger *slog.Logger) {
	if err == nil {
		return
	}

	reason := classifyFailure(err)
	p.metrics.pullFailures.WithLabelValues(reason.String()).Inc()
	// Expected shutdown cancellations remain visible in metrics, without noise.
	if reason == failureCanceled {
		return
	}

	p.failureLogs.mu.Lock()

	slot := &p.failureLogs.slots[reason]
	if now.Before(slot.next) {
		slot.suppressed++
		p.failureLogs.mu.Unlock()

		return
	}

	suppressed := slot.suppressed
	slot.next, slot.suppressed = now.Add(failureLogInterval), 0
	p.failureLogs.mu.Unlock()

	kind, status := "unknown", 0

	var failure *pullFailure
	if errors.As(err, &failure) {
		switch failure.kind {
		case "manifest", "config", "layer", "blob":
			kind = failure.kind
		}

		if failure.status >= 100 && failure.status <= 599 {
			status = failure.status
		}
	}

	attrs := []any{"reason", reason.String(), "kind", kind, "http_status", status, "suppressed", suppressed}
	attrs = append(attrs, "error", failureDiagnostic(err))

	if reason == failureDigest && failure != nil && failure.integrity != nil {
		evidence := failure.integrity
		expected := safeSHA256(evidence.expectedDigest)

		attrs = append(attrs, slog.Group("integrity",
			slog.String("content_digest", expected),
			slog.String("expected_digest", expected),
			slog.String("actual_digest", safeSHA256(evidence.actualDigest)),
			slog.Int64("expected_size", evidence.expectedSize),
			slog.Int64("received_size", evidence.receivedSize),
		))
		if evidence.pages != nil {
			attrs = append(attrs, slog.Any("pages", evidence.pages))
		}
	}

	logger.Warn("blob batch failed", attrs...)
}
