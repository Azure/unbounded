// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

func TestCheckKubeletBindAddressCollisionIncludesOwner(t *testing.T) {
	checker := kubeletBindAddressChecker{
		log: slog.New(slog.DiscardHandler),
		listen: func(string, string) (io.Closer, error) {
			return nil, syscall.EADDRINUSE
		},
		findPortOwner: func(port uint16) string {
			assert.Equal(t, kubeletBindPort, port)

			return `"port-owner" (PID 123)`
		},
	}

	results := checker.Check(context.Background())

	assert.Equal(t, preflight.ResultsError(
		checkKubeletBindAddressName,
		kubeletBindAddress,
		`kubelet bind address is already in use by process "port-owner" (PID 123)`,
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

func TestFindTCPListenerOwner(t *testing.T) {
	procRoot := t.TempDir()
	err := os.MkdirAll(filepath.Join(procRoot, "123", "fd"), 0o755)
	assert.NoError(t, err)
	err = os.MkdirAll(filepath.Join(procRoot, "net"), 0o755)
	assert.NoError(t, err)
	err = os.WriteFile(
		filepath.Join(procRoot, "net", "tcp"),
		[]byte("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
			"   0: 00000000:280A 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 45678\n"),
		0o600,
	)
	assert.NoError(t, err)
	err = os.WriteFile(filepath.Join(procRoot, "123", "comm"), []byte("port-owner\n"), 0o600)
	assert.NoError(t, err)
	err = os.Symlink("socket:[45678]", filepath.Join(procRoot, "123", "fd", "4"))
	assert.NoError(t, err)

	assert.Equal(t, `"port-owner" (PID 123)`, findTCPListenerOwner(procRoot, kubeletBindPort))
	assert.Empty(t, findTCPListenerOwner(procRoot, 10251))
}
