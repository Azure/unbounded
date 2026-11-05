// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/isolationtest"
)

// fakeRuntime stands in for systemd and runc: it tracks which supervising
// units are active and answers the commands the backend makes.
type fakeRuntime struct {
	mu     sync.Mutex
	active map[string]bool  // unit name
	fail   map[string]error // keyed by the systemctl verb
}

func (f *fakeRuntime) run(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if name == "systemctl" {
		return f.systemctl(args)
	}

	switch args[0] {
	case "state":
		slot := strings.TrimPrefix(args[1], "unbounded-")
		if !f.active[UnitName(slot)] {
			return "", errors.New("container does not exist")
		}

		return `{"status":"running"}`, nil
	case "exec":
		slot := strings.TrimPrefix(args[1], "unbounded-")
		if !f.active[UnitName(slot)] {
			return "", errors.New("container not running")
		}

		switch args[2] {
		case "echo":
			return strings.Join(args[3:], " "), nil
		case "false":
			return "", errors.New("exit status 1")
		}

		return "", nil
	case "delete":
		return "", nil
	}

	return "", errors.New("unexpected runtime call " + strings.Join(args, " "))
}

func (f *fakeRuntime) systemctl(args []string) (string, error) {
	verb := args[0]
	if err := f.fail[verb]; err != nil {
		delete(f.fail, verb)
		return "", err
	}

	unit := args[len(args)-1]

	switch verb {
	case "enable", "restart":
		f.active[unit] = true
	case "stop", "disable":
		delete(f.active, unit)
	case "is-active":
		if !f.active[unit] {
			return "", errors.New("inactive")
		}
	}

	return "", nil
}

// TestConformance runs the shared suite against the backend as the registry
// builds it, with systemd and the runtime stubbed.
func TestConformance(t *testing.T) {
	isolationtest.Run(t, func(t *testing.T) (isolation.Backend, isolationtest.Options) {
		f := &fakeRuntime{active: map[string]bool{}, fail: map[string]error{}}
		dir := t.TempDir()

		deps := &Backend{
			Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
			Layout:        Layout{Base: filepath.Join(dir, "containers")},
			SystemdDir:    filepath.Join(dir, "systemd"),
			RuntimePath:   "/opt/unbounded/runtime/runc",
			HelperPath:    "/usr/local/bin/unbounded-agent",
			Run:           f.run,
			SourceRoot:    filepath.Join(dir, "host"),
			MountInfoPath: filepath.Join(dir, "mountinfo"),
			Host:          &HostFS{SELinuxEnforce: filepath.Join(dir, "absent")},
			Stream: func(context.Context, string, ...string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("log line\n")), nil
			},
			Unpack: func(_ context.Context, _ isolation.NodeSpec, d string) error {
				return os.MkdirAll(filepath.Join(d, "etc"), 0o755)
			},
			StatDevice: func(p string) (DeviceInfo, error) {
				if d, ok := nvidiaDevices[p]; ok {
					return d, nil
				}

				return DeviceInfo{}, os.ErrNotExist
			},
		}
		require.NoError(t, os.MkdirAll(filepath.Join(deps.SourceRoot, "dev/nvidia-caps"), 0o755))
		require.NoError(t, os.WriteFile(deps.MountInfoPath, nil, 0o644))

		return deps, isolationtest.Options{
			Spec:         isolation.NodeSpec{Slot: "kube1", Version: "v1.34.0"},
			ExecArgs:     []string{"echo", "hello"},
			ExecWant:     "hello",
			ExecFailArgs: []string{"false"},
			// The NVIDIA set the machine asks for, as devices, a bound device
			// directory and read-only mounts.
			Grants: nvidiaGrants,
			RefusedGrants: []isolation.Grant{
				{Kind: isolation.GrantMount, Source: "/x", Target: "/var/lib/kubelet"},
				{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, Source: "/dev/absent"},
				{Kind: isolation.GrantMount, Source: "relative"},
			},
			Fail: func(op isolationtest.Op, err error) {
				verb := map[isolationtest.Op]string{
					isolationtest.OpPrepare: "daemon-reload",
					isolationtest.OpStart:   "enable",
					isolationtest.OpStop:    "stop",
					isolationtest.OpRestart: "restart",
				}[op]

				f.mu.Lock()
				f.fail[verb] = err
				f.mu.Unlock()
			},
		}
	})
}
