// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runner

// Config contains all settings for one standalone playpen runner pod.
type Config struct {
	ListenAddr       string
	PublicRedfishURL string
	DataDir          string
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
	PrivateKeyFile      string
	ClientPublicKey     string
	ClientPublicKeyFile string
	Interface           string
	ServerAddress       string
	ClientAddress       string
	ListenPort          int
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
	OVMFCodeFile     string
	OVMFVarsTemplate string
	DiskSize         string
	MemoryMiB        int
	CPUs             int
}

func DefaultConfig() Config {
	return Config{
		ListenAddr:       "10.88.0.1:8443",
		DataDir:          "/var/lib/playpen-runner",
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
			Binary:           "qemu-system-x86_64",
			ImgBinary:        "qemu-img",
			SWTPMBinary:      "swtpm",
			EnableTPM:        true,
			OVMFCodeFile:     "/usr/share/OVMF/OVMF_CODE_4M.fd",
			OVMFVarsTemplate: "/usr/share/OVMF/OVMF_VARS_4M.fd",
			DiskSize:         "20G",
			MemoryMiB:        4096,
			CPUs:             2,
		},
	}
}
