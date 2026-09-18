// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestConfigureKubeletAuthenticationModes(t *testing.T) {
	t.Parallel()

	rawFirst := []byte("apiVersion: v1\nkind: Config\n# impersonation and headers remain verbatim\n")
	rawSecond := []byte("apiVersion: v1\nkind: Config\n# second exact payload\n")

	tests := []struct {
		name                 string
		auth                 config.KubeletAuthInfo
		raw                  []byte
		wantDirect           bool
		wantRotate           bool
		wantBootstrap        bool
		reconfigureRaw       []byte
		wantKubeconfigPrefix string
	}{
		{
			name:          "bootstrap token",
			auth:          config.KubeletAuthInfo{BootstrapToken: "abc123.secret456"},
			wantRotate:    true,
			wantBootstrap: true,
		},
		{
			name: "exec credential",
			auth: config.KubeletAuthInfo{ExecCredential: &clientcmdapi.ExecConfig{
				APIVersion: "client.authentication.k8s.io/v1",
				Command:    "/usr/local/bin/kubelogin",
			}},
			wantDirect:           true,
			wantRotate:           true,
			wantKubeconfigPrefix: "apiVersion:",
		},
		{
			name:                 "raw kubeconfig",
			raw:                  rawFirst,
			wantDirect:           true,
			wantRotate:           false,
			reconfigureRaw:       rawSecond,
			wantKubeconfigPrefix: "apiVersion: v1\nkind: Config\n# second exact payload\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			machineDir := t.TempDir()
			goalState := &goalstates.NodeStart{
				MachineDir: machineDir,
				NodeName:   "worker-1",
				Kubelet: goalstates.Kubelet{
					KubeletAuthInfo: tt.auth,
					KubeconfigData:  tt.raw,
					APIServer:       "https://api.example.com",
					CACertData:      []byte("ca"),
					ClusterDNS:      "10.0.0.10",
				},
			}

			if err := ConfigureKubelet(goalState).Do(context.Background()); err != nil {
				t.Fatalf("ConfigureKubelet.Do() error = %v", err)
			}

			dropIn := readTestFile(t, filepath.Join(machineDir, goalstates.KubeletServiceDropInDir, "10-kubeconfig.conf"))
			if tt.wantDirect {
				if strings.Contains(string(dropIn), "--bootstrap-kubeconfig") {
					t.Fatalf("direct mode drop-in contains bootstrap kubeconfig flag:\n%s", dropIn)
				}
				if !strings.Contains(string(dropIn), "--kubeconfig="+goalstates.KubeletKubeconfigPath) {
					t.Fatalf("direct mode drop-in missing kubeconfig flag:\n%s", dropIn)
				}
			} else if !strings.Contains(string(dropIn), "--bootstrap-kubeconfig="+goalstates.KubeletBootstrapKubeconfigPath) {
				t.Fatalf("bootstrap mode drop-in missing bootstrap flag:\n%s", dropIn)
			}

			configurationData := readTestFile(t, filepath.Join(machineDir, goalstates.KubeletConfigurationPath))
			var configuration map[string]any
			if err := yaml.Unmarshal(configurationData, &configuration); err != nil {
				t.Fatalf("unmarshal KubeletConfiguration: %v", err)
			}
			if got, ok := configuration["rotateCertificates"].(bool); !ok || got != tt.wantRotate {
				t.Fatalf("rotateCertificates = %#v, want %t", configuration["rotateCertificates"], tt.wantRotate)
			}

			bootstrapPath := filepath.Join(machineDir, goalstates.KubeletBootstrapKubeconfigPath)
			if _, err := os.Stat(bootstrapPath); tt.wantBootstrap && err != nil {
				t.Fatalf("bootstrap kubeconfig stat error = %v", err)
			} else if !tt.wantBootstrap && !os.IsNotExist(err) {
				t.Fatalf("bootstrap kubeconfig exists in direct mode, stat error = %v", err)
			}

			if tt.reconfigureRaw != nil {
				kubeconfigPath := filepath.Join(machineDir, goalstates.KubeletKubeconfigPath)
				if err := os.Chmod(kubeconfigPath, 0o644); err != nil {
					t.Fatalf("chmod kubeconfig: %v", err)
				}
				goalState.Kubelet.KubeconfigData = tt.reconfigureRaw
				if err := ConfigureKubelet(goalState).Do(context.Background()); err != nil {
					t.Fatalf("second ConfigureKubelet.Do() error = %v", err)
				}
			}

			if tt.wantKubeconfigPrefix != "" {
				kubeconfigPath := filepath.Join(machineDir, goalstates.KubeletKubeconfigPath)
				kubeconfig := readTestFile(t, kubeconfigPath)
				if tt.reconfigureRaw != nil {
					if !bytes.Equal(kubeconfig, tt.reconfigureRaw) {
						t.Fatalf("raw kubeconfig bytes = %q, want %q", kubeconfig, tt.reconfigureRaw)
					}
				} else if !strings.HasPrefix(string(kubeconfig), tt.wantKubeconfigPrefix) {
					t.Fatalf("kubeconfig = %q, want prefix %q", kubeconfig, tt.wantKubeconfigPrefix)
				}

				info, err := os.Stat(kubeconfigPath)
				if err != nil {
					t.Fatalf("stat kubeconfig: %v", err)
				}
				if got := info.Mode().Perm(); got != 0o600 {
					t.Fatalf("kubeconfig permissions = %04o, want 0600", got)
				}
			}
		})
	}
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
