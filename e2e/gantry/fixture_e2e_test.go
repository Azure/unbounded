//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"crypto/x509"
	"debug/elf"
	"encoding/pem"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestFixtureBinariesStaticAndReproducible(t *testing.T) {
	root := repoRoot(t)
	for _, tc := range []struct {
		command string
		arch    string
	}{
		{"pause", "amd64"},
		{"pause", "arm64"},
		{"registry", runtime.GOARCH},
	} {
		t.Run(tc.command+"/"+tc.arch, func(t *testing.T) {
			h := &harness{t: t, repoRoot: root}
			binary := h.buildFixtureBinary(t.Context(), tc.command, tc.arch)

			file, err := elf.NewFile(bytes.NewReader(binary))
			if err != nil {
				t.Fatalf("fixture is not a Linux executable: %v", err)
			}
			defer file.Close()

			want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[tc.arch]
			if file.Machine != want {
				t.Fatalf("fixture machine = %v, want %v", file.Machine, want)
			}

			for _, program := range file.Progs {
				if program.Type == elf.PT_INTERP {
					t.Fatal("scratch-image fixture requires a dynamic interpreter")
				}
			}

			if second := h.buildFixtureBinary(t.Context(), tc.command, tc.arch); !bytes.Equal(binary, second) {
				t.Fatal("rebuilding changed the fixture executable")
			}
		})
	}
}

func TestPatchPeerOnlyRegistryConfig(t *testing.T) {
	const raw = `apiVersion: v1
kind: ConfigMap
metadata:
  name: gantry-config
data:
  config.yaml: |
    chair_cluster_size_estimate: 100000
    chair_count: 64
    chair_claim_initial_divisor: 2048
    chair_startup_jitter: "30s"
    upstream_registries:
      - name: "registry.example.com"
        endpoint: "https://registry.example.com"
`

	for _, host := range []string{"10.96.0.2:5443", "[fd00::2]:5443"} {
		t.Run(host, func(t *testing.T) {
			patched, err := patchPeerOnlyRegistryConfig(raw, host)
			if err != nil {
				t.Fatal(err)
			}

			if strings.Contains(patched, "registry.k8s.io") || strings.Contains(patched, "registry.example.com") {
				t.Fatal("fixture still depends on an external registry")
			}

			var configMap struct {
				Data map[string]string `json:"data"`
			}
			if err := yaml.Unmarshal([]byte(patched), &configMap); err != nil {
				t.Fatal(err)
			}

			var config struct {
				Registries []struct {
					Name     string `json:"name"`
					Endpoint string `json:"endpoint"`
				} `json:"upstream_registries"`
			}
			if err := yaml.Unmarshal([]byte(configMap.Data["config.yaml"]), &config); err != nil {
				t.Fatal(err)
			}

			if len(config.Registries) != 1 || config.Registries[0].Name != host || config.Registries[0].Endpoint != "https://"+host {
				t.Fatalf("unexpected fixture upstreams: %+v", config.Registries)
			}

			if !strings.Contains(patched, "chair_count: 8") || !strings.Contains(patched, "chair_claim_initial_divisor: 1") {
				t.Fatal("fixture lost normal kind tuning")
			}
		})
	}

	if _, err := patchPeerOnlyRegistryConfig(raw, "no-port"); err == nil {
		t.Fatal("invalid fixture registry address accepted")
	}

	if _, err := patchPeerOnlyRegistryConfig("missing anchors", "10.96.0.2:5443"); err == nil {
		t.Fatal("invalid source ConfigMap accepted")
	}
}

func TestRegistryCertificateNames(t *testing.T) {
	for _, host := range []string{authRegistryHost, "10.96.0.2", "fd00::2"} {
		t.Run(host, func(t *testing.T) {
			certPEM, keyPEM := newRegistryCertificate(t, host)

			block, _ := pem.Decode(certPEM)
			if block == nil {
				t.Fatal("missing fixture certificate")
			}

			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}

			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(certPEM) {
				t.Fatal("fixture certificate is not a CA bundle")
			}

			if _, err := cert.Verify(x509.VerifyOptions{DNSName: host, Roots: pool}); err != nil {
				t.Fatalf("fixture certificate does not verify for %s: %v", host, err)
			}

			keyBlock, _ := pem.Decode(keyPEM)
			if keyBlock == nil {
				t.Fatal("missing fixture key")
			}

			if _, err := x509.ParseECPrivateKey(keyBlock.Bytes); err != nil {
				t.Fatal(err)
			}
		})
	}
}
