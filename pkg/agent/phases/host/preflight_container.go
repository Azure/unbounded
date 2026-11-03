// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkContainerBackendName = "container-backend"

// CheckContainerBackend runs the container backend's own preflight: cgroup v2,
// an SELinux label the kernel accepts when enforcing, and writable /etc, /opt
// and /var. The OCI runtime itself is installed by the agent later in
// bootstrap, so a missing runtime is not an error here. PROVISIONAL: the
// container mode is experimental and these checks are modeled on the design,
// not on a verified Azure Container Linux node. It mutates nothing.
func CheckContainerBackend(log *slog.Logger) preflight.Checker {
	return checkContainerBackend(log, &container.Backend{Log: log, RuntimeProvisioned: true})
}

func checkContainerBackend(log *slog.Logger, b *container.Backend) preflight.Checker {
	return simpleHostChecker{name: checkContainerBackendName, check: func(context.Context) []preflight.Result {
		log.Debug("running container backend preflight")

		if err := b.Preflight(); err != nil {
			return preflight.ResultsError(checkContainerBackendName, "isolation mode container (experimental)", "%s", err.Error())
		}

		return preflight.ResultsOK(checkContainerBackendName, "isolation mode container (experimental)", "cgroup v2 is available, the SELinux label is accepted when enforcing, and /etc, /opt and /var are writable")
	}}
}
