// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	_ "embed"
)

// FirstBootBootstrapUnit is the unit an Ignition config installs to bootstrap
// the agent on every boot. kubectl-unbounded writes it and reset removes it.
const FirstBootBootstrapUnit = "unbounded-agent-bootstrap.service"

//go:embed assets/unbounded-agent-install.sh
var unboundedAgentInstallScript string

// UnboundedAgentInstallScript returns the install script for using the
// unbounded-agent to bootstrap a node.
func UnboundedAgentInstallScript() string {
	return unboundedAgentInstallScript
}
