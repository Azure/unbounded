// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
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

// AllocationBootstrapDir is the directory the bootstrap files of an allocated
// host are written to.
const AllocationBootstrapDir = "/var/lib/unbounded/bootstrap"

// allocationBootstrapRunFile is the bootstrap file run before the agent
// installer.
const allocationBootstrapRunFile = "run"

// ReplaceCloudInit renders cloud-init user data that reinstalls the
// unbounded-agent after a host VM replacement.
func ReplaceCloudInit(agentConfig UnboundedAgentConfig, installEnv []string) (string, error) {
	cfg, installCommand, err := agentInstallCloudInit(agentConfig, installEnv)
	if err != nil {
		return "", err
	}

	cfg.Runcmd = [][]string{
		{"bash", "-lc", installCommand},
		{"rm", "-f", replaceAgentConfigPath, replaceInstallPath},
	}

	return marshalCloudInit(cfg)
}

// AllocationCloudInit renders first-boot cloud-init user data for a host
// launched for a MachineAllocation. Each bootstrap file is written, sorted by
// name, to AllocationBootstrapDir with mode 0700, and the "run" file runs
// before the agent installer, which starts only when it succeeds. The agent
// config and the installer are removed when the command exits. Without
// bootstrap files only the installer runs. A bootstrap file name that is not a
// plain file name, or bootstrap files without "run", is an error. The output
// is the same for the same inputs.
func AllocationCloudInit(agentConfig UnboundedAgentConfig, installEnv []string, bootstrapFiles map[string]string) (string, error) {
	cfg, installCommand, err := agentInstallCloudInit(agentConfig, installEnv)
	if err != nil {
		return "", err
	}

	if len(bootstrapFiles) > 0 {
		files, err := allocationBootstrapWriteFiles(bootstrapFiles)
		if err != nil {
			return "", err
		}

		cfg.WriteFiles = append(files, cfg.WriteFiles...)
		installCommand = ShellSingleQuote(path.Join(AllocationBootstrapDir, allocationBootstrapRunFile)) + " && " + installCommand
	}

	// The trap keeps the exit status of the bootstrap run file or the
	// installer, which a separate cleanup command would hide.
	cleanup := "rm -f -- " + ShellSingleQuote(replaceAgentConfigPath) + " " + ShellSingleQuote(replaceInstallPath)
	cfg.Runcmd = [][]string{
		{"bash", "-lc", "trap " + ShellSingleQuote(cleanup) + " EXIT\n" + installCommand},
	}

	return marshalCloudInit(cfg)
}

func allocationBootstrapWriteFiles(bootstrapFiles map[string]string) ([]cloudInitWriteFile, error) {
	if _, ok := bootstrapFiles[allocationBootstrapRunFile]; !ok {
		return nil, fmt.Errorf("bootstrap files must include %q", allocationBootstrapRunFile)
	}

	names := slices.Sorted(maps.Keys(bootstrapFiles))

	files := make([]cloudInitWriteFile, 0, len(names))
	for _, name := range names {
		if !plainFileName(name) {
			return nil, fmt.Errorf("bootstrap file name %q is not a plain file name", name)
		}

		files = append(files, cloudInitWriteFile{
			Path:        path.Join(AllocationBootstrapDir, name),
			Permissions: "0700",
			Owner:       "root:root",
			Content:     bootstrapFiles[name],
		})
	}

	return files, nil
}

// plainFileName reports whether name is a file name made of letters, digits,
// '-', '_' and '.', other than "." and "..".
func plainFileName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}

	for _, character := range name {
		valid := character == '-' || character == '_' || character == '.' ||
			(character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9')
		if !valid {
			return false
		}
	}

	return true
}

// agentInstallCloudInit returns the cloud-init files that install the agent,
// and the command that runs the installer.
func agentInstallCloudInit(agentConfig UnboundedAgentConfig, installEnv []string) (cloudInitConfig, string, error) {
	configJSON, err := json.MarshalIndent(agentConfig, "", "  ")
	if err != nil {
		return cloudInitConfig{}, "", fmt.Errorf("marshal agent config: %w", err)
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
	}

	return cfg, installCommand, nil
}

func marshalCloudInit(cfg cloudInitConfig) (string, error) {
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
