package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWriteSummaryIncludesModeAndResult(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.yaml")
	if err := WriteSummary(path, RunSummary{Mode: "fresh", Result: "success"}); err != nil {
		t.Fatalf("WriteSummary() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), "mode: fresh") || !strings.Contains(string(data), "result: success") {
		t.Fatalf("unexpected summary:\n%s", string(data))
	}
}

func TestWriteSummaryCreatesParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tmp", "nested", "summary.yaml")

	if err := WriteSummary(path, RunSummary{Mode: "fresh", Result: "success"}); err != nil {
		t.Fatalf("WriteSummary() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), "result: success") {
		t.Fatalf("unexpected summary:\n%s", string(data))
	}
}

type scriptedExecutor struct {
	calls       []string
	responses   map[string]error
	sideEffects map[string]func() error
	outputs     map[string][]scriptedOutput
	outputIndex map[string]int
}

type scriptedOutput struct {
	stdout string
	err    error
}

func (s *scriptedExecutor) Run(_ context.Context, name string, args ...string) error {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	s.calls = append(s.calls, key)
	if sideEffect := s.sideEffects[key]; sideEffect != nil {
		if err := sideEffect(); err != nil {
			return err
		}
	}
	if s.responses == nil {
		return fmt.Errorf("unexpected command: %s", key)
	}
	resp, ok := s.responses[key]
	if !ok {
		return fmt.Errorf("unexpected command: %s", key)
	}
	return resp
}

func (s *scriptedExecutor) Output(_ context.Context, name string, args ...string) (string, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	s.calls = append(s.calls, key)
	if sideEffect := s.sideEffects[key]; sideEffect != nil {
		if err := sideEffect(); err != nil {
			return "", err
		}
	}
	if s.outputs == nil {
		return "", fmt.Errorf("unexpected command: %s", key)
	}
	results, ok := s.outputs[key]
	if !ok {
		return "", fmt.Errorf("unexpected command: %s", key)
	}
	if s.outputIndex == nil {
		s.outputIndex = map[string]int{}
	}
	idx := s.outputIndex[key]
	s.outputIndex[key] = idx + 1
	if idx >= len(results) {
		last := results[len(results)-1]
		return last.stdout, last.err
	}
	result := results[idx]
	return result.stdout, result.err
}

func TestRunSetupWritesInventoryEnvAndSummary(t *testing.T) {
	useFastWaitPolling(t)

	tempDir := t.TempDir()
	cmd := Command{
		Name: "setup",
		Setup: &SetupConfig{
			CommonConfig: CommonConfig{
				Site:           "proxmox-lab",
				NodeCount:      2,
				KubeconfigPath: "/root/.kube/config",
				RunSummaryOut:  filepath.Join(tempDir, "summary.yaml"),
			},
			ProxmoxHost:    "10.10.100.2",
			InventoryOut:   filepath.Join(tempDir, "inventory.yaml"),
			EnvOut:         filepath.Join(tempDir, "env.yaml"),
			PXEImage:       "ghcr.io/example/custom:v1",
			BootstrapToken: "bootstrap-token-np1tzg",
			BMCSecretKey:   "stretch-pxe-0",
			ProvisionFresh: true,
		},
	}

	scpCommand := "scp -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2:" + remoteInventoryPath + " " + cmd.Setup.InventoryOut
	exec := &scriptedExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config cluster-info":                                                                                             nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 systemctl is-active proxmox-redfish":                                            nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 test -f /root/.kube/config":                                                     nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; test -n \"${GHCR_PAT:-}\"'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'source ~/.bashrc >/dev/null 2>&1 || true; mkdir -p /root/.docker && printf %s \"$GHCR_PAT\" | skopeo login --compat-auth-file /root/.docker/config.json --username x-access-token --password-stdin ghcr.io'": nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 bash -lc 'skopeo inspect --authfile /root/.docker/config.json docker://ghcr.io/example/custom:v1 >/dev/null'":                                                                                                          nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 /root/create-stretch-pxe-vms.sh 2":                                                                                                                                                                                     nil,
		scpCommand: nil,
	}, sideEffects: map[string]func() error{
		scpCommand: func() error {
			return os.WriteFile(cmd.Setup.InventoryOut, currentProxmoxInventoryYAML(2), 0o644)
		},
	}, outputs: bmcSecretOutputs("stretch-pxe-0")}

	if err := runCommandWithExecutor(context.Background(), cmd, exec); err != nil {
		t.Fatalf("runCommandWithExecutor() error = %v", err)
	}

	if _, err := os.Stat(cmd.Setup.InventoryOut); err != nil {
		t.Fatalf("inventory artifact missing: %v", err)
	}
	inventoryData, err := os.ReadFile(cmd.Setup.InventoryOut)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", cmd.Setup.InventoryOut, err)
	}
	for _, want := range []string{"mac: 02:10:10:64:00:00", "ipv4: 10.10.100.50", "mac: 02:10:10:64:00:01", "ipv4: 10.10.100.51"} {
		if !strings.Contains(string(inventoryData), want) {
			t.Fatalf("expected normalized inventory to contain %q, got:\n%s", want, string(inventoryData))
		}
	}
	inv, err := ReadInventoryFile(cmd.Setup.InventoryOut)
	if err != nil {
		t.Fatalf("ReadInventoryFile() error = %v", err)
	}
	if len(inv.VMs) != 2 {
		t.Fatalf("len(inv.VMs) = %d", len(inv.VMs))
	}

	env, err := ReadEnvironmentFile(cmd.Setup.EnvOut)
	if err != nil {
		t.Fatalf("ReadEnvironmentFile() error = %v", err)
	}
	if env.Redfish.URL != "https://10.10.100.2:8000" {
		t.Fatalf("env.Redfish.URL = %q", env.Redfish.URL)
	}
	if env.BootstrapTokenName != "bootstrap-token-np1tzg" {
		t.Fatalf("env.BootstrapTokenName = %q", env.BootstrapTokenName)
	}
	if env.InitialRebootCounter != 1 {
		t.Fatalf("env.InitialRebootCounter = %d", env.InitialRebootCounter)
	}
	if env.InitialRepaveCounter != 1 {
		t.Fatalf("env.InitialRepaveCounter = %d", env.InitialRepaveCounter)
	}
	if env.Artifacts.InventoryPath != cmd.Setup.InventoryOut {
		t.Fatalf("env.Artifacts.InventoryPath = %q", env.Artifacts.InventoryPath)
	}
	if env.Artifacts.MachineManifestPath != "" {
		t.Fatalf("env.Artifacts.MachineManifestPath = %q", env.Artifacts.MachineManifestPath)
	}

	summaryData, err := os.ReadFile(cmd.Setup.RunSummaryOut)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", cmd.Setup.RunSummaryOut, err)
	}
	summary := string(summaryData)
	for _, want := range []string{"mode: setup", "result: success", "phase: complete", "- stretch-pxe-0", "- stretch-pxe-1"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("expected summary to contain %q, got:\n%s", want, summary)
		}
	}
}

