package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
)

var sitePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type Command struct {
	Name           string
	Setup          *SetupConfig
	RenderMachines *RenderMachinesConfig
	VerifyReady    *VerifyReadyConfig
	Reset          *ResetConfig
}

type CommonConfig struct {
	KubeconfigPath string
	Site           string
	NodeCount      int
	RunSummaryOut  string
}

type SetupConfig struct {
	CommonConfig
	ProxmoxHost    string
	InventoryOut   string
	EnvOut         string
	PXEImage       string
	BootstrapToken string
	BMCSecretKey   string
	StartMetalman  bool
	ProvisionFresh bool
}

type RenderMachinesConfig struct {
	InventoryPath  string
	EnvPath        string
	OutputPath     string
	PXEImage       string
	BootstrapToken string
	BMCSecretKey   string
}

type VerifyReadyConfig struct {
	KubeconfigPath string
	InventoryPath  string
	NodeCount      int
	RunSummaryOut  string
}

type ResetConfig struct {
	KubeconfigPath string
	ProxmoxHost    string
	InventoryPath  string
	NodeNames      []string
	DestroyVMs     bool
}

func (c Command) Validate() error {
	switch c.Name {
	case "setup":
		if c.Setup == nil {
			return errors.New("setup config is required")
		}
		return c.Setup.Validate()
	case "render-machines":
		if c.RenderMachines == nil {
			return errors.New("render-machines config is required")
		}
		return c.RenderMachines.Validate()
	case "verify-ready":
		if c.VerifyReady == nil {
			return errors.New("verify-ready config is required")
		}
		return c.VerifyReady.Validate()
	case "reset":
		if c.Reset == nil {
			return errors.New("reset config is required")
		}
		return c.Reset.Validate()
	default:
		return fmt.Errorf("unknown command %q", c.Name)
	}
}

func (c SetupConfig) Validate() error {
	if err := validateCommonConfig(c.CommonConfig, true); err != nil {
		return err
	}
	if c.ProxmoxHost == "" {
		return errors.New("proxmox-host is required")
	}
	if c.InventoryOut == "" {
		return errors.New("inventory-out is required")
	}
	if c.EnvOut == "" {
		return errors.New("env-out is required")
	}
	if c.BootstrapToken == "" {
		return errors.New("bootstrap-token is required")
	}
	return nil
}

func (c RenderMachinesConfig) Validate() error {
	if c.InventoryPath == "" {
		return errors.New("inventory is required")
	}
	if c.EnvPath == "" {
		return errors.New("env is required")
	}
	if c.OutputPath == "" {
		return errors.New("out is required")
	}
	return nil
}

func (c VerifyReadyConfig) Validate() error {
	if c.KubeconfigPath == "" {
		return errors.New("kubeconfig is required")
	}
	if c.InventoryPath == "" {
		return errors.New("inventory is required")
	}
	if c.NodeCount < 2 {
		return errors.New("node-count must be >= 2")
	}
	return nil
}

func (c ResetConfig) Validate() error {
	if c.KubeconfigPath == "" {
		return errors.New("kubeconfig is required")
	}
	if c.ProxmoxHost == "" {
		return errors.New("proxmox-host is required")
	}
	if c.InventoryPath == "" && len(c.NodeNames) == 0 {
		return errors.New("inventory or explicit node names are required")
	}
	if c.InventoryPath != "" && len(c.NodeNames) > 0 {
		return errors.New("inventory and explicit node names cannot be used together")
	}
	return nil
}

