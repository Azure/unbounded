package host

import (
	"context"
	"log/slog"
	"os/exec"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/utilexec"
)

// kvmModules lists the KVM kernel modules loaded on the host. The base "kvm"
// module is always required; exactly one of the CPU-vendor modules must also
// succeed.
//
// Module loading is best-effort: on a host that already has the modules loaded
// (or compiled into the kernel) the modprobe calls are no-ops. On a host
// without KVM support (e.g. a VM without nested virtualization) the loads
// fail silently and /dev/kvm simply won't exist — the KVM device discovery
// in the goalstate will then return an empty list and no bind-mount is
// generated.
var kvmModules = struct {
	base     string
	vendorOr []string // at least one must succeed (or already be loaded)
}{
	base:     "kvm",
	vendorOr: []string{"kvm_intel", "kvm_amd"},
}

type loadKVMModules struct {
	log *slog.Logger
}

// LoadKVMModules returns a task that loads the KVM kernel modules on the host.
// This must run before the nspawn container starts so that /dev/kvm is
// available for bind-mounting. If the host does not support KVM (no hardware
// virtualization or modules unavailable) the task succeeds silently —
// downstream device discovery will simply find no /dev/kvm.
func LoadKVMModules(log *slog.Logger) phases.Task {
	return &loadKVMModules{log: log}
}

func (l *loadKVMModules) Name() string { return "load-kvm-modules" }

func (l *loadKVMModules) Do(ctx context.Context) error {
	modprobe := modprobeCmd()

	// Load the base kvm module. If it fails the vendor modules will also
	// fail, so we log and return early.
	if err := utilexec.RunCmd(ctx, l.log, modprobe, kvmModules.base); err != nil {
		l.log.Info("kvm module not available (host may lack KVM support), skipping",
			"error", err)
		return nil
	}

	// Try each vendor-specific module. Exactly one should succeed on any
	// given CPU; failures for the other vendor are expected.
	vendorLoaded := false

	for _, mod := range kvmModules.vendorOr {
		if err := utilexec.RunCmd(ctx, l.log, modprobe, mod); err != nil {
			l.log.Debug("vendor KVM module not available",
				"module", mod, "error", err)
			continue
		}

		l.log.Info("loaded KVM vendor module", "module", mod)

		vendorLoaded = true

		break
	}

	if !vendorLoaded {
		l.log.Warn("no vendor KVM module loaded (kvm_intel or kvm_amd); " +
			"/dev/kvm may not be functional")
	}

	return nil
}

// modprobeCmd returns a command factory for modprobe.
func modprobeCmd() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "modprobe")
	}
}
