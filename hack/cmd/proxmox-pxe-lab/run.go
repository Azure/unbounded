package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultBMCSecretName = "bmc-passwords"
	defaultPXEImage      = "ghcr.io/azure/host-ubuntu2404:v0.0.13"
	remoteInventoryPath  = "/root/stretch-pxe-inventory.yaml"
)

type Runner struct {
	Exec Executor
}

type preflightConfig struct {
	KubeconfigPath string
	ProxmoxHost    string
	PXEImage       string
	BMCSecretKey   string
	SkipProxmox    bool
}

type summaryConfig struct {
	RunSummaryOut string
}

type RunSummary struct {
	Mode   string   `yaml:"mode"`
	Result string   `yaml:"result"`
	Phase  string   `yaml:"phase,omitempty"`
	Site   string   `yaml:"site,omitempty"`
	Nodes  []string `yaml:"nodes,omitempty"`
}

func WriteSummary(path string, summary RunSummary) error {
	data, err := yaml.Marshal(summary)
	if err != nil {
		return err
	}
	if err := ensureParentDir(path); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func ensureParentDir(path string) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func resolveAPIServerURL(ctx context.Context, exec Executor, kubeconfigPath string) (string, error) {
	return exec.Output(ctx, "kubectl", "--kubeconfig", kubeconfigPath, "config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}")
}

func startMetalman(ctx context.Context, exec Executor, site, proxmoxHost, apiServerURL string) error {
	sshArgs := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"root@" + proxmoxHost,
		"sh -lc \"pkill -f '^/root/metalman serve-pxe --site " + site +
			"( |$)' >/dev/null 2>&1 || true; nohup env KUBECONFIG=/root/.kube/config METALMAN_APISERVER_URL=" + apiServerURL + " /root/metalman serve-pxe --site " + site +
			" --dhcp-interface vmbr0 --bind-address " + proxmoxHost +
			" --serve-url http://" + proxmoxHost +
			":8880 > /root/metalman-proxmox-e2e.log 2>&1 </dev/null &\"",
	}

	return exec.Run(ctx, "ssh", sshArgs...)
}

func runCommandWithExecutor(ctx context.Context, cmd Command, exec Executor) error {
	if exec == nil {
		exec = realExecutor{}
	}

	switch cmd.Name {
	case "setup":
		if cmd.Setup == nil {
			return fmt.Errorf("setup config is required")
		}
		return runSetup(ctx, *cmd.Setup, exec)
	case "render-machines":
		if cmd.RenderMachines == nil {
			return fmt.Errorf("render-machines config is required")
		}
		return runRenderMachines(ctx, *cmd.RenderMachines)
	case "verify-ready":
		if cmd.VerifyReady == nil {
			return fmt.Errorf("verify-ready config is required")
		}
		return runVerifyReady(ctx, *cmd.VerifyReady, exec)
	case "reset":
		if cmd.Reset == nil {
			return fmt.Errorf("reset config is required")
		}
		return runReset(ctx, *cmd.Reset, exec)
	default:
		return fmt.Errorf("unsupported command %q", cmd.Name)
	}
}

