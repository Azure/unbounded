// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	replaceAgentConfigPath = "/tmp/unbounded-agent.json"
	replaceInstallPath     = "/tmp/machina-agent-install.sh"
)

// cloudInitConfig is the top-level cloud-init user-data structure.
type cloudInitConfig struct {
	WriteFiles []cloudInitWriteFile `yaml:"write_files"`
	Runcmd     [][]string           `yaml:"runcmd"`
}

// cloudInitWriteFile represents a single entry under write_files.
type cloudInitWriteFile struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Owner       string `yaml:"owner"`
	Content     string `yaml:"content"`
}

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

	cfg := cloudInitConfig{
		WriteFiles: []cloudInitWriteFile{
			{
				Path:        replaceAgentConfigPath,
				Permissions: "0600",
				Owner:       "root:root",
				// json.MarshalIndent does not add a trailing newline; add one
				// so the YAML literal block scalar is well-formed.
				Content: string(configJSON) + "\n",
			},
			{
				Path:        replaceInstallPath,
				Permissions: "0755",
				Owner:       "root:root",
				// The embedded script already ends with a newline.
				Content: UnboundedAgentInstallScript(),
			},
		},
		Runcmd: [][]string{
			{"bash", "-lc", installCommand},
			{"rm", "-f", replaceAgentConfigPath, replaceInstallPath},
		},
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal cloud-init: %w", err)
	}

	return "#cloud-config\n" + string(out), nil
}

func installEnvPrefix(env []string) string {
	if len(env) == 0 {
		return ""
	}

	return strings.Join(env, " ") + " "
}
