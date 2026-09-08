// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newCNISafetyTestConfig(t *testing.T) *config {
	t.Helper()

	return &config{
		NodeName:         "node-a",
		CNIConfDir:       t.TempDir(),
		CNIConfFile:      "10-unbounded.conflist",
		BridgeName:       "cbr0",
		MTU:              1400,
		cniInspector:     allowAllCNIInspection,
		cniRetryInterval: 10 * time.Millisecond,
	}
}

func TestGuardedWriteCNIConfigInspectsWithoutPriorConfig(t *testing.T) {
	cfg := newCNISafetyTestConfig(t)

	var calls atomic.Int32

	cfg.cniInspector = func(_ context.Context, bridgeName, procRoot string, cidrs []string) error {
		calls.Add(1)

		if bridgeName != "cbr0" || procRoot != defaultCNIInspectionRoot || strings.Join(cidrs, ",") != "10.244.1.0/24" {
			t.Fatalf("unexpected inspection: bridge=%q procRoot=%q cidrs=%v", bridgeName, procRoot, cidrs)
		}

		return nil
	}

	health := &nodeHealthState{}
	health.beginManagedCNI(cfg.BridgeName)

	if err := guardedWriteCNIConfig(context.Background(), cfg, []string{"10.244.1.0/24"}, health); err != nil {
		t.Fatalf("guardedWriteCNIConfig returned error: %v", err)
	}

	if calls.Load() != 1 {
		t.Fatalf("inspector calls = %d, want 1", calls.Load())
	}

	if _, err := os.Stat(cniConfigPath(cfg)); err != nil {
		t.Fatalf("expected active CNI configuration: %v", err)
	}

	if ready, reason := health.cniReadiness(); !ready {
		t.Fatalf("expected ready state, reason=%q", reason)
	}
}

func TestGuardedWriteCNIConfigDisablesOwnedConfigAndRecovers(t *testing.T) {
	cfg := newCNISafetyTestConfig(t)
	oldCIDRs := []string{"10.244.1.0/24"}
	newCIDRs := []string{"10.244.2.0/24"}

	if err := writeCNIConfigUnchecked(cfg, oldCIDRs); err != nil {
		t.Fatalf("write old config: %v", err)
	}

	oldBytes, err := os.ReadFile(cniConfigPath(cfg))
	if err != nil {
		t.Fatalf("read old config: %v", err)
	}

	var unsafe atomic.Bool
	unsafe.Store(true)

	cfg.cniInspector = func(context.Context, string, string, []string) error {
		if unsafe.Load() {
			return errors.New("interface=veth-old address=10.244.1.8 outside assigned PodCIDRs")
		}

		return nil
	}

	health := &nodeHealthState{}
	health.beginManagedCNI(cfg.BridgeName)

	err = guardedWriteCNIConfig(context.Background(), cfg, newCIDRs, health)
	if err == nil {
		t.Fatal("expected unsafe bridge to block CNI write")
	}

	if _, statErr := os.Stat(cniConfigPath(cfg)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("active config should be disabled, stat error=%v", statErr)
	}

	disabledBytes, readErr := os.ReadFile(cniDisabledPath(cfg))
	if readErr != nil {
		t.Fatalf("read disabled config: %v", readErr)
	}

	if string(disabledBytes) != string(oldBytes) {
		t.Fatal("disabled config bytes changed")
	}

	if ready, reason := health.cniReadiness(); ready || !strings.Contains(reason, "veth-old") ||
		!strings.Contains(reason, cniDisabledPath(cfg)) {
		t.Fatalf("unexpected blocked readiness: ready=%t reason=%q", ready, reason)
	}

	if err := guardedWriteCNIConfig(context.Background(), cfg, newCIDRs, health); err == nil {
		t.Fatal("restart-style retry should remain blocked while live mismatch exists")
	}

	if _, statErr := os.Stat(cniDisabledPath(cfg)); statErr != nil {
		t.Fatalf("already-disabled config should remain preserved: %v", statErr)
	}

	snapshot := health.getStatusSnapshot()
	if len(snapshot.NodeErrors) != 1 || snapshot.NodeErrors[0].Type != configPodCIDRGuard {
		t.Fatalf("expected persistent CNI guard error, got %#v", snapshot.NodeErrors)
	}

	unsafe.Store(false)

	if err := guardedWriteCNIConfig(context.Background(), cfg, newCIDRs, health); err != nil {
		t.Fatalf("recovery write failed: %v", err)
	}

	if _, statErr := os.Stat(cniDisabledPath(cfg)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("disabled config should be removed after recovery, stat error=%v", statErr)
	}

	if ready, reason := health.cniReadiness(); !ready {
		t.Fatalf("expected readiness recovery, reason=%q", reason)
	}

	if got := health.getStatusSnapshot().NodeErrors; len(got) != 0 {
		t.Fatalf("expected CNI error to clear after recovery, got %#v", got)
	}
}

