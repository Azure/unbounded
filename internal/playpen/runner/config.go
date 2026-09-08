// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/playpen/meta"
)

const (
	ArchitectureAMD64 = meta.ArchitectureAMD64
	ArchitectureARM64 = meta.ArchitectureARM64

	defaultAMD64QEMUBinary       = "qemu-system-x86_64"
	defaultAMD64QEMUMachine      = "q35,accel=kvm"
	defaultAMD64QEMUCPU          = "host"
	defaultAMD64QEMUNICDevice    = "virtio-net-pci"
	defaultAMD64QEMUSerialDevice = "virtio-serial-pci"
	defaultAMD64QEMUTPMDevice    = "tpm-tis"
	defaultAMD64OVMFCodeFile     = "/usr/share/OVMF/OVMF_CODE_4M.fd"
	defaultAMD64OVMFVarsTemplate = "/usr/share/OVMF/OVMF_VARS_4M.fd"

	defaultARM64QEMUBinary       = "qemu-system-aarch64"
	defaultARM64QEMUMachine      = "virt,accel=kvm"
	defaultARM64QEMUCPU          = "host"
	defaultARM64QEMUNICDevice    = "virtio-net-pci"
	defaultARM64QEMUSerialDevice = "virtio-serial-pci"
	defaultARM64QEMUTPMDevice    = "tpm-tis-device"
	defaultARM64OVMFCodeFile     = "/usr/share/AAVMF/AAVMF_CODE.fd"
	defaultARM64OVMFVarsTemplate = "/usr/share/AAVMF/AAVMF_VARS.fd"
)

// Config contains all settings for one standalone playpen runner pod.
type Config struct {
	ListenAddr       string
	PublicRedfishURL string
	DataDir          string
	PodName          string
	PodNamespace     string
	Architecture     string
	KubernetesClient client.Client
	ConfigureNetwork bool
	WireGuard        WireGuardConfig
	VXLAN            VXLANConfig
	BridgeName       string
	TapName          string
	Guest            GuestConfig
	Redfish          RedfishConfig
	QEMU             QEMUConfig
}

type WireGuardConfig struct {
	PrivateKeyFile  string
	ClientPublicKey string
	Interface       string
	ServerAddress   string
	ClientAddress   string
	ListenPort      int
}

type VXLANConfig struct {
	Interface string
	VNI       int
	Port      int
}

type GuestConfig struct {
	MAC        string
	IPv4       string
	SubnetMask string
	Gateway    string
	DNS        []string
}

type RedfishConfig struct {
	Username string
	Password string
	DeviceID string
}

type QEMUConfig struct {
	Binary           string
	ImgBinary        string
	SWTPMBinary      string
	EnableTPM        bool
	Machine          string
	CPU              string
	NICDevice        string
	SerialDevice     string
	TPMDevice        string
	OVMFCodeFile     string
	OVMFVarsTemplate string
	DiskSize         string
	MemoryMiB        int
	CPUs             int
}

func DefaultConfig() Config {
	return Config{
		ListenAddr:       ":8443",
		DataDir:          "/var/lib/playpen-runner",
		Architecture:     ArchitectureAMD64,
		ConfigureNetwork: true,
		WireGuard: WireGuardConfig{
			PrivateKeyFile: "/etc/playpen/wireguard/privatekey",
			Interface:      "wg0",
			ServerAddress:  "10.88.0.1/24",
			ClientAddress:  "10.88.0.2/32",
			ListenPort:     51820,
		},
		VXLAN: VXLANConfig{
			Interface: "vxlan0",
			VNI:       12001,
			Port:      4789,
		},
		BridgeName: "br0",
		TapName:    "tap0",
		Guest: GuestConfig{
			MAC:        "52:54:00:aa:bb:01",
			IPv4:       "192.168.200.10",
			SubnetMask: "255.255.255.0",
			Gateway:    "192.168.200.1",
			DNS:        []string{"8.8.8.8"},
		},
		Redfish: RedfishConfig{
			DeviceID: "1",
		},
		QEMU: QEMUConfig{
			Binary:           defaultAMD64QEMUBinary,
			ImgBinary:        "qemu-img",
			SWTPMBinary:      "swtpm",
			EnableTPM:        true,
			Machine:          defaultAMD64QEMUMachine,
			CPU:              defaultAMD64QEMUCPU,
			NICDevice:        defaultAMD64QEMUNICDevice,
			SerialDevice:     defaultAMD64QEMUSerialDevice,
			TPMDevice:        defaultAMD64QEMUTPMDevice,
			OVMFCodeFile:     defaultAMD64OVMFCodeFile,
			OVMFVarsTemplate: defaultAMD64OVMFVarsTemplate,
			DiskSize:         "20G",
			MemoryMiB:        4096,
			CPUs:             2,
		},
	}
}

