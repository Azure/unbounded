// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package confsnap

import (
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

// SharedPaths returns the host configuration paths that the node stacks of
// different isolation modes (or two versions of one) both write, at the same
// location: the containerd and kubelet configuration, the kubelet identity and
// PKI, the CNI configuration and the LocalDNS files. Units and binaries are not
// here; each backend restores those itself. The list is a superset: a path a
// mode does not use on the host is recorded as absent and stays absent.
func SharedPaths() []string {
	return []string{
		goalstates.ContainerdConfigPath,
		// The none mode's containerd runs with the agent's own configuration.
		none.ContainerdConfigPath,
		goalstates.ContainerdConfDropInDir,
		goalstates.ContainerdCertsDir,
		goalstates.KubeletConfigurationPath,
		goalstates.KubeletKubeconfigPath,
		goalstates.KubeletBootstrapKubeconfigPath,
		"/var/lib/kubelet/pki",
		goalstates.KubeletPKIDir,
		goalstates.KubeletStaticPodManifestsDir,
		goalstates.KubeletServiceDropInDir,
		// The none mode writes the kubelet's drop-ins next to its own unit.
		none.DropInDir(goalstates.SystemdSystemDir, none.UnitKubelet),
		goalstates.CNIConfigDir,
		filepath.Dir(goalstates.LocalDNSResolvConfPath),
	}
}
