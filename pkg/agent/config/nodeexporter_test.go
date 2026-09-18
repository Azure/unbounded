// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestValidateNodeExporter(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		config  *AgentNodeExporterConfig
		wantErr string
	}{
		"disabled":                 {config: &AgentNodeExporterConfig{}},
		"defaults":                 {config: &AgentNodeExporterConfig{Enabled: true}},
		"explicit address":         {config: &AgentNodeExporterConfig{Enabled: true, ListenAddress: "10.0.0.4:19100"}},
		"wildcard address":         {config: &AgentNodeExporterConfig{Enabled: true, ListenAddress: "0.0.0.0:9100"}, wantErr: "unicast IPv4"},
		"reserved listen argument": {config: &AgentNodeExporterConfig{Enabled: true, ExtraArgs: []string{"--web.listen-address=:1234"}}, wantErr: "cannot override"},
		"reserved config argument": {config: &AgentNodeExporterConfig{Enabled: true, ExtraArgs: []string{"--web.config.file=/tmp/web.yml"}}, wantErr: "cannot override"},
		"control character":        {config: &AgentNodeExporterConfig{Enabled: true, ExtraArgs: []string{"--collector.cpu\n"}}, wantErr: "control character"},
		"tls": {config: &AgentNodeExporterConfig{Enabled: true, TLS: &NodeExporterTLSConfig{
			Enabled: true, CertificateFile: "/etc/node-exporter/tls.crt", PrivateKeyFile: "/etc/node-exporter/tls.key",
		}}},
		"tls missing key": {config: &AgentNodeExporterConfig{Enabled: true, TLS: &NodeExporterTLSConfig{
			Enabled: true, CertificateFile: "/etc/node-exporter/tls.crt",
		}}, wantErr: "PrivateKeyFile"},
		"tls relative CA": {config: &AgentNodeExporterConfig{Enabled: true, TLS: &NodeExporterTLSConfig{
			Enabled: true, CertificateFile: "/etc/node-exporter/tls.crt", PrivateKeyFile: "/etc/node-exporter/tls.key", ClientCAFile: "ca.crt",
		}}, wantErr: "ClientCAFile"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := (&AgentConfig{NodeExporter: test.config}).validateNodeExporter()
			if test.wantErr == "" && err != nil {
				t.Fatalf("validateNodeExporter() error = %v", err)
			}

			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("validateNodeExporter() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestAgentNodeExporterConfigDeepCopy(t *testing.T) {
	t.Parallel()

	input := &AgentNodeExporterConfig{
		Enabled:   true,
		ExtraArgs: []string{"--collector.cpu.info"},
		TLS:       &NodeExporterTLSConfig{Enabled: true, CertificateFile: "/tls.crt", PrivateKeyFile: "/tls.key"},
	}
	copy := input.DeepCopy()
	copy.ExtraArgs[0] = "changed"
	copy.TLS.CertificateFile = "/changed.crt"

	if input.ExtraArgs[0] == copy.ExtraArgs[0] || input.TLS.CertificateFile == copy.TLS.CertificateFile {
		t.Fatal("DeepCopy() shares mutable state")
	}
}
