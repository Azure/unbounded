// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package provision

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	replaceAgentConfigPath = "/tmp/unbounded-agent.json"
	replaceInstallPath     = "/tmp/machina-agent-install.sh"
)

// ReplaceCloudInit renders cloud-init user data that reinstalls the
// unbounded-agent after a host VM replacement.
func ReplaceCloudInit(agentConfig UnboundedAgentConfig, installEnv []string) (string, error) {
	configJSON, err := json.MarshalIndent(agentConfig, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal agent config: %w", err)
	}

	installCommand := fmt.Sprintf(
		"%sUNBOUNDED_AGENT_CONFIG_FILE=%s bash %s",
		installEnvPrefix(installEnv),
		replaceAgentConfigPath,
		replaceInstallPath,
	)

	return fmt.Sprintf(
		`#cloud-config
write_files:
  - path: %s
    permissions: '0600'
    owner: root:root
    content: |
%s
  - path: %s
    permissions: '0755'
    owner: root:root
    content: |
%s
runcmd:
  - [ bash, -lc, %q ]
  - [ rm, -f, %s, %s ]
`,
		replaceAgentConfigPath,
		indentBlock(string(configJSON), 6),
		replaceInstallPath,
		indentBlock(UnboundedAgentInstallScript(), 6),
		installCommand,
		replaceAgentConfigPath,
		replaceInstallPath,
	), nil
}

func installEnvPrefix(env []string) string {
	if len(env) == 0 {
		return ""
	}

	return strings.Join(env, " ") + " "
}

func indentBlock(s string, spaces int) string {
	padding := strings.Repeat(" ", spaces)

	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = padding + line
	}

	return strings.Join(lines, "\n")
}
