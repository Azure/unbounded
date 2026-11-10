// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/meshfetch"
)

// renderedAgentConfig returns config.yaml from a rendered ConfigMap.
func renderedAgentConfig(t *testing.T, dir string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "configmap.yaml"))
	if err != nil {
		t.Fatalf("read rendered config: %v", err)
	}

	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("unmarshal ConfigMap: %v", err)
	}

	return cm.Data["config.yaml"]
}

func TestDefaultRenderHasNoCachemesh(t *testing.T) {
	t.Parallel()

	for _, operator := range []bool{false, true} {
		dir := renderChart(t, operator)

		if cfg := renderedAgentConfig(t, dir); strings.Contains(cfg, "cachemesh") {
			t.Fatalf("operator=%v: default config renders cachemesh:\n%s", operator, cfg)
		}

		ds, err := os.ReadFile(filepath.Join(dir, "daemonset.yaml"))
		if err != nil {
			t.Fatal(err)
		}

		if bytes.Contains(ds, []byte("GANTRY_NODE_NAME")) {
			t.Fatalf("operator=%v: default DaemonSet sets GANTRY_NODE_NAME", operator)
		}

		if bytes.Contains(ds, []byte("containerd-content")) || bytes.Contains(ds, []byte("io.containerd.content.v1.content")) {
			t.Fatalf("operator=%v: default DaemonSet mounts the content store", operator)
		}

		if bytes.Contains(ds, []byte("cachemesh-peer-key")) {
			t.Fatalf("operator=%v: default DaemonSet mounts the cachemesh peer key", operator)
		}

		if _, err := os.Stat(filepath.Join(dir, "cachemesh-peer-key.yaml")); err == nil {
			t.Fatalf("operator=%v: default render has a cachemesh peer key Secret", operator)
		}
	}
}

