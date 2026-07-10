// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/playpen"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "playpen",
		Short:        "Run KVM playpens and isolated client endpoints",
		SilenceUsage: true,
		Version:      version.String(),
	}

	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)
	cmd.AddCommand(newServerCommand())
	cmd.AddCommand(newClientCommand())
	cmd.AddCommand(version.Command())

	return cmd
}

func newServerCommand() *cobra.Command {
	cfg := playpen.DefaultConfig()

	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run one KVM VM attached to a pod-local VXLAN network",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return playpen.Run(ctx, cfg)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&cfg.Name, "name", cfg.Name, "VM name")
	flags.StringVar(&cfg.Arch, "arch", cfg.Arch, "VM architecture: amd64 or arm64")
	flags.IntVar(&cfg.CPUs, "cpus", cfg.CPUs, "Number of vCPUs")
	flags.StringVar(&cfg.Memory, "memory", cfg.Memory, "VM memory size passed to QEMU")
	flags.StringVar(&cfg.QEMUBinary, "qemu-binary", cfg.QEMUBinary, "QEMU binary path or name; defaults by architecture")
	flags.StringVar(&cfg.VXLANRemote, "vxlan-remote", cfg.VXLANRemote, "Client VXLAN endpoint IP reachable through the mesh")
	flags.StringVar(&cfg.VXLANLocal, "vxlan-local", cfg.VXLANLocal, "Local VXLAN source IP; auto-detected when empty")
	flags.IntVar(&cfg.VXLANVNI, "vxlan-vni", cfg.VXLANVNI, "VXLAN network identifier")
	flags.IntVar(&cfg.VXLANPort, "vxlan-port", cfg.VXLANPort, "VXLAN UDP destination port")
	flags.IntVar(&cfg.MTU, "mtu", cfg.MTU, "MTU for the bridge, TAP, and VXLAN links")
	flags.StringVar(&cfg.BridgeName, "bridge", cfg.BridgeName, "Linux bridge interface name")
	flags.StringVar(&cfg.TapName, "tap", cfg.TapName, "QEMU TAP interface name")
	flags.StringVar(&cfg.VXLANName, "vxlan", cfg.VXLANName, "VXLAN interface name")
	flags.StringVar(&cfg.MAC, "mac", cfg.MAC, "VM NIC MAC address; generated when empty")
	flags.StringVar(&cfg.MACIdentity, "mac-identity", cfg.MACIdentity, "Stable identity used to derive the VM MAC when --mac is empty")
	flags.StringVar(&cfg.UEFICode, "uefi-code", cfg.UEFICode, "UEFI code firmware path; defaults by architecture")
	flags.StringVar(&cfg.UEFIVars, "uefi-vars", cfg.UEFIVars, "UEFI vars template path; defaults by architecture")
	flags.StringVar(&cfg.RuntimeDir, "runtime-dir", cfg.RuntimeDir, "Directory for runtime state such as writable UEFI vars")
	flags.StringVar(&cfg.DiskPath, "disk", cfg.DiskPath, "Persistent writable guest disk path")
	flags.StringVar(&cfg.DiskSize, "disk-size", cfg.DiskSize, "Guest disk size used when creating an empty disk")
	flags.StringVar(&cfg.SWTPMBinary, "swtpm-binary", cfg.SWTPMBinary, "swtpm binary path or name")
	flags.StringVar(&cfg.TPMStateDir, "tpm-state-dir", cfg.TPMStateDir, "Persistent software TPM state directory")
	flags.StringVar(&cfg.TPMSocket, "tpm-socket", cfg.TPMSocket, "Software TPM control socket path")
	flags.StringVar(&cfg.BMCListen, "bmc-listen", cfg.BMCListen, "HTTPS listen address for the Redfish BMC")
	flags.StringVar(&cfg.BMCUsername, "bmc-username", cfg.BMCUsername, "Redfish BMC username")
	flags.StringVar(&cfg.BMCPassword, "bmc-password", cfg.BMCPassword, "Redfish BMC password")
	flags.StringVar(&cfg.BMCDeviceID, "bmc-device-id", cfg.BMCDeviceID, "Redfish ComputerSystem device ID")
	flags.StringVar(&cfg.BMCCertPath, "bmc-cert", cfg.BMCCertPath, "Persistent Redfish TLS certificate path")
	flags.StringVar(&cfg.BMCKeyPath, "bmc-key", cfg.BMCKeyPath, "Persistent Redfish TLS private key path")
	flags.StringVar(&cfg.KVMPath, "kvm", cfg.KVMPath, "KVM device path")
	flags.StringVar(&cfg.TUNPath, "tun", cfg.TUNPath, "TUN/TAP device path")
	flags.StringArrayVar(&cfg.ExtraQEMUArgs, "extra-qemu-arg", cfg.ExtraQEMUArgs, "Additional QEMU argument; repeat for multiple arguments")

	return cmd
}

