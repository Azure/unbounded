package cmd

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases/host"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases/nodestart"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases/rootfs"
	"github.com/project-unbounded/unbounded-kube/internal/provision"
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

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			rootFSGoalState, err := resolveRootFSGoalState(cfg)
			if err != nil {
				return err
			}

			nodeStartGoalState, err := resolveNodeStartGoalState(cfg)
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
				nodestart.InstallKernelHeader(log, nodeStartGoalState),
				nodestart.StartContainerd(log, nodeStartGoalState),
				nodestart.StartKubelet(log, nodeStartGoalState),
			).Do(ctx)
		},
	}

	return cmd
}

// ref: cmd/machina/machina/controller/machine_controller.go
func resolveRootFSGoalState(cfg *provision.AgentConfig) (*goalstates.RootFS, error) {
	// TODO: investigate whether the rootfs name can be decoupled from the
	// machine name. Using a fixed rootfs name (e.g. "node") would simplify
	// tool invocations (machinectl, systemctl) and allow the nspawn unit to
	// be templated once. For now we derive it from the machine name so the
	// rootfs identity matches what the controller expects.
	machineName := cfg.MachineName
	kubeVersion := cfg.Cluster.Version

	kernel, err := hostKernel() //nolint:staticcheck // SA4023: non-Linux stub always errors; this is intentional.
	if err != nil {             //nolint:staticcheck // SA4023: see above.
		return nil, err
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("get host hostname: %w", err)
	}

	// Prefer OCIImage from config; fall back to AGENT_OCI_IMAGE env var
	// for backward compatibility with older bootstrap scripts.
	ociImage := cfg.OCIImage
	if ociImage == "" {
		ociImage = strings.TrimSpace(os.Getenv("AGENT_OCI_IMAGE"))
	}

	return &goalstates.RootFS{
		MachineDir: filepath.Join("/var/lib/machines", machineName),
		NSpawnConfigFile: filepath.Join(
			goalstates.SystemdNSpawnDir,
			machineName+".nspawn",
		),
		ServiceOverrideFile: filepath.Join(
			goalstates.SystemdSystemDir,
			fmt.Sprintf("systemd-nspawn@%s.service.d", machineName),
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
	}, nil
}

// ref: cmd/machina/machina/controller/machine_controller.go
func resolveNodeStartGoalState(cfg *provision.AgentConfig) (*goalstates.NodeStart, error) {
	machineName := cfg.MachineName

	kernel, err := hostKernel() //nolint:staticcheck // SA4023: non-Linux stub always errors; this is intentional.
	if err != nil {             //nolint:staticcheck // SA4023: see above.
		return nil, err
	}

	kubelet, err := resolveKubeletGoalState(cfg)
	if err != nil {
		return nil, err
	}

	return &goalstates.NodeStart{
		MachineName: machineName,
		MachineDir:  filepath.Join("/var/lib/machines", machineName),
		HostKernel:  kernel,
		Containerd:  goalstates.DefaultContainerd(),
		Kubelet:     kubelet,
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
