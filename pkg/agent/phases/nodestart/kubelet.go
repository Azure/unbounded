// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type configureKubelet struct {
	goalState *goalstates.NodeStart
}

// ConfigureKubelet returns a task that writes the kubelet configuration into the machine rootfs.
// It runs before the nspawn machine is started, so all paths are relative to
// the machine directory on the host filesystem.
func ConfigureKubelet(goalState *goalstates.NodeStart) phases.Task {
	return &configureKubelet{goalState: goalState}
}

func (c *configureKubelet) Name() string { return "configure-kubelet" }

func (c *configureKubelet) Do(_ context.Context) error {
	if err := c.ensureRuntimeFolders(); err != nil {
		return fmt.Errorf("ensure runtime folders: %w", err)
	}

	if err := c.ensureKubeletCACert(); err != nil {
		return fmt.Errorf("ensure kubelet CA cert: %w", err)
	}

	if err := c.ensureKubeletConfiguration(); err != nil {
		return fmt.Errorf("ensure kubelet configuration: %w", err)
	}

	if err := c.ensureKubeletServiceUnit(); err != nil {
		return fmt.Errorf("ensure kubelet service unit: %w", err)
	}

	if err := c.ensureKubeletDropIns(); err != nil {
		return fmt.Errorf("ensure kubelet drop-ins: %w", err)
	}

	return nil
}

// ensureRuntimeFolders creates directories that must exist inside the machine
// rootfs before the kubelet starts, but that are not implicitly created by
// writing a file. For example, the static-pod manifests directory must be
// present even when no static pods have been configured yet.
func (c *configureKubelet) ensureRuntimeFolders() error {
	dir := filepath.Join(c.goalState.MachineDir, goalstates.KubeletStaticPodManifestsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	return nil
}

type startKubelet struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
}

// StartKubelet returns a task that enables and starts the kubelet systemd service inside
// the running nspawn machine.
func StartKubelet(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &startKubelet{log: log, goalState: goalState}
}

func (s *startKubelet) Name() string { return "start-kubelet" }

func (s *startKubelet) Do(ctx context.Context) error {
	if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName,
		"systemctl", "enable", "--now", goalstates.SystemdUnitKubelet,
	); err != nil {
		return fmt.Errorf("systemctl enable --now %s in %s: %w",
			goalstates.SystemdUnitKubelet, s.goalState.MachineName, err)
	}

	return nil
}

// ensureKubeletCACert writes the API server CA certificate into the machine
// rootfs PKI directory.
func (c *configureKubelet) ensureKubeletCACert() error {
	dest := filepath.Join(c.goalState.MachineDir, goalstates.KubeletAPIServerCACertPath)

	return utilio.WriteFile(dest, c.goalState.Kubelet.CACertData, 0o644)
}

// ensureKubeletConfiguration writes a KubeletConfiguration assembled from the
// agent baseline and the user-provided overlay. Agent-owned connectivity and
// authentication fields are applied after the overlay.
func (c *configureKubelet) ensureKubeletConfiguration() error {
	spec := c.goalState.Kubelet
	configuration := defaultKubeletConfiguration()
	mergeKubeletConfiguration(configuration, spec.Configuration)

	configuration["apiVersion"] = "kubelet.config.k8s.io/v1beta1"
	configuration["kind"] = "KubeletConfiguration"
	configuration["clusterDNS"] = []string{spec.ClusterDNS}
	configuration["containerRuntimeEndpoint"] = "unix:///run/containerd/containerd.sock"

	authentication := ensureConfigurationMap(configuration, "authentication")
	x509 := ensureConfigurationMap(authentication, "x509")
	x509["clientCAFile"] = goalstates.KubeletAPIServerCACertPath

	data, err := yaml.Marshal(configuration)
	if err != nil {
		return fmt.Errorf("marshal KubeletConfiguration: %w", err)
	}

	dest := filepath.Join(c.goalState.MachineDir, goalstates.KubeletConfigurationPath)

	return utilio.WriteFile(dest, data, 0o644)
}

