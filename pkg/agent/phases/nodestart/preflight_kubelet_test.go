// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestCheckKubeletBindAddressAvailable(t *testing.T) {
	var network, address string

	closed := false

	checker := kubeletBindAddressChecker{
		log: slog.New(slog.DiscardHandler),
		listen: func(gotNetwork, gotAddress string) (io.Closer, error) {
			network, address = gotNetwork, gotAddress

			return closerFunc(func() error {
				closed = true

				return nil
			}), nil
		},
	}

	results := checker.Check(context.Background())

	assert.Equal(t, "tcp", network)
	assert.Equal(t, kubeletBindAddress, address)
	assert.True(t, closed)
	assert.Equal(t, preflight.ResultsOK(
		checkKubeletBindAddressName,
		kubeletBindAddress,
		"kubelet bind address is available",
	), results)
}

type closerFunc func() error

func (f closerFunc) Close() error {
	return f()
}

func TestCheckKubeletBindAddressCollision(t *testing.T) {
	checker := kubeletBindAddressChecker{
		log: slog.New(slog.DiscardHandler),
		listen: func(string, string) (io.Closer, error) {
			return nil, syscall.EADDRINUSE
		},
	}

	results := checker.Check(context.Background())

	assert.Equal(t, preflight.ResultsError(
		checkKubeletBindAddressName,
		kubeletBindAddress,
		"kubelet bind address is already in use",
	), results)
}

func TestCheckKubeletBindAddressProbeFailure(t *testing.T) {
	checker := kubeletBindAddressChecker{
		log: slog.New(slog.DiscardHandler),
		listen: func(string, string) (io.Closer, error) {
			return nil, errors.New("probe failed")
		},
	}

	results := checker.Check(context.Background())

	assert.Equal(t, preflight.ResultsError(
		checkKubeletBindAddressName,
		kubeletBindAddress,
		"kubelet bind address availability could not be determined",
	), results)
}
