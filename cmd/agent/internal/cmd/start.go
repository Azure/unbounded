// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/host"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/nodestart"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/rootfs"
	"github.com/Azure/unbounded-kube/internal/provision"
	"github.com/Azure/unbounded-kube/internal/version"
)

func newCmdStart(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Bootstrap the host, rootfs, and start the node",
		Long:  "Run all three phases (host, rootfs, node-start) in sequence to fully bootstrap a machine and join it to the cluster.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()

			cmdCtx.Logger.Info("starting unbounded-agent",
				"version", version.Version,
				"commit", version.GitCommit,
			)

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			rootFSGoalState, err := resolveRootFSGoalState(cmdCtx.Logger, cfg)
			if err != nil {
				return err
			}

			nodeStartGoalState, err := resolveNodeStartGoalState(cfg, rootFSGoalState.Nvidia)
			if err != nil {
				return err
			}

			log := cmdCtx.Logger

			return phases.Serial(log,
				// Phase 1: host
				host.InstallPackages(log),
				phases.Parallel(log,
					host.ConfigureOS(log),
					host.ConfigureNFTables(log),
					host.DisableDocker(log),
					host.DisableSwap(log),
				),

				// TPM Attestation (no-op when not configured).
				host.ApplyAttestation(log, cfg.Attest, cfg.MachineName, nodeStartGoalState),

				// Phase 2: rootfs
				rootfs.EnsureNSpawnWorkspace(log, rootFSGoalState),
				phases.Parallel(log,
					rootfs.DownloadKubeBinaries(log, rootFSGoalState),
					rootfs.DownloadCRIBinaries(log, rootFSGoalState),
					rootfs.DownloadCNIBinaries(log, rootFSGoalState),
					rootfs.ConfigureOS(rootFSGoalState),
					rootfs.DisableResolved(rootFSGoalState),
				),

				// Phase 3: node-start
				phases.Parallel(log,
					nodestart.ConfigureContainerd(nodeStartGoalState),
					nodestart.ConfigureKubelet(nodeStartGoalState),
				),
				nodestart.StartNSpawnMachine(log, nodeStartGoalState),
				nodestart.SetupNVIDIA(log, nodeStartGoalState),
				nodestart.StartContainerd(log, nodeStartGoalState),
				nodestart.StartKubelet(log, nodeStartGoalState),
			).Do(ctx)
		},
	}

	return cmd
}

// ref: cmd/machina/machina/controller/machine_controller.go
func resolveRootFSGoalState(log *slog.Logger, cfg *provision.AgentConfig) (*goalstates.RootFS, error) {
	nspawnMachineName := goalstates.NSpawnMachineKube1
	kubeVersion := cfg.Cluster.Version

	kernel, err := hostKernel() //nolint:staticcheck // SA4023: non-Linux stub always errors; this is intentional.
	if err != nil {             //nolint:staticcheck // SA4023: see above.
		return nil, err
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("get host hostname: %w", err)
	}

	nvidia, err := goalstates.ResolveNvidiaHost(runtime.GOARCH)
	if err != nil {
		return nil, err
	}

	ociImage := resolveOCIImage(log, cfg.OCIImage, len(nvidia.GPUDevicePaths) > 0)

	return &goalstates.RootFS{
		MachineDir: filepath.Join("/var/lib/machines", nspawnMachineName),
		NSpawnConfigFile: filepath.Join(
			goalstates.SystemdNSpawnDir,
			nspawnMachineName+".nspawn",
		),
		ServiceOverrideFile: filepath.Join(
			goalstates.SystemdSystemDir,
			fmt.Sprintf("systemd-nspawn@%s.service.d", nspawnMachineName),
			"override.conf",
		),
		HostArch:          runtime.GOARCH,
		HostKernel:        kernel,
		Hostname:          hostname,
		ContainerdVersion: goalstates.ContainerdVersion, // FIXME: allow overriding
		RunCVersion:       goalstates.RunCVersion,       // FIXME allow overriding
		CNIPluginVersion:  goalstates.CNIPluginVersion,  // FIXME: allow overriding
		KubernetesVersion: kubeVersion,
		OCIImage:          ociImage,
		Nvidia:            nvidia,
		HostDevicePaths:   goalstates.DiscoverHostDevicePaths(),
	}, nil
}

// ref: cmd/machina/machina/controller/machine_controller.go
func resolveNodeStartGoalState(cfg *provision.AgentConfig, nvidia goalstates.NvidiaHost) (*goalstates.NodeStart, error) {
	nspawnMachineName := goalstates.NSpawnMachineKube1

	kubelet, err := resolveKubeletGoalState(cfg)
	if err != nil {
		return nil, err
	}

	return &goalstates.NodeStart{
		MachineName: nspawnMachineName,
		MachineDir:  filepath.Join("/var/lib/machines", nspawnMachineName),
		Containerd:  goalstates.ResolveContainerd(),
		Kubelet:     kubelet,
		Nvidia:      nvidia,
	}, nil
}

func resolveKubeletGoalState(cfg *provision.AgentConfig) (goalstates.Kubelet, error) {
	var zero goalstates.Kubelet

	caCert, err := base64.StdEncoding.DecodeString(cfg.Cluster.CaCertBase64)
	if err != nil {
		return zero, fmt.Errorf("decode CaCertBase64: %w", err)
	}

	labels := cfg.Kubelet.Labels
	if labels == nil {
		labels = make(map[string]string)
	}

	return goalstates.Kubelet{
		KubeletBinPath:     filepath.Join("/"+goalstates.BinDir, "kubelet"),
		BootstrapToken:     cfg.Kubelet.BootstrapToken,
		APIServer:          cfg.Kubelet.ApiServer,
		CACertData:         caCert,
		ClusterDNS:         cfg.Cluster.ClusterDNS,
		NodeLabels:         labels,
		RegisterWithTaints: cfg.Kubelet.RegisterWithTaints,
	}, nil
}

// resolveOCIImage determines the OCI image to use for the nspawn rootfs.
//
// The priority is: configImage (from the agent config) >
// AGENT_DISABLE_OCI_IMAGE env var > AGENT_OCI_IMAGE env var > built-in
// default selected by GPU presence.
//
// When AGENT_DISABLE_OCI_IMAGE is set to a truthy value (e.g. "1", "true"),
// OCI-based rootfs provisioning is disabled and the agent falls back to
// debootstrap.
func resolveOCIImage(log *slog.Logger, configImage string, nvidiaGPUAvailable bool) string {
	if configImage != "" {
		return configImage
	}

	if disabled, err := strconv.ParseBool(os.Getenv("AGENT_DISABLE_OCI_IMAGE")); err == nil && disabled {
		log.Info("OCI image usage disabled via AGENT_DISABLE_OCI_IMAGE, falling back to debootstrap")
		return ""
	}

	if v := strings.TrimSpace(os.Getenv("AGENT_OCI_IMAGE")); v != "" {
		return v
	}

	var image string
	if nvidiaGPUAvailable {
		image = goalstates.DefaultNvidiaOCImage
	} else {
		image = goalstates.DefaultOCIImage
	}

	log.Info("no OCI image configured, using default", "image", image)

	return image
}
