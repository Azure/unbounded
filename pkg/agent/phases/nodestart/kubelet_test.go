// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestConfigureKubeletWritesHostnameOverride(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		NodeName:   "worker-1",
		Kubelet: goalstates.Kubelet{
			APIServer:  "https://api.example.com",
			CACertData: []byte("ca"),
			NodeIP:     "10.0.0.15",
			KubeletAuthInfo: config.KubeletAuthInfo{
				BootstrapToken: "token",
			},
			ClusterDNS: "10.0.0.10",
		},
	}
	require.NoError(t, ConfigureKubelet(goalState).Do(context.Background()))

	data, err := os.ReadFile(filepath.Join(
		machineDir,
		goalstates.KubeletServiceDropInDir,
		"20-node-config.conf",
	))
	require.NoError(t, err)
	require.Contains(t, string(data), "--hostname-override=worker-1")
	require.Contains(t, string(data), "--node-ip=10.0.0.15")
}

func TestConfigureKubeletOmitsNodeIPWhenEmpty(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		NodeName:   "worker-1",
		Kubelet: goalstates.Kubelet{
			APIServer:  "https://api.example.com",
			CACertData: []byte("ca"),
			KubeletAuthInfo: config.KubeletAuthInfo{
				BootstrapToken: "token",
			},
			ClusterDNS: "10.0.0.10",
		},
	}
	require.NoError(t, ConfigureKubelet(goalState).Do(context.Background()))

	data, err := os.ReadFile(filepath.Join(
		machineDir,
		goalstates.KubeletServiceDropInDir,
		"20-node-config.conf",
	))
	require.NoError(t, err)
	require.NotContains(t, string(data), "--node-ip=")
}

func TestConfigureKubeletWritesNodeLabelsAndTaints(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		NodeName:   "worker-1",
		Kubelet: goalstates.Kubelet{
			APIServer:  "https://api.example.com",
			CACertData: []byte("ca"),
			KubeletAuthInfo: config.KubeletAuthInfo{
				BootstrapToken: "token",
			},
			ClusterDNS: "10.0.0.10",
			NodeLabels: map[string]string{
				"team": "infra",
				"env":  "prod",
			},
			RegisterWithTaints: []string{
				"workload=batch:NoSchedule",
				"dedicated=gpu:NoSchedule",
			},
		},
	}

	require.NoError(t, ConfigureKubelet(goalState).Do(context.Background()))

	data, err := os.ReadFile(filepath.Join(
		machineDir,
		goalstates.KubeletServiceDropInDir,
		"20-node-config.conf",
	))
	require.NoError(t, err)
	require.Contains(t, string(data), "--node-labels=env=prod,team=infra")
	require.Contains(t, string(data), "--register-with-taints=dedicated=gpu:NoSchedule,workload=batch:NoSchedule")
}

func TestConfigureKubeletWritesConfiguration(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		NodeName:   "worker-1",
		Kubelet: goalstates.Kubelet{
			APIServer:  "https://api.example.com",
			CACertData: []byte("ca"),
			KubeletAuthInfo: config.KubeletAuthInfo{
				BootstrapToken: "token",
			},
			ClusterDNS: "10.0.0.10",
			ResolvConf: goalstates.LocalDNSResolvConfPath,
			Configuration: map[string]any{
				"maxPods":    250,
				"resolvConf": "/caller/override/resolv.conf",
				"logging":    map[string]any{"verbosity": 4},
				"authorization": map[string]any{
					"webhook": map[string]any{"cacheAuthorizedTTL": "10m"},
				},
			},
		},
	}

	require.NoError(t, ConfigureKubelet(goalState).Do(context.Background()))
	_, modeWasAddedToOverlay := goalState.Kubelet.Configuration["authorization"].(map[string]any)["mode"]
	require.False(t, modeWasAddedToOverlay)

	data, err := os.ReadFile(filepath.Join(machineDir, goalstates.KubeletConfigurationPath))
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, yaml.Unmarshal(data, &got))
	require.Equal(t, "kubelet.config.k8s.io/v1beta1", got["apiVersion"])
	require.Equal(t, "KubeletConfiguration", got["kind"])
	require.EqualValues(t, 250, got["maxPods"])
	require.Equal(t, []any{"10.0.0.10"}, got["clusterDNS"])
	require.Equal(t, "unix:///run/containerd/containerd.sock", got["containerRuntimeEndpoint"])
	require.Equal(t, "0.0.0.0", got["address"])
	require.Equal(t, true, got["enableServer"])
	require.Equal(t, "/etc/kubernetes/volumeplugins", got["volumePluginDir"])
	require.Equal(t, goalstates.KubeletStaticPodManifestsDir, got["staticPodPath"])
	require.Equal(t, "15m", got["runtimeRequestTimeout"])
	require.Equal(t, "systemd", got["cgroupDriver"])
	require.Equal(t, true, got["cgroupsPerQOS"])
	require.Equal(t, []any{"pods"}, got["enforceNodeAllocatable"])
	require.Equal(t, "cluster.local", got["clusterDomain"])
	require.EqualValues(t, 0, got["eventRecordQPS"])
	require.Equal(t, "10s", got["nodeStatusUpdateFrequency"])
	require.EqualValues(t, -1, got["podPidsLimit"])
	require.Equal(t, true, got["protectKernelDefaults"])
	require.EqualValues(t, 0, got["readOnlyPort"])
	require.Equal(t, goalstates.LocalDNSResolvConfPath, got["resolvConf"])
	require.Equal(t, "4h", got["streamingConnectionIdleTimeout"])
	require.Len(t, got["tlsCipherSuites"], 8)
	require.Equal(t, true, got["rotateCertificates"])

	authentication := got["authentication"].(map[string]any)
	require.Equal(t, false, authentication["anonymous"].(map[string]any)["enabled"])
	require.Equal(t, true, authentication["webhook"].(map[string]any)["enabled"])
	require.Equal(t, goalstates.KubeletAPIServerCACertPath, authentication["x509"].(map[string]any)["clientCAFile"])

	authorization := got["authorization"].(map[string]any)
	require.Equal(t, "Webhook", authorization["mode"])
	require.Equal(t, "10m", authorization["webhook"].(map[string]any)["cacheAuthorizedTTL"])
	require.EqualValues(t, 4, got["logging"].(map[string]any)["verbosity"])

	service, err := os.ReadFile(filepath.Join(machineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitKubelet))
	require.NoError(t, err)
	require.Contains(t, string(service), "--config=/var/lib/kubelet/config.yaml")
	require.NotContains(t, string(service), "--pod-max-pids=")
	require.NotContains(t, string(service), "--cluster-dns=")
}

