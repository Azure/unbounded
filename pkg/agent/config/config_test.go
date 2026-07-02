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
		CNI:                   CNIConfig{PluginVersion: "1.6.0"},
		AdditionalHostDevices: []string{"/dev/uinput"},
	}

	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	var decoded AgentConfig
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "2.1.0", decoded.CRI.Containerd.Version)
	assert.Equal(t, "1.2.0", decoded.CRI.Runc.Version)
	assert.Equal(t, "1.6.0", decoded.CNI.PluginVersion)
	assert.Equal(t, []string{"/dev/uinput"}, decoded.AdditionalHostDevices)
}

func TestAgentConfig_Validate(t *testing.T) {
	t.Parallel()

	valid := func() *AgentConfig {
		return &AgentConfig{
			MachineName: "machine-1",
			NodeName:    "node-1",
			Cluster: AgentClusterConfig{
				CaCertBase64: "Y2E=",
				ClusterDNS:   "10.0.0.10",
				Version:      "1.34.0",
			},
			Kubelet: AgentKubeletConfig{
				ApiServer: "https://api.example.com:443",
				Auth: KubeletAuthInfo{
					BootstrapToken: "abc123.secret456",
				},
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*AgentConfig)
		wantErr string
	}{
		{
			name: "valid",
		},
		{
			name: "missing machine name",
			mutate: func(cfg *AgentConfig) {
				cfg.MachineName = ""
			},
			wantErr: "MachineName",
		},
		{
			name: "invalid node name",
			mutate: func(cfg *AgentConfig) {
				cfg.NodeName = "Invalid_Node"
			},
			wantErr: "NodeName",
		},
		{
			name: "missing auth allowed",
			mutate: func(cfg *AgentConfig) {
				cfg.Kubelet.Auth = KubeletAuthInfo{}
			},
		},
		{
			name: "additional host device",
			mutate: func(cfg *AgentConfig) {
				cfg.AdditionalHostDevices = []string{"/dev/uinput"}
			},
		},
		{
			name: "additional host device outside dev",
			mutate: func(cfg *AgentConfig) {
				cfg.AdditionalHostDevices = []string{"/sys/class/uinput"}
			},
			wantErr: "AdditionalHostDevices",
		},
		{
			name: "additional host device with bind separator",
			mutate: func(cfg *AgentConfig) {
				cfg.AdditionalHostDevices = []string{"/dev/uinput:/dev/uinput"}
			},
			wantErr: "AdditionalHostDevices",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := valid()
			if tt.mutate != nil {
				tt.mutate(cfg)
			}

			err := cfg.Validate()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
		})
	}
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
		AdditionalHostDevices: []string{"/dev/uinput"},
	}

	copy := original.DeepCopy()
	require.NotSame(t, original, copy)
	require.Equal(t, original, copy)

	copy.Kubelet.Labels["env"] = "prod"
	copy.Kubelet.RegisterWithTaints[0] = "dedicated=prod:NoSchedule"
	copy.AdditionalHostDevices[0] = "/dev/input/event0"

	require.Equal(t, "test", original.Kubelet.Labels["env"])
	require.Equal(t, "dedicated=test:NoSchedule", original.Kubelet.RegisterWithTaints[0])
	require.Equal(t, "/dev/uinput", original.AdditionalHostDevices[0])
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