func TestRunRenderMachinesUsesInventoryAndEnvironmentArtifacts(t *testing.T) {
	tempDir := t.TempDir()
	invPath := filepath.Join(tempDir, "inventory.yaml")
	envPath := filepath.Join(tempDir, "env.yaml")
	outPath := filepath.Join(tempDir, "machines.yaml")

	if err := os.WriteFile(invPath, inventoryYAML(2), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := WriteEnvironmentFile(envPath, EnvironmentFile{
		Site:                 "proxmox-lab",
		PXEImage:             "ghcr.io/example/default:v1",
		BootstrapTokenName:   "bootstrap-token-default",
		InitialRebootCounter: 1,
		InitialRepaveCounter: 1,
		Redfish: RedfishDefaults{
			URL:             "https://10.10.100.2:8000",
			Username:        "root@pam",
			SecretName:      "bmc-passwords",
			SecretNamespace: "unbounded-kube",
		},
		Network: NetworkDefaults{
			SubnetMask: "255.255.255.0",
			Gateway:    "10.10.100.1",
			DNS:        []string{"1.1.1.1"},
		},
		Artifacts: ArtifactPaths{
			MachineManifestPath: outPath,
		},
	}); err != nil {
		t.Fatalf("WriteEnvironmentFile() error = %v", err)
	}

	cmd := Command{
		Name: "render-machines",
		RenderMachines: &RenderMachinesConfig{
			InventoryPath: invPath,
			EnvPath:       envPath,
			OutputPath:    outPath,
			PXEImage:      "ghcr.io/example/override:v2",
		},
	}

	if err := runCommandWithExecutor(context.Background(), cmd, &scriptedExecutor{}); err != nil {
		t.Fatalf("runCommandWithExecutor() error = %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	manifest := string(data)
	for _, want := range []string{"kind: Machine", "image: ghcr.io/example/override:v2", "rebootCounter: 1", "repaveCounter: 1", "name: stretch-pxe-0", "name: stretch-pxe-1"} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("expected manifest to contain %q, got:\n%s", want, manifest)
		}
	}
}

func TestRunRenderMachinesFailsForMissingRequiredEnvironmentInputs(t *testing.T) {
	tempDir := t.TempDir()
	invPath := filepath.Join(tempDir, "inventory.yaml")
	envPath := filepath.Join(tempDir, "env.yaml")
	outPath := filepath.Join(tempDir, "machines.yaml")

	if err := os.WriteFile(invPath, inventoryYAML(2), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := WriteEnvironmentFile(envPath, EnvironmentFile{
		Site:               "proxmox-lab",
		BootstrapTokenName: "bootstrap-token-default",
		Redfish: RedfishDefaults{
			Username:        "root@pam",
			SecretName:      "bmc-passwords",
			SecretNamespace: "unbounded-kube",
		},
		Network: NetworkDefaults{
			SubnetMask: "255.255.255.0",
			Gateway:    "10.10.100.1",
			DNS:        []string{"1.1.1.1"},
		},
	}); err != nil {
		t.Fatalf("WriteEnvironmentFile() error = %v", err)
	}

	err := runCommandWithExecutor(context.Background(), Command{
		Name: "render-machines",
		RenderMachines: &RenderMachinesConfig{
			InventoryPath: invPath,
			EnvPath:       envPath,
			OutputPath:    outPath,
		},
	}, &scriptedExecutor{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "render-machines input validation:") {
		t.Fatalf("unexpected error = %v", err)
	}
	if !strings.Contains(err.Error(), "pxeImage is required") {
		t.Fatalf("unexpected error = %v", err)
	}
	if !strings.Contains(err.Error(), "redfish.url is required") {
		t.Fatalf("unexpected error = %v", err)
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected no output manifest, stat error = %v", statErr)
	}
}

func TestRunVerifyReadyCommandRefreshesSummaryFromExistingInventory(t *testing.T) {
	useFastWaitPolling(t)
	tempDir := t.TempDir()
	invPath := filepath.Join(tempDir, "inventory.yaml")
	summaryPath := filepath.Join(tempDir, "summary.yaml")
	if err := os.WriteFile(invPath, inventoryYAML(2), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cmd := Command{
		Name: "verify-ready",
		VerifyReady: &VerifyReadyConfig{
			KubeconfigPath: "/root/.kube/config",
			InventoryPath:  invPath,
			NodeCount:      2,
			RunSummaryOut:  summaryPath,
		},
	}

	exec := &scriptedExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-0 --timeout=30s": nil,
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-1 --timeout=30s": nil,
	}}

	if err := runCommandWithExecutor(context.Background(), cmd, exec); err != nil {
		t.Fatalf("runCommandWithExecutor() error = %v", err)
	}

	data, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, want := range []string{"result: success", "phase: complete", "- stretch-pxe-0", "- stretch-pxe-1"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("expected summary to contain %q, got:\n%s", want, string(data))
		}
	}
}

func TestRunVerifyReadyCommandWritesFailureSummaryWhenNodeNeverBecomesReady(t *testing.T) {
	useFastWaitPolling(t)
	tempDir := t.TempDir()
	invPath := filepath.Join(tempDir, "inventory.yaml")
	summaryPath := filepath.Join(tempDir, "summary.yaml")
	if err := os.WriteFile(invPath, inventoryYAML(2), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cmd := Command{
		Name: "verify-ready",
		VerifyReady: &VerifyReadyConfig{
			KubeconfigPath: "/root/.kube/config",
			InventoryPath:  invPath,
			NodeCount:      2,
			RunSummaryOut:  summaryPath,
		},
	}

	exec := &scriptedExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-0 --timeout=30s": fmt.Errorf("node not ready"),
	}}

	err := runCommandWithExecutor(context.Background(), cmd, exec)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "node stretch-pxe-0 did not become Ready before timeout") {
		t.Fatalf("unexpected error = %v", err)
	}

	data, readErr := os.ReadFile(summaryPath)
	if readErr != nil {
		t.Fatalf("ReadFile() error = %v", readErr)
	}
	for _, want := range []string{"result: failure", "phase: verify-ready", "- stretch-pxe-0", "- stretch-pxe-1"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("expected summary to contain %q, got:\n%s", want, string(data))
		}
	}

	if len(exec.calls) == 0 {
		t.Fatal("expected readiness checks")
	}
	for _, call := range exec.calls {
		if call != "kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-0 --timeout=30s" {
			t.Fatalf("unexpected call %q, calls = %#v", call, exec.calls)
		}
	}
}

