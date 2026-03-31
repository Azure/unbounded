package provision

import (
	_ "embed"
)

//go:embed assets/aks-flex-node-install.sh
var aksFlexInstallScript string

// AKSFlexInstallScript returns the install script for using AKS Flex Node to
// bootstrap an unbounded machine.
// TODO: this will be replaced after porting AKS Flex Node provisioning logic to this repo.
func AKSFlexInstallScript() string {
	return aksFlexInstallScript
}

//go:embed assets/unbounded-agent-install.sh
var unboundedAgentInstallScript string

// UnboundedAgentInstallScript returns the install script for using the
// unbounded-agent to bootstrap a node.
func UnboundedAgentInstallScript() string {
	return unboundedAgentInstallScript
}
