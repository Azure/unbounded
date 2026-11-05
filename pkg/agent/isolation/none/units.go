// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// Agent-owned unit names. They differ from the distro names so the agent never
// edits or removes a unit it did not create.
const (
	UnitContainerd = "unbounded-containerd.service"
	UnitKubelet    = "unbounded-kubelet.service"
)

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