func TestSummarizeCNIDiagnosticBoundsJoinedFindings(t *testing.T) {
	findings := make([]error, 0, 20)
	for i := range 20 {
		findings = append(findings, errors.New(
			"interface=veth-"+string(rune('a'+i))+" address=10.244.1.5 detail="+strings.Repeat("x", 500),
		))
	}

	summary := summarizeCNIDiagnostic(errors.Join(findings...))
	if len([]rune(summary)) > cniDiagnosticMaxFindings*(cniDiagnosticMaxFindingCharacters+20)+100 {
		t.Fatalf("joined diagnostic was not bounded: length=%d", len([]rune(summary)))
	}

	if !strings.Contains(summary, "interface=veth-a") ||
		!strings.Contains(summary, "interface=veth-c") {
		t.Fatalf("representative findings missing: %q", summary)
	}

	if strings.Contains(summary, "interface=veth-d") {
		t.Fatalf("diagnostic included more than %d findings: %q", cniDiagnosticMaxFindings, summary)
	}

	if !strings.Contains(summary, "17 additional finding(s) omitted") {
		t.Fatalf("omitted finding count missing: %q", summary)
	}

	if strings.Contains(summary, "\n") {
		t.Fatalf("diagnostic should be a single log/status line: %q", summary)
	}
}

func TestSummarizeCNIDiagnosticPreservesConciseIOReason(t *testing.T) {
	const message = "open /proc/123/ns/net: permission denied"

	if got := summarizeCNIDiagnostic(errors.New(message)); got != message {
		t.Fatalf("concise diagnostic changed: got %q want %q", got, message)
	}
}

func TestGuardedWriteCNIConfigPreservesSymlinks(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		for _, disabled := range []bool{false, true} {
			cfg := newCNISafetyTestConfig(t)
			if unsafe {
				cfg.cniInspector = func(context.Context, string, string, []string) error {
					return errors.New("unsafe address")
				}
			}

			path := cniConfigPath(cfg)
			if disabled {
				path = cniDisabledPath(cfg)
			}

			target := filepath.Join(cfg.CNIConfDir, "operator-target")
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}

			err := guardedWriteCNIConfig(context.Background(), cfg, []string{"10.244.2.0/24"}, &nodeHealthState{})
			if err == nil {
				t.Fatal("dangling symlink collision must block")
			}

			if got, err := os.Readlink(path); err != nil || got != target {
				t.Fatalf("symlink was overwritten or removed: target=%q err=%v", got, err)
			}

			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("guard wrote through symlink: %v", err)
			}
		}
	}
}

func TestGuardedWriteCNIConfigPendingAndCanceledRewrite(t *testing.T) {
	cfg := newCNISafetyTestConfig(t)
	cidrs := []string{"10.244.1.0/24"}

	health := &nodeHealthState{}
	if err := guardedWriteCNIConfig(context.Background(), cfg, cidrs, health); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(cniConfigPath(cfg))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg.cniInspector = func(context.Context, string, string, []string) error {
		if ready, reason := health.cniReadiness(); ready || reason == "" {
			t.Fatalf("runtime inspection must be unready with a diagnostic: %v %q", ready, reason)
		}

		cancel()

		return ctx.Err()
	}
	if err := guardedWriteCNIConfig(ctx, cfg, cidrs, health); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected preserved cancellation cause, got %v", err)
	}

	after, err := os.ReadFile(cniConfigPath(cfg))
	if err != nil || string(after) != string(before) {
		t.Fatalf("shutdown during inspection changed existing CNI config: %v", err)
	}

	if _, err := os.Stat(cniDisabledPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shutdown alone disabled existing CNI: %v", err)
	}
}

