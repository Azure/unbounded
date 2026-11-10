// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// Agent-owned unit names. They differ from the distro names so the agent never
// edits or removes a unit it did not create.
const (
	UnitContainerd = "unbounded-containerd.service"
	UnitKubelet    = "unbounded-kubelet.service"
	// UnitKubeletLaunchCheck is the allocation launch check the kubelet
	// requires on an allocation host; see RenderKubeletLaunchCheck.
	UnitKubeletLaunchCheck = "unbounded-kubelet-launch-check.service"
)

// kubeletLaunchCheckDropIn is the kubelet drop-in that requires the launch
// check.
const kubeletLaunchCheckDropIn = "10-launch-check.conf"

// ContainerdConfigPath is the agent-owned containerd configuration the
// containerd unit runs with. It is not the distro's /etc/containerd/config.toml,
// so a host-native node never writes or deletes a file a host package owns.
const ContainerdConfigPath = "/etc/unbounded/containerd/config.toml"

// SystemdDir is where the agent writes its units.
const SystemdDir = "/etc/systemd/system"

// UnitSpec holds what the unit templates need.
type UnitSpec struct {
	// BinDir is the directory the binaries are executed from, normally
	// Layout.BinDir so the units follow the current link.
	BinDir string
	// KubeletConfigPath is the kubelet configuration file. Empty means
	// DefaultKubeletConfigPath.
	KubeletConfigPath string
	// LocalDNS adds the CoreDNS unit and makes containerd and kubelet depend on
	// it.
	LocalDNS bool
	// ContainerdConfigPath is the configuration containerd runs with. Empty
	// means ContainerdConfigPath.
	ContainerdConfigPath string
	// GPUSetup are the commands containerd runs before each start to prepare
	// GPU support: the NVIDIA CDI spec names driver libraries by version, so it
	// is regenerated whenever the stack starts, a boot included, not only when
	// the agent starts it. Empty, on a node without GPUs, writes no drop-in.
	GPUSetup [][]string
	// LaunchCheckCommand is the command the launch-check oneshot runs before
	// every kubelet start, set only on an allocation host. It is a separate
	// oneshot so a final refusal fails the kubelet's start rather than loops
	// under the kubelet's Restart=always. Empty writes no launch check.
	LaunchCheckCommand string
}

// DefaultKubeletConfigPath is where the node-start phases write the kubelet
// configuration, the same path the nspawn machine uses.
const DefaultKubeletConfigPath = goalstates.KubeletConfigurationPath

// RenderContainerd returns the containerd unit.
func RenderContainerd(s UnitSpec) []byte {
	return fmt.Appendf(nil, `[Unit]
Description=containerd container runtime (unbounded agent)
Documentation=https://containerd.io
After=network.target local-fs.target

[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=%s/containerd --config %s
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=1
RestartMaxDelaySec=60
RestartSteps=10
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
TasksMax=infinity
OOMScoreAdjust=-999

[Install]
WantedBy=multi-user.target
`, s.BinDir, s.containerdConfig())
}

func (s UnitSpec) containerdConfig() string {
	if s.ContainerdConfigPath == "" {
		return ContainerdConfigPath
	}

	return s.ContainerdConfigPath
}

// RenderKubelet returns the kubelet unit. It orders after and wants the
// agent's containerd, never the distro's.
func RenderKubelet(s UnitSpec) []byte {
	return fmt.Appendf(nil, `[Unit]
Description=Kubelet (unbounded agent)
ConditionPathExists=%[1]s/kubelet
Wants=network-online.target %[3]s
After=network-online.target %[3]s

[Service]
Restart=always
RestartSec=1
RestartMaxDelaySec=60
RestartSteps=10
SuccessExitStatus=143
ExecStart=%[1]s/kubelet \
        --config=%[2]s \
        $KUBELET_NODE_CONFIG_ARGS \
        $KUBELET_TUNING_ARGS \
        $KUBELET_KUBECONFIG_ARGS \
        $KUBELET_EXTRA_ARGS

[Install]
WantedBy=multi-user.target
`, s.BinDir, s.KubeletConfigPath, UnitContainerd)
}

// RenderKubeletLaunchCheck returns the oneshot that runs the allocation launch
// check before every kubelet start on an allocation host. The kubelet requires
// it through RenderKubeletLaunchCheckDropIn. It is a separate unit, not an
// ExecStartPre, because RestartPreventExitStatus= applies only to a unit's main
// process: a final refusal must fail the kubelet's start rather than loop under
// its Restart=always. It has no [Install] section; only the kubelet pulls it in.
func RenderKubeletLaunchCheck(command string) []byte {
	return fmt.Appendf(nil, `# Managed by unbounded-agent. Do not edit directly.
[Unit]
Description=Allocation launch check for the kubelet (unbounded agent)
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
ExecStart=%s
Restart=on-failure
RestartMode=direct
RestartSec=10s
# A final allocation launch refusal must fail the start rather than loop.
RestartPreventExitStatus=%d
`, command, goalstates.AllocationLaunchRefusedExitCode)
}