func newClientCommand() *cobra.Command {
	cfg := playpen.DefaultClientConfig()

	cmd := &cobra.Command{
		Use:   "client [flags] -- command [args...]",
		Short: "Run a PXE service in an isolated VXLAN-connected network namespace",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg.Command = args
			cfg.ClaimOutput = cmd.ErrOrStderr()

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return playpen.RunClient(ctx, cfg)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&cfg.Namespace, "namespace", cfg.Namespace, "Unique network namespace name")
	flags.StringVar(&cfg.EndpointCIDR, "endpoint-cidr", cfg.EndpointCIDR, "Client VXLAN endpoint and veth prefix, for example 172.30.1.2/30")
	flags.StringVar(&cfg.GatewayIP, "gateway-ip", cfg.GatewayIP, "Host-side veth address in the endpoint prefix")
	flags.StringVar(&cfg.RemoteIP, "remote", cfg.RemoteIP, "Playpen pod VXLAN endpoint IP")
	flags.StringVar(&cfg.PodNamespace, "pod-namespace", cfg.PodNamespace, "Kubernetes namespace containing the playpen pod pool")
	flags.StringVar(&cfg.PodSelector, "pod-selector", cfg.PodSelector, "Label selector for playpen pods")
	flags.StringVar(&cfg.Kubeconfig, "kubeconfig", cfg.Kubeconfig, "Path to the kubeconfig used to claim a playpen pod")
	flags.StringVar(&cfg.KubeContext, "context", cfg.KubeContext, "Kubeconfig context used to claim a playpen pod")
	flags.StringVar(&cfg.BridgeCIDR, "bridge-cidr", cfg.BridgeCIDR, "PXE server address assigned to the client bridge")
	flags.StringVar(&cfg.BridgeName, "bridge", cfg.BridgeName, "Client bridge interface name")
	flags.StringVar(&cfg.VXLANName, "vxlan", cfg.VXLANName, "VXLAN interface name")
	flags.StringVar(&cfg.UnderlayName, "underlay", cfg.UnderlayName, "Namespace underlay interface name")
	flags.StringVar(&cfg.NodeIP, "node-ip", cfg.NodeIP, "Internal IP advertised by the temporary unbounded-net Node (enables WireGuard)")
	flags.StringVar(&cfg.NodeCIDR, "node-cidr", cfg.NodeCIDR, "Pod CIDR advertised by the temporary unbounded-net Node (enables WireGuard)")
	flags.StringVar(&cfg.Site, "site", cfg.Site, "unbounded-net Site for the temporary Node")
	flags.StringVar(&cfg.GatewayPool, "gateway-pool", cfg.GatewayPool, "External unbounded-net GatewayPool used for the tunnel")
	flags.StringVar(&cfg.WireGuardName, "wireguard", cfg.WireGuardName, "WireGuard interface name")
	flags.IntVar(&cfg.WireGuardPort, "wireguard-port", cfg.WireGuardPort, "Gateway WireGuard mesh listen port")
	flags.IntVar(&cfg.VXLANVNI, "vxlan-vni", cfg.VXLANVNI, "VXLAN network identifier")
	flags.IntVar(&cfg.VXLANPort, "vxlan-port", cfg.VXLANPort, "VXLAN UDP destination port")
	flags.IntVar(&cfg.MTU, "mtu", cfg.MTU, "MTU for the bridge and VXLAN links (default 1360 direct, 1230 over WireGuard)")
	flags.StringVar(&cfg.IPBinary, "ip-binary", cfg.IPBinary, "Path to the ip command")
	flags.StringVar(&cfg.WGBinary, "wg-binary", cfg.WGBinary, "Path to the wg command")

	return cmd
}
