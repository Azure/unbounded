// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	configpkg "github.com/Azure/unbounded/internal/net/config"
	"github.com/Azure/unbounded/internal/net/ipam"
)

func TestIPAMVerificationAndGuard(t *testing.T) {
	health := &nodeHealthState{}

	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, "ipam.sock")

	server, err := ipam.Listen(t.Context(), path, health.getIPAMConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	health.setCNIAssignment([]string{"10.244.1.0/24"})

	if _, err := ipam.GetConfig(t.Context(), path); err == nil {
		t.Fatal("status assignment alone enabled allocation")
	}

	health.beginManagedCNI("cbr0")
	health.setCNIReady("cbr0", []string{"10.244.2.0/24"})

	conf, err := ipam.GetConfig(t.Context(), path)
	if err != nil || conf.GetRanges()[0].GetRanges()[0].GetSubnet() != "10.244.2.0/24" {
		t.Fatalf("verified configuration: %v, %v", conf, err)
	}

	health.setCNIBlocked("stale bridge")

	if _, err := ipam.GetConfig(t.Context(), path); err == nil {
		t.Fatal("blocked bridge enabled IPAM")
	}

	health.setCNIReady("cbr0", []string{"10.244.2.0/24"})

	if _, err := ipam.GetConfig(t.Context(), path); err != nil {
		t.Fatal(err)
	}

	unmanaged := &nodeHealthState{}
	unmanaged.markCNIUnmanaged()
	unmanaged.setVerifiedIPAMAssignment([]string{"fd00::/64"})

	if _, err := unmanaged.getIPAMConfig(); err != nil {
		t.Fatal(err)
	}
}

func TestDelegatedIPAMWaitsForStartupAssignment(t *testing.T) {
	client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	health := &nodeHealthState{}
	health.markCNIUnmanaged()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)

		acquireDelegatedIPAMAssignment(ctx, client, &config{NodeName: "node-a", cniRetryInterval: time.Millisecond}, health)
	}()

	if _, err := health.getIPAMConfig(); err == nil {
		t.Fatal("empty Node assignment enabled IPAM")
	}

	node, err := client.CoreV1().Nodes().Get(t.Context(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	node.Spec.PodCIDRs = []string{"10.244.3.0/24"}
	if _, err := client.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("assignment acquisition did not finish")
	}

	actions := len(client.Actions())

	if _, err := health.getIPAMConfig(); err != nil {
		t.Fatal(err)
	}

	if len(client.Actions()) != actions {
		t.Fatal("IPAM request contacted Kubernetes after startup verification")
	}
}

func TestInstallIPAMBinary(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	directory := t.TempDir()
	target := filepath.Join(directory, "unbounded-ipam")

	for _, contents := range []string{"first", "upgraded"} {
		if err := os.WriteFile(source, []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := installIPAMBinary(source, directory); err != nil {
			t.Fatal(err)
		}

		got, err := os.ReadFile(target)
		if err != nil || string(got) != contents {
			t.Fatalf("installed %q: %v", got, err)
		}

		info, err := os.Stat(target)
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Fatalf("binary permissions: %v %v", info, err)
		}
	}

	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary binary leaked: %v %v", entries, err)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(source, target); err != nil {
		t.Fatal(err)
	}

	if err := installIPAMBinary(source, directory); err == nil {
		t.Fatal("installer replaced symlink")
	}

	if err := installIPAMBinary(source, filepath.Join(directory, "unmounted")); err == nil {
		t.Fatal("installer created an unmounted directory")
	}
}

