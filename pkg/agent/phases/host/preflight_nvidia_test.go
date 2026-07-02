// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestCheckNvidiaDriverNoHardware(t *testing.T) {
	t.Parallel()

	env := newNvidiaPreflightTestEnv(t)
	results := checkNvidiaDriver(slog.New(slog.DiscardHandler), env.deps()).Check(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
	assert.Contains(t, results[0].Message, "no NVIDIA GPU hardware detected")
}

func TestCheckNvidiaDriverMissingDriverCollectsErrors(t *testing.T) {
	t.Parallel()

	env := newNvidiaPreflightTestEnv(t)
	env.addNvidiaPCI("0001:00:00.0", "")
	env.paths["ldconfig"] = true
	env.outputs["ldconfig"] = []byte("")

	results := checkNvidiaDriver(slog.New(slog.DiscardHandler), env.deps()).Check(context.Background())

	assertHasNvidiaResult(t, results, preflight.SeverityOK, "NVIDIA PCI hardware", "NVIDIA GPU hardware detected")
	assertHasNvidiaResult(t, results, preflight.SeverityError, "NVIDIA PCI driver", "0 of 1")
	assertHasNvidiaResult(t, results, preflight.SeverityError, "NVIDIA kernel modules", "required NVIDIA kernel modules are not loaded")
	assertHasNvidiaResult(t, results, preflight.SeverityError, "NVIDIA device nodes", "required NVIDIA device nodes are missing")
	assertHasNvidiaResult(t, results, preflight.SeverityError, "NVIDIA driver libraries", "libcuda.so.1")
	assertHasNvidiaResult(t, results, preflight.SeverityError, "NVIDIA driver libraries", "libnvidia-ml.so.1")
	assertHasNvidiaResult(t, results, preflight.SeverityWarning, "NVIDIA diagnostic tooling", "nvidia-smi is not installed")
}

func TestCheckNvidiaDriverAvailable(t *testing.T) {
	t.Parallel()

	env := newNvidiaPreflightTestEnv(t)
	env.addNvidiaPCI("0001:00:00.0", "../../../bus/pci/drivers/nvidia")

	for _, module := range nvidiaRequiredModules {
		env.mkdir(filepath.Join(env.moduleDir, module))
	}

	for _, name := range []string{
		"nvidiactl",
		"nvidia0",
		"nvidia-modeset",
		"nvidia-uvm",
		"nvidia-uvm-tools",
	} {
		env.touch(filepath.Join(env.devDir, name))
	}

	env.mkdir(filepath.Join(env.devDir, "nvidia-caps"))
	env.touch(filepath.Join(env.devDir, "nvidia-caps", "nvidia-cap1"))
	env.mkdir(filepath.Join(env.devDir, "dri"))
	env.touch(filepath.Join(env.devDir, "dri", "renderD128"))

	libDir := filepath.Join(env.root, "lib", "x86_64-linux-gnu")
	env.mkdir(libDir)

	cuda := filepath.Join(libDir, "libcuda.so.1")
	nvml := filepath.Join(libDir, "libnvidia-ml.so.1")

	env.touch(cuda)
	env.touch(nvml)
	env.outputs["ldconfig"] = []byte(fmt.Sprintf("\tlibcuda.so.1 (libc6,%s) => %s\n\tlibnvidia-ml.so.1 (libc6,%s) => %s\n", nvidiaLdconfigArchTag(), cuda, nvidiaLdconfigArchTag(), nvml))
	env.outputs["modinfo"] = []byte("580.159.03\n")
	env.outputs["nvidia-smi"] = []byte("GPU 0: Tesla T4\n")
	env.outputs["systemctl list-unit-files nvidia-persistence-mode.service"] = []byte("nvidia-persistence-mode.service enabled enabled\n")
	env.outputs["systemctl is-active nvidia-persistence-mode.service"] = []byte("active\n")
	env.paths["modinfo"] = true
	env.paths["ldconfig"] = true
	env.paths["nvidia-smi"] = true
	env.paths["systemctl"] = true

	results := checkNvidiaDriver(slog.New(slog.DiscardHandler), env.deps()).Check(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
	assert.Equal(t, checkNvidiaDriverName, results[0].Name)
	assert.Contains(t, results[0].Message, "NVIDIA driver stack is available")
}

func assertHasNvidiaResult(t *testing.T, results []preflight.Result, severity preflight.Severity, target, message string) {
	t.Helper()

	for _, result := range results {
		if result.Name == checkNvidiaDriverName && result.Severity == severity && result.Target == target && strings.Contains(result.Message, message) {
			return
		}
	}

	t.Fatalf("missing NVIDIA result severity=%s target=%q containing %q in %#v", severity, target, message, results)
}

type nvidiaPreflightTestEnv struct {
	root      string
	pciDir    string
	devDir    string
	moduleDir string
	paths     map[string]bool
	outputs   map[string][]byte
}

func newNvidiaPreflightTestEnv(t *testing.T) *nvidiaPreflightTestEnv {
	t.Helper()

	root := t.TempDir()
	env := &nvidiaPreflightTestEnv{
		root:      root,
		pciDir:    filepath.Join(root, "sys", "bus", "pci", "devices"),
		devDir:    filepath.Join(root, "dev"),
		moduleDir: filepath.Join(root, "sys", "module"),
		paths:     map[string]bool{},
		outputs:   map[string][]byte{},
	}
	env.mkdir(env.pciDir)
	env.mkdir(env.devDir)
	env.mkdir(env.moduleDir)

	return env
}

func (e *nvidiaPreflightTestEnv) deps() nvidiaDriverDeps {
	return nvidiaDriverDeps{
		pciDevicesDir: e.pciDir,
		devDir:        e.devDir,
		moduleDir:     e.moduleDir,
		readDir:       os.ReadDir,
		readFile:      os.ReadFile,
		readLink:      os.Readlink,
		stat:          os.Stat,
		lookPath: func(name string) (string, error) {
			if e.paths[name] {
				return name, nil
			}

			return "", errors.New("not found")
		},
		outputCmd: func(_ context.Context, name string, args ...string) ([]byte, error) {
			key := strings.Join(append([]string{filepath.Base(name)}, args...), " ")
			if out, ok := e.outputs[key]; ok {
				return out, nil
			}

			if out, ok := e.outputs[filepath.Base(name)]; ok {
				return out, nil
			}

			return nil, fmt.Errorf("unexpected command: %s", key)
		},
	}
}

func (e *nvidiaPreflightTestEnv) addNvidiaPCI(addr, driverTarget string) {
	path := filepath.Join(e.pciDir, addr)
	e.mkdir(path)
	e.write(filepath.Join(path, "vendor"), "0x10de\n")
	e.write(filepath.Join(path, "class"), "0x030200\n")

	if driverTarget != "" {
		if err := os.Symlink(driverTarget, filepath.Join(path, "driver")); err != nil {
			panic(err)
		}
	}
}

func (e *nvidiaPreflightTestEnv) mkdir(path string) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		panic(err)
	}
}

func (e *nvidiaPreflightTestEnv) touch(path string) {
	e.write(path, "")
}

func (e *nvidiaPreflightTestEnv) write(path, content string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		panic(err)
	}
}
