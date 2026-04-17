package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWriteAndReadEnvironmentRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.yaml")
	want := EnvironmentFile{
		Site:                 "proxmox-lab",
		ProxmoxHost:          "10.10.100.2",
		KubeconfigPath:       "/root/.kube/config",
		PXEImage:             "ghcr.io/example/custom:v1",
		BootstrapTokenName:   "bootstrap-token-np1tzg",
		InitialRebootCounter: 1,
		InitialRepaveCounter: 1,
		Redfish: RedfishDefaults{
			URL:             "https://10.10.100.2:8000",
			Username:        "root@pam",
			SecretName:      "bmc-passwords",
			SecretNamespace: "unbounded-kube",
			SecretKey:       "stretch-pxe-0",
		},
		Network: NetworkDefaults{
			SubnetMask: "255.255.255.0",
			Gateway:    "10.10.100.1",
			DNS:        []string{"1.1.1.1"},
		},
		Artifacts: ArtifactPaths{
			InventoryPath:       "tmp/inventory.yaml",
			MachineManifestPath: "tmp/machines.yaml",
			RunSummaryPath:      "tmp/summary.yaml",
		},
	}

	if err := WriteEnvironmentFile(path, want); err != nil {
		t.Fatalf("WriteEnvironmentFile() error = %v", err)
	}
	got, err := ReadEnvironmentFile(path)
	if err != nil {
		t.Fatalf("ReadEnvironmentFile() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadEnvironmentFile() = %#v, want %#v", got, want)
	}
}

func TestReadInventoryFileUsesExistingParseInventoryNormalization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	data := []byte("vms:\n  - name: stretch-pxe-4\n    vmid: 104\n    mac: \"04\"\n    intendedIPv4: \"\"\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	inv, err := ReadInventoryFile(path)
	if err != nil {
		t.Fatalf("ReadInventoryFile() error = %v", err)
	}
	if inv.VMs[0].IPv4 != "10.10.100.54" {
		t.Fatalf("inv.VMs[0].IPv4 = %q, want %q", inv.VMs[0].IPv4, "10.10.100.54")
	}
}