func (c *Config) ApplyArchitectureDefaults() error {
	arch, err := normalizeArchitecture(c.Architecture)
	if err != nil {
		return err
	}

	c.Architecture = arch

	switch arch {
	case ArchitectureAMD64:
		applyDefault(&c.QEMU.Binary, defaultAMD64QEMUBinary)
		applyDefault(&c.QEMU.Machine, defaultAMD64QEMUMachine)
		applyDefault(&c.QEMU.CPU, defaultAMD64QEMUCPU)
		applyDefault(&c.QEMU.NICDevice, defaultAMD64QEMUNICDevice)
		applyDefault(&c.QEMU.SerialDevice, defaultAMD64QEMUSerialDevice)
		applyDefault(&c.QEMU.TPMDevice, defaultAMD64QEMUTPMDevice)
		applyDefault(&c.QEMU.OVMFCodeFile, defaultAMD64OVMFCodeFile)
		applyDefault(&c.QEMU.OVMFVarsTemplate, defaultAMD64OVMFVarsTemplate)
	case ArchitectureARM64:
		applyArchitectureDefault(&c.QEMU.Binary, defaultARM64QEMUBinary, defaultAMD64QEMUBinary)
		applyArchitectureDefault(&c.QEMU.Machine, defaultARM64QEMUMachine, defaultAMD64QEMUMachine)
		applyArchitectureDefault(&c.QEMU.CPU, defaultARM64QEMUCPU, defaultAMD64QEMUCPU)
		applyArchitectureDefault(&c.QEMU.NICDevice, defaultARM64QEMUNICDevice, defaultAMD64QEMUNICDevice)
		applyArchitectureDefault(&c.QEMU.SerialDevice, defaultARM64QEMUSerialDevice, defaultAMD64QEMUSerialDevice)
		applyArchitectureDefault(&c.QEMU.TPMDevice, defaultARM64QEMUTPMDevice, defaultAMD64QEMUTPMDevice)
		applyArchitectureDefault(&c.QEMU.OVMFCodeFile, defaultARM64OVMFCodeFile, defaultAMD64OVMFCodeFile)
		applyArchitectureDefault(&c.QEMU.OVMFVarsTemplate, defaultARM64OVMFVarsTemplate, defaultAMD64OVMFVarsTemplate)
	}

	return nil
}

func normalizeArchitecture(value string) (string, error) {
	switch arch := strings.ToLower(strings.TrimSpace(value)); arch {
	case "", ArchitectureAMD64:
		return ArchitectureAMD64, nil
	case ArchitectureARM64:
		return ArchitectureARM64, nil
	default:
		return "", fmt.Errorf("architecture must be %q or %q", ArchitectureAMD64, ArchitectureARM64)
	}
}

func applyDefault(value *string, defaultValue string) {
	if strings.TrimSpace(*value) == "" {
		*value = defaultValue
	}
}

func applyArchitectureDefault(value *string, archDefault string, replaceValues ...string) {
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		*value = archDefault

		return
	}

	for _, replaceValue := range replaceValues {
		if trimmed == replaceValue {
			*value = archDefault

			return
		}
	}
}
