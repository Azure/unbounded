// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package provision

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

func TestAgentConfig_MarshalJSON(t *testing.T) {
	t.Parallel()

	cfg := AgentConfig{
		MachineName: "my-machine",
		Cluster: AgentClusterConfig{
			CaCertBase64: "dGVzdC1jYQ==",
			ClusterDNS:   "10.0.0.10",
			Version:      "v1.34.0",
		},
		Kubelet: AgentKubeletConfig{
			ApiServer:      "api.example.com:443",
			BootstrapToken: "abc123.secret456",
			Labels: map[string]string{
				"app": "my-machine",
				"env": "prod",
			},
			RegisterWithTaints: []string{"dedicated=gpu:NoSchedule"},
		},
		OCIImage: "ghcr.io/azure/agent:v1.0.0",
	}

	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &parsed))

	require.Equal(t, "my-machine", parsed["MachineName"])

	cluster := parsed["Cluster"].(map[string]interface{})
	require.Equal(t, "dGVzdC1jYQ==", cluster["CaCertBase64"])
	require.Equal(t, "10.0.0.10", cluster["ClusterDNS"])
	require.Equal(t, "v1.34.0", cluster["Version"])

	kubelet := parsed["Kubelet"].(map[string]interface{})
	require.Equal(t, "api.example.com:443", kubelet["ApiServer"])
	require.Equal(t, "abc123.secret456", kubelet["BootstrapToken"])
	labels := kubelet["Labels"].(map[string]interface{})
	require.Equal(t, "my-machine", labels["app"])
	require.Equal(t, "prod", labels["env"])

	taints := kubelet["RegisterWithTaints"].([]interface{})
	require.Len(t, taints, 1)
	require.Equal(t, "dedicated=gpu:NoSchedule", taints[0])

	require.Equal(t, "ghcr.io/azure/agent:v1.0.0", parsed["OCIImage"])
}

func TestAgentConfig_RoundTrip(t *testing.T) {
	t.Parallel()

	original := AgentConfig{
		MachineName: "round-trip-machine",
		Cluster: AgentClusterConfig{
			CaCertBase64: "Y2VydA==",
			ClusterDNS:   "10.96.0.10",
			Version:      "v1.33.1",
		},
		Kubelet: AgentKubeletConfig{
			ApiServer:          "k8s.example.com:6443",
			BootstrapToken:     "tok.sec",
			Labels:             map[string]string{"key": "value"},
			RegisterWithTaints: []string{"key=value:NoSchedule", "key2=value2:NoExecute"},
		},
		OCIImage: "ghcr.io/azure/rootfs:v2.0.0",
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded AgentConfig
	require.NoError(t, json.Unmarshal(data, &decoded))

	require.Equal(t, original.MachineName, decoded.MachineName)
	require.Equal(t, original.Cluster, decoded.Cluster)
	require.Equal(t, original.Kubelet.Labels, decoded.Kubelet.Labels)
	require.Equal(t, original.Kubelet.RegisterWithTaints, decoded.Kubelet.RegisterWithTaints)
	require.Equal(t, original.OCIImage, decoded.OCIImage)
}

func TestAgentConfig_EmptyFields(t *testing.T) {
	t.Parallel()

	cfg := AgentConfig{}

	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &parsed))

	require.Equal(t, "", parsed["MachineName"])

	cluster := parsed["Cluster"].(map[string]interface{})
	require.Equal(t, "", cluster["CaCertBase64"])
	require.Equal(t, "", cluster["ClusterDNS"])
	require.Equal(t, "", cluster["Version"])

	kubelet := parsed["Kubelet"].(map[string]interface{})
	require.Equal(t, "", kubelet["ApiServer"])
	require.Nil(t, kubelet["Labels"])
	require.Nil(t, kubelet["RegisterWithTaints"])

	// OCIImage has omitempty so should be absent from zero-value config.
	_, hasOCIImage := parsed["OCIImage"]
	require.False(t, hasOCIImage, "OCIImage should be omitted when empty")

	// Attest should be omitted when nil.
	require.Nil(t, parsed["Attest"])
}

