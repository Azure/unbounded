package main

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMachineRenderInputFromEnvironmentMapsEnvironmentDefaultsAndOverrides(t *testing.T) {
	env := EnvironmentFile{
		Site:                 "proxmox-lab",
		PXEImage:             "ghcr.io/example/default:v1",
		BootstrapTokenName:   "bootstrap-token-default",
		InitialRebootCounter: 1,
		InitialRepaveCounter: 1,
		Redfish: RedfishDefaults{
			URL:             "https://10.10.100.2:8000",
			Username:        "admin",
			SecretName:      "bmc-passwords",
			SecretNamespace: "custom-namespace",
			SecretKey:       "default-key",
		},
		Network: NetworkDefaults{
			SubnetMask: "255.255.254.0",
			Gateway:    "10.10.100.254",
			DNS:        []string{"8.8.8.8", "1.1.1.1"},
		},
	}

	got := MachineRenderInputFromEnvironment(env, RenderMachinesConfig{
		PXEImage:       "ghcr.io/example/override:v2",
		BootstrapToken: "bootstrap-token-override",
		BMCSecretKey:   "override-key",
	})

	if got.RedfishUsername != "admin" {
		t.Fatalf("got.RedfishUsername = %q, want %q", got.RedfishUsername, "admin")
	}
	if got.BMCSecretNamespace != "custom-namespace" {
		t.Fatalf("got.BMCSecretNamespace = %q, want %q", got.BMCSecretNamespace, "custom-namespace")
	}
	wantNetwork := MachineNetworkInput{
		SubnetMask: "255.255.254.0",
		Gateway:    "10.10.100.254",
		DNS:        []string{"8.8.8.8", "1.1.1.1"},
	}
	if !reflect.DeepEqual(got.Network, wantNetwork) {
		t.Fatalf("got.Network = %#v, want %#v", got.Network, wantNetwork)
	}
	if got.Image != "ghcr.io/example/override:v2" {
		t.Fatalf("got.Image = %q, want %q", got.Image, "ghcr.io/example/override:v2")
	}
	if got.BootstrapTokenName != "bootstrap-token-override" {
		t.Fatalf("got.BootstrapTokenName = %q, want %q", got.BootstrapTokenName, "bootstrap-token-override")
	}
	if got.BMCSecretKey != "override-key" {
		t.Fatalf("got.BMCSecretKey = %q, want %q", got.BMCSecretKey, "override-key")
	}
	if got.InitialRebootCounter != 1 {
		t.Fatalf("got.InitialRebootCounter = %d, want 1", got.InitialRebootCounter)
	}
	if got.InitialRepaveCounter != 1 {
		t.Fatalf("got.InitialRepaveCounter = %d, want 1", got.InitialRepaveCounter)
	}
}

