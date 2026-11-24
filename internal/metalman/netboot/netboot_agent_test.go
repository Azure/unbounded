// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"testing"

	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func machineWithAgent(agent *v1alpha3.AgentSpec) *v1alpha3.Machine {
	return &v1alpha3.Machine{Spec: v1alpha3.MachineSpec{Agent: agent}}
}

func TestAgentInstallEnv(t *testing.T) {
	t.Parallel()

	serving := FileResolver{Agent: &AgentArtifact{}, ServeURL: "http://10.0.0.1:8880/"}

	tests := []struct {
		name     string
		resolver FileResolver
		machine  *v1alpha3.Machine
		want     []string
	}{
		{
			name: "no machine and no artifact",
		},
		{
			name:     "no artifact keeps machine overrides",
			resolver: FileResolver{},
			machine:  machineWithAgent(&v1alpha3.AgentSpec{BaseURL: "https://mirror.example/agent", Version: "v1.0.0"}),
			want: []string{
				"AGENT_VERSION='v1.0.0'",
				"AGENT_BASE_URL='https://mirror.example/agent'",
			},
		},
		{
			name:     "artifact served by metalman",
			resolver: serving,
			want:     []string{"AGENT_URL='http://10.0.0.1:8880/unbounded-agent.tar.gz'"},
		},
		{
			name:     "artifact served by metalman ignores the mirror base URL",
			resolver: serving,
			machine:  machineWithAgent(&v1alpha3.AgentSpec{BaseURL: "https://mirror.example/agent", Version: "v1.0.0"}),
			want:     []string{"AGENT_URL='http://10.0.0.1:8880/unbounded-agent.tar.gz'"},
		},
		{
			name:     "explicit agent URL wins",
			resolver: serving,
			machine:  machineWithAgent(&v1alpha3.AgentSpec{URL: "https://example.com/agent.tar.gz"}),
			want:     []string{"AGENT_URL='https://example.com/agent.tar.gz'"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, tt.resolver.agentInstallEnv(tt.machine))
		})
	}
}

func TestOfflineArtifactsSource(t *testing.T) {
	t.Parallel()

	const (
		repository = "ghcr.io/azure/unbounded/bootstrap-artifacts"
		reference  = "v1-k8s-v1.34.2"
		digest     = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	)

	cached := func(t *testing.T) *OCICache {
		t.Helper()

		cache := NewOCICache(t.TempDir())
		cache.SetDigestForArchitecture(repository+":"+reference, v1alpha3.DefaultPXEArchitecture, digest)

		return cache
	}

	tests := []struct {
		name     string
		resolver FileResolver
		machine  *v1alpha3.Machine
		want     string
	}{
		{
			name:     "no bundle configured",
			resolver: FileResolver{Cache: cached(t)},
		},
		{
			name:     "no cache",
			resolver: FileResolver{BootstrapArtifactsRef: repository + ":" + reference},
		},
		{
			name:     "bundle not cached yet",
			resolver: FileResolver{Cache: NewOCICache(t.TempDir()), BootstrapArtifactsRef: repository + ":" + reference},
		},
		{
			name:     "cached bundle",
			resolver: FileResolver{Cache: cached(t), BootstrapArtifactsRef: repository + ":" + reference},
			want:     "oci://" + repository + ":" + reference,
		},
		{
			name:     "machine downloads override the bundle",
			resolver: FileResolver{Cache: cached(t), BootstrapArtifactsRef: repository + ":" + reference},
			machine:  machineWithAgent(&v1alpha3.AgentSpec{Downloads: &v1alpha3.AgentDownloadsSpec{}}),
		},
		{
			name:     "reference without a tag or digest",
			resolver: FileResolver{Cache: cached(t), BootstrapArtifactsRef: repository},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, tt.resolver.offlineArtifactsSource(tt.machine))
		})
	}
}
