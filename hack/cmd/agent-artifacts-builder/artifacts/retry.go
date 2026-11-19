// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
)

// isRetryableNetworkError reports whether err represents a transient network or
// response-body read failure that is safe to retry. It copies the agent's
// utilio.IsRetryableNetworkError, which this tool cannot import from outside the
// agent's tree. Delete it when #1175 moves this tool into the agent's tree.
func isRetryableNetworkError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	if errors.Is(err, io.ErrUnexpectedEOF) {
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
