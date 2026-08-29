// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	// SystemExtensionDir is where systemd-sysext looks for persistent
	// extensions. Extensions merged from here are re-merged on every boot by
	// systemd-sysext.service.
	SystemExtensionDir = "/var/lib/extensions"

	// systemExtensionSuffix is the required file extension. The image must be a
	// disk image rather than a directory tree: a directory extension is merged
	// by bind-mounting it out of /var/lib/extensions, which carries SELinux
	// xattrs from the writable root, and binaries with that label cannot
	// acquire a D-Bus name under SELinux Enforcing. The failure is silent,
	// because the relevant denials are dontaudit'ed.
	systemExtensionSuffix = ".raw"
)

type installSystemExtension struct {
	log *slog.Logger
	cfg config.AgentConfig
}

// InstallSystemExtension returns a task that installs and merges a configured
// systemd system extension.
//
// It must run before any task that needs the tools the extension provides. On a
// host with no package manager the extension is the only way systemd-nspawn can
// appear, so this runs ahead of package installation.
func InstallSystemExtension(log *slog.Logger, cfg config.AgentConfig) phases.Task {
	return &installSystemExtension{log: log, cfg: cfg}
}

func (i *installSystemExtension) Name() string { return "install-system-extension" }

func (i *installSystemExtension) Do(ctx context.Context) error {
	if !i.cfg.SystemExtensionConfigured() {
		i.log.Debug("no system extension configured")

		return nil
	}

	ext := i.cfg.SystemExtension
	target := filepath.Join(SystemExtensionDir, ext.Name+systemExtensionSuffix)

	// systemd-sysext matches only on ID and SYSEXT_LEVEL, so it will merge an
	// extension whose binaries cannot load, and the failure surfaces much later
	// as a loader error. Everything below is gated on the running systemd.
	hostVersion, err := hostSystemdVersion(ctx, i.log)
	if err != nil {
		return err
	}

	// Check what is already installed before reaching for the source. An
	// extension that is present and compatible needs nothing, and requiring the
	// source to still be reachable would make a correctly provisioned host fail
	// a re-run whenever the source was transient, such as a file staged under
	// /tmp that a reboot cleared.
	installed, err := installedSystemExtensionProvenance(target)
	if err != nil {
		return err
	}

	if installed != nil {
		if err := CheckSysextCompatibility(*installed, hostVersion); err == nil {
			i.log.Info("system extension already installed",
				"name", ext.Name, "systemd", installed.SystemdNVR)

			return nil
		}

		i.log.Info("installed system extension is not compatible with the running systemd, replacing",
			"name", ext.Name, "installed", installed.SystemdNVR, "host", hostVersion)
	}

	provenance, err := fetchSystemExtensionProvenance(ctx, ext.Source)
	if err != nil {
		return err
	}

	// Refuse before touching the host.
	if err := CheckSysextCompatibility(provenance, hostVersion); err != nil {
		return err
	}

	if err := os.MkdirAll(SystemExtensionDir, 0o755); err != nil {
		return fmt.Errorf("create system extension directory: %w", err)
	}

	if err := downloadSystemExtension(ctx, ext.Source, target); err != nil {
		return err
	}

	if err := writeSystemExtensionProvenance(target, provenance); err != nil {
		return err
	}

	i.log.Info("merging system extension", "name", ext.Name, "systemd", provenance.SystemdNVR)

	if err := executil.RunCmd(ctx, i.log, executil.SystemdSysext(), "refresh"); err != nil {
		return fmt.Errorf("systemd-sysext refresh: %w", err)
	}

	// PID 1 must pick up the unit files the extension merged into /usr,
	// notably systemd-nspawn@.service.
	if err := executil.RunCmd(ctx, i.log, executil.Systemctl(), "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload after merging system extension: %w", err)
	}

	return i.verifyExtensionUsable(ctx)
}

