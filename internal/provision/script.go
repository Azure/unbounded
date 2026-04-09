// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package provision

import (
	_ "embed"
	"strings"
)

//go:embed assets/unbounded-agent-install.sh
var unboundedAgentInstallScript string

//go:embed assets/unbounded-agent-uninstall.sh
var unboundedAgentUninstallScript string

// UnboundedAgentInstallScript returns the install script for using the
// unbounded-agent to bootstrap a node.
func UnboundedAgentInstallScript() string {
	return unboundedAgentInstallScript
}

// unboundedAgentUninstallPlaceholder is the sentinel value in the uninstall
// script template that gets replaced with the actual machine name.
const unboundedAgentUninstallPlaceholder = "UNBOUNDED_MACHINE_NAME_PLACEHOLDER"

// UnboundedAgentUninstallScript returns the uninstall script with the given
// machine name baked in. The script reverses the bootstrap process: it stops
// and removes the nspawn machine, cleans up network interfaces, removes
// configuration files, and restores the host to its original state.
func UnboundedAgentUninstallScript(machineName string) string {
	return strings.ReplaceAll(unboundedAgentUninstallScript, unboundedAgentUninstallPlaceholder, machineName)
}
