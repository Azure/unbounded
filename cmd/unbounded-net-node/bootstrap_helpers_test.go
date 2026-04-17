// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Azure/unbounded-kube/internal/net/buildinfo"
)

// TestWaitForPodCIDRsAndConfigureImmediate tests WaitForPodCIDRsAndConfigureImmediate.
func TestWaitForPodCIDRsAndConfigureImmediate(t *testing.T) {
	ctx := context.Background()
	nodeName := "node-a"
	client := fake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Spec: corev1.NodeSpec{
			PodCIDRs: []string{"10.244.1.0/24"},
		},
	})

	cfg := &config{
		NodeName:    nodeName,
		CNIConfDir:  t.TempDir(),
		CNIConfFile: "10-unbounded.conflist",
		BridgeName:  "cbr0",
		MTU:         1400,
	}
	cniConfigured := false

	podCIDRs, err := waitForPodCIDRsAndConfigure(ctx, client, cfg, &cniConfigured)
	if err != nil {
		t.Fatalf("waitForPodCIDRsAndConfigure returned error: %v", err)
	}

	if !cniConfigured {
		t.Fatalf("expected cniConfigured to be true")
	}

	if len(podCIDRs) != 1 || podCIDRs[0] != "10.244.1.0/24" {
		t.Fatalf("unexpected podCIDRs: %#v", podCIDRs)
	}
}

// TestWaitForPodCIDRsAndConfigureContextCanceled tests WaitForPodCIDRsAndConfigureContextCanceled.
func TestWaitForPodCIDRsAndConfigureContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := fake.NewClientset()
	cfg := &config{NodeName: "node-a"}
	cniConfigured := false

	podCIDRs, err := waitForPodCIDRsAndConfigure(ctx, client, cfg, &cniConfigured)
	if err == nil {
		t.Fatalf("expected context cancellation error")
	}

	if podCIDRs != nil {
		t.Fatalf("expected nil podCIDRs on cancellation, got %#v", podCIDRs)
	}

	if cniConfigured {
		t.Fatalf("expected cniConfigured to remain false on cancellation")
	}
}

// TestWaitForPodCIDRsAndConfigureViaWatchEvent tests WaitForPodCIDRsAndConfigureViaWatchEvent.
func TestWaitForPodCIDRsAndConfigureViaWatchEvent(t *testing.T) {
	nodeName := "node-a"
	client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}})

	watcher := watch.NewFake()

	client.PrependWatchReactor("nodes", func(action k8stesting.Action) (bool, watch.Interface, error) {
		return true, watcher, nil
	})

	cfg := &config{
		NodeName:    nodeName,
		CNIConfDir:  t.TempDir(),
		CNIConfFile: "10-unbounded.conflist",
		BridgeName:  "cbr0",
		MTU:         1400,
	}
	cniConfigured := false

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		time.Sleep(100 * time.Millisecond)
		watcher.Modify(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.244.2.0/24"}},
		})
	}()

	podCIDRs, err := waitForPodCIDRsAndConfigure(ctx, client, cfg, &cniConfigured)
	if err != nil {
		t.Fatalf("waitForPodCIDRsAndConfigure returned error: %v", err)
	}

	if !cniConfigured {
		t.Fatalf("expected cniConfigured to be true after watch update")
	}

	if len(podCIDRs) != 1 || podCIDRs[0] != "10.244.2.0/24" {
		t.Fatalf("unexpected podCIDRs from watch update: %#v", podCIDRs)
	}
}

// TestWaitForPodCIDRsAndConfigureNodeDeleted tests WaitForPodCIDRsAndConfigureNodeDeleted.
func TestWaitForPodCIDRsAndConfigureNodeDeleted(t *testing.T) {
	nodeName := "node-a"
	client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}})

	watcher := watch.NewFake()

	client.PrependWatchReactor("nodes", func(action k8stesting.Action) (bool, watch.Interface, error) {
		return true, watcher, nil
	})

	cfg := &config{NodeName: nodeName}
	cniConfigured := false

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		time.Sleep(100 * time.Millisecond)
		watcher.Delete(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}})
	}()

	podCIDRs, err := waitForPodCIDRsAndConfigure(ctx, client, cfg, &cniConfigured)
	if err == nil || !strings.Contains(err.Error(), "was deleted") {
		t.Fatalf("expected node deleted error, got podCIDRs=%#v err=%v", podCIDRs, err)
	}

	if cniConfigured {
		t.Fatalf("expected cniConfigured to remain false when node is deleted")
	}
}

