package provision

import (
	_ "embed"
)

//go:embed assets/unbounded-agent-install.sh
var unboundedAgentInstallScript string

// UnboundedAgentInstallScript returns the install script for using the
// unbounded-agent to bootstrap a node.
func UnboundedAgentInstallScript() string {
	return unboundedAgentInstallScript
}