// With cachemesh enabled the content store is mounted read-only from the
// host at the agent's default content_root, and no chunk volume exists.
func TestRendersCachemeshContentRootMount(t *testing.T) {
	t.Parallel()

	dir := renderChart(t, false, "--set", "gantry.cachemesh.enabled=true")

	ds, err := os.ReadFile(filepath.Join(dir, "daemonset.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	root := config.DefaultCachemeshContentRoot

	for _, want := range []string{
		"- name: containerd-content\n              mountPath: " + root + "\n              readOnly: true\n",
		"- name: containerd-content\n          hostPath:\n            path: " + root + "\n            type: Directory\n",
	} {
		if !bytes.Contains(ds, []byte(want)) {
			t.Fatalf("DaemonSet lacks %q:\n%s", want, ds)
		}
	}

	if bytes.Contains(ds, []byte("cachemesh-chunks")) || bytes.Contains(ds, []byte("emptyDir:\n            {}")) {
		t.Fatalf("DaemonSet still has a chunk volume:\n%s", ds)
	}

	if cfg := renderedAgentConfig(t, dir); !strings.Contains(cfg, `  content_root: "`+root+`"`) {
		t.Fatalf("config lacks content_root:\n%s", cfg)
	}
}

// The chart rejects a relative content root and keys it does not know.
func TestRejectsBadCachemeshValues(t *testing.T) {
	t.Parallel()

	for _, kv := range []string{"contentRoot: relative", "pageSize: 1"} {
		values := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(values, []byte("gantry:\n  cachemesh:\n    enabled: true\n    "+kv+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if out, err := helmTemplate(t, "--values", values); err == nil {
			t.Errorf("chart accepted %q:\n%s", kv, out)
		}
	}
}

func TestRendersEnabledCachemesh(t *testing.T) {
	t.Parallel()

	// A values file makes Helm read numbers as float64, the case where a
	// careless template prints 8.589934592e+09.
	values := filepath.Join(t.TempDir(), "values.yaml")

	err := os.WriteFile(values, []byte(`gantry:
  cachemesh:
    enabled: true
    expectedSize: 150
    maxNeighbors: 32
    chunkSize: 33554432
    contentRoot: /data/containerd/io.containerd.content.v1.content
    inflightBytes: 536870912
    maxConcurrentChunkFills: 32
    cacheTags: false
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	dir := renderChart(t, false, "--values", values)
	cfgText := renderedAgentConfig(t, dir)

	ds, err := os.ReadFile(filepath.Join(dir, "daemonset.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(ds, []byte("name: GANTRY_NODE_NAME")) || !bytes.Contains(ds, []byte("fieldPath: spec.nodeName")) {
		t.Fatal("DaemonSet lacks GANTRY_NODE_NAME from spec.nodeName with cachemesh enabled")
	}

	// A custom content root is mounted at the same path.
	for _, want := range []string{
		"mountPath: /data/containerd/io.containerd.content.v1.content\n              readOnly: true\n",
		"path: /data/containerd/io.containerd.content.v1.content\n            type: Directory\n",
	} {
		if !bytes.Contains(ds, []byte(want)) {
			t.Fatalf("DaemonSet lacks %q:\n%s", want, ds)
		}
	}

	for _, want := range []string{
		"cachemesh:\n  enabled: true\n",
		`  content_root: "/data/containerd/io.containerd.content.v1.content"`,
		"  inflight_bytes: 536870912\n",
		"  max_concurrent_chunk_fills: 32\n",
		"  chunk_size: 33554432\n",
		"  max_neighbors: 32\n",
		"  expected_size: 150\n",
		"  cache_tags: false\n",
	} {
		if !strings.Contains(cfgText, want) {
			t.Fatalf("rendered config lacks %q:\n%s", want, cfgText)
		}
	}

	// Removed knobs are never rendered.
	if strings.Contains(cfgText, "lookahead") || strings.Contains(cfgText, "hedge") {
		t.Fatalf("removed knobs rendered:\n%s", cfgText)
	}

	c := config.NewDefault()
	if err := c.LoadYAML(strings.NewReader(cfgText)); err != nil {
		t.Fatalf("agent rejects rendered config: %v", err)
	}

	m := c.Cachemesh
	if !m.Enabled || m.ContentRoot != "/data/containerd/io.containerd.content.v1.content" ||
		m.InflightBytes != 512<<20 || m.MaxConcurrentChunkFills != 32 || m.ChunkSize != 32<<20 || m.MaxNeighbors != 32 ||
		m.ExpectedSize != 150 || m.CacheTags || !m.VerifyDigest {
		t.Fatalf("parsed cachemesh config: %+v", m)
	}

	if m.PeerKeyFile != peerKeyFile {
		t.Fatalf("peer_key_file = %q, want %q", m.PeerKeyFile, peerKeyFile)
	}

	c.PodIP = "10.0.0.1"
	if err := c.Validate(); err != nil {
		t.Fatalf("rendered config does not validate: %v", err)
	}
}

const peerKeyFile = "/etc/gantry/cachemesh/peer-key"

// peerKeyVolume is the rendered peer key volume for secret name and key.
func peerKeyVolume(name, key string) string {
	return "- name: cachemesh-peer-key\n          secret:\n            secretName: " + name +
		"\n            defaultMode: 0444\n            items:\n              - key: " + key + "\n                path: peer-key\n"
}

const peerKeyMount = "- name: cachemesh-peer-key\n              mountPath: /etc/gantry/cachemesh\n              readOnly: true\n"

// With cachemesh enabled the chart renders a random peer key Secret, mounts
// it read-only, and points peer_key_file at it.
func TestRendersCachemeshPeerKey(t *testing.T) {
	t.Parallel()

	dir := renderChart(t, false, "--set", "gantry.cachemesh.enabled=true")

	raw, err := os.ReadFile(filepath.Join(dir, "cachemesh-peer-key.yaml"))
	if err != nil {
		t.Fatalf("no peer key Secret: %v", err)
	}

	var secret struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &secret); err != nil {
		t.Fatal(err)
	}

	if secret.Kind != "Secret" || secret.Metadata.Name != "gantry-cachemesh-peer-key" || secret.Metadata.Namespace != "unbounded-system" {
		t.Fatalf("unexpected Secret:\n%s", raw)
	}

	key, err := base64.StdEncoding.DecodeString(secret.Data["peer-key"])
	if err != nil {
		t.Fatal(err)
	}

	if len(key) != 64 {
		t.Fatalf("peer key is %d bytes, want 64", len(key))
	}

	if _, err := meshfetch.ParsePeerKeys(key); err != nil {
		t.Fatalf("agent rejects rendered key: %v", err)
	}

	ds, err := os.ReadFile(filepath.Join(dir, "daemonset.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{peerKeyMount, peerKeyVolume("gantry-cachemesh-peer-key", "peer-key")} {
		if !bytes.Contains(ds, []byte(want)) {
			t.Fatalf("DaemonSet lacks %q:\n%s", want, ds)
		}
	}

	if cfg := renderedAgentConfig(t, dir); !strings.Contains(cfg, `  peer_key_file: "`+peerKeyFile+`"`) {
		t.Fatalf("config lacks peer_key_file:\n%s", cfg)
	}
}

// An existing Secret replaces the generated one.
func TestRendersCachemeshExistingPeerKeySecret(t *testing.T) {
	t.Parallel()

	dir := renderChart(t, false,
		"--set", "gantry.cachemesh.enabled=true",
		"--set", "gantry.cachemesh.peerKey.existingSecret=my-mesh-key",
		"--set", "gantry.cachemesh.peerKey.secretKey=keys",
	)

	if _, err := os.Stat(filepath.Join(dir, "cachemesh-peer-key.yaml")); err == nil {
		t.Fatal("chart renders a peer key Secret despite existingSecret")
	}

	ds, err := os.ReadFile(filepath.Join(dir, "daemonset.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{peerKeyMount, peerKeyVolume("my-mesh-key", "keys")} {
		if !bytes.Contains(ds, []byte(want)) {
			t.Fatalf("DaemonSet lacks %q:\n%s", want, ds)
		}
	}

	if cfg := renderedAgentConfig(t, dir); !strings.Contains(cfg, `  peer_key_file: "`+peerKeyFile+`"`) {
		t.Fatalf("config lacks peer_key_file:\n%s", cfg)
	}
}

// helmTemplate renders the chart with extra args and returns helm's output
// and error, without failing the test.
func helmTemplate(t *testing.T, extraArgs ...string) (string, error) {
	t.Helper()

	deployDir := filepath.Dir(sourceFile(t))

	helm := filepath.Join(deployDir, "..", "..", "bin", "helm")
	if _, err := os.Stat(helm); err != nil {
		var lookupErr error

		helm, lookupErr = exec.LookPath("helm")
		if lookupErr != nil {
			t.Fatal("helm not found; run make install-helm")
		}
	}

	args := append([]string{
		"template", "gantry", filepath.Join(deployDir, "chart"),
		"--namespace", "unbounded-system",
		"--set-string", "image.reference=gantry:test",
	}, extraArgs...)

	out, err := exec.Command(helm, args...).CombinedOutput()

	return string(out), err
}
