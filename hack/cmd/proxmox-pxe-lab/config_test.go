package main

import (
	"flag"
	"os"
	"strings"
	"testing"
)

func validSetupConfig() SetupConfig {
	return SetupConfig{
		CommonConfig: CommonConfig{
			KubeconfigPath: "/root/.kube/config",
			Site:           "proxmox-lab",
			NodeCount:      2,
		},
		ProxmoxHost:    "10.10.100.2",
		InventoryOut:   "tmp/inventory.yaml",
		EnvOut:         "tmp/env.yaml",
		BootstrapToken: "bootstrap-token-kepqb3",
	}
}

func TestConfigValidateSetupRequiresRequiredFields(t *testing.T) {
	cfg := SetupConfig{}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestConfigValidateSetupAcceptsValidConfig(t *testing.T) {
	cfg := validSetupConfig()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestConfigValidateSetupRejectsInvalidSiteValues(t *testing.T) {
	tests := []string{
		"Proxmox-lab",
		"proxmox_lab",
		"proxmox.lab",
		"-proxmox-lab",
		"proxmox-lab-",
	}

	for _, site := range tests {
		t.Run(site, func(t *testing.T) {
			cfg := validSetupConfig()
			cfg.Site = site

			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected validation error for site %q", site)
			}
		})
	}
}

func TestConfigValidateSetupRequiresBootstrapToken(t *testing.T) {
	cfg := validSetupConfig()
	cfg.BootstrapToken = ""

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestConfigValidateRenderMachinesRequiresPaths(t *testing.T) {
	cfg := RenderMachinesConfig{}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestConfigValidateRenderMachinesAcceptsValidConfig(t *testing.T) {
	cfg := RenderMachinesConfig{
		InventoryPath: "tmp/inventory.yaml",
		EnvPath:       "tmp/env.yaml",
		OutputPath:    "tmp/machines.yaml",
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestConfigValidateVerifyReadyRequiresKubeconfigInventoryAndNodeCount(t *testing.T) {
	cfg := VerifyReadyConfig{}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestConfigValidateVerifyReadyAcceptsValidConfig(t *testing.T) {
	cfg := VerifyReadyConfig{
		KubeconfigPath: "/root/.kube/config",
		InventoryPath:  "tmp/inventory.yaml",
		NodeCount:      2,
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestResetConfigRequiresHostAndAtLeastOneTarget(t *testing.T) {
	cfg := ResetConfig{KubeconfigPath: "/root/.kube/config"}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestResetConfigRejectsMixedInventoryAndExplicitNodeNames(t *testing.T) {
	cfg := ResetConfig{
		KubeconfigPath: "/root/.kube/config",
		ProxmoxHost:    "10.10.100.2",
		InventoryPath:  "tmp/inventory.yaml",
		NodeNames:      []string{"stretch-pxe-9"},
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestParseCommandParsesSetupSubcommand(t *testing.T) {
	originalCommandLine := flag.CommandLine
	originalArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = originalCommandLine
		os.Args = originalArgs
	})

	flag.CommandLine = flag.NewFlagSet("proxmox-pxe-lab", flag.ContinueOnError)
	os.Args = []string{
		"proxmox-pxe-lab",
		"setup",
		"--site", "proxmox-lab",
		"--node-count", "3",
		"--proxmox-host", "10.10.100.2",
		"--kubeconfig", "/root/.kube/config",
		"--inventory-out", "tmp/inventory.yaml",
		"--env-out", "tmp/env.yaml",
		"--bootstrap-token", "bootstrap-token-abc123",
		"--pxe-image", "ghcr.io/example/custom:v1",
		"--bmc-secret-key", "stretch-pxe-0",
		"--run-summary-out", "tmp/summary.yaml",
		"--start-metalman",
		"--provision-fresh",
	}

	cmd, err := parseCommand()
	if err != nil {
		t.Fatalf("parseCommand() error = %v", err)
	}
	if cmd.Name != "setup" {
		t.Fatalf("cmd.Name = %q, want %q", cmd.Name, "setup")
	}
	if cmd.Setup == nil {
		t.Fatal("expected setup config")
	}
	if cmd.Setup.Site != "proxmox-lab" {
		t.Fatalf("cmd.Setup.Site = %q", cmd.Setup.Site)
	}
	if cmd.Setup.NodeCount != 3 {
		t.Fatalf("cmd.Setup.NodeCount = %d", cmd.Setup.NodeCount)
	}
	if cmd.Setup.ProxmoxHost != "10.10.100.2" {
		t.Fatalf("cmd.Setup.ProxmoxHost = %q", cmd.Setup.ProxmoxHost)
	}
	if cmd.Setup.KubeconfigPath != "/root/.kube/config" {
		t.Fatalf("cmd.Setup.KubeconfigPath = %q", cmd.Setup.KubeconfigPath)
	}
	if cmd.Setup.InventoryOut != "tmp/inventory.yaml" {
		t.Fatalf("cmd.Setup.InventoryOut = %q", cmd.Setup.InventoryOut)
	}
	if cmd.Setup.EnvOut != "tmp/env.yaml" {
		t.Fatalf("cmd.Setup.EnvOut = %q", cmd.Setup.EnvOut)
	}
	if cmd.Setup.BootstrapToken != "bootstrap-token-abc123" {
		t.Fatalf("cmd.Setup.BootstrapToken = %q", cmd.Setup.BootstrapToken)
	}
	if cmd.Setup.PXEImage != "ghcr.io/example/custom:v1" {
		t.Fatalf("cmd.Setup.PXEImage = %q", cmd.Setup.PXEImage)
	}
	if cmd.Setup.BMCSecretKey != "stretch-pxe-0" {
		t.Fatalf("cmd.Setup.BMCSecretKey = %q", cmd.Setup.BMCSecretKey)
	}
	if cmd.Setup.RunSummaryOut != "tmp/summary.yaml" {
		t.Fatalf("cmd.Setup.RunSummaryOut = %q", cmd.Setup.RunSummaryOut)
	}
	if !cmd.Setup.StartMetalman {
		t.Fatal("expected cmd.Setup.StartMetalman to be true")
	}
	if !cmd.Setup.ProvisionFresh {
		t.Fatal("expected cmd.Setup.ProvisionFresh to be true")
	}
}

func TestParseCommandParsesRenderMachinesSubcommand(t *testing.T) {
	originalCommandLine := flag.CommandLine
	originalArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = originalCommandLine
		os.Args = originalArgs
	})

	flag.CommandLine = flag.NewFlagSet("proxmox-pxe-lab", flag.ContinueOnError)
	os.Args = []string{
		"proxmox-pxe-lab",
		"render-machines",
		"--inventory", "tmp/inventory.yaml",
		"--env", "tmp/env.yaml",
		"--out", "tmp/machines.yaml",
		"--bootstrap-token", "bootstrap-token-override",
		"--pxe-image", "ghcr.io/example/override:v2",
		"--bmc-secret-key", "shared-key",
	}

	cmd, err := parseCommand()
	if err != nil {
		t.Fatalf("parseCommand() error = %v", err)
	}
	if cmd.Name != "render-machines" {
		t.Fatalf("cmd.Name = %q, want %q", cmd.Name, "render-machines")
	}
	if cmd.RenderMachines == nil {
		t.Fatal("expected render-machines config")
	}
	if cmd.RenderMachines.InventoryPath != "tmp/inventory.yaml" {
		t.Fatalf("cmd.RenderMachines.InventoryPath = %q", cmd.RenderMachines.InventoryPath)
	}
	if cmd.RenderMachines.EnvPath != "tmp/env.yaml" {
		t.Fatalf("cmd.RenderMachines.EnvPath = %q", cmd.RenderMachines.EnvPath)
	}
	if cmd.RenderMachines.OutputPath != "tmp/machines.yaml" {
		t.Fatalf("cmd.RenderMachines.OutputPath = %q", cmd.RenderMachines.OutputPath)
	}
	if cmd.RenderMachines.BootstrapToken != "bootstrap-token-override" {
		t.Fatalf("cmd.RenderMachines.BootstrapToken = %q", cmd.RenderMachines.BootstrapToken)
	}
	if cmd.RenderMachines.PXEImage != "ghcr.io/example/override:v2" {
		t.Fatalf("cmd.RenderMachines.PXEImage = %q", cmd.RenderMachines.PXEImage)
	}
	if cmd.RenderMachines.BMCSecretKey != "shared-key" {
		t.Fatalf("cmd.RenderMachines.BMCSecretKey = %q", cmd.RenderMachines.BMCSecretKey)
	}
}

func TestParseCommandParsesVerifyReadySubcommand(t *testing.T) {
	originalCommandLine := flag.CommandLine
	originalArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = originalCommandLine
		os.Args = originalArgs
	})

	flag.CommandLine = flag.NewFlagSet("proxmox-pxe-lab", flag.ContinueOnError)
	os.Args = []string{
		"proxmox-pxe-lab",
		"verify-ready",
		"--kubeconfig", "/root/.kube/config",
		"--inventory", "tmp/inventory.yaml",
		"--node-count", "3",
		"--run-summary-out", "tmp/summary.yaml",
	}

	cmd, err := parseCommand()
	if err != nil {
		t.Fatalf("parseCommand() error = %v", err)
	}
	if cmd.Name != "verify-ready" {
		t.Fatalf("cmd.Name = %q, want %q", cmd.Name, "verify-ready")
	}
	if cmd.VerifyReady == nil {
		t.Fatal("expected verify-ready config")
	}
	if cmd.VerifyReady.KubeconfigPath != "/root/.kube/config" {
		t.Fatalf("cmd.VerifyReady.KubeconfigPath = %q", cmd.VerifyReady.KubeconfigPath)
	}
	if cmd.VerifyReady.InventoryPath != "tmp/inventory.yaml" {
		t.Fatalf("cmd.VerifyReady.InventoryPath = %q", cmd.VerifyReady.InventoryPath)
	}
	if cmd.VerifyReady.NodeCount != 3 {
		t.Fatalf("cmd.VerifyReady.NodeCount = %d", cmd.VerifyReady.NodeCount)
	}
	if cmd.VerifyReady.RunSummaryOut != "tmp/summary.yaml" {
		t.Fatalf("cmd.VerifyReady.RunSummaryOut = %q", cmd.VerifyReady.RunSummaryOut)
	}
}

func TestParseCommandParsesResetSubcommand(t *testing.T) {
	originalCommandLine := flag.CommandLine
	originalArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = originalCommandLine
		os.Args = originalArgs
	})

	flag.CommandLine = flag.NewFlagSet("proxmox-pxe-lab", flag.ContinueOnError)
	os.Args = []string{
		"proxmox-pxe-lab",
		"reset",
		"--kubeconfig", "/root/.kube/config",
		"--proxmox-host", "10.10.100.2",
		"--node-name", "stretch-pxe-9",
		"--node-name", "stretch-pxe-10",
		"--destroy-vms",
	}

	cmd, err := parseCommand()
	if err != nil {
		t.Fatalf("parseCommand() error = %v", err)
	}
	if cmd.Name != "reset" {
		t.Fatalf("cmd.Name = %q, want %q", cmd.Name, "reset")
	}
	if cmd.Reset == nil {
		t.Fatal("expected reset config")
	}
	if cmd.Reset.KubeconfigPath != "/root/.kube/config" {
		t.Fatalf("cmd.Reset.KubeconfigPath = %q", cmd.Reset.KubeconfigPath)
	}
	if cmd.Reset.ProxmoxHost != "10.10.100.2" {
		t.Fatalf("cmd.Reset.ProxmoxHost = %q", cmd.Reset.ProxmoxHost)
	}
	if got, want := cmd.Reset.NodeNames, []string{"stretch-pxe-9", "stretch-pxe-10"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("cmd.Reset.NodeNames = %#v", got)
	}
	if !cmd.Reset.DestroyVMs {
		t.Fatal("expected cmd.Reset.DestroyVMs to be true")
	}
}

func TestParseCommandResetRejectsLegacySiteFlag(t *testing.T) {
	originalCommandLine := flag.CommandLine
	originalArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = originalCommandLine
		os.Args = originalArgs
	})

	flag.CommandLine = flag.NewFlagSet("proxmox-pxe-lab", flag.ContinueOnError)
	os.Args = []string{
		"proxmox-pxe-lab",
		"reset",
		"--kubeconfig", "/root/.kube/config",
		"--proxmox-host", "10.10.100.2",
		"--site", "proxmox-lab",
		"--node-name", "stretch-pxe-9",
	}

	_, err := parseCommand()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseCommandRejectsUnknownSubcommand(t *testing.T) {
	originalCommandLine := flag.CommandLine
	originalArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = originalCommandLine
		os.Args = originalArgs
	})

	flag.CommandLine = flag.NewFlagSet("proxmox-pxe-lab", flag.ContinueOnError)
	os.Args = []string{"proxmox-pxe-lab", "bogus"}

	_, err := parseCommand()
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestParseCommandRejectsLegacyModeEntrypoint(t *testing.T) {
	originalCommandLine := flag.CommandLine
	originalArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = originalCommandLine
		os.Args = originalArgs
	})

	flag.CommandLine = flag.NewFlagSet("proxmox-pxe-lab", flag.ContinueOnError)
	os.Args = []string{"proxmox-pxe-lab", "--mode", "fresh"}

	_, err := parseCommand()
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got != `unknown subcommand "--mode"` {
		t.Fatalf("parseCommand() error = %q", got)
	}
}

func TestParseCommandWithoutSubcommandListsSupportedCommands(t *testing.T) {
	originalCommandLine := flag.CommandLine
	originalArgs := os.Args
	t.Cleanup(func() {
		flag.CommandLine = originalCommandLine
		os.Args = originalArgs
	})

	flag.CommandLine = flag.NewFlagSet("proxmox-pxe-lab", flag.ContinueOnError)
	os.Args = []string{"proxmox-pxe-lab"}

	_, err := parseCommand()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"setup", "render-machines", "verify-ready", "reset"} {
		if got := err.Error(); !strings.Contains(got, want) {
			t.Fatalf("expected error to mention %q, got %q", want, got)
		}
	}
	for _, forbidden := range []string{"fresh", "repave", "--mode"} {
		if got := err.Error(); strings.Contains(got, forbidden) {
			t.Fatalf("expected error to avoid legacy mode hint %q, got %q", forbidden, got)
		}
	}
}
