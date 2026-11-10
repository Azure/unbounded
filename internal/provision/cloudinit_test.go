// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestReplaceCloudInit(t *testing.T) {
	t.Parallel()

	cloudInit, err := ReplaceCloudInit(UnboundedAgentConfig{
		AgentConfig: AgentConfig{
			MachineName: "worker-1",
			Kubelet: AgentKubeletConfig{
				ApiServer: "api.example.com:443",
				Auth:      KubeletAuthInfo{BootstrapToken: "abc.def"},
			},
		},
	}, []string{"AGENT_VERSION='v1.2.3'"})

	require.NoError(t, err)
	require.Contains(t, cloudInit, "#cloud-config")
	require.Contains(t, cloudInit, replaceAgentConfigPath)
	require.Contains(t, cloudInit, replaceInstallPath)
	require.Contains(t, cloudInit, `"MachineName": "worker-1"`)
	require.Contains(t, cloudInit, `"BootstrapToken": "abc.def"`)
	require.Contains(t, cloudInit, "AGENT_VERSION='v1.2.3' UNBOUNDED_AGENT_CONFIG_FILE=/tmp/unbounded-agent.json bash /tmp/machina-agent-install.sh")
	require.Contains(t, cloudInit, "unbounded-agent")
}

func TestAllocationCloudInit(t *testing.T) {
	t.Parallel()

	agentConfig := UnboundedAgentConfig{
		AgentConfig: AgentConfig{
			MachineName: "ub-1234",
			NodeName:    "ub-1234",
			Kubelet: AgentKubeletConfig{
				ApiServer:     "api.example.com:443",
				Auth:          KubeletAuthInfo{BootstrapToken: "abc123.0123456789abcdef"},
				Configuration: map[string]any{"providerID": "unbounded:///machineallocations/1234"},
				Labels:        map[string]string{"b": "2", "a": "1"},
			},
		},
	}
	installEnv := []string{"AGENT_URL='https://artifacts.example.com/agent.tar.gz'", "AGENT_SHA256='abc'"}
	files := map[string]string{"zeta.sh": "echo zeta\n", "run": "#!/bin/bash\necho run\n", "alpha.conf": "a=1\n"}

	t.Run("renders files and one command", func(t *testing.T) {
		t.Parallel()

		userData, err := AllocationCloudInit(agentConfig, installEnv, files)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(userData, "#cloud-config\n"))

		var cfg cloudInitConfig
		require.NoError(t, yaml.Unmarshal([]byte(strings.TrimPrefix(userData, "#cloud-config\n")), &cfg))

		paths := make([]string, 0, len(cfg.WriteFiles))
		for _, file := range cfg.WriteFiles {
			paths = append(paths, file.Path)
		}

		require.Equal(t, []string{
			"/var/lib/unbounded/bootstrap/alpha.conf",
			"/var/lib/unbounded/bootstrap/run",
			"/var/lib/unbounded/bootstrap/zeta.sh",
			replaceAgentConfigPath,
			replaceInstallPath,
		}, paths)

		for _, file := range cfg.WriteFiles[:3] {
			require.Equal(t, "0700", file.Permissions, file.Path)
			require.Equal(t, "root:root", file.Owner, file.Path)
		}

		require.Equal(t, "#!/bin/bash\necho run\n", cfg.WriteFiles[1].Content)
		require.Equal(t, "0600", cfg.WriteFiles[3].Permissions)
		require.Contains(t, cfg.WriteFiles[3].Content, `"providerID": "unbounded:///machineallocations/1234"`)
		require.Contains(t, cfg.WriteFiles[3].Content, `"BootstrapToken": "abc123.0123456789abcdef"`)
		require.Equal(t, UnboundedAgentInstallScript(), cfg.WriteFiles[4].Content)

		require.Len(t, cfg.Runcmd, 1)
		require.Equal(t, []string{"bash", "-lc"}, cfg.Runcmd[0][:2])

		command := cfg.Runcmd[0][2]
		require.Equal(t,
			`trap 'rm -f -- '\''/tmp/unbounded-agent.json'\'' '\''/tmp/machina-agent-install.sh'\''' EXIT`+"\n"+
				`'/var/lib/unbounded/bootstrap/run' && AGENT_URL='https://artifacts.example.com/agent.tar.gz' AGENT_SHA256='abc' `+
				`UNBOUNDED_AGENT_CONFIG_FILE=/tmp/unbounded-agent.json bash /tmp/machina-agent-install.sh`,
			command)

		out, err := exec.Command("bash", "-n", "-c", command).CombinedOutput()
		require.NoError(t, err, "%s", out)
	})

	t.Run("run failure skips the installer and still cleans up", func(t *testing.T) {
		t.Parallel()

		userData, err := AllocationCloudInit(agentConfig, nil, files)
		require.NoError(t, err)

		var cfg cloudInitConfig
		require.NoError(t, yaml.Unmarshal([]byte(strings.TrimPrefix(userData, "#cloud-config\n")), &cfg))

		dir := t.TempDir()
		marker := filepath.Join(dir, "installer-ran")
		config := filepath.Join(dir, "agent.json")
		installer := filepath.Join(dir, "install.sh")
		run := filepath.Join(dir, "run")

		require.NoError(t, os.WriteFile(config, []byte("{}"), 0o600))
		require.NoError(t, os.WriteFile(installer, []byte("touch "+marker+"\n"), 0o600))
		require.NoError(t, os.WriteFile(run, []byte("#!/bin/bash\nexit 3\n"), 0o700))

		command := strings.NewReplacer(
			replaceAgentConfigPath, config,
			replaceInstallPath, installer,
			AllocationBootstrapDir+"/run", run,
		).Replace(cfg.Runcmd[0][2])

		err = exec.Command("bash", "-c", command).Run()

		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		require.Equal(t, 3, exitErr.ExitCode())
		require.NoFileExists(t, marker)
		require.NoFileExists(t, config)
		require.NoFileExists(t, installer)
	})

	t.Run("deterministic", func(t *testing.T) {
		t.Parallel()

		first, err := AllocationCloudInit(agentConfig, installEnv, files)
		require.NoError(t, err)

		for range 10 {
			again, err := AllocationCloudInit(agentConfig, installEnv, files)
			require.NoError(t, err)
			require.Equal(t, first, again)
		}
	})

	t.Run("without bootstrap files runs only the installer", func(t *testing.T) {
		t.Parallel()

		userData, err := AllocationCloudInit(agentConfig, nil, nil)
		require.NoError(t, err)

		var cfg cloudInitConfig
		require.NoError(t, yaml.Unmarshal([]byte(strings.TrimPrefix(userData, "#cloud-config\n")), &cfg))
		require.Len(t, cfg.WriteFiles, 2)
		require.Len(t, cfg.Runcmd, 1)
		require.NotContains(t, cfg.Runcmd[0][2], AllocationBootstrapDir)
		require.Contains(t, cfg.Runcmd[0][2], "trap ")
	})

	t.Run("invalid bootstrap files", func(t *testing.T) {
		t.Parallel()

		for name, invalid := range map[string]map[string]string{
			"missing run":   {"other": "echo"},
			"nested path":   {"run": "echo", "dir/file": "x"},
			"parent":        {"run": "echo", "..": "x"},
			"absolute path": {"run": "echo", "/etc/passwd": "x"},
			"space":         {"run": "echo", "a b": "x"},
		} {
			_, err := AllocationCloudInit(agentConfig, nil, invalid)
			require.Error(t, err, name)
		}
	})
}