func parseCommand() (Command, error) {
	if len(os.Args) < 2 {
		return Command{}, fmt.Errorf("subcommand is required: setup, render-machines, verify-ready, or reset")
	}

	switch os.Args[1] {
	case "setup":
		cfg, err := parseSetupConfig(os.Args[2:])
		if err != nil {
			return Command{}, err
		}
		return Command{Name: "setup", Setup: &cfg}, nil
	case "render-machines":
		cfg, err := parseRenderMachinesConfig(os.Args[2:])
		if err != nil {
			return Command{}, err
		}
		return Command{Name: "render-machines", RenderMachines: &cfg}, nil
	case "verify-ready":
		cfg, err := parseVerifyReadyConfig(os.Args[2:])
		if err != nil {
			return Command{}, err
		}
		return Command{Name: "verify-ready", VerifyReady: &cfg}, nil
	case "reset":
		cfg, err := parseResetConfig(os.Args[2:])
		if err != nil {
			return Command{}, err
		}
		return Command{Name: "reset", Reset: &cfg}, nil
	default:
		return Command{}, fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return ""
}

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parseSetupConfig(args []string) (SetupConfig, error) {
	var cfg SetupConfig
	fs := newFlagSet("setup")
	bindCommonFlags(fs, &cfg.CommonConfig)
	fs.StringVar(&cfg.ProxmoxHost, "proxmox-host", "", "Proxmox host address")
	fs.StringVar(&cfg.InventoryOut, "inventory-out", "", "inventory output path")
	fs.StringVar(&cfg.EnvOut, "env-out", "", "environment output path")
	fs.StringVar(&cfg.PXEImage, "pxe-image", defaultPXEImage, "PXE image reference")
	fs.StringVar(&cfg.BootstrapToken, "bootstrap-token", "", "bootstrap token secret name")
	fs.StringVar(&cfg.BMCSecretKey, "bmc-secret-key", "", "shared BMC password secret key override")
	fs.BoolVar(&cfg.StartMetalman, "start-metalman", false, "start metalman on Proxmox host")
	fs.BoolVar(&cfg.ProvisionFresh, "provision-fresh", false, "provision fresh VMs")
	if err := fs.Parse(args); err != nil {
		return SetupConfig{}, err
	}
	return cfg, nil
}

func parseRenderMachinesConfig(args []string) (RenderMachinesConfig, error) {
	var cfg RenderMachinesConfig
	fs := newFlagSet("render-machines")
	fs.StringVar(&cfg.InventoryPath, "inventory", "", "inventory input path")
	fs.StringVar(&cfg.EnvPath, "env", "", "environment input path")
	fs.StringVar(&cfg.OutputPath, "out", "", "machine manifest output path")
	fs.StringVar(&cfg.PXEImage, "pxe-image", "", "PXE image reference override")
	fs.StringVar(&cfg.BootstrapToken, "bootstrap-token", "", "bootstrap token secret name override")
	fs.StringVar(&cfg.BMCSecretKey, "bmc-secret-key", "", "shared BMC password secret key override")
	if err := fs.Parse(args); err != nil {
		return RenderMachinesConfig{}, err
	}
	return cfg, nil
}

func parseVerifyReadyConfig(args []string) (VerifyReadyConfig, error) {
	var cfg VerifyReadyConfig
	fs := newFlagSet("verify-ready")
	fs.StringVar(&cfg.KubeconfigPath, "kubeconfig", "", "kubeconfig path")
	fs.StringVar(&cfg.InventoryPath, "inventory", "", "inventory input path")
	fs.IntVar(&cfg.NodeCount, "node-count", 2, "number of PXE nodes")
	fs.StringVar(&cfg.RunSummaryOut, "run-summary-out", "tmp/proxmox-pxe-run-summary.yaml", "run summary output path")
	if err := fs.Parse(args); err != nil {
		return VerifyReadyConfig{}, err
	}
	return cfg, nil
}

func parseResetConfig(args []string) (ResetConfig, error) {
	var cfg ResetConfig
	fs := newFlagSet("reset")
	fs.StringVar(&cfg.KubeconfigPath, "kubeconfig", "", "kubeconfig path")
	fs.StringVar(&cfg.ProxmoxHost, "proxmox-host", "", "Proxmox host address")
	fs.StringVar(&cfg.InventoryPath, "inventory", "", "inventory input path")
	fs.Var((*stringSliceFlag)(&cfg.NodeNames), "node-name", "node name to reset")
	fs.BoolVar(&cfg.DestroyVMs, "destroy-vms", false, "destroy matching Proxmox VMs")
	if err := fs.Parse(args); err != nil {
		return ResetConfig{}, err
	}
	return cfg, nil
}

func bindCommonFlags(fs *flag.FlagSet, cfg *CommonConfig) {
	fs.StringVar(&cfg.KubeconfigPath, "kubeconfig", "", "kubeconfig path")
	fs.StringVar(&cfg.Site, "site", "", "site label")
	fs.IntVar(&cfg.NodeCount, "node-count", 2, "number of PXE nodes")
	fs.StringVar(&cfg.RunSummaryOut, "run-summary-out", "tmp/proxmox-pxe-run-summary.yaml", "run summary output path")
}

func validateCommonConfig(c CommonConfig, requireSite bool) error {
	if c.KubeconfigPath == "" {
		return errors.New("kubeconfig is required")
	}
	if c.NodeCount < 2 {
		return errors.New("node-count must be >= 2")
	}
	if requireSite {
		if c.Site == "" {
			return errors.New("site is required")
		}
		if err := validateSite(c.Site); err != nil {
			return err
		}
	}
	return nil
}

func validateSite(site string) error {
	if len(site) > 63 || !sitePattern.MatchString(site) {
		return errors.New("site must be a lowercase DNS label")
	}
	return nil
}