// verifyExtensionUsable checks that the tools the extension merged can actually
// be used, and reports what to do when they cannot.
//
// Merging an extension makes its binaries visible immediately, but D-Bus loads
// its policy at startup. An extension that adds a D-Bus service, as
// systemd-container does with org.freedesktop.machine1, is therefore not usable
// by the already-running dbus-daemon: activation is refused, systemd-machined
// cannot acquire its name, and reloading dbus is itself denied. A reboot merges
// the extension before dbus starts and resolves it permanently.
//
// Detect that here rather than letting bootstrap continue, because the symptom
// otherwise surfaces much later as "machinectl enable: Access denied", which
// gives no indication that a reboot is all that is required.
func (i *installSystemExtension) verifyExtensionUsable(ctx context.Context) error {
	if _, err := executil.OutputCmd(ctx, i.log, "machinectl", "list"); err != nil {
		return fmt.Errorf(
			"system extension %q was merged but its D-Bus services are not usable until the host reboots, "+
				"because dbus loads its policy at startup; reboot and run bootstrap again: %w",
			i.cfg.SystemExtension.Name, err,
		)
	}

	return nil
}

// provenanceSuffix is appended to the installed image path to record what the
// extension was built against, so a later run can tell whether it changed.
const provenanceSuffix = ".provenance"

func systemExtensionProvenanceSource(source string) string {
	return strings.TrimSuffix(source, systemExtensionSuffix) + provenanceSuffix
}

func fetchSystemExtensionProvenance(ctx context.Context, source string) (SysextProvenance, error) {
	parsed, err := artifactsource.Parse(systemExtensionProvenanceSource(source))
	if err != nil {
		return SysextProvenance{}, fmt.Errorf("parse system extension provenance source: %w", err)
	}

	data, err := parsed.ReadAll(ctx)
	if err != nil {
		return SysextProvenance{}, fmt.Errorf("read system extension provenance: %w", err)
	}

	return ParseSysextProvenance(string(data))
}

func installedSystemExtensionProvenance(target string) (*SysextProvenance, error) {
	data, err := os.ReadFile(target + provenanceSuffix)
	if os.IsNotExist(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read installed system extension provenance: %w", err)
	}

	provenance, err := ParseSysextProvenance(string(data))
	if err != nil {
		// A corrupt record should force a reinstall rather than fail the host.
		return nil, nil //nolint:nilerr // treated as "not installed"
	}

	return &provenance, nil
}

func writeSystemExtensionProvenance(target string, provenance SysextProvenance) error {
	record := fmt.Sprintf(
		"SYSEXT_NAME=%s\nSYSTEMD_CONTAINER_NVR=%s\nSYSTEMD_NVR=%s\nBUNDLED_SHARED_LIB=%s\n",
		provenance.Name, provenance.SystemdContainerNVR, provenance.SystemdNVR, provenance.BundledSharedLib,
	)

	if err := os.WriteFile(target+provenanceSuffix, []byte(record), 0o644); err != nil {
		return fmt.Errorf("write system extension provenance: %w", err)
	}

	return nil
}

func downloadSystemExtension(ctx context.Context, source, target string) error {
	parsed, err := artifactsource.Parse(source)
	if err != nil {
		return fmt.Errorf("parse system extension source: %w", err)
	}

	checksumSource, err := artifactsource.Parse(source + ".sha256")
	if err != nil {
		return fmt.Errorf("parse system extension checksum source: %w", err)
	}

	expected, err := artifactsource.ReadExpectedSHA256(ctx, checksumSource)
	if err != nil {
		return fmt.Errorf("read system extension checksum: %w", err)
	}

	// The extension is merged into /usr and executed by PID 1, so it is
	// verified rather than merely downloaded.
	if err := parsed.DownloadWithSHA256Verification(ctx, expected, target, 0o644); err != nil {
		return fmt.Errorf("download system extension: %w", err)
	}

	return nil
}

// hostSystemdVersion returns the version string reported by systemctl.
func hostSystemdVersion(ctx context.Context, log *slog.Logger) (string, error) {
	out, err := executil.OutputCmd(ctx, log, "systemctl", "--version")
	if err != nil {
		return "", fmt.Errorf("determine host systemd version: %w", err)
	}

	line, _, _ := strings.Cut(out, "\n")

	return strings.TrimSpace(line), nil
}
