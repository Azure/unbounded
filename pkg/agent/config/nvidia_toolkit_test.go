// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateNvidiaToolkit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		isolation string
		toolkit   *NvidiaToolkitConfig
		wantErr   string
	}{
		{name: "unset", isolation: "none"},
		{name: "unset on nspawn", isolation: ""},
		{name: "both paths on none", isolation: "none", toolkit: &NvidiaToolkitConfig{
			CTKPath: "/usr/local/nvidia/toolkit/nvidia-ctk", ContainerRuntimePath: "/usr/local/nvidia/toolkit/nvidia-container-runtime",
		}},
		{name: "one path on none", isolation: "none", toolkit: &NvidiaToolkitConfig{CTKPath: "/opt/nvidia-ctk"}},
		{name: "empty struct on none", isolation: "none", toolkit: &NvidiaToolkitConfig{}},
		{name: "relative", isolation: "none", toolkit: &NvidiaToolkitConfig{CTKPath: "bin/nvidia-ctk"}, wantErr: "not an absolute path"},
		{name: "traversal", isolation: "none", toolkit: &NvidiaToolkitConfig{ContainerRuntimePath: "/opt/../usr/bin/nvidia-container-runtime"}, wantErr: `must not contain ".."`},
		{name: "unclean double slash", isolation: "none", toolkit: &NvidiaToolkitConfig{CTKPath: "/opt//nvidia-ctk"}, wantErr: "not a clean path"},
		{name: "unclean trailing slash", isolation: "none", toolkit: &NvidiaToolkitConfig{CTKPath: "/opt/nvidia-ctk/"}, wantErr: "not a clean path"},
		{name: "root", isolation: "none", toolkit: &NvidiaToolkitConfig{CTKPath: "/"}, wantErr: "not an executable"},
		{name: "nspawn refuses", isolation: "nspawn", toolkit: &NvidiaToolkitConfig{CTKPath: "/opt/nvidia-ctk"}, wantErr: "only supported with Isolation"},
		{name: "default mode refuses", isolation: "", toolkit: &NvidiaToolkitConfig{CTKPath: "/opt/nvidia-ctk"}, wantErr: "only supported with Isolation"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := AgentConfig{Isolation: tt.isolation, NvidiaToolkit: tt.toolkit}

			err := cfg.validateNvidiaToolkit()
			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestNvidiaToolkitIsOmittedWhenUnset(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(AgentConfig{MachineName: "m"})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "NvidiaToolkit")

	raw, err = json.Marshal(AgentConfig{NvidiaToolkit: &NvidiaToolkitConfig{CTKPath: "/opt/ctk"}})
	require.NoError(t, err)
	require.Contains(t, string(raw), `"NvidiaToolkit":{"CTKPath":"/opt/ctk"}`)
}

func TestAgentConfigDeepCopyOwnsNvidiaToolkit(t *testing.T) {
	t.Parallel()

	original := &AgentConfig{NvidiaToolkit: &NvidiaToolkitConfig{CTKPath: "/opt/ctk"}}
	clone := original.DeepCopy()
	clone.NvidiaToolkit.CTKPath = "/opt/other"

	require.Equal(t, "/opt/ctk", original.NvidiaToolkit.CTKPath)
	require.Nil(t, (&AgentConfig{}).DeepCopy().NvidiaToolkit)
}
