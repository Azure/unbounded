// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestKubeletAuthInfo_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		auth    KubeletAuthInfo
		wantErr string
	}{
		{
			name: "bootstrap token only",
			auth: KubeletAuthInfo{
				BootstrapToken: "abc123.secret456",
			},
		},
		{
			name: "exec credential only",
			auth: KubeletAuthInfo{
				ExecCredential: &clientcmdapi.ExecConfig{
					Command:    "/usr/local/bin/kubelogin",
					APIVersion: "client.authentication.k8s.io/v1",
				},
			},
		},
		{
			name: "both set",
			auth: KubeletAuthInfo{
				BootstrapToken: "abc123.secret456",
				ExecCredential: &clientcmdapi.ExecConfig{
					Command: "/usr/local/bin/kubelogin",
				},
			},
			wantErr: "mutually exclusive",
		},
		{
			name:    "neither set",
			auth:    KubeletAuthInfo{},
			wantErr: "one of BootstrapToken or ExecCredential must be set",
		},
		{
			name: "exec credential without command",
			auth: KubeletAuthInfo{
				ExecCredential: &clientcmdapi.ExecConfig{
					APIVersion: "client.authentication.k8s.io/v1",
				},
			},
			wantErr: "Command is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.auth.Validate()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCRIConfig_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	cfg := AgentConfig{
		MachineName: "test",
		CRI: CRIConfig{
			Containerd: ContainerdConfig{Version: "2.1.0"},
			Runc:       RuncConfig{Version: "1.2.0"},
		},
		CNI: CNIConfig{PluginVersion: "1.6.0"},
	}

	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	var decoded AgentConfig
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "2.1.0", decoded.CRI.Containerd.Version)
	assert.Equal(t, "1.2.0", decoded.CRI.Runc.Version)
	assert.Equal(t, "1.6.0", decoded.CNI.PluginVersion)
}

func TestCRIConfig_OmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	cfg := AgentConfig{MachineName: "test"}

	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &parsed))

	// CRI sub-structs should have no version keys when empty (omitempty).
	cri := parsed["CRI"].(map[string]interface{})
	containerd := cri["Containerd"].(map[string]interface{})
	assert.NotContains(t, containerd, "Version")

	runc := cri["Runc"].(map[string]interface{})
	assert.NotContains(t, runc, "Version")

	cni := parsed["CNI"].(map[string]interface{})
	assert.NotContains(t, cni, "PluginVersion")
}

func TestAgentConfig_DeepCopy(t *testing.T) {
	t.Parallel()

	original := &AgentConfig{
		MachineName: "machine-a",
		Kubelet: AgentKubeletConfig{
			Labels: map[string]string{
				"env": "test",
			},
			RegisterWithTaints: []string{"dedicated=test:NoSchedule"},
		},
	}

	copy := original.DeepCopy()
	require.NotSame(t, original, copy)
	require.Equal(t, original, copy)

	copy.Kubelet.Labels["env"] = "prod"
	copy.Kubelet.RegisterWithTaints[0] = "dedicated=prod:NoSchedule"

	require.Equal(t, "test", original.Kubelet.Labels["env"])
	require.Equal(t, "dedicated=test:NoSchedule", original.Kubelet.RegisterWithTaints[0])
}

func TestAgentConfig_DeepCopyNil(t *testing.T) {
	t.Parallel()

	var original *AgentConfig
	require.Nil(t, original.DeepCopy())
}

func TestAgentConfig_BackfillNodeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		nodeName    string
		machineName string
		want        string
		wantErr     string
	}{
		{
			name:        "config override",
			nodeName:    "configured-node",
			machineName: "machine-1",
			want:        "configured-node",
		},
		{
			name:        "trimmed config override",
			nodeName:    " configured-node ",
			machineName: "machine-1",
			want:        "configured-node",
		},
		{
			name:        "invalid config override errors",
			nodeName:    "Configured_Node",
			machineName: "machine-1",
			wantErr:     "node name override",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &AgentConfig{
				MachineName: tt.machineName,
				NodeName:    tt.nodeName,
			}

			err := cfg.BackfillNodeName()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.NodeName)
		})
	}
}

func TestAgentConfig_BackfillNodeName_UsesHostHostname(t *testing.T) {
	t.Parallel()

	cfg := &AgentConfig{MachineName: "machine-1"}

	err := cfg.BackfillNodeName()
	require.NoError(t, err)

	hostname, err := os.Hostname()
	require.NoError(t, err)

	want := "machine-1"
	if nodeName := strings.TrimSpace(hostname); len(validation.IsDNS1123Subdomain(nodeName)) == 0 {
		want = nodeName
	}

	assert.Equal(t, want, cfg.NodeName)
}