func runSetup(ctx context.Context, cfg SetupConfig, exec Executor) (runErr error) {
	pxeImage := cfg.PXEImage
	if pxeImage == "" {
		pxeImage = defaultPXEImage
	}

	summary := RunSummary{
		Mode:   "setup",
		Result: "failure",
		Phase:  "preflight",
		Site:   cfg.Site,
	}
	defer func() {
		runErr = writeSummaryOutput(cfg.RunSummaryOut, summary, runErr)
	}()

	if err := RunSetupPreflight(ctx, cfg, exec); err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	if cfg.StartMetalman {
		summary.Phase = "start-metalman"
		apiServerURL, err := resolveAPIServerURL(ctx, exec, cfg.KubeconfigPath)
		if err != nil {
			return fmt.Errorf("resolve API server URL: %w", err)
		}
		if err := startMetalman(ctx, exec, cfg.Site, cfg.ProxmoxHost, apiServerURL); err != nil {
			return fmt.Errorf("start metalman: %w", err)
		}
	}

	if cfg.ProvisionFresh {
		summary.Phase = "provision"
		sshArgs := []string{
			"-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=no",
			"root@" + cfg.ProxmoxHost,
			fmt.Sprintf("/root/create-stretch-pxe-vms.sh %d", cfg.NodeCount),
		}
		if err := exec.Run(ctx, "ssh", sshArgs...); err != nil {
			return fmt.Errorf("provision: %w", err)
		}
	}

	summary.Phase = "retrieve-inventory"
	if err := ensureParentDir(cfg.InventoryOut); err != nil {
		return fmt.Errorf("prepare inventory path: %w", err)
	}
	if err := exec.Run(ctx, "scp",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"root@"+cfg.ProxmoxHost+":"+remoteInventoryPath,
		cfg.InventoryOut,
	); err != nil {
		return fmt.Errorf("retrieve inventory: %w", err)
	}

	inventory, err := ReadInventoryFile(cfg.InventoryOut)
	if err != nil {
		return fmt.Errorf("read inventory: %w", err)
	}
	if len(inventory.VMs) != cfg.NodeCount {
		return fmt.Errorf("inventory VM count %d does not match node-count %d", len(inventory.VMs), cfg.NodeCount)
	}
	summary.Nodes = inventoryNodeNames(inventory)
	if cfg.BMCSecretKey == "" {
		if err := validateBMCSecretKeys(ctx, preflightConfig{KubeconfigPath: cfg.KubeconfigPath}, exec, summary.Nodes); err != nil {
			return err
		}
	}
	if err := WriteInventoryFile(cfg.InventoryOut, inventory); err != nil {
		return fmt.Errorf("write inventory: %w", err)
	}

	summary.Phase = "write-artifacts"
	initialCounter := 0
	if cfg.ProvisionFresh {
		initialCounter = 1
	}
	env := EnvironmentFile{
		Site:                 cfg.Site,
		ProxmoxHost:          cfg.ProxmoxHost,
		KubeconfigPath:       cfg.KubeconfigPath,
		PXEImage:             pxeImage,
		BootstrapTokenName:   cfg.BootstrapToken,
		InitialRebootCounter: initialCounter,
		InitialRepaveCounter: initialCounter,
		Redfish: RedfishDefaults{
			URL:             "https://" + cfg.ProxmoxHost + ":8000",
			Username:        "root@pam",
			SecretName:      defaultBMCSecretName,
			SecretNamespace: "unbounded-kube",
			SecretKey:       cfg.BMCSecretKey,
		},
		Network: NetworkDefaults{
			SubnetMask: "255.255.255.0",
			Gateway:    "10.10.100.1",
			DNS:        []string{"1.1.1.1"},
		},
		Artifacts: ArtifactPaths{
			InventoryPath:  cfg.InventoryOut,
			RunSummaryPath: cfg.RunSummaryOut,
		},
	}
	if err := WriteEnvironmentFile(cfg.EnvOut, env); err != nil {
		return fmt.Errorf("write environment: %w", err)
	}

	summary.Result = "success"
	summary.Phase = "complete"
	return nil
}

func runRenderMachines(_ context.Context, cfg RenderMachinesConfig) error {
	env, err := ReadEnvironmentFile(cfg.EnvPath)
	if err != nil {
		return fmt.Errorf("read environment: %w", err)
	}
	inv, err := ReadInventoryFile(cfg.InventoryPath)
	if err != nil {
		return fmt.Errorf("read inventory: %w", err)
	}
	input := MachineRenderInputFromEnvironment(env, cfg)
	if err := ValidateMachineRenderInput(input); err != nil {
		return fmt.Errorf("render-machines input validation: %w", err)
	}
	manifest, err := RenderMachines(inv, input)
	if err != nil {
		return fmt.Errorf("render machines: %w", err)
	}
	if err := ensureParentDir(cfg.OutputPath); err != nil {
		return fmt.Errorf("prepare machine manifest path: %w", err)
	}
	if err := os.WriteFile(cfg.OutputPath, []byte(manifest), 0o644); err != nil {
		return fmt.Errorf("write machine manifest: %w", err)
	}
	return nil
}