// TestWriteCNIConfigWritesExpectedConflist tests WriteCNIConfigWritesExpectedConflist.
func TestWriteCNIConfigWritesExpectedConflist(t *testing.T) {
	confDir := t.TempDir()
	cfg := &config{
		CNIConfDir:  confDir,
		CNIConfFile: "10-unbounded.conflist",
		BridgeName:  "cbr0",
		MTU:         1400,
	}

	podCIDRs := []string{"10.244.0.0/24", "fd00:10::/64"}
	if err := writeCNIConfig(cfg, podCIDRs); err != nil {
		t.Fatalf("writeCNIConfig returned error: %v", err)
	}

	confPath := filepath.Join(confDir, cfg.CNIConfFile)

	raw, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("failed to read conflist: %v", err)
	}

	var got CNIConfig
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("failed to unmarshal conflist: %v", err)
	}

	if got.Name != "unbounded-net" || len(got.Plugins) != 3 {
		t.Fatalf("unexpected CNI config shape: %#v", got)
	}

	if got.Plugins[0].Bridge != "cbr0" || got.Plugins[0].MTU != 1400 {
		t.Fatalf("unexpected bridge plugin config: %#v", got.Plugins[0])
	}

	if len(got.Plugins[0].IPAM.Ranges) != 2 {
		t.Fatalf("expected two IPAM ranges, got %d", len(got.Plugins[0].IPAM.Ranges))
	}

	if got.Plugins[0].IPAM.Ranges[0][0].Subnet != podCIDRs[0] || got.Plugins[0].IPAM.Ranges[1][0].Subnet != podCIDRs[1] {
		t.Fatalf("unexpected IPAM ranges: %#v", got.Plugins[0].IPAM.Ranges)
	}

	if _, err := os.Stat(confPath + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected temp file to be renamed away, stat err=%v", err)
	}
}

// TestCheckCNIConfDirForConflists tests checkCNIConfDirForConflists behavior.
func TestCheckCNIConfDirForConflists(t *testing.T) {
	ownFile := "10-unbounded.conflist"

	t.Run("nonexistent directory", func(t *testing.T) {
		if err := checkCNIConfDirForConflists(filepath.Join(t.TempDir(), "does-not-exist"), ownFile); err != nil {
			t.Fatalf("expected nil for nonexistent dir, got %v", err)
		}
	})
	t.Run("empty directory", func(t *testing.T) {
		if err := checkCNIConfDirForConflists(t.TempDir(), ownFile); err != nil {
			t.Fatalf("expected nil for empty dir, got %v", err)
		}
	})
	t.Run("directory with foreign conflist file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "10-flannel.conflist"), []byte("{}"), 0o644); err != nil {
			t.Fatalf("setup: %v", err)
		}

		err := checkCNIConfDirForConflists(dir, ownFile)
		if err == nil {
			t.Fatal("expected error for dir with foreign conflist")
		}

		if !errors.Is(err, errCNIConfDirNotEmpty) {
			t.Fatalf("expected errCNIConfDirNotEmpty, got %v", err)
		}

		if !strings.Contains(err.Error(), "10-flannel.conflist") {
			t.Fatalf("expected error to list the offending file, got %v", err)
		}
	})
	t.Run("directory with own conflist file only", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ownFile), []byte("{}"), 0o644); err != nil {
			t.Fatalf("setup: %v", err)
		}

		if err := checkCNIConfDirForConflists(dir, ownFile); err != nil {
			t.Fatalf("expected nil when only own conflist is present, got %v", err)
		}
	})
	t.Run("directory with non-conflist file only", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "bridge.conf"), []byte("{}"), 0o644); err != nil {
			t.Fatalf("setup: %v", err)
		}

		if err := checkCNIConfDirForConflists(dir, ownFile); err != nil {
			t.Fatalf("expected nil for dir with only non-conflist files, got %v", err)
		}
	})
	t.Run("directory with subdirectory only", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}

		if err := checkCNIConfDirForConflists(dir, ownFile); err != nil {
			t.Fatalf("expected nil for dir with only subdirs, got %v", err)
		}
	})
}

// TestWriteCNIConfigRefusesNonEmptyDir tests that writeCNIConfig refuses to
// write when the CNI conf directory already contains files.
func TestWriteCNIConfigRefusesNonEmptyDir(t *testing.T) {
	confDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(confDir, "05-cilium.conflist"), []byte(`{"cniVersion":"0.4.0"}`), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cfg := &config{
		CNIConfDir:  confDir,
		CNIConfFile: "10-unbounded.conflist",
		BridgeName:  "cbr0",
		MTU:         1400,
	}

	err := writeCNIConfig(cfg, []string{"10.244.0.0/24"})
	if err == nil {
		t.Fatal("expected error when CNI dir is not empty")
	}

	if !errors.Is(err, errCNIConfDirNotEmpty) {
		t.Fatalf("expected errCNIConfDirNotEmpty, got %v", err)
	}
}

// TestWaitForPodCIDRsAndConfigureExistingCNIFatal tests that
// waitForPodCIDRsAndConfigure returns immediately (without retrying)
// when the CNI conf directory already contains files.
func TestWaitForPodCIDRsAndConfigureExistingCNIFatal(t *testing.T) {
	nodeName := "node-a"
	client := fake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.244.1.0/24"}},
	})

	confDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(confDir, "99-other.conflist"), []byte(`{"cniVersion":"0.4.0"}`), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cfg := &config{
		NodeName:    nodeName,
		CNIConfDir:  confDir,
		CNIConfFile: "10-unbounded.conflist",
		BridgeName:  "cbr0",
		MTU:         1400,
	}
	cniConfigured := false

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := waitForPodCIDRsAndConfigure(ctx, client, cfg, &cniConfigured)
	if err == nil {
		t.Fatal("expected error for non-empty CNI dir")
	}

	if !errors.Is(err, errCNIConfDirNotEmpty) {
		t.Fatalf("expected errCNIConfDirNotEmpty, got %v", err)
	}

	if cniConfigured {
		t.Fatal("expected cniConfigured to remain false")
	}
}