func TestRunResetDeletesMachinesNodesAndVMsFromInventory(t *testing.T) {
	tempDir := t.TempDir()
	invPath := filepath.Join(tempDir, "inventory.yaml")
	if err := os.WriteFile(invPath, inventoryYAML(2), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cmd := Command{
		Name: "reset",
		Reset: &ResetConfig{
			KubeconfigPath: "/root/.kube/config",
			ProxmoxHost:    "10.10.100.2",
			InventoryPath:  invPath,
			DestroyVMs:     true,
		},
	}
	exec := &scriptedExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config delete machine stretch-pxe-0 stretch-pxe-1 --ignore-not-found":                                                                                                                                                 nil,
		"kubectl --kubeconfig /root/.kube/config delete node stretch-pxe-0 stretch-pxe-1 --ignore-not-found":                                                                                                                                                    nil,
		"ssh -o BatchMode=yes -o StrictHostKeyChecking=no root@10.10.100.2 qm stop 100 || true; qm stop 101 || true; sleep 3; qm destroy 100 --destroy-unreferenced-disks 1 --purge 1 || true; qm destroy 101 --destroy-unreferenced-disks 1 --purge 1 || true": nil,
	}}

	if err := runCommandWithExecutor(context.Background(), cmd, exec); err != nil {
		t.Fatalf("runCommandWithExecutor() error = %v", err)
	}
}

