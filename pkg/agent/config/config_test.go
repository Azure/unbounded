// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
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