func defaultKubeletConfiguration() map[string]any {
	return map[string]any{
		"address": "0.0.0.0",
		"authentication": map[string]any{
			"anonymous": map[string]any{"enabled": false},
			"webhook":   map[string]any{"enabled": true},
		},
		"authorization": map[string]any{
			"mode": "Webhook",
		},
		"cgroupDriver":                   "systemd",
		"cgroupsPerQOS":                  true,
		"clusterDomain":                  "cluster.local",
		"enableServer":                   true,
		"enforceNodeAllocatable":         []string{"pods"},
		"eventRecordQPS":                 0,
		"nodeStatusUpdateFrequency":      "10s",
		"podPidsLimit":                   -1,
		"protectKernelDefaults":          true,
		"readOnlyPort":                   0,
		"resolvConf":                     "/etc/resolv.conf",
		"rotateCertificates":             true,
		"runtimeRequestTimeout":          "15m",
		"staticPodPath":                  goalstates.KubeletStaticPodManifestsDir,
		"streamingConnectionIdleTimeout": "4h",
		"tlsCipherSuites": []string{
			"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305",
			"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
			"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305",
			"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
			"TLS_RSA_WITH_AES_256_GCM_SHA384",
			"TLS_RSA_WITH_AES_128_GCM_SHA256",
		},
		"volumePluginDir": "/etc/kubernetes/volumeplugins",
	}
}

func mergeKubeletConfiguration(destination, overlay map[string]any) {
	for key, overlayValue := range overlay {
		overlayMap, overlayIsMap := overlayValue.(map[string]any)

		destinationMap, destinationIsMap := destination[key].(map[string]any)
		if overlayIsMap && destinationIsMap {
			mergeKubeletConfiguration(destinationMap, overlayMap)
			continue
		}

		destination[key] = overlayValue
	}
}

func ensureConfigurationMap(parent map[string]any, key string) map[string]any {
	if value, ok := parent[key].(map[string]any); ok {
		return value
	}

	value := map[string]any{}
	parent[key] = value

	return value
}

// ensureKubeletServiceUnit renders and writes the kubelet systemd unit file
// into the machine rootfs.
func (c *configureKubelet) ensureKubeletServiceUnit() error {
	spec := c.goalState.Kubelet

	buf := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(buf, "kubelet.service", map[string]any{
		"KubeletBinPath":           spec.KubeletBinPath,
		"KubeletConfigurationPath": goalstates.KubeletConfigurationPath,
	}); err != nil {
		return err
	}

	dest := filepath.Join(c.goalState.MachineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitKubelet)

	return utilio.WriteFile(dest, buf.Bytes(), 0o644)
}

