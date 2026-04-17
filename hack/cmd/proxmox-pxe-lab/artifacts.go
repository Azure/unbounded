package main

import (
	"os"

	"gopkg.in/yaml.v3"
)

type EnvironmentFile struct {
	Site                 string          `yaml:"site"`
	ProxmoxHost          string          `yaml:"proxmoxHost"`
	KubeconfigPath       string          `yaml:"kubeconfigPath"`
	PXEImage             string          `yaml:"pxeImage"`
	BootstrapTokenName   string          `yaml:"bootstrapTokenName"`
	InitialRebootCounter int             `yaml:"initialRebootCounter,omitempty"`
	InitialRepaveCounter int             `yaml:"initialRepaveCounter,omitempty"`
	Redfish              RedfishDefaults `yaml:"redfish"`
	Network              NetworkDefaults `yaml:"network"`
	Artifacts            ArtifactPaths   `yaml:"artifacts,omitempty"`
}

type RedfishDefaults struct {
	URL             string `yaml:"url"`
	Username        string `yaml:"username"`
	SecretName      string `yaml:"secretName"`
	SecretNamespace string `yaml:"secretNamespace"`
	SecretKey       string `yaml:"secretKey,omitempty"`
}

type NetworkDefaults struct {
	SubnetMask string   `yaml:"subnetMask"`
	Gateway    string   `yaml:"gateway"`
	DNS        []string `yaml:"dns"`
}

type ArtifactPaths struct {
	InventoryPath       string `yaml:"inventoryPath,omitempty"`
	MachineManifestPath string `yaml:"machineManifestPath,omitempty"`
	RunSummaryPath      string `yaml:"runSummaryPath,omitempty"`
}

func WriteEnvironmentFile(path string, env EnvironmentFile) error {
	data, err := yaml.Marshal(env)
	if err != nil {
		return err
	}
	if err := ensureParentDir(path); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func ReadEnvironmentFile(path string) (EnvironmentFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return EnvironmentFile{}, err
	}
	var env EnvironmentFile
	if err := yaml.Unmarshal(data, &env); err != nil {
		return EnvironmentFile{}, err
	}
	return env, nil
}

func WriteInventoryFile(path string, inv Inventory) error {
	data, err := yaml.Marshal(inv)
	if err != nil {
		return err
	}
	if err := ensureParentDir(path); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func ReadInventoryFile(path string) (Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Inventory{}, err
	}
	return ParseInventory(data)
}