func TestRunResetDeletesExplicitNodesWithoutInventory(t *testing.T) {
	cmd := Command{
		Name: "reset",
		Reset: &ResetConfig{
			KubeconfigPath: "/root/.kube/config",
			ProxmoxHost:    "10.10.100.2",
			NodeNames:      []string{"stretch-pxe-9", "stretch-pxe-10"},
		},
	}
	exec := &scriptedExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config delete machine stretch-pxe-9 stretch-pxe-10 --ignore-not-found": nil,
		"kubectl --kubeconfig /root/.kube/config delete node stretch-pxe-9 stretch-pxe-10 --ignore-not-found":    nil,
	}}

	if err := runCommandWithExecutor(context.Background(), cmd, exec); err != nil {
		t.Fatalf("runCommandWithExecutor() error = %v", err)
	}

	if !reflect.DeepEqual(exec.calls, []string{
		"kubectl --kubeconfig /root/.kube/config delete machine stretch-pxe-9 stretch-pxe-10 --ignore-not-found",
		"kubectl --kubeconfig /root/.kube/config delete node stretch-pxe-9 stretch-pxe-10 --ignore-not-found",
	}) {
		t.Fatalf("calls = %#v", exec.calls)
	}
}

func TestRunResetRejectsMixedInventoryAndExplicitNodeNames(t *testing.T) {
	tempDir := t.TempDir()
	invPath := filepath.Join(tempDir, "inventory.yaml")
	if err := os.WriteFile(invPath, inventoryYAML(2), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cmd := Command{
		Name: "reset",
		Reset: &ResetConfig{
			KubeconfigPath: "/root/.kube/config",
			ProxmoxHost:    "10.10.100.2",
			InventoryPath:  invPath,
			NodeNames:      []string{"stretch-pxe-9"},
		},
	}
	exec := &scriptedExecutor{}

	err := runCommandWithExecutor(context.Background(), cmd, exec)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "inventory and explicit node names cannot be used together") {
		t.Fatalf("unexpected error = %v", err)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("expected no executor calls, got %#v", exec.calls)
	}
}

func TestRunResetDestroyVMsWithoutInventoryDoesNotDestroyVMs(t *testing.T) {
	cmd := Command{
		Name: "reset",
		Reset: &ResetConfig{
			KubeconfigPath: "/root/.kube/config",
			ProxmoxHost:    "10.10.100.2",
			NodeNames:      []string{"stretch-pxe-9"},
			DestroyVMs:     true,
		},
	}
	exec := &scriptedExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config delete machine stretch-pxe-9 --ignore-not-found": nil,
		"kubectl --kubeconfig /root/.kube/config delete node stretch-pxe-9 --ignore-not-found":    nil,
	}}

	if err := runCommandWithExecutor(context.Background(), cmd, exec); err != nil {
		t.Fatalf("runCommandWithExecutor() error = %v", err)
	}

	for _, call := range exec.calls {
		if strings.HasPrefix(call, "ssh ") {
			t.Fatalf("did not expect VM destroy call, calls = %#v", exec.calls)
		}
	}
}