func TestWaitForPodCIDRsAndConfigureCancelsNodeReadRetry(t *testing.T) {
	cfg := newCNISafetyTestConfig(t)
	cfg.cniRetryInterval = time.Hour
	client := fake.NewClientset()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		cancel()
		return true, nil, errors.New("node lookup failed")
	})

	done := make(chan error, 1)

	go func() {
		_, err := waitForPodCIDRsAndConfigure(ctx, client, cfg, &nodeHealthState{})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation during retry: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("node read retry ignored cancellation")
	}
}

func TestCNIWriteRetryRetainsBlockingReason(t *testing.T) {
	health := &nodeHealthState{}
	health.setCNIBlocked("old interface address outside assigned PodCIDRs")
	_, originalReason := health.cniReadiness()
	health.beginCNIWrite("cbr0", []string{"10.244.2.0/24"})

	if ready, reason := health.cniReadiness(); ready || reason != originalReason {
		t.Fatalf("retry must not erase the active diagnostic: %v %q", ready, reason)
	}
}

func TestGuardedWriteCNIConfigPreservesCollisions(t *testing.T) {
	tests := []struct {
		name     string
		path     func(*config) string
		contents string
		unsafe   bool
	}{
		{name: "foreign active unsafe", path: cniConfigPath, contents: `{"name":"foreign","plugins":[]}`, unsafe: true},
		{name: "malformed active safe", path: cniConfigPath, contents: `{`, unsafe: false},
		{name: "incomplete bridge plugin", path: cniConfigPath, contents: `{"cniVersion":"0.4.0","name":"unbounded-net","plugins":[{"type":"bridge","bridge":"cbr0"}]}`, unsafe: true},
		{name: "foreign disabled safe", path: cniDisabledPath, contents: `{"name":"foreign","plugins":[]}`, unsafe: false},
		{name: "temporary collision safe", path: func(cfg *config) string { return cniConfigPath(cfg) + ".tmp" }, contents: "operator data", unsafe: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newCNISafetyTestConfig(t)

			path := tt.path(cfg)
			if err := os.WriteFile(path, []byte(tt.contents), 0o644); err != nil {
				t.Fatalf("setup collision: %v", err)
			}

			if tt.unsafe {
				cfg.cniInspector = func(context.Context, string, string, []string) error {
					return errors.New("address outside assigned PodCIDRs")
				}
			}

			err := guardedWriteCNIConfig(context.Background(), cfg, []string{"10.244.2.0/24"}, &nodeHealthState{})
			if err == nil {
				t.Fatal("expected collision to block write")
			}

			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("collision file was removed: %v", readErr)
			}

			if string(got) != tt.contents {
				t.Fatalf("collision file changed: got %q want %q", got, tt.contents)
			}
		})
	}
}