// ensureKubeletDropIns renders and writes all kubelet systemd drop-in files
// into the machine rootfs.
func (c *configureKubelet) ensureKubeletDropIns() error {
	spec := c.goalState.Kubelet

	// Format node labels as a comma-separated "key=value" string, sorted for
	// deterministic output.
	nodeLabels := formatNodeLabels(spec.NodeLabels)

	// Format taints as a comma-separated "key=value:effect" string, sorted
	// for deterministic output.
	registerWithTaints := formatRegisterWithTaints(spec.RegisterWithTaints)

	dropIns := []struct {
		name string
		data any
	}{
		{
			name: "10-kubeconfig.conf",
			data: map[string]any{
				"KubeconfigPath":          goalstates.KubeletKubeconfigPath,
				"BootstrapKubeconfigPath": goalstates.KubeletBootstrapKubeconfigPath,
				"UseExecCredential":       spec.ExecCredential != nil,
			},
		},
		{
			name: "20-node-config.conf",
			data: map[string]any{
				"NodeName":                c.goalState.NodeName,
				"NodeIP":                  spec.NodeIP,
				"NodeLabels":              nodeLabels,
				"RegisterWithTaints":      registerWithTaints,
				"ImageCredentialProvider": spec.ImageCredentialProvider,
			},
		},
		{
			name: "50-env-file.conf",
			data: nil,
		},
	}

	for _, d := range dropIns {
		buf := &bytes.Buffer{}
		if err := assetsTemplate.ExecuteTemplate(buf, d.name, d.data); err != nil {
			return fmt.Errorf("render %s: %w", d.name, err)
		}

		dest := filepath.Join(c.goalState.MachineDir, goalstates.KubeletServiceDropInDir, d.name)
		if err := utilio.WriteFile(dest, buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
	}

	// Write the kubelet kubeconfig (bootstrap or exec-based).
	return c.ensureKubeconfig()
}

// ensureKubeconfig writes the appropriate kubeconfig into the machine rootfs
// based on the configured authentication method.
func (c *configureKubelet) ensureKubeconfig() error {
	spec := c.goalState.Kubelet
	switch {
	case spec.ExecCredential != nil:
		return c.ensureExecKubeconfig()
	case spec.BootstrapToken != "":
		return c.ensureBootstrapKubeconfig()
	default:
		return fmt.Errorf("no kubelet auth method configured")
	}
}

// buildKubeconfig creates a kubeconfig with the given auth info and the
// cluster CA and server from the kubelet goal state.
func (c *configureKubelet) buildKubeconfig(authInfo *clientcmdapi.AuthInfo) clientcmdapi.Config {
	spec := c.goalState.Kubelet

	return clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"cluster": {
				CertificateAuthority: goalstates.KubeletAPIServerCACertPath,
				Server:               spec.APIServer,
			},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"kubelet": authInfo,
		},
		Contexts: map[string]*clientcmdapi.Context{
			"default": {
				Cluster:  "cluster",
				AuthInfo: "kubelet",
			},
		},
		CurrentContext: "default",
	}
}

// ensureBootstrapKubeconfig writes a bootstrap kubeconfig that uses a bearer
// token for TLS bootstrapping.
func (c *configureKubelet) ensureBootstrapKubeconfig() error {
	cfg := c.buildKubeconfig(&clientcmdapi.AuthInfo{
		Token: c.goalState.Kubelet.BootstrapToken,
	})

	data, err := clientcmd.Write(cfg)
	if err != nil {
		return fmt.Errorf("serialize bootstrap kubeconfig: %w", err)
	}

	dest := filepath.Join(c.goalState.MachineDir, goalstates.KubeletBootstrapKubeconfigPath)

	return utilio.WriteFile(dest, data, 0o600)
}

// ensureExecKubeconfig writes a kubeconfig that uses an exec credential
// plugin for authentication.
func (c *configureKubelet) ensureExecKubeconfig() error {
	cfg := c.buildKubeconfig(&clientcmdapi.AuthInfo{
		Exec: c.goalState.Kubelet.ExecCredential,
	})

	data, err := clientcmd.Write(cfg)
	if err != nil {
		return fmt.Errorf("serialize exec kubeconfig: %w", err)
	}
	// Exec credential plugins provide renewable tokens, so write directly
	// to the kubelet kubeconfig path (no TLS bootstrap needed).
	dest := filepath.Join(c.goalState.MachineDir, goalstates.KubeletKubeconfigPath)

	return utilio.WriteFile(dest, data, 0o600)
}

// formatNodeLabels formats a map of node labels as a sorted, comma-separated
// "key=value" string suitable for the kubelet --node-labels flag.
func formatNodeLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}

	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+labels[k])
	}

	return strings.Join(pairs, ",")
}

// formatRegisterWithTaints formats a slice of taints as a sorted,
// comma-separated string suitable for the kubelet --register-with-taints flag.
// Each entry is expected to already be in "key=value:effect" format.
func formatRegisterWithTaints(taints []string) string {
	if len(taints) == 0 {
		return ""
	}

	sorted := make([]string, len(taints))
	copy(sorted, taints)
	sort.Strings(sorted)

	return strings.Join(sorted, ",")
}