func TestAgentConfig_WithAttest(t *testing.T) {
	t.Parallel()

	cfg := AgentConfig{
		MachineName: "metal-node-1",
		Cluster: AgentClusterConfig{
			CaCertBase64: "dGVzdC1jYQ==",
			ClusterDNS:   "10.0.0.10",
			Version:      "v1.34.0",
		},
		Kubelet: AgentKubeletConfig{
			ApiServer: "api.example.com:443",
			Labels: map[string]string{
				"kubernetes.azure.com/managed": "false",
			},
		},
		Attest: &AgentAttestConfig{
			URL: "http://10.0.0.1:8880",
		},
	}

	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	var decoded AgentConfig
	require.NoError(t, json.Unmarshal(data, &decoded))

	require.Equal(t, cfg.MachineName, decoded.MachineName)
	require.Equal(t, cfg.Cluster, decoded.Cluster)
	require.NotNil(t, decoded.Attest)
	require.Equal(t, "http://10.0.0.1:8880", decoded.Attest.URL)
	// BootstrapToken should be empty when using attestation.
	require.Empty(t, decoded.Kubelet.BootstrapToken)
}

func TestAgentConfig_AttestOmittedWhenNil(t *testing.T) {
	t.Parallel()

	cfg := AgentConfig{
		MachineName: "ssh-node",
		Kubelet: AgentKubeletConfig{
			BootstrapToken: "abc123.secret456",
		},
	}

	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	// Attest should not appear in JSON output when nil.
	require.NotContains(t, string(data), "Attest")

	var decoded AgentConfig
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Nil(t, decoded.Attest)
	require.Equal(t, "abc123.secret456", decoded.Kubelet.BootstrapToken)
}

func TestBuildAgentConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params BuildAgentConfigParams
		assert func(t *testing.T, cfg AgentConfig)
	}{
		{
			name: "fully populated",
			params: BuildAgentConfigParams{
				Machine: &v1alpha3.Machine{
					ObjectMeta: metav1.ObjectMeta{Name: "my-machine"},
					Spec: v1alpha3.MachineSpec{
						Kubernetes: &v1alpha3.KubernetesSpec{
							Version:            "v1.34.0",
							NodeLabels:         map[string]string{"env": "prod", "team": "infra"},
							RegisterWithTaints: []string{"dedicated=gpu:NoSchedule"},
						},
						Agent: &v1alpha3.AgentSpec{Image: "ghcr.io/org/rootfs:v1"},
					},
				},
				Cluster: ClusterEndpoint{
					APIServer:    "api.example.com:443",
					CACertBase64: "dGVzdC1jYQ==",
					ClusterDNS:   "10.0.0.10",
					KubeVersion:  "v1.33.0", // should be overridden by Machine spec
				},
				ProviderLabels: map[string]string{"provider-key": "provider-val"},
				BootstrapToken: "abc123.secret456",
			},
			assert: func(t *testing.T, cfg AgentConfig) {
				require.Equal(t, "my-machine", cfg.MachineName)
				require.Equal(t, "dGVzdC1jYQ==", cfg.Cluster.CaCertBase64)
				require.Equal(t, "10.0.0.10", cfg.Cluster.ClusterDNS)
				require.Equal(t, "v1.34.0", cfg.Cluster.Version) // Machine spec overrides KubeVersion
				require.Equal(t, "api.example.com:443", cfg.Kubelet.ApiServer)
				require.Equal(t, "abc123.secret456", cfg.Kubelet.BootstrapToken)
				require.Equal(t, "ghcr.io/org/rootfs:v1", cfg.OCIImage)
				require.Nil(t, cfg.Attest)

				// Labels: user + common + provider
				require.Equal(t, "prod", cfg.Kubelet.Labels["env"])
				require.Equal(t, "infra", cfg.Kubelet.Labels["team"])
				require.Equal(t, "provider-val", cfg.Kubelet.Labels["provider-key"])

				require.Equal(t, []string{"dedicated=gpu:NoSchedule"}, cfg.Kubelet.RegisterWithTaints)
			},
		},
		{
			name: "nil kubernetes spec",
			params: BuildAgentConfigParams{
				Machine: &v1alpha3.Machine{
					ObjectMeta: metav1.ObjectMeta{Name: "bare-machine"},
				},
				Cluster: ClusterEndpoint{
					APIServer:   "api.example.com:443",
					KubeVersion: "v1.33.0",
				},
			},
			assert: func(t *testing.T, cfg AgentConfig) {
				require.Equal(t, "bare-machine", cfg.MachineName)
				require.Equal(t, "v1.33.0", cfg.Cluster.Version)

				// No labels are added for a bare machine.
				require.Empty(t, cfg.Kubelet.Labels)
				require.Nil(t, cfg.Kubelet.RegisterWithTaints)
				require.Empty(t, cfg.OCIImage)
			},
		},
		{
			name: "version prefix normalization from machine spec",
			params: BuildAgentConfigParams{
				Machine: &v1alpha3.Machine{
					ObjectMeta: metav1.ObjectMeta{Name: "version-test"},
					Spec: v1alpha3.MachineSpec{
						Kubernetes: &v1alpha3.KubernetesSpec{
							Version: "1.34.0", // no "v" prefix
						},
					},
				},
			},
			assert: func(t *testing.T, cfg AgentConfig) {
				require.Equal(t, "v1.34.0", cfg.Cluster.Version)
			},
		},
		{
			name: "version prefix normalization from KubeVersion fallback",
			params: BuildAgentConfigParams{
				Machine: &v1alpha3.Machine{
					ObjectMeta: metav1.ObjectMeta{Name: "fallback-test"},
				},
				Cluster: ClusterEndpoint{
					KubeVersion: "1.33.0", // no "v" prefix, should be normalized
				},
			},
			assert: func(t *testing.T, cfg AgentConfig) {
				require.Equal(t, "v1.33.0", cfg.Cluster.Version)
			},
		},
		{
			name: "label priority",
			params: BuildAgentConfigParams{
				Machine: &v1alpha3.Machine{
					ObjectMeta: metav1.ObjectMeta{Name: "label-test"},
					Spec: v1alpha3.MachineSpec{
						Kubernetes: &v1alpha3.KubernetesSpec{
							NodeLabels: map[string]string{
								"user-label":   "user-value",
								"provider-key": "user-tries-this", // should be overridden by provider
							},
						},
					},
				},
				ProviderLabels: map[string]string{"provider-key": "provider-wins"},
			},
			assert: func(t *testing.T, cfg AgentConfig) {
				require.Equal(t, "user-value", cfg.Kubelet.Labels["user-label"])
				require.Equal(t, "provider-wins", cfg.Kubelet.Labels["provider-key"]) // provider wins over user
			},
		},
		{
			name: "nil provider labels",
			params: BuildAgentConfigParams{
				Machine: &v1alpha3.Machine{
					ObjectMeta: metav1.ObjectMeta{Name: "nil-provider"},
					Spec: v1alpha3.MachineSpec{
						Kubernetes: &v1alpha3.KubernetesSpec{
							NodeLabels: map[string]string{"key": "value"},
						},
					},
				},
				ProviderLabels: nil, // should be safe
			},
			assert: func(t *testing.T, cfg AgentConfig) {
				require.Equal(t, "value", cfg.Kubelet.Labels["key"])
				require.Len(t, cfg.Kubelet.Labels, 1)
			},
		},
		{
			name: "with attest URL",
			params: BuildAgentConfigParams{
				Machine: &v1alpha3.Machine{
					ObjectMeta: metav1.ObjectMeta{Name: "attest-node"},
				},
				AttestURL: "http://10.0.0.1:8880",
			},
			assert: func(t *testing.T, cfg AgentConfig) {
				require.NotNil(t, cfg.Attest)
				require.Equal(t, "http://10.0.0.1:8880", cfg.Attest.URL)
				require.Empty(t, cfg.Kubelet.BootstrapToken)
			},
		},
		{
			name: "without attest URL",
			params: BuildAgentConfigParams{
				Machine: &v1alpha3.Machine{
					ObjectMeta: metav1.ObjectMeta{Name: "no-attest-node"},
				},
				BootstrapToken: "tok.sec",
			},
			assert: func(t *testing.T, cfg AgentConfig) {
				require.Nil(t, cfg.Attest)
				require.Equal(t, "tok.sec", cfg.Kubelet.BootstrapToken)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := BuildAgentConfig(tt.params)
			tt.assert(t, cfg)
		})
	}
}