// TestValidateWireGuardDir tests validateWireGuardDir path validation.
func TestValidateWireGuardDir(t *testing.T) {
	tests := []struct {
		name    string
		dir     string
		wantErr bool
	}{
		{"valid /etc path", "/etc/wireguard", false},
		{"valid /var path", "/var/lib/wireguard", false},
		{"valid /run path", "/run/wireguard", false},
		{"relative path", "etc/wireguard", true},
		{"traversal components", "/etc/../tmp/evil", true},
		{"disallowed prefix", "/home/user/wg", true},
		{"empty string", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWireGuardDir(tt.dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateWireGuardDir(%q) error = %v, wantErr %v", tt.dir, err, tt.wantErr)
			}
		})
	}
}

// TestEnsureWireGuardKeysGenerateAndReuse tests EnsureWireGuardKeysGenerateAndReuse.
func TestEnsureWireGuardKeysGenerateAndReuse(t *testing.T) {
	origPrefixes := allowedWireGuardPrefixes
	allowedWireGuardPrefixes = []string{"/"}

	defer func() { allowedWireGuardPrefixes = origPrefixes }()

	cfg := &config{WireGuardDir: t.TempDir()}

	pubKey1, err := ensureWireGuardKeys(cfg)
	if err != nil {
		t.Fatalf("ensureWireGuardKeys generate returned error: %v", err)
	}

	privPath := filepath.Join(cfg.WireGuardDir, "server.priv")
	pubPath := filepath.Join(cfg.WireGuardDir, "server.pub")

	privRaw, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatalf("failed to read private key: %v", err)
	}

	pubRaw, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatalf("failed to read public key: %v", err)
	}

	if _, err := base64.StdEncoding.DecodeString(string(privRaw)); err != nil {
		t.Fatalf("private key is not valid base64: %v", err)
	}

	if _, err := base64.StdEncoding.DecodeString(string(pubRaw)); err != nil {
		t.Fatalf("public key is not valid base64: %v", err)
	}

	pubKey2, err := ensureWireGuardKeys(cfg)
	if err != nil {
		t.Fatalf("ensureWireGuardKeys reuse returned error: %v", err)
	}

	if pubKey1 != pubKey2 {
		t.Fatalf("expected stable public key reuse, got %q then %q", pubKey1, pubKey2)
	}
}

// TestAnnotateNodeWithPubKeyAndTaintGatewayNode tests AnnotateNodeWithPubKeyAndTaintGatewayNode.
func TestAnnotateNodeWithPubKeyAndTaintGatewayNode(t *testing.T) {
	ctx := context.Background()
	nodeName := "node-a"
	client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}})

	if err := annotateNodeWithPubKey(ctx, client, nodeName, "pub-123"); err != nil {
		t.Fatalf("annotateNodeWithPubKey returned error: %v", err)
	}

	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node after annotate failed: %v", err)
	}

	if node.Annotations[WireGuardPubKeyAnnotation] != "pub-123" {
		t.Fatalf("annotation not applied, got annotations=%#v", node.Annotations)
	}

	if err := taintGatewayNode(ctx, client, nodeName); err != nil {
		t.Fatalf("taintGatewayNode returned error: %v", err)
	}

	if err := taintGatewayNode(ctx, client, nodeName); err != nil {
		t.Fatalf("taintGatewayNode idempotent call returned error: %v", err)
	}

	node, err = client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node after taint failed: %v", err)
	}

	count := 0

	for _, taint := range node.Spec.Taints {
		if taint.Key == GatewayNodeTaintKey && taint.Effect == corev1.TaintEffectNoSchedule {
			count++
		}
	}

	if count != 1 {
		t.Fatalf("expected exactly one gateway taint, got %d taints=%#v", count, node.Spec.Taints)
	}
}

// TestNodeAgentBuildInfo tests NodeAgentBuildInfo.
func TestNodeAgentBuildInfo(t *testing.T) {
	oldVersion, oldCommit, oldBuildTime := buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime

	defer func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime = oldVersion, oldCommit, oldBuildTime
	}()

	buildinfo.Version = "v-test"
	buildinfo.Commit = "abc123"
	buildinfo.BuildTime = "2026-02-20T00:00:00Z"

	got := nodeAgentBuildInfo()
	if got.Version != buildinfo.Version || got.Commit != buildinfo.Commit || got.BuildTime != buildinfo.BuildTime {
		t.Fatalf("unexpected build info: %#v", got)
	}
}
