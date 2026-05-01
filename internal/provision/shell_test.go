// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package provision

import (
	"testing"

	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestShellSingleQuote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: "''"},
		{name: "simple", in: "hello", want: "'hello'"},
		{name: "with space", in: "hello world", want: "'hello world'"},
		{name: "with single quote", in: "it's", want: `'it'\''s'`},
		{name: "shell metachars", in: "a; rm -rf /", want: "'a; rm -rf /'"},
	}

	for i := range tests {
		tt := tests[i]
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ShellSingleQuote(tt.in)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestAgentInstallEnv(t *testing.T) {
	t.Parallel()

	t.Run("nil spec returns nil", func(t *testing.T) {
		t.Parallel()

		require.Nil(t, AgentInstallEnv(nil))
	})

	t.Run("empty spec returns nil", func(t *testing.T) {
		t.Parallel()

		require.Empty(t, AgentInstallEnv(&v1alpha3.AgentSpec{}))
	})

	t.Run("all fields emits ordered env vars", func(t *testing.T) {
		t.Parallel()

		got := AgentInstallEnv(&v1alpha3.AgentSpec{
			Version: "v0.0.10",
			BaseURL: "https://mirror.example.com/unbounded",
			URL:     "https://mirror.example.com/unbounded/releases/download/v0.0.10/agent.tgz",
		})
		require.Equal(t, []string{
			"AGENT_VERSION='v0.0.10'",
			"AGENT_BASE_URL='https://mirror.example.com/unbounded'",
			"AGENT_URL='https://mirror.example.com/unbounded/releases/download/v0.0.10/agent.tgz'",
		}, got)
	})

	t.Run("escapes single quotes", func(t *testing.T) {
		t.Parallel()

		got := AgentInstallEnv(&v1alpha3.AgentSpec{Version: "it's a version"})
		require.Equal(t, []string{`AGENT_VERSION='it'\''s a version'`}, got)
	})
}

func TestBuildAgentConfig_Downloads(t *testing.T) {
	t.Parallel()

	t.Run("no overrides leaves Downloads nil", func(t *testing.T) {
		t.Parallel()

		cfg := BuildAgentConfig(BuildAgentConfigParams{
			Machine: &v1alpha3.Machine{},
			Cluster: ClusterEndpoint{KubeVersion: "v1.34.0"},
		})
		require.Nil(t, cfg.Downloads)
	})

	t.Run("threads overrides from AgentSpec.Downloads", func(t *testing.T) {
		t.Parallel()

		machine := &v1alpha3.Machine{
			Spec: v1alpha3.MachineSpec{
				Agent: &v1alpha3.AgentSpec{
					Downloads: &v1alpha3.AgentDownloadsSpec{
						Kubernetes: &v1alpha3.DownloadSource{BaseURL: "https://mirror.example.com/k8s"},
						Containerd: &v1alpha3.DownloadSource{Version: "2.0.5"},
						Crictl:     &v1alpha3.DownloadSource{BaseURL: "https://mirror.example.com/cri-tools"},
					},
				},
			},
		}

		cfg := BuildAgentConfig(BuildAgentConfigParams{
			Machine: machine,
			Cluster: ClusterEndpoint{KubeVersion: "v1.34.0"},
		})

		require.NotNil(t, cfg.Downloads)
		require.NotNil(t, cfg.Downloads.Kubernetes)
		require.Equal(t, "https://mirror.example.com/k8s", cfg.Downloads.Kubernetes.BaseURL)
		require.NotNil(t, cfg.Downloads.Containerd)
		require.Equal(t, "2.0.5", cfg.Downloads.Containerd.Version)
		require.NotNil(t, cfg.Downloads.Crictl)
		require.Nil(t, cfg.Downloads.Runc)
		require.Nil(t, cfg.Downloads.CNI)
	})

	t.Run("empty download source fields result in nil", func(t *testing.T) {
		t.Parallel()

		machine := &v1alpha3.Machine{
			Spec: v1alpha3.MachineSpec{
				Agent: &v1alpha3.AgentSpec{
					Downloads: &v1alpha3.AgentDownloadsSpec{
						Kubernetes: &v1alpha3.DownloadSource{},
					},
				},
			},
		}

		cfg := BuildAgentConfig(BuildAgentConfigParams{
			Machine: machine,
			Cluster: ClusterEndpoint{KubeVersion: "v1.34.0"},
		})

		require.Nil(t, cfg.Downloads, "empty download source should yield nil Downloads")
	})
}
