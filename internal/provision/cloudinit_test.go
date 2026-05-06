// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package provision

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReimageCloudInit(t *testing.T) {
	t.Parallel()

	cloudInit, err := ReimageCloudInit(UnboundedAgentConfig{
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
	require.Contains(t, cloudInit, reimageAgentConfigPath)
	require.Contains(t, cloudInit, reimageInstallPath)
	require.Contains(t, cloudInit, `"MachineName": "worker-1"`)
	require.Contains(t, cloudInit, `"BootstrapToken": "abc.def"`)
	require.Contains(t, cloudInit, "AGENT_VERSION='v1.2.3' UNBOUNDED_AGENT_CONFIG_FILE=/tmp/unbounded-agent.json bash /tmp/machina-agent-install.sh")
	require.Contains(t, cloudInit, "unbounded-agent")
}