func TestGuardedWriteCNIConfigSurfacesFileOperationFailures(t *testing.T) {
	t.Run("disable rename", func(t *testing.T) {
		cfg := newCNISafetyTestConfig(t)
		if err := writeCNIConfigUnchecked(cfg, []string{"10.244.1.0/24"}); err != nil {
			t.Fatalf("write old config: %v", err)
		}

		cfg.cniInspector = func(context.Context, string, string, []string) error {
			return errors.New("unsafe address")
		}
		cfg.cniRename = func(string, string) error {
			return errors.New("rename denied")
		}

		err := guardedWriteCNIConfig(context.Background(), cfg, []string{"10.244.2.0/24"}, &nodeHealthState{})
		if err == nil || !strings.Contains(err.Error(), "rename denied") {
			t.Fatalf("expected rename failure, got %v", err)
		}

		if _, statErr := os.Stat(cniConfigPath(cfg)); statErr != nil {
			t.Fatalf("active config should remain after rename failure: %v", statErr)
		}
	})

	t.Run("temporary write", func(t *testing.T) {
		cfg := newCNISafetyTestConfig(t)
		cfg.cniWriteFile = func(string, []byte, os.FileMode) error {
			return errors.New("disk full")
		}

		err := guardedWriteCNIConfig(context.Background(), cfg, []string{"10.244.2.0/24"}, &nodeHealthState{})
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("expected write failure, got %v", err)
		}

		if _, statErr := os.Stat(cniConfigPath(cfg)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("write failure published an active config, stat error=%v", statErr)
		}
	})

	t.Run("publish rename cleans temporary file", func(t *testing.T) {
		cfg := newCNISafetyTestConfig(t)
		cfg.cniRename = func(_, newPath string) error {
			if newPath == cniConfigPath(cfg) {
				return errors.New("atomic publish failed")
			}

			return nil
		}

		err := guardedWriteCNIConfig(context.Background(), cfg, []string{"10.244.2.0/24"}, &nodeHealthState{})
		if err == nil || !strings.Contains(err.Error(), "atomic publish failed") {
			t.Fatalf("expected publish rename failure, got %v", err)
		}

		if _, statErr := os.Stat(cniConfigPath(cfg) + ".tmp"); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("temporary config remained after rename failure, stat error=%v", statErr)
		}
	})

	t.Run("disabled cleanup", func(t *testing.T) {
		cfg := newCNISafetyTestConfig(t)
		if err := writeCNIConfigUnchecked(cfg, []string{"10.244.1.0/24"}); err != nil {
			t.Fatalf("write old config: %v", err)
		}

		var unsafe atomic.Bool
		unsafe.Store(true)

		cfg.cniInspector = func(context.Context, string, string, []string) error {
			if unsafe.Load() {
				return errors.New("unsafe address")
			}

			return nil
		}

		health := &nodeHealthState{}
		health.beginManagedCNI(cfg.BridgeName)

		if err := guardedWriteCNIConfig(context.Background(), cfg, []string{"10.244.2.0/24"}, health); err == nil {
			t.Fatal("expected initial safety block")
		}

		unsafe.Store(false)

		cfg.cniRemove = func(path string) error {
			if path == cniDisabledPath(cfg) {
				return errors.New("cleanup denied")
			}

			return os.Remove(path)
		}

		err := guardedWriteCNIConfig(context.Background(), cfg, []string{"10.244.2.0/24"}, health)
		if err == nil || !strings.Contains(err.Error(), "cleanup denied") {
			t.Fatalf("expected cleanup failure, got %v", err)
		}

		if ready, _ := health.cniReadiness(); ready {
			t.Fatal("cleanup failure must retain readiness block")
		}

		if _, statErr := os.Stat(cniDisabledPath(cfg)); statErr != nil {
			t.Fatalf("disabled diagnostics should remain after cleanup failure: %v", statErr)
		}
	})
}

func TestWaitForPodCIDRsAndConfigureRefreshesAssignmentAfterBlock(t *testing.T) {
	cfg := newCNISafetyTestConfig(t)
	client := fake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: cfg.NodeName},
		Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.244.1.0/24"}},
	})

	var (
		mu        sync.Mutex
		inspected [][]string
	)

	cfg.cniInspector = func(ctx context.Context, _, _ string, cidrs []string) error {
		mu.Lock()

		inspected = append(inspected, append([]string(nil), cidrs...))
		call := len(inspected)
		mu.Unlock()

		if call == 1 {
			node, err := client.CoreV1().Nodes().Get(ctx, cfg.NodeName, metav1.GetOptions{})
			if err != nil {
				return err
			}

			node.Spec.PodCIDRs = []string{"10.244.2.0/24"}
			if _, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
				return err
			}

			return errors.New("old interface address blocks first assignment")
		}

		return nil
	}

	health := &nodeHealthState{}
	health.beginManagedCNI(cfg.BridgeName)

	got, err := waitForPodCIDRsAndConfigure(context.Background(), client, cfg, health)
	if err != nil {
		t.Fatalf("waitForPodCIDRsAndConfigure returned error: %v", err)
	}

	if strings.Join(got, ",") != "10.244.2.0/24" {
		t.Fatalf("configured stale assignment: %v", got)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(inspected) < 2 || strings.Join(inspected[0], ",") != "10.244.1.0/24" ||
		strings.Join(inspected[len(inspected)-1], ",") != "10.244.2.0/24" {
		t.Fatalf("unexpected inspected assignments: %v", inspected)
	}
}

