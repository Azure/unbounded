// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"encoding/json"
	"testing"

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
