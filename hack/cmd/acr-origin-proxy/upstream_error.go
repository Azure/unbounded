// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
)

type upstreamErrorReason string

const (
	upstreamErrorCanceled           upstreamErrorReason = "canceled"
	upstreamErrorConnectionRefused  upstreamErrorReason = "connection_refused"
	upstreamErrorConnectionReset    upstreamErrorReason = "connection_reset"
	upstreamErrorDNS                upstreamErrorReason = "dns"
	upstreamErrorNetworkUnreachable upstreamErrorReason = "network_unreachable"
	upstreamErrorTimeout            upstreamErrorReason = "timeout"
	upstreamErrorUnexpectedEOF      upstreamErrorReason = "unexpected_eof"
	upstreamErrorOther              upstreamErrorReason = "other"
)

func classifyUpstreamError(err error) upstreamErrorReason {
	switch {
	case errors.Is(err, context.Canceled):
		return upstreamErrorCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return upstreamErrorTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		return upstreamErrorConnectionRefused
	case errors.Is(err, syscall.ECONNRESET):
		return upstreamErrorConnectionReset
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return upstreamErrorNetworkUnreachable
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return upstreamErrorUnexpectedEOF
	}

	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return upstreamErrorDNS
	}

	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return upstreamErrorTimeout
	}

	return upstreamErrorOther
}
