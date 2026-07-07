// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/playpen/runner"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	cfg := runner.DefaultConfig()
	cfg.PodName = os.Getenv("POD_NAME")
	cfg.PodNamespace = os.Getenv("POD_NAMESPACE")

	cfg.PodIP = os.Getenv("POD_IP")
	if cfg.PodName != "" && cfg.PodNamespace != "" {
		scheme := runtime.NewScheme()
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		utilruntime.Must(corev1.AddToScheme(scheme))

		kubeClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
		if err != nil {
			slog.Error("create Kubernetes client", "error", err)
			os.Exit(1)
		}

		cfg.KubernetesClient = kubeClient
	}

	root := &cobra.Command{
		Use:   "playpen-runner",
		Short: "Standalone VM runner and k3s control-plane helper for playpen tests",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runner.Run(cmd.Context(), cfg)
		},
		Version: version.Version + " (commit: " + version.GitCommit + ")",
	}

	flags := root.Flags()
	flags.StringVar(&cfg.ListenAddr, "listen-addr", cfg.ListenAddr, "HTTPS Redfish and info listen address")
	flags.StringVar(&cfg.PublicRedfishURL, "public-redfish-url", cfg.PublicRedfishURL, "Redfish URL returned by /playpen/v1/info; defaults to the pod IP")
	flags.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "runner state directory")
	flags.StringVar(&cfg.Architecture, "architecture", cfg.Architecture, "runner VM architecture: amd64 or arm64")
	flags.BoolVar(&cfg.ConfigureNetwork, "configure-network", cfg.ConfigureNetwork, "configure VXLAN, bridge, and tap interfaces")
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
	flags.StringVar(&cfg.QEMU.Machine, "qemu-machine", cfg.QEMU.Machine, "QEMU machine type")
	flags.StringVar(&cfg.QEMU.CPU, "qemu-cpu", cfg.QEMU.CPU, "QEMU CPU model")
	flags.StringVar(&cfg.QEMU.NICDevice, "qemu-nic-device", cfg.QEMU.NICDevice, "QEMU network device")
	flags.StringVar(&cfg.QEMU.SerialDevice, "qemu-serial-device", cfg.QEMU.SerialDevice, "QEMU virtio serial device")
	flags.StringVar(&cfg.QEMU.TPMDevice, "qemu-tpm-device", cfg.QEMU.TPMDevice, "QEMU TPM device")
	flags.StringVar(&cfg.QEMU.OVMFCodeFile, "ovmf-code-file", cfg.QEMU.OVMFCodeFile, "OVMF code pflash image")
	flags.StringVar(&cfg.QEMU.OVMFVarsTemplate, "ovmf-vars-template", cfg.QEMU.OVMFVarsTemplate, "OVMF vars template copied per runner")
	flags.StringVar(&cfg.QEMU.DiskSize, "disk-size", cfg.QEMU.DiskSize, "VM disk size passed to qemu-img create")
	flags.IntVar(&cfg.QEMU.MemoryMiB, "memory-mib", cfg.QEMU.MemoryMiB, "VM memory in MiB")
	flags.IntVar(&cfg.QEMU.CPUs, "cpus", cfg.QEMU.CPUs, "VM vCPU count")

	root.AddCommand(newControlPlaneCommand())
	root.AddCommand(version.Command())

	root.CompletionOptions.DisableDefaultCmd = true
	root.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
