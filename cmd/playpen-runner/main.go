// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Azure/unbounded/internal/playpen/runner"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	cfg := runner.DefaultConfig()
	root := &cobra.Command{
		Use:   "playpen-runner",
		Short: "Standalone VM runner for metalman smoke tests",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runner.Run(cmd.Context(), cfg)
		},
	}

	flags := root.Flags()
	flags.StringVar(&cfg.ListenAddr, "listen-addr", cfg.ListenAddr, "HTTPS Redfish and info listen address")
	flags.StringVar(&cfg.PublicRedfishURL, "public-redfish-url", cfg.PublicRedfishURL, "Redfish URL returned by /playpen/v1/info; defaults to https://<listen-addr>")
	flags.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "runner state directory")
	flags.DurationVar(&cfg.TTL, "ttl", cfg.TTL, "maximum runner lifetime before self-termination")
	flags.BoolVar(&cfg.ConfigureNetwork, "configure-network", cfg.ConfigureNetwork, "configure WireGuard, VXLAN, bridge, and tap interfaces")
	flags.StringVar(&cfg.WireGuard.PrivateKeyFile, "wireguard-private-key-file", cfg.WireGuard.PrivateKeyFile, "path to the runner WireGuard private key")
	flags.StringVar(&cfg.WireGuard.ClientPublicKey, "wireguard-client-public-key", cfg.WireGuard.ClientPublicKey, "client WireGuard public key")
	flags.StringVar(&cfg.WireGuard.ClientPublicKeyFile, "wireguard-client-public-key-file", cfg.WireGuard.ClientPublicKeyFile, "path to a file containing the client WireGuard public key for delayed claims")
	flags.StringVar(&cfg.WireGuard.Interface, "wireguard-interface", cfg.WireGuard.Interface, "WireGuard interface name")
	flags.StringVar(&cfg.WireGuard.ServerAddress, "wireguard-server-address", cfg.WireGuard.ServerAddress, "runner WireGuard address with prefix")
	flags.StringVar(&cfg.WireGuard.ClientAddress, "wireguard-client-address", cfg.WireGuard.ClientAddress, "client WireGuard address with prefix")
	flags.IntVar(&cfg.WireGuard.ListenPort, "wireguard-listen-port", cfg.WireGuard.ListenPort, "WireGuard UDP listen port")
	flags.StringVar(&cfg.VXLAN.Interface, "vxlan-interface", cfg.VXLAN.Interface, "VXLAN interface name")
	flags.IntVar(&cfg.VXLAN.VNI, "vxlan-vni", cfg.VXLAN.VNI, "VXLAN network identifier")
	flags.IntVar(&cfg.VXLAN.Port, "vxlan-port", cfg.VXLAN.Port, "VXLAN UDP destination port")
	flags.StringVar(&cfg.BridgeName, "bridge", cfg.BridgeName, "bridge interface name")
	flags.StringVar(&cfg.TapName, "tap", cfg.TapName, "tap interface name")
	flags.StringVar(&cfg.Guest.MAC, "guest-mac", cfg.Guest.MAC, "guest NIC MAC address")
	flags.StringVar(&cfg.Guest.IPv4, "guest-ipv4", cfg.Guest.IPv4, "guest DHCP IPv4 address returned by info endpoint")
	flags.StringVar(&cfg.Guest.SubnetMask, "guest-subnet-mask", cfg.Guest.SubnetMask, "guest DHCP subnet mask returned by info endpoint")
	flags.StringVar(&cfg.Guest.Gateway, "guest-gateway", cfg.Guest.Gateway, "guest DHCP gateway returned by info endpoint")
	flags.StringSliceVar(&cfg.Guest.DNS, "guest-dns", cfg.Guest.DNS, "guest DNS servers returned by info endpoint")
	flags.StringVar(&cfg.Redfish.Username, "redfish-username", cfg.Redfish.Username, "Redfish username")
	flags.StringVar(&cfg.Redfish.Password, "redfish-password", cfg.Redfish.Password, "Redfish password")
	flags.StringVar(&cfg.Redfish.DeviceID, "redfish-device-id", cfg.Redfish.DeviceID, "Redfish system device ID")
	flags.StringVar(&cfg.QEMU.Binary, "qemu-binary", cfg.QEMU.Binary, "qemu-system binary")
	flags.StringVar(&cfg.QEMU.ImgBinary, "qemu-img-binary", cfg.QEMU.ImgBinary, "qemu-img binary")
	flags.StringVar(&cfg.QEMU.SWTPMBinary, "swtpm-binary", cfg.QEMU.SWTPMBinary, "swtpm binary")
	flags.BoolVar(&cfg.QEMU.EnableTPM, "enable-tpm", cfg.QEMU.EnableTPM, "attach a software TPM to the VM")
	flags.StringVar(&cfg.QEMU.OVMFCodeFile, "ovmf-code-file", cfg.QEMU.OVMFCodeFile, "OVMF code pflash image")
	flags.StringVar(&cfg.QEMU.OVMFVarsTemplate, "ovmf-vars-template", cfg.QEMU.OVMFVarsTemplate, "OVMF vars template copied per runner")
	flags.StringVar(&cfg.QEMU.DiskSize, "disk-size", cfg.QEMU.DiskSize, "VM disk size passed to qemu-img create")
	flags.IntVar(&cfg.QEMU.MemoryMiB, "memory-mib", cfg.QEMU.MemoryMiB, "VM memory in MiB")
	flags.IntVar(&cfg.QEMU.CPUs, "cpus", cfg.QEMU.CPUs, "VM vCPU count")

	root.AddCommand(version.Command())
	root.CompletionOptions.DisableDefaultCmd = true
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
