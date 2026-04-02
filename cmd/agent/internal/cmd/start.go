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

			rootFSGoalState, err := resolveRootFSGoalState()
			if err != nil {
				return err
			}

			nodeStartGoalState, err := resolveNodeStartGoalState()
			if err != nil {
				return err
			}

			log := cmdCtx.Logger

			return phases.Serial(log,
				// Phase 1: host
				host.InstallPackages(log),
				host.ConfigureOS(log),

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
func resolveRootFSGoalState() (*goalstates.RootFS, error) {
	// TODO: investigate whether the rootfs name can be decoupled from the
	// machine name. Using a fixed rootfs name (e.g. "node") would simplify
	// tool invocations (machinectl, systemctl) and allow the nspawn unit to
	// be templated once. For now we derive it from the machine name so the
	// rootfs identity matches what the controller expects.
	machineName, err := requiredEnv("MACHINA_MACHINE_NAME")
	if err != nil {
		return nil, err
	}

	kubeVersion, err := requiredEnv("KUBE_VERSION")
	if err != nil {
		return nil, err
	}

	kernel, err := hostKernel() //nolint:staticcheck // SA4023: non-Linux stub always errors; this is intentional.
	if err != nil {             //nolint:staticcheck // SA4023: see above.
		return nil, err
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("get host hostname: %w", err)
	}

	// allow passing in through env var
	ociImage := strings.TrimSpace(os.Getenv("AGENT_OCI_IMAGE"))

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
func resolveNodeStartGoalState() (*goalstates.NodeStart, error) {
	machineName, err := requiredEnv("MACHINA_MACHINE_NAME")
	if err != nil {
		return nil, err
	}

	kernel, err := hostKernel() //nolint:staticcheck // SA4023: non-Linux stub always errors; this is intentional.
	if err != nil {             //nolint:staticcheck // SA4023: see above.
		return nil, err
	}

	kubelet, err := resolveKubeletGoalState()
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

func resolveKubeletGoalState() (goalstates.Kubelet, error) {
	var zero goalstates.Kubelet

	apiServer, err := requiredEnv("API_SERVER")
	if err != nil {
		return zero, err
	}
	// FIXME: should we set the scheme in machina side?
	if !strings.HasPrefix(apiServer, "https://") {
		apiServer = "https://" + apiServer
	}

	bootstrapToken, err := requiredEnv("BOOTSTRAP_TOKEN")
	if err != nil {
		return zero, err
	}

	caCertB64, err := requiredEnv("CA_CERT_BASE64")
	if err != nil {
		return zero, err
	}

	caCert, err := base64.StdEncoding.DecodeString(caCertB64)
	if err != nil {
		return zero, fmt.Errorf("decode CA_CERT_BASE64: %w", err)
	}

	clusterDNS, err := requiredEnv("CLUSTER_DNS")
	if err != nil {
		return zero, err
	}

	// NODE_LABELS is optional; parse "key1=val1,key2=val2" format.
	labels := make(map[string]string)

	if raw := strings.TrimSpace(os.Getenv("NODE_LABELS")); raw != "" {
		for _, pair := range strings.Split(raw, ",") {
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				return zero, fmt.Errorf("invalid NODE_LABELS entry %q", pair)
			}

			labels[k] = v
		}
	}

	return goalstates.Kubelet{
		KubeletBinPath: filepath.Join("/"+goalstates.BinDir, "kubelet"),
		BootstrapToken: bootstrapToken,
		APIServer:      apiServer,
		CACertData:     caCert,
		ClusterDNS:     clusterDNS,
		NodeLabels:     labels,
	}, nil
}
