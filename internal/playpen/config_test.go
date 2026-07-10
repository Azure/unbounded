// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"net"
	"strings"
	"testing"
)

func TestNormalizeAppliesDefaults(t *testing.T) {
	cfg, err := Normalize(Config{
		Arch:        ArchAMD64,
		VXLANRemote: "100.64.0.8",
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	if cfg.Name != defaultName {
		t.Fatalf("Name = %q, want %q", cfg.Name, defaultName)
	}

	if cfg.CPUs != defaultCPUs {
		t.Fatalf("CPUs = %d, want %d", cfg.CPUs, defaultCPUs)
	}

	if cfg.Memory != defaultMemory {
		t.Fatalf("Memory = %q, want %q", cfg.Memory, defaultMemory)
	}

	if cfg.QEMUBinary != "qemu-system-x86_64" {
		t.Fatalf("QEMUBinary = %q", cfg.QEMUBinary)
	}

	if cfg.DiskPath != defaultDiskPath || cfg.DiskSize != defaultDiskSize {
		t.Fatalf("disk defaults = path %q size %q", cfg.DiskPath, cfg.DiskSize)
	}

	if cfg.SWTPMBinary != defaultSWTPMBinary || cfg.TPMStateDir != defaultTPMStateDir || cfg.TPMSocket != defaultTPMSocket {
		t.Fatalf("TPM defaults = binary %q state %q socket %q", cfg.SWTPMBinary, cfg.TPMStateDir, cfg.TPMSocket)
	}

	if cfg.BMCListen != defaultBMCListen || cfg.BMCUsername != defaultBMCUsername || cfg.BMCPassword != defaultBMCPassword ||
		cfg.BMCDeviceID != defaultBMCDeviceID || cfg.BMCCertPath != defaultBMCCertPath || cfg.BMCKeyPath != defaultBMCKeyPath {
		t.Fatalf("BMC defaults = listen %q username %q password %q device %q cert %q key %q",
			cfg.BMCListen, cfg.BMCUsername, cfg.BMCPassword, cfg.BMCDeviceID, cfg.BMCCertPath, cfg.BMCKeyPath)
	}

	if cfg.VXLANVNI != defaultVXLANVNI || cfg.VXLANPort != defaultVXLANPort || cfg.MTU != defaultMTU {
		t.Fatalf("VXLAN defaults = vni %d port %d mtu %d", cfg.VXLANVNI, cfg.VXLANPort, cfg.MTU)
	}

	if cfg.BridgeName != defaultBridgeName || cfg.TapName != defaultTapName || cfg.VXLANName != defaultVXLANName {
		t.Fatalf("interface defaults = %q %q %q", cfg.BridgeName, cfg.TapName, cfg.VXLANName)
	}

	mac, err := ParseMAC(cfg.MAC)
	if err != nil {
		t.Fatalf("generated MAC did not validate: %v", err)
	}

	if !isLocallyAdministered(mac) {
		t.Fatalf("generated MAC %s is not locally administered", mac)
	}
}

func TestNormalizeRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "missing remote",
			cfg:  Config{Arch: ArchAMD64},
			want: "vxlan-remote is required",
		},
		{
			name: "bad arch",
			cfg:  Config{Arch: "s390x", VXLANRemote: "100.64.0.8"},
			want: "unsupported arch",
		},
		{
			name: "bad cpus",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", CPUs: -1},
			want: "cpus must be greater than zero",
		},
		{
			name: "bad memory",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", Memory: "0M"},
			want: "memory must be a positive QEMU memory size",
		},
		{
			name: "empty disk path",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", DiskPath: " "},
			want: "disk path must not be empty",
		},
		{
			name: "zero disk size",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", DiskSize: "0G"},
			want: "disk size must be a positive size",
		},
		{
			name: "fractional disk size",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", DiskSize: "1.5G"},
			want: "disk size must be a positive size",
		},
		{
			name: "overflowing disk size",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", DiskSize: "9999999999999999999G"},
			want: "disk size must be a positive size",
		},
		{
			name: "disk size unit overflow",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", DiskSize: "9223372036854775807G"},
			want: "disk size is too large",
		},
		{
			name: "empty TPM state directory",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", TPMStateDir: " "},
			want: "TPM state directory must not be empty",
		},
		{
			name: "empty TPM socket path",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", TPMSocket: " "},
			want: "TPM socket path must not be empty",
		},
		{
			name: "bad BMC listen address",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", BMCListen: "127.0.0.1:not-a-port"},
			want: "BMC listen address is invalid",
		},
		{
			name: "empty BMC password",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", BMCPassword: " "},
			want: "BMC password must not be empty",
		},
		{
			name: "bad vni",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", VXLANVNI: maxVXLANVNI + 1},
			want: "vxlan-vni must be between",
		},
		{
			name: "bad port",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", VXLANPort: 70000},
			want: "vxlan-port must be between",
		},
		{
			name: "bad mtu",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", MTU: 100},
			want: "mtu must be between",
		},
		{
			name: "bad mac",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", MAC: "01:00:5e:00:00:01"},
			want: "must be a unicast address",
		},
		{
			name: "duplicate links",
			cfg:  Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", BridgeName: "tap0", TapName: "tap0"},
			want: "interface names must be distinct",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Normalize(tt.cfg)
			if err == nil {
				t.Fatal("Normalize() succeeded, want error")
			}

			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Normalize() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestBuildNetworkSpec(t *testing.T) {
	cfg, err := Normalize(Config{
		Arch:        ArchARM64,
		VXLANRemote: "100.64.0.8",
		VXLANLocal:  "10.42.3.20",
		VXLANVNI:    7,
		VXLANPort:   8472,
		MTU:         1300,
		BridgeName:  "brp0",
		TapName:     "tap7",
		VXLANName:   "vx7",
		MAC:         "02:00:00:00:00:07",
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	spec, err := BuildNetworkSpec(cfg)
	if err != nil {
		t.Fatalf("BuildNetworkSpec() error = %v", err)
	}

	if spec.BridgeName != "brp0" || spec.TapName != "tap7" || spec.VXLANName != "vx7" {
		t.Fatalf("unexpected link names: %#v", spec)
	}

	if !spec.RemoteIP.Equal(net.ParseIP("100.64.0.8")) || !spec.LocalIP.Equal(net.ParseIP("10.42.3.20")) {
		t.Fatalf("unexpected IPs: remote %s local %s", spec.RemoteIP, spec.LocalIP)
	}

	if spec.VNI != 7 || spec.Port != 8472 || spec.MTU != 1300 {
		t.Fatalf("unexpected VXLAN parameters: %#v", spec)
	}
}

func TestParseAndGenerateMAC(t *testing.T) {
	mac, err := GenerateMAC()
	if err != nil {
		t.Fatalf("GenerateMAC() error = %v", err)
	}

	if len(mac) != 6 {
		t.Fatalf("GenerateMAC() length = %d", len(mac))
	}

	if mac[0]&0x01 != 0 {
		t.Fatalf("GenerateMAC() produced multicast MAC %s", mac)
	}

	if !isLocallyAdministered(mac) {
		t.Fatalf("GenerateMAC() produced globally administered MAC %s", mac)
	}

	if _, err := ParseMAC("02:00:00:00:00:01"); err != nil {
		t.Fatalf("ParseMAC() error = %v", err)
	}

	if _, err := ParseMAC("33:33:00:00:00:01"); err == nil {
		t.Fatal("ParseMAC() accepted multicast MAC")
	}
}

func TestNormalizeDerivesStableMACFromIdentity(t *testing.T) {
	identity := "unbounded-kube/playpen-0"
	cfg := Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", MACIdentity: identity}

	first, err := Normalize(cfg)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	second, err := Normalize(cfg)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	if first.MAC != second.MAC || first.MAC != MACFromIdentity(identity).String() {
		t.Fatalf("derived MACs = %q and %q, want stable MAC for %q", first.MAC, second.MAC, identity)
	}

	other, err := Normalize(Config{Arch: ArchAMD64, VXLANRemote: "100.64.0.8", MACIdentity: "unbounded-kube/playpen-1"})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	if first.MAC == other.MAC {
		t.Fatalf("different replica identities produced the same MAC %q", first.MAC)
	}
}

func TestNormalizeExplicitMACOverridesIdentity(t *testing.T) {
	cfg, err := Normalize(Config{
		Arch:        ArchAMD64,
		VXLANRemote: "100.64.0.8",
		MAC:         "02:00:00:00:00:07",
		MACIdentity: "unbounded-kube/playpen-0",
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}

	if cfg.MAC != "02:00:00:00:00:07" {
		t.Fatalf("MAC = %q, want explicit value", cfg.MAC)
	}
}
