// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestAgentKubeletConfigValidateKubeconfigData(t *testing.T) {
	t.Parallel()

	const valid = `apiVersion: v1
kind: Config
current-context: current
clusters:
- name: cluster
  cluster:
    server: https://api.example.com:443
    certificate-authority-data: Y2E=
users:
- name: user
  user:
    client-certificate-data: Y2VydA==
    client-key-data: a2V5
    as: kubelet
    as-groups:
    - system:nodes
    as-user-extra:
      example.com/header:
      - preserved
contexts:
- name: current
  context:
    cluster: cluster
    user: user
`

	tests := []struct {
		name    string
		data    string
		auth    KubeletAuthInfo
		wantErr string
	}{
		{name: "embedded data and impersonation", data: valid},
		{name: "absolute exec command", data: strings.Replace(valid, "    client-certificate-data: Y2VydA==\n    client-key-data: a2V5", "    exec:\n      apiVersion: client.authentication.k8s.io/v1\n      command: /usr/local/bin/kubelogin", 1)},
		{name: "malformed", data: "secret-sentinel: [", wantErr: "not a valid kubeconfig"},
		{name: "missing current context", data: strings.Replace(valid, "current-context: current", "current-context: \"\"", 1), wantErr: "current context is required"},
		{name: "missing context", data: strings.Replace(valid, "current-context: current", "current-context: missing", 1), wantErr: "current context is invalid"},
		{name: "missing cluster reference", data: strings.Replace(valid, "    cluster: cluster", "    cluster: missing", 1), wantErr: "valid cluster"},
		{name: "missing auth reference", data: strings.Replace(valid, "    user: user", "    user: missing", 1), wantErr: "valid auth info"},
		{name: "http server", data: strings.Replace(valid, "https://api.example.com:443", "http://api.example.com:80", 1), wantErr: "valid HTTPS URL"},
		{name: "external certificate authority", data: strings.Replace(valid, "    certificate-authority-data: Y2E=", "    certificate-authority: /etc/kubernetes/ca.crt", 1), wantErr: "certificate-authority paths"},
		{name: "external client certificate", data: strings.Replace(valid, "    client-certificate-data: Y2VydA==", "    client-certificate: /etc/kubernetes/client.crt", 1), wantErr: "client-certificate paths"},
		{name: "external client key", data: strings.Replace(valid, "    client-key-data: a2V5", "    client-key: /etc/kubernetes/client.key", 1), wantErr: "client-key paths"},
		{name: "external token file", data: strings.Replace(valid, "    client-certificate-data: Y2VydA==\n    client-key-data: a2V5", "    tokenFile: /var/run/secrets/token", 1), wantErr: "token-file paths"},
		{name: "relative exec command", data: strings.Replace(valid, "    client-certificate-data: Y2VydA==\n    client-key-data: a2V5", "    exec:\n      apiVersion: client.authentication.k8s.io/v1\n      command: kubelogin", 1), wantErr: "absolute path"},
		{name: "external path in unused auth info", data: strings.Replace(valid, "contexts:", "- name: unused\n  user:\n    tokenFile: /var/run/secrets/token\ncontexts:", 1), wantErr: "token-file paths"},
		{name: "http server in unused cluster", data: strings.Replace(valid, "users:", "- name: unused\n  cluster:\n    server: http://api.example.com\nusers:", 1), wantErr: "valid HTTPS URL"},
		{name: "mutually exclusive with bootstrap token", data: valid, auth: KubeletAuthInfo{BootstrapToken: "secret-sentinel"}, wantErr: "mutually exclusive"},
		{name: "mutually exclusive with exec credential", data: valid, auth: KubeletAuthInfo{ExecCredential: &clientcmdapi.ExecConfig{Command: "/usr/bin/auth"}}, wantErr: "mutually exclusive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := AgentKubeletConfig{
				KubeconfigData: []byte(tt.data),
				Auth:           tt.auth,
			}
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() error = nil, want substring %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "secret-sentinel") {
				t.Fatalf("Validate() error disclosed kubeconfig or credential data: %q", err)
			}
		})
	}
}

func TestAgentConfigValidateAllowsRawKubeconfigWithoutAPIServer(t *testing.T) {
	t.Parallel()

	cfg := &AgentConfig{
		MachineName: "machine-1",
		NodeName:    "node-1",
		Cluster: AgentClusterConfig{
			ClusterDNS: "10.0.0.10",
		},
		Kubelet: AgentKubeletConfig{
			KubeconfigData: []byte(`apiVersion: v1
kind: Config
current-context: current
clusters:
- name: cluster
  cluster:
    server: https://api.example.com
users:
- name: user
  user:
    token: embedded
contexts:
- name: current
  context:
    cluster: cluster
    user: user
`),
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