// RenderKubeletLaunchCheckDropIn returns the kubelet drop-in that makes every
// kubelet start wait for, and depend on, the launch check.
func RenderKubeletLaunchCheckDropIn() []byte {
	return fmt.Appendf(nil, `# Managed by unbounded-agent. Do not edit directly.
[Unit]
Requires=%[1]s
After=%[1]s
`, UnitKubeletLaunchCheck)
}

// WriteUnits writes both units into dir and returns their paths. Writes are
// atomic so a crash never leaves a half written unit.
func WriteUnits(dir string, s UnitSpec) ([]string, error) {
	units := []struct {
		name string
		body []byte
	}{
		{UnitContainerd, RenderContainerd(s)},
		{UnitKubelet, RenderKubelet(s)},
	}

	var paths []string

	for _, u := range units {
		path := filepath.Join(dir, u.name)
		if err := fsutil.WriteFileAtomic(path, u.body, 0o644); err != nil {
			return paths, err
		}

		paths = append(paths, path)
	}

	return paths, nil
}

// RenderGPUSetupDropIn returns the containerd drop-in that runs each command
// before containerd starts. A failure fails the start, as it does when the
// agent runs the same setup, and Restart= retries it.
func RenderGPUSetupDropIn(cmds [][]string) []byte {
	out := []byte("# Managed by unbounded-agent. Do not edit directly.\n[Service]\n")
	for _, c := range cmds {
		out = fmt.Appendf(out, "ExecStartPre=%s\n", strings.Join(c, " "))
	}

	return out
}

// DropInDir is the drop-in directory of an agent-owned unit.
func DropInDir(dir, unit string) string { return filepath.Join(dir, unit+".d") }

// WriteDropIns writes the PATH drop-in for every unit, the LocalDNS ordering
// drop-ins for the node units, and the LocalDNS unit itself when s.LocalDNS is
// set. It returns the paths written.
func WriteDropIns(dir string, s UnitSpec) ([]string, error) {
	type file struct {
		path string
		body []byte
	}

	var files []file

	for _, u := range []string{UnitContainerd, UnitKubelet} {
		files = append(files, file{filepath.Join(DropInDir(dir, u), "00-path.conf"), RenderPathDropIn(s.BinDir)})
	}

	if s.LocalDNS {
		// The supervisor script lives in the version directory beside bin.
		supervisor := filepath.Join(filepath.Dir(s.BinDir), "libexec", "unbounded-localdns-supervisor")

		files = append(files, file{filepath.Join(dir, UnitLocalDNS), RenderLocalDNS(supervisor)})
		files = append(files, file{filepath.Join(DropInDir(dir, UnitLocalDNS), "00-path.conf"), RenderPathDropIn(s.BinDir)})

		for _, u := range []string{UnitContainerd, UnitKubelet} {
			files = append(files, file{filepath.Join(DropInDir(dir, u), "05-localdns.conf"), RenderLocalDNSDropIn()})
		}
	}

	// Without a command nothing is written and nothing is removed: a spec that
	// lacks it never takes the check away from a host that has it. Only
	// Destroy removes it.
	if s.LaunchCheckCommand != "" {
		files = append(files,
			file{filepath.Join(dir, UnitKubeletLaunchCheck), RenderKubeletLaunchCheck(s.LaunchCheckCommand)},
			file{filepath.Join(DropInDir(dir, UnitKubelet), kubeletLaunchCheckDropIn), RenderKubeletLaunchCheckDropIn()},
		)
	}

	gpuDropIn := filepath.Join(DropInDir(dir, UnitContainerd), "10-gpu-setup.conf")
	if len(s.GPUSetup) > 0 {
		files = append(files, file{gpuDropIn, RenderGPUSetupDropIn(s.GPUSetup)})
	} else if err := os.Remove(gpuDropIn); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if !s.LocalDNS {
		// A reconfigured node that no longer uses LocalDNS must not keep units
		// and ordering from the previous configuration.
		stale := []string{
			filepath.Join(dir, UnitLocalDNS),
			filepath.Join(DropInDir(dir, UnitContainerd), "05-localdns.conf"),
			filepath.Join(DropInDir(dir, UnitKubelet), "05-localdns.conf"),
		}
		for _, path := range stale {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}

	var paths []string

	for _, f := range files {
		if err := fsutil.WriteFileAtomic(f.path, f.body, 0o644); err != nil {
			return paths, err
		}

		paths = append(paths, f.path)
	}

	return paths, nil
}