func TestRunCommandWithExecutorRunsVerifyReady(t *testing.T) {
	useFastWaitPolling(t)
	invPath := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(invPath, inventoryYAML(2), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cmd := Command{
		Name: "verify-ready",
		VerifyReady: &VerifyReadyConfig{
			KubeconfigPath: "/root/.kube/config",
			InventoryPath:  invPath,
			NodeCount:      2,
		},
	}

	exec := &scriptedExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-0 --timeout=30s": nil,
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-1 --timeout=30s": nil,
	}}

	if err := runCommandWithExecutor(context.Background(), cmd, exec); err != nil {
		t.Fatalf("runCommandWithExecutor() error = %v", err)
	}

	if !reflect.DeepEqual(exec.calls, []string{
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-0 --timeout=30s",
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-1 --timeout=30s",
	}) {
		t.Fatalf("calls = %#v", exec.calls)
	}
}

func TestRunCommandRunsVerifyReady(t *testing.T) {
	useFastWaitPolling(t)
	tempDir := t.TempDir()
	invPath := filepath.Join(tempDir, "inventory.yaml")
	summaryPath := filepath.Join(tempDir, "summary.yaml")
	if err := os.WriteFile(invPath, inventoryYAML(2), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cmd := Command{
		Name: "verify-ready",
		VerifyReady: &VerifyReadyConfig{
			KubeconfigPath: "/root/.kube/config",
			InventoryPath:  invPath,
			NodeCount:      2,
			RunSummaryOut:  summaryPath,
		},
	}

	exec := &scriptedExecutor{responses: map[string]error{
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-0 --timeout=30s": nil,
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-1 --timeout=30s": nil,
	}}

	original := newRunExecutor
	newRunExecutor = func() Executor { return exec }
	t.Cleanup(func() {
		newRunExecutor = original
	})

	if err := runCommand(cmd); err != nil {
		t.Fatalf("runCommand() error = %v", err)
	}

	data, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, want := range []string{"result: success", "phase: complete", "- stretch-pxe-0", "- stretch-pxe-1"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("expected summary to contain %q, got:\n%s", want, string(data))
		}
	}

	if !reflect.DeepEqual(exec.calls, []string{
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-0 --timeout=30s",
		"kubectl --kubeconfig /root/.kube/config wait --for=condition=Ready node/stretch-pxe-1 --timeout=30s",
	}) {
		t.Fatalf("calls = %#v", exec.calls)
	}
}

func inventoryYAML(nodeCount int) []byte {
	var builder strings.Builder
	builder.WriteString("vms:\n")
	for i := 0; i < nodeCount; i++ {
		fmt.Fprintf(&builder, "  - name: stretch-pxe-%d\n", i)
		fmt.Fprintf(&builder, "    vmid: %d\n", 100+i)
		fmt.Fprintf(&builder, "    mac: 02:10:10:64:00:%02d\n", i)
		fmt.Fprintf(&builder, "    ipv4: 10.10.100.%d\n", 50+i)
	}
	return []byte(builder.String())
}

func inventoryYAMLWithNames(names []string) []byte {
	var builder strings.Builder
	builder.WriteString("vms:\n")
	for i, name := range names {
		fmt.Fprintf(&builder, "  - name: %s\n", name)
		fmt.Fprintf(&builder, "    vmid: %d\n", 100+i)
		fmt.Fprintf(&builder, "    mac: 02:10:10:64:00:%02d\n", i)
		fmt.Fprintf(&builder, "    ipv4: 10.10.100.%d\n", 50+i)
	}
	return []byte(builder.String())
}

func bmcSecretOutputs(keys ...string) map[string][]scriptedOutput {
	outputs := make(map[string][]scriptedOutput, len(keys))
	for _, key := range keys {
		outputs["kubectl --kubeconfig /root/.kube/config -n unbounded-kube get secret bmc-passwords -o jsonpath={.data['"+key+"']}"] = []scriptedOutput{{stdout: "c2VjcmV0"}}
	}
	return outputs
}

func mergeOutputs(maps ...map[string][]scriptedOutput) map[string][]scriptedOutput {
	merged := map[string][]scriptedOutput{}
	for _, current := range maps {
		for key, value := range current {
			merged[key] = value
		}
	}
	return merged
}

func currentProxmoxInventoryYAML(nodeCount int) []byte {
	var builder strings.Builder
	builder.WriteString("vms:\n")
	for i := 0; i < nodeCount; i++ {
		fmt.Fprintf(&builder, "  - name: stretch-pxe-%d\n", i)
		fmt.Fprintf(&builder, "    vmid: %d\n", 100+i)
		fmt.Fprintf(&builder, "    mac: \"%02d\"\n", i)
		builder.WriteString("    bridge: vmbr0\n")
		builder.WriteString("    powerState: stopped\n")
		builder.WriteString("    intendedIPv4: \"\"\n")
	}
	return []byte(builder.String())
}
