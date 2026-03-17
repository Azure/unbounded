package agent

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
