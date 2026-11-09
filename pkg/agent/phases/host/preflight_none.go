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

// CheckNoneBackend runs the none backend's own preflight: it fails when the
// host already has a kubelet or containerd the agent does not own (unit,
// binary or running process), or when /etc, /opt or /var is not writable. The
// failure names the mode to use instead, rather than switching silently. It
// mutates nothing.
func CheckNoneBackend(log *slog.Logger) preflight.Checker {
	return checkNoneBackend(log, &none.Backend{Log: log})
}

func checkNoneBackend(log *slog.Logger, b *none.Backend) preflight.Checker {
	return simpleHostChecker{name: checkNoneBackendName, check: func(context.Context) []preflight.Result {
		log.Debug("running none backend preflight")

		if err := b.Preflight(); err != nil {
			return preflight.ResultsError(checkNoneBackendName, "isolation mode none", "%s", err.Error())
		}

		return preflight.ResultsOK(checkNoneBackendName, "isolation mode none", "no unmanaged kubelet or containerd, and /etc, /opt and /var are writable")
	}}
}