func TestIPAMRuntimeConfiguration(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "yaml", true: "flags"}[override], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")

			data := "node:\n  ipamSocketPath: /run/custom/ipam.sock\n  installIPAMPlugin: false\n  ipamBinDir: /mounted/cni\n"
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg := &config{
				ConfigFile: path, GeneveInterfaceName: "geneve0", VXLANInterfaceName: "vxlan0",
				IPIPInterfaceName: "ipip0", WireGuardInterfacePrefix: "wg",
			}
			cmd := newNodeConfigTestCommand(cfg)

			if override {
				for key, value := range map[string]string{
					"ipam-socket-path": "/run/override/ipam.sock", "install-ipam-plugin": "true", "ipam-bin-dir": "/override/cni",
				} {
					if err := cmd.Flags().Set(key, value); err != nil {
						t.Fatal(err)
					}
				}
			}

			if err := applyNodeRuntimeConfig(cmd, cfg); err != nil {
				t.Fatal(err)
			}

			if override {
				if cfg.IPAMSocketPath != "/run/override/ipam.sock" || !cfg.InstallIPAMPlugin || cfg.IPAMBinDir != "/override/cni" {
					t.Fatalf("flag precedence: %#v", cfg)
				}
			} else if cfg.IPAMSocketPath != "/run/custom/ipam.sock" || cfg.InstallIPAMPlugin || cfg.IPAMBinDir != "/mounted/cni" {
				t.Fatalf("YAML settings: %#v", cfg)
			}
		})
	}

	for _, cfg := range []*config{
		{IPAMSocketPath: "relative"},
		{IPAMSocketPath: "/run/../tmp/ipam.sock"},
		{InstallIPAMPlugin: true, IPAMBinDir: "relative"},
	} {
		if err := validateIPAMConfig(cfg); err == nil {
			t.Fatalf("accepted invalid configuration %#v", cfg)
		}
	}
}

func TestIPAMConflistSocketOverride(t *testing.T) {
	cfg := newCNISafetyTestConfig(t)

	cfg.IPAMSocketPath = "/run/custom/ipam.sock"
	if err := guardedWriteCNIConfig(t.Context(), cfg, []string{"10.244.0.0/24"}, &nodeHealthState{}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(cniConfigPath(cfg))
	if err != nil || !strings.Contains(string(raw), cfg.IPAMSocketPath) || strings.Contains(string(raw), "10.244.0.0") {
		t.Fatalf("custom socket conflist %s: %v", raw, err)
	}
}

func TestIPAMManifestSettings(t *testing.T) {
	for _, value := range []string{"", "false"} {
		t.Run("install="+value, func(t *testing.T) {
			output := t.TempDir()

			data := map[string]string{}
			if value != "" {
				data["NodeInstallIPAMPlugin"] = value
				data["NodeIPAMSocketPath"] = "/run/unbounded-net/custom.sock"
				data["NodeIPAMBinDir"] = "/custom/bin"
			}

			if err := render.Render("../../deploy/net", output, data); err != nil {
				t.Fatal(err)
			}

			var cm corev1.ConfigMap

			raw, err := os.ReadFile(filepath.Join(output, "01-configmap.yaml"))
			if err != nil {
				t.Fatal(err)
			}

			if err := yaml.Unmarshal(raw, &cm); err != nil {
				t.Fatal(err)
			}

			configPath := filepath.Join(output, "config.yaml")
			if err := os.WriteFile(configPath, []byte(cm.Data["config.yaml"]), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := configpkg.LoadRuntimeConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}

			if cfg.Node.InstallIPAMPlugin == nil || *cfg.Node.InstallIPAMPlugin != (value == "") {
				t.Fatalf("installation setting %v", cfg.Node.InstallIPAMPlugin)
			}

			if value != "" && (cfg.Node.IPAMSocketPath != data["NodeIPAMSocketPath"] || cfg.Node.IPAMBinDir != data["NodeIPAMBinDir"]) {
				t.Fatalf("path overrides: %#v", cfg.Node)
			}

			raw, err = os.ReadFile(filepath.Join(output, "node", "03-daemonset.yaml"))
			if err != nil || !strings.Contains(string(raw), "path: /run/unbounded-net") || strings.Count(string(raw), "mountPath: /host/opt/cni/bin") != 2 {
				t.Fatalf("missing socket or agent binary mount: %v", err)
			}
		})
	}
}

func TestInvalidAssignmentFailsClosed(t *testing.T) {
	cfg := newCNISafetyTestConfig(t)
	client := fake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: cfg.NodeName},
		Spec:       corev1.NodeSpec{PodCIDRs: []string{"malformed"}},
	})

	health := &nodeHealthState{}
	if _, err := waitForPodCIDRsAndConfigure(t.Context(), client, cfg, health); err == nil {
		t.Fatal("invalid PodCIDR configured")
	}

	if _, err := os.Stat(cniConfigPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid PodCIDR published conflist: %v", err)
	}

	if _, err := health.getIPAMConfig(); err == nil {
		t.Fatal("invalid PodCIDR served")
	}
}
