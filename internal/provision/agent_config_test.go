package provision

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
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
				"unbounded-kube.io/machine": "my-machine",
				"env":                       "prod",
			},
			RegisterWithTaints: []string{"dedicated=gpu:NoSchedule"},
		},
		OCIImage: "ghcr.io/project-unbounded/agent:v1.0.0",
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
	require.Equal(t, "my-machine", labels["unbounded-kube.io/machine"])
	require.Equal(t, "prod", labels["env"])

	taints := kubelet["RegisterWithTaints"].([]interface{})
	require.Len(t, taints, 1)
	require.Equal(t, "dedicated=gpu:NoSchedule", taints[0])

	require.Equal(t, "ghcr.io/project-unbounded/agent:v1.0.0", parsed["OCIImage"])
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
		OCIImage: "ghcr.io/project-unbounded/rootfs:v2.0.0",
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