func TestConfigureKubeletGatesNVIDIAStartup(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir:     machineDir,
		NodeName:       "worker-1",
		NVIDIARequired: true,
		Nvidia: goalstates.NvidiaHost{
			GPUDevicePaths: []string{"/dev/nvidia0"},
			LibMappings:    []goalstates.NvidiaLibMapping{{HostPath: "/usr/lib/libcuda.so.1"}},
		},
		Kubelet: goalstates.Kubelet{
			CACertData: []byte("ca"),
			ClusterDNS: "10.0.0.10",
			KubeletAuthInfo: config.KubeletAuthInfo{
				BootstrapToken: "token",
			},
		},
	}
	goalState.Containerd.NvidiaRuntime.Enabled = true

	require.NoError(t, ConfigureKubelet(goalState).Do(context.Background()))

	service, err := os.ReadFile(filepath.Join(machineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitKubelet))
	require.NoError(t, err)
	require.Contains(t, string(service), "Requires=unbounded-nvidia-ready.service")
	require.Contains(t, string(service), "After=unbounded-nvidia-ready.service")
}

func TestConfigureKubeletWritesImageCredentialProviderFlags(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		NodeName:   "worker-1",
		Kubelet: goalstates.Kubelet{
			APIServer:  "https://api.example.com",
			CACertData: []byte("ca"),
			KubeletAuthInfo: config.KubeletAuthInfo{
				BootstrapToken: "token",
			},
			ClusterDNS: "10.0.0.10",
			ImageCredentialProvider: &goalstates.ImageCredentialProvider{
				ConfigPath: "/etc/kubernetes/credential-provider.yaml",
				BinDir:     "/usr/local/lib/kubelet-credential-providers",
			},
		},
	}

	require.NoError(t, ConfigureKubelet(goalState).Do(context.Background()))

	data, err := os.ReadFile(filepath.Join(
		machineDir,
		goalstates.KubeletServiceDropInDir,
		"20-node-config.conf",
	))
	require.NoError(t, err)
	require.Contains(t, string(data), "--image-credential-provider-config=/etc/kubernetes/credential-provider.yaml")
	require.Contains(t, string(data), "--image-credential-provider-bin-dir=/usr/local/lib/kubelet-credential-providers")
}

func TestConfigureKubeletOmitsEmptyNodeLabelsAndTaints(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		NodeName:   "worker-1",
		Kubelet: goalstates.Kubelet{
			APIServer:  "https://api.example.com",
			CACertData: []byte("ca"),
			KubeletAuthInfo: config.KubeletAuthInfo{
				BootstrapToken: "token",
			},
			ClusterDNS: "10.0.0.10",
		},
	}

	require.NoError(t, ConfigureKubelet(goalState).Do(context.Background()))

	data, err := os.ReadFile(filepath.Join(
		machineDir,
		goalstates.KubeletServiceDropInDir,
		"20-node-config.conf",
	))
	require.NoError(t, err)
	require.NotContains(t, string(data), "--node-labels=")
	require.NotContains(t, string(data), "--register-with-taints=")
	require.NotContains(t, string(data), "--image-credential-provider-config=")
	require.NotContains(t, string(data), "--image-credential-provider-bin-dir=")
}
