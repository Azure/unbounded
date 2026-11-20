// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package utilio

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	"golang.org/x/net/http2"
)

// IsRetryableNetworkError reports whether err represents a transient network or
// response-body read failure that is safe to retry.
func IsRetryableNetworkError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// A mid-body HTTP/2 stream reset is a transient failure. net/http reports
	// these through an internal type that converts to http2.StreamError.
	var streamErr http2.StreamError
	if errors.As(err, &streamErr) {
		return true
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	for _, target := range []error{
		syscall.ECONNABORTED,
		syscall.ECONNREFUSED,
		syscall.ECONNRESET,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
	} {
		if errors.Is(err, target) {
			return true
		}
	}

	return false
}
