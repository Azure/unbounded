// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package utilio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"golang.org/x/net/http2"
)

func TestIsRetryableNetworkError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", want: false},
		{name: "connection reset", err: fmt.Errorf("fetch blob: %w", syscall.ECONNRESET), want: true},
		{name: "connection refused", err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, want: true},
		{name: "DNS", err: &net.DNSError{Err: "temporary failure", Name: "registry.example.test"}, want: true},
		{name: "unexpected EOF", err: fmt.Errorf("read blob: %w", io.ErrUnexpectedEOF), want: true},
		{
			name: "HTTP/2 stream reset",
			err:  fmt.Errorf("copy blob: %w", http2.StreamError{StreamID: 13, Code: http2.ErrCodeProtocol}),
			want: true,
		},
		{name: "canceled", err: context.Canceled, want: false},
		{name: "deadline", err: context.DeadlineExceeded, want: false},
		{name: "permanent", err: errors.New("unauthorized"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := IsRetryableNetworkError(tt.err); got != tt.want {
				t.Fatalf("IsRetryableNetworkError(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}
