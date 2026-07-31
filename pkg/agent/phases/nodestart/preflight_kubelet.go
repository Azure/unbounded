// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"syscall"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkKubeletBindAddressName = "kubelet-bind-address"
	kubeletBindAddress          = "0.0.0.0:10250"
)

type kubeletBindAddressChecker struct {
	log    *slog.Logger
	listen func(network, address string) (io.Closer, error)
}

// CheckKubeletBindAddress returns a non-mutating checker that verifies the
// kubelet bind address is available in the host network namespace.
func CheckKubeletBindAddress(log *slog.Logger) preflight.Checker {
	return kubeletBindAddressChecker{
		log: log,
		listen: func(network, address string) (io.Closer, error) {
			return net.Listen(network, address)
		},
	}
}

func (c kubeletBindAddressChecker) Name() string { return checkKubeletBindAddressName }

func (c kubeletBindAddressChecker) Check(context.Context) []preflight.Result {
	listener, err := c.listen("tcp", kubeletBindAddress)
	if err != nil {
		c.log.Debug("kubelet bind address probe failed", "address", kubeletBindAddress, "error", err)

		if errors.Is(err, syscall.EADDRINUSE) {
			return preflight.ResultsError(
				checkKubeletBindAddressName,
				kubeletBindAddress,
				"kubelet bind address is already in use",
			)
		}

		return preflight.ResultsError(
			checkKubeletBindAddressName,
			kubeletBindAddress,
			"kubelet bind address availability could not be determined",
		)
	}
	defer listener.Close() //nolint:errcheck // best effort close

	return preflight.ResultsOK(
		checkKubeletBindAddressName,
		kubeletBindAddress,
		"kubelet bind address is available",
	)
}