func TestParseInventoryRejectsMissingOrZeroVMID(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "missing vmid",
			data: []byte("vms:\n  - name: stretch-pxe-0\n    mac: 02:10:10:64:00:00\n    ipv4: 10.10.100.50\n"),
		},
		{
			name: "zero vmid",
			data: []byte("vms:\n  - name: stretch-pxe-0\n    vmid: 0\n    mac: 02:10:10:64:00:00\n    ipv4: 10.10.100.50\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseInventory(tt.data)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestParseInventoryRejectsMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "missing name",
			data: []byte("vms:\n  - vmid: 100\n    mac: 02:10:10:64:00:00\n    ipv4: 10.10.100.50\n"),
		},
		{
			name: "missing mac",
			data: []byte("vms:\n  - name: stretch-pxe-0\n    vmid: 100\n    ipv4: 10.10.100.50\n"),
		},
		{
			name: "missing ipv4 without derivable name",
			data: []byte("vms:\n  - name: custom-node\n    vmid: 100\n    mac: 02:10:10:64:00:00\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseInventory(tt.data)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestParseInventoryNormalizesCurrentProxmoxInventoryFormat(t *testing.T) {
	data := []byte("vms:\n  - name: stretch-pxe-4\n    vmid: 104\n    mac: \"04\"\n    bridge: vmbr0\n    powerState: stopped\n    intendedIPv4: \"\"\n")

	inv, err := ParseInventory(data)
	if err != nil {
		t.Fatalf("ParseInventory() error = %v", err)
	}
	if len(inv.VMs) != 1 {
		t.Fatalf("len(inv.VMs) = %d, want 1", len(inv.VMs))
	}

	vm := inv.VMs[0]
	if vm.MAC != "02:10:10:64:00:04" {
		t.Fatalf("vm.MAC = %q, want %q", vm.MAC, "02:10:10:64:00:04")
	}
	if vm.IPv4 != "10.10.100.54" {
		t.Fatalf("vm.IPv4 = %q, want %q", vm.IPv4, "10.10.100.54")
	}
}

func TestParseInventoryParsesVMInventoryFormat(t *testing.T) {
	data := []byte("vms:\n  - name: stretch-pxe-0\n    vmid: 100\n    mac: 02:10:10:64:00:00\n    ipv4: 10.10.100.50\n")

	inv, err := ParseInventory(data)
	if err != nil {
		t.Fatalf("ParseInventory() error = %v", err)
	}
	if len(inv.VMs) != 1 {
		t.Fatalf("len(inv.VMs) = %d, want 1", len(inv.VMs))
	}
	if inv.VMs[0].VMID != 100 {
		t.Fatalf("inv.VMs[0].VMID = %d, want 100", inv.VMs[0].VMID)
	}
}

func TestRenderMachinesIncludesExplicitDeviceID(t *testing.T) {
	inv := Inventory{VMs: []InventoryVM{{Name: "stretch-pxe-0", VMID: 100, MAC: "02:10:10:64:00:00", IPv4: "10.10.100.50"}}}

	out, err := RenderMachines(inv, MachineRenderInput{
		Site:               "proxmox-lab",
		BootstrapTokenName: "bootstrap-token-kepqb3",
		RedfishURL:         "https://10.10.100.2:8000",
		RedfishUsername:    "root@pam",
		BMCSecretName:      "bmc-passwords",
		BMCSecretNamespace: "unbounded-kube",
		Image:              "ghcr.io/azure/host-ubuntu2404:v0.0.13",
		Network: MachineNetworkInput{
			SubnetMask: "255.255.255.0",
			Gateway:    "10.10.100.1",
			DNS:        []string{"1.1.1.1"},
		},
	})
	if err != nil {
		t.Fatalf("RenderMachines() error = %v", err)
	}
	if !strings.Contains(out, "deviceID: \"100\"") {
		t.Fatalf("expected explicit deviceID in output, got:\n%s", out)
	}
	if !strings.Contains(out, "name: bootstrap-token-kepqb3") {
		t.Fatalf("expected bootstrap token reference in output, got:\n%s", out)
	}
}

func TestRenderMachinesUsesConfiguredNetworkAndRedfishDefaults(t *testing.T) {
	inv := Inventory{VMs: []InventoryVM{{Name: "stretch-pxe-0", VMID: 100, MAC: "02:10:10:64:00:00", IPv4: "10.10.100.50"}}}

	out, err := RenderMachines(inv, MachineRenderInput{
		Site:               "proxmox-lab",
		BootstrapTokenName: "bootstrap-token-kepqb3",
		RedfishURL:         "https://10.10.100.2:8000",
		RedfishUsername:    "admin",
		BMCSecretName:      "bmc-passwords",
		BMCSecretNamespace: "custom-namespace",
		Image:              "ghcr.io/azure/host-ubuntu2404:v0.0.13",
		Network: MachineNetworkInput{
			SubnetMask: "255.255.254.0",
			Gateway:    "10.10.100.254",
			DNS:        []string{"8.8.8.8", "1.1.1.1"},
		},
	})
	if err != nil {
		t.Fatalf("RenderMachines() error = %v", err)
	}
	if !strings.Contains(out, "subnetMask: \"255.255.254.0\"") {
		t.Fatalf("expected configured subnet mask in output, got:\n%s", out)
	}
	if !strings.Contains(out, "gateway: \"10.10.100.254\"") {
		t.Fatalf("expected configured gateway in output, got:\n%s", out)
	}
	if !strings.Contains(out, "dns: [\"8.8.8.8\", \"1.1.1.1\"]") {
		t.Fatalf("expected configured DNS in output, got:\n%s", out)
	}
	if !strings.Contains(out, "username: admin") {
		t.Fatalf("expected configured redfish username in output, got:\n%s", out)
	}
	if !strings.Contains(out, "namespace: custom-namespace") {
		t.Fatalf("expected configured BMC secret namespace in output, got:\n%s", out)
	}
}

func TestRenderMachinesLabelsSite(t *testing.T) {
	inv := Inventory{VMs: []InventoryVM{{Name: "stretch-pxe-1", VMID: 101, MAC: "02:10:10:64:00:01", IPv4: "10.10.100.51"}}}
	out, err := RenderMachines(inv, MachineRenderInput{Site: "proxmox-lab", BootstrapTokenName: "bootstrap-token-kepqb3", RedfishURL: "https://10.10.100.2:8000", RedfishUsername: "root@pam", BMCSecretName: "bmc-passwords", BMCSecretNamespace: "unbounded-kube", Image: "ghcr.io/azure/host-ubuntu2404:v0.0.13", Network: MachineNetworkInput{SubnetMask: "255.255.255.0", Gateway: "10.10.100.1", DNS: []string{"1.1.1.1"}}})
	if err != nil {
		t.Fatalf("RenderMachines() error = %v", err)
	}
	if !strings.Contains(out, "unbounded-kube.io/site: proxmox-lab") {
		t.Fatalf("expected site label in output, got:\n%s", out)
	}
}

func TestRenderMachinesUsesExplicitBMCSecretKey(t *testing.T) {
	inv := Inventory{VMs: []InventoryVM{{Name: "stretch-pxe-7", VMID: 107, MAC: "02:10:10:64:00:07", IPv4: "10.10.100.57"}}}

	out, err := RenderMachines(inv, MachineRenderInput{
		Site:               "proxmox-lab",
		BootstrapTokenName: "bootstrap-token-kepqb3",
		RedfishURL:         "https://10.10.100.2:8000",
		RedfishUsername:    "root@pam",
		BMCSecretName:      "bmc-passwords",
		BMCSecretNamespace: "unbounded-kube",
		BMCSecretKey:       "stretch-pxe-0",
		Image:              "ghcr.io/azure/host-ubuntu2404:v0.0.13",
		Network: MachineNetworkInput{
			SubnetMask: "255.255.255.0",
			Gateway:    "10.10.100.1",
			DNS:        []string{"1.1.1.1"},
		},
	})
	if err != nil {
		t.Fatalf("RenderMachines() error = %v", err)
	}
	if !strings.Contains(out, "key: stretch-pxe-0") {
		t.Fatalf("expected explicit BMC secret key in output, got:\n%s", out)
	}
	if strings.Contains(out, "key: stretch-pxe-7") {
		t.Fatalf("expected machine name to not be used as BMC secret key, got:\n%s", out)
	}
}

func TestRenderMachinesUsesExplicitBMCSecretNamespace(t *testing.T) {
	inv := Inventory{VMs: []InventoryVM{{Name: "stretch-pxe-0", VMID: 100, MAC: "02:10:10:64:00:00", IPv4: "10.10.100.50"}}}

	out, err := RenderMachines(inv, MachineRenderInput{
		Site:               "proxmox-lab",
		BootstrapTokenName: "bootstrap-token-kepqb3",
		RedfishURL:         "https://10.10.100.2:8000",
		RedfishUsername:    "root@pam",
		BMCSecretName:      "bmc-passwords",
		BMCSecretNamespace: "custom-namespace",
		Image:              "ghcr.io/azure/host-ubuntu2404:v0.0.13",
		Network: MachineNetworkInput{
			SubnetMask: "255.255.255.0",
			Gateway:    "10.10.100.1",
			DNS:        []string{"1.1.1.1"},
		},
	})
	if err != nil {
		t.Fatalf("RenderMachines() error = %v", err)
	}
	if !strings.Contains(out, "namespace: custom-namespace") {
		t.Fatalf("expected explicit BMC secret namespace in output, got:\n%s", out)
	}
	if strings.Contains(out, "namespace: unbounded-kube") {
		t.Fatalf("expected hardcoded namespace to be absent, got:\n%s", out)
	}
}

func TestRenderMachinesProducesValidYAMLWithExplicitBMCSecretKey(t *testing.T) {
	inv := Inventory{VMs: []InventoryVM{{Name: "stretch-pxe-7", VMID: 107, MAC: "02:10:10:64:00:07", IPv4: "10.10.100.57"}}}

	out, err := RenderMachines(inv, MachineRenderInput{
		Site:               "proxmox-lab",
		BootstrapTokenName: "bootstrap-token-kepqb3",
		RedfishURL:         "https://10.10.100.2:8000",
		RedfishUsername:    "root@pam",
		BMCSecretName:      "bmc-passwords",
		BMCSecretNamespace: "unbounded-kube",
		BMCSecretKey:       "stretch-pxe-0",
		Image:              "ghcr.io/azure/host-ubuntu2404:v0.0.13",
		Network: MachineNetworkInput{
			SubnetMask: "255.255.255.0",
			Gateway:    "10.10.100.1",
			DNS:        []string{"1.1.1.1"},
		},
	})
	if err != nil {
		t.Fatalf("RenderMachines() error = %v", err)
	}

	decoder := yaml.NewDecoder(strings.NewReader(out))
	for document := 1; ; document++ {
		var rendered map[string]any
		err = decoder.Decode(&rendered)
		if err == nil {
			continue
		}
		if err.Error() == "EOF" {
			break
		}
		t.Fatalf("rendered YAML document %d failed to parse: %v\n%s", document, err, out)
	}
	if !strings.Contains(out, "passwordRef:") {
		t.Fatalf("expected passwordRef block in output, got:\n%s", out)
	}
}
