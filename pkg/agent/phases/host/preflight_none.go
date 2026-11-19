// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkNoneBackendName = "none-backend"

// CheckNoneBackend checks for processes bootstrap cannot stop and required
// writable paths without changing the host.
func CheckNoneBackend(log *slog.Logger) preflight.Checker {
	return checkNoneBackend(log, &none.Backend{Log: log})
}

func checkNoneBackend(log *slog.Logger, b *none.Backend) preflight.Checker {
	return simpleHostChecker{name: checkNoneBackendName, check: func(context.Context) []preflight.Result {
		log.Debug("running none backend preflight")

		if err := b.Preflight(); err != nil {
			return preflight.ResultsError(checkNoneBackendName, "isolation mode none", "%s", err.Error())
		}

		return preflight.ResultsOK(checkNoneBackendName, "isolation mode none", "no unmanaged kubelet or containerd outside the host services, and /etc, /opt and /var are writable")
	}}
}
