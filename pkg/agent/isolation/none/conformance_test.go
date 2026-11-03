// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/isolationtest"
)

// hostUnits stands in for systemd: it tracks which units are active and
// answers the systemctl calls the backend makes. Other commands run for real,
// which is what Exec means for a node stack that is the host.
type hostUnits struct {
	mu     sync.Mutex
	active map[string]bool
	fail   map[string]error // keyed by the systemctl verb
}

func (h *hostUnits) run(ctx context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	if name != "systemctl" {
		out, err := exec.CommandContext(ctx, name, args...).Output()
		return strings.TrimRight(string(out), "\n"), err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	verb := args[0]
	if err := h.fail[verb]; err != nil {
		delete(h.fail, verb)
		return "", err
	}

	units := args[1:]

	switch verb {
	case "enable":
		// enable --now <unit>
		h.active[units[len(units)-1]] = true
	case "restart":
		h.active[units[0]] = true
	case "stop", "disable":
		delete(h.active, units[len(units)-1])
	case "is-active":
		if !h.active[units[len(units)-1]] {
			return "", &exec.ExitError{}
		}
	}

	return "", nil
}

// TestConformance runs the shared suite with systemd and the host journal
// stubbed.
func TestConformance(t *testing.T) {
	isolationtest.Run(t, func(t *testing.T) (isolation.Backend, isolationtest.Options) {
		h := &hostUnits{active: map[string]bool{}, fail: map[string]error{}}

		bin := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(bin, "journalctl"), []byte("#!/bin/sh\necho log line\n"), 0o755))
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

		hostRoot, grants := gpuHost(t)

		b := &Backend{
			Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
			Layout:       Layout{Base: filepath.Join(t.TempDir(), "node")},
			SystemdDir:   t.TempDir(),
			Run:          h.run,
			Units:        UnitSpec{KubeletConfigPath: "/var/lib/kubelet/config.yaml"},
			HostRoot:     hostRoot,
			GPUHostPaths: []string{"/usr/bin/nvidia-ctk", "/usr/bin/nvidia-container-runtime"},
		}

		return b, isolationtest.Options{
			Spec:         isolation.NodeSpec{Slot: "v1.34.0", Version: "v1.34.0"},
			ExecArgs:     []string{"echo", "hello"},
			ExecWant:     "hello",
			ExecFailArgs: []string{"false"},
			// The NVIDIA set from a tree standing in for a GPU host, which the
			// host-native stack satisfies natively.
			Grants: grants,
			RefusedGrants: []isolation.Grant{
				{Kind: isolation.GrantMount, Purpose: isolation.PurposeHostMount, Source: "/usr/bin", Target: "/elsewhere"},
				{Kind: isolation.GrantLibraryPath, Source: "/usr/lib"},
				{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, Source: "/dev/absent"},
			},
			Fail: func(op isolationtest.Op, err error) {
				verb := map[isolationtest.Op]string{
					isolationtest.OpPrepare: "daemon-reload",
					isolationtest.OpStart:   "enable",
					isolationtest.OpStop:    "stop",
					isolationtest.OpRestart: "restart",
				}[op]

				h.mu.Lock()
				h.fail[verb] = err
				h.mu.Unlock()
			},
		}
	})
}