func runVerifyReady(ctx context.Context, cfg VerifyReadyConfig, exec Executor) (runErr error) {
	summary := RunSummary{Mode: "verify-ready", Result: "failure", Phase: "verify-ready"}
	defer func() {
		runErr = writeTerminalSummary(summaryConfig{RunSummaryOut: cfg.RunSummaryOut}, summary, runErr)
	}()

	inv, err := ReadInventoryFile(cfg.InventoryPath)
	if err != nil {
		return fmt.Errorf("read inventory: %w", err)
	}
	if len(inv.VMs) != cfg.NodeCount {
		return fmt.Errorf("inventory VM count %d does not match node-count %d", len(inv.VMs), cfg.NodeCount)
	}
	summary.Nodes = inventoryNodeNames(inv)
	for _, vm := range inv.VMs {
		if err := WaitForNodeReady(ctx, exec, cfg.KubeconfigPath, vm.Name); err != nil {
			return err
		}
	}
	summary.Result = "success"
	summary.Phase = "complete"
	return nil
}

func runReset(ctx context.Context, cfg ResetConfig, exec Executor) error {
	if cfg.InventoryPath != "" && len(cfg.NodeNames) > 0 {
		return fmt.Errorf("inventory and explicit node names cannot be used together")
	}

	nodeNames := append([]string(nil), cfg.NodeNames...)
	vmIDs := []int{}

	if cfg.InventoryPath != "" {
		inv, err := ReadInventoryFile(cfg.InventoryPath)
		if err != nil {
			return fmt.Errorf("read inventory: %w", err)
		}
		nodeNames = append(nodeNames, inventoryNodeNames(inv)...)
		vmIDs = append(vmIDs, inventoryVMIDs(inv)...)
	}

	nodeNames = uniqueStrings(nodeNames)
	if len(nodeNames) == 0 {
		return fmt.Errorf("reset requires at least one node target")
	}

	machineArgs := append([]string{"--kubeconfig", cfg.KubeconfigPath, "delete", "machine"}, nodeNames...)
	machineArgs = append(machineArgs, "--ignore-not-found")
	if err := exec.Run(ctx, "kubectl", machineArgs...); err != nil {
		return fmt.Errorf("delete machines: %w", err)
	}

	nodeArgs := append([]string{"--kubeconfig", cfg.KubeconfigPath, "delete", "node"}, nodeNames...)
	nodeArgs = append(nodeArgs, "--ignore-not-found")
	if err := exec.Run(ctx, "kubectl", nodeArgs...); err != nil {
		return fmt.Errorf("delete nodes: %w", err)
	}

	if cfg.DestroyVMs && len(vmIDs) > 0 {
		cleanup := proxmoxDestroyCommand(vmIDs)
		if err := exec.Run(ctx,
			"ssh",
			"-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=no",
			"root@"+cfg.ProxmoxHost,
			cleanup,
		); err != nil {
			return fmt.Errorf("destroy VMs: %w", err)
		}
	}

	return nil
}

func inventoryNodeNames(inventory Inventory) []string {
	nodes := make([]string, 0, len(inventory.VMs))
	for _, vm := range inventory.VMs {
		nodes = append(nodes, vm.Name)
	}
	return nodes
}

func inventoryVMIDs(inventory Inventory) []int {
	vmIDs := make([]int, 0, len(inventory.VMs))
	for _, vm := range inventory.VMs {
		vmIDs = append(vmIDs, vm.VMID)
	}
	return vmIDs
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func proxmoxDestroyCommand(vmIDs []int) string {
	parts := make([]string, 0, len(vmIDs)*2+1)
	for _, vmID := range vmIDs {
		parts = append(parts, fmt.Sprintf("qm stop %d || true", vmID))
	}
	parts = append(parts, "sleep 3")
	for _, vmID := range vmIDs {
		parts = append(parts, fmt.Sprintf("qm destroy %d --destroy-unreferenced-disks 1 --purge 1 || true", vmID))
	}
	return strings.Join(parts, "; ")
}

func writeTerminalSummary(cfg summaryConfig, summary RunSummary, runErr error) error {
	return writeSummaryOutput(cfg.RunSummaryOut, summary, runErr)
}

func writeSummaryOutput(path string, summary RunSummary, runErr error) error {
	if path == "" {
		return runErr
	}
	if err := WriteSummary(path, summary); err != nil {
		if runErr != nil {
			return fmt.Errorf("%w; write summary: %v", runErr, err)
		}
		return fmt.Errorf("write summary: %w", err)
	}
	return runErr
}