func TestGuardedWriteCNIConfigCancellationAndConcurrentReadiness(t *testing.T) {
	cfg := newCNISafetyTestConfig(t)
	cfg.cniInspector = func(ctx context.Context, _, _ string, _ []string) error {
		<-ctx.Done()

		return ctx.Err()
	}

	health := &nodeHealthState{}
	health.beginManagedCNI(cfg.BridgeName)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- guardedWriteCNIConfig(ctx, cfg, []string{"10.244.1.0/24"}, health)
	}()

	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()

			for range 100 {
				_, _ = health.cniReadiness()
				_ = health.getStatusSnapshot()
			}
		}()
	}

	cancel()

	if err := <-done; err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("expected cancellation error, got %v", err)
	}

	readers.Wait()

	if _, err := os.Stat(filepath.Join(cfg.CNIConfDir, cfg.CNIConfFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled guard must not publish CNI config, stat error=%v", err)
	}
}

func TestCNIGuardConditionSurvivesTTLAndFullServerHandoff(t *testing.T) {
	health := &nodeHealthState{
		transientErrors: []NodeError{{
			Type:      "directPush",
			Message:   "expired",
			Timestamp: time.Now().Add(-2 * time.Minute),
		}},
	}

	health.setBootstrapSnapshot("node-a", "site-a", "pub-a", []string{"10.244.1.0/24"}, false)
	health.beginManagedCNI("cbr0")
	health.setCNIBlocked("CNI configuration blocked; remaining unready bridge=cbr0 assignedPodCIDRs=[10.244.1.0/24] reason=test")

	bootstrap := health.getStatusSnapshot()
	if len(bootstrap.NodeErrors) != 1 || bootstrap.NodeErrors[0].Type != configPodCIDRGuard {
		t.Fatalf("persistent guard should outlive transient TTL, got %#v", bootstrap.NodeErrors)
	}

	health.setStatusServer(&nodeStatusServer{
		cfg:    &config{NodeName: "node-a", WireGuardInterfacePrefix: "wg"},
		pubKey: "pub-a",
		state: &wireGuardState{
			siteName:     "site-a",
			nodePodCIDRs: []string{"10.244.1.0/24"},
		},
	})

	full := health.getStatusSnapshot()
	if len(full.NodeErrors) != 1 || full.NodeErrors[0].Type != configPodCIDRGuard {
		t.Fatalf("guard lost during full-server handoff: %#v", full.NodeErrors)
	}

	health.setCNIReady("cbr0", []string{"10.244.1.0/24"})

	recovered := health.getStatusSnapshot()
	if len(recovered.NodeErrors) != 0 {
		t.Fatalf("guard not cleared from full snapshot: %#v", recovered.NodeErrors)
	}

	delta, err := computeStatusDelta(full, recovered)
	if err != nil {
		t.Fatalf("compute recovery delta: %v", err)
	}

	if _, ok := delta["nodeErrors"]; !ok {
		t.Fatalf("recovery delta does not clear nodeErrors: %#v", delta)
	}
}

func TestManagedCNIWriteRequiredRetriesBlockedStateWithoutMTUChange(t *testing.T) {
	health := &nodeHealthState{}
	health.beginManagedCNI("cbr0")
	health.setCNIBlocked("blocked")

	if !managedCNIWriteRequired(false, health) {
		t.Fatal("blocked CNI must be retried even when MTU is unchanged")
	}

	health.setCNIReady("cbr0", []string{"10.244.1.0/24"})

	if managedCNIWriteRequired(false, health) {
		t.Fatal("ready CNI should not be rewritten when MTU is unchanged")
	}

	if !managedCNIWriteRequired(true, health) {
		t.Fatal("MTU changes must still trigger guarded CNI rewrites")
	}
}
