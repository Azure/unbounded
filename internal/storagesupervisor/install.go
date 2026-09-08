// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package storagesupervisor

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

// daemonBinaryRel is the path, relative to a release-layout root, of the daemon
// binary that must be present after extraction.
const daemonBinaryRel = "bin/unbounded-storage"

// Preconditions verifies the host-level requirements for an install: the
// process must run as root (it writes under the host prefix and the systemd
// unit directory) and the configured systemctl command must be resolvable.
func Preconditions(cfg Config) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("install must run as root (it writes to %s and the systemd unit directory)", cfg.Prefix)
	}

	if len(cfg.Systemctl) == 0 {
		return fmt.Errorf("SYSTEMCTL must not be empty")
	}

	if _, err := exec.LookPath(cfg.Systemctl[0]); err != nil {
		return fmt.Errorf("systemctl command %q not found in PATH: %w", cfg.Systemctl[0], err)
	}

	return nil
}

// Install runs the full install workflow using the real systemctl runner.
func Install(ctx context.Context, cfg Config) error {
	return InstallWithRunner(ctx, cfg, execRunner{})
}

// InstallWithRunner runs the install workflow, delegating systemctl calls to
// runner. It acquires and verifies the release tarball, stages it into a
// versioned directory under the host prefix, atomically flips the "current"
// symlink, writes the systemd unit, and reloads/starts the service.
func InstallWithRunner(ctx context.Context, cfg Config, runner CommandRunner) error {
	staging, err := os.MkdirTemp("", "unbounded-storage-stage-")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}

	defer os.RemoveAll(staging) //nolint:errcheck // best-effort cleanup

	if err := acquireAndExtract(ctx, cfg, staging); err != nil {
		return err
	}

	if err := validatePayload(staging); err != nil {
		return err
	}

	releaseDir, err := stageRelease(cfg, staging)
	if err != nil {
		return err
	}

	if err := flipCurrent(cfg); err != nil {
		return err
	}

	unitPath, err := writeUnit(cfg)
	if err != nil {
		return err
	}

	slog.Info("installed unbounded-storage",
		"release", releaseDir,
		"unit", unitPath,
		"config", cfg.ConfigPath,
		"version", cfg.Version,
		"arch", cfg.Arch,
	)

	return reloadAndStart(ctx, cfg, runner)
}

// validatePayload ensures the extracted staging tree contains an executable
// daemon binary at the expected path.
func validatePayload(staging string) error {
	bin := filepath.Join(staging, daemonBinaryRel)

	info, err := os.Stat(bin)
	if err != nil {
		return fmt.Errorf("staged payload missing %s; layout changed? %w", daemonBinaryRel, err)
	}

	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("staged %s is not an executable file", daemonBinaryRel)
	}

	return nil
}

// releaseDirName returns the versioned release directory name for the config.
func releaseDirName(cfg Config) string {
	return cfg.Version + "-" + cfg.Arch
}

// stageRelease copies the extracted payload into a fresh versioned release
// directory under the host prefix, returning the host-mount path it was written
// to.
func stageRelease(cfg Config, staging string) (string, error) {
	releaseDir := filepath.Join(cfg.HostRoot, cfg.Prefix, "releases", releaseDirName(cfg))

	if err := os.RemoveAll(releaseDir); err != nil {
		return "", fmt.Errorf("clear release dir %q: %w", releaseDir, err)
	}

	if err := copyTree(staging, releaseDir); err != nil {
		return "", fmt.Errorf("copy payload into %q: %w", releaseDir, err)
	}

	slog.Info("staged release", "path", releaseDir)

	return releaseDir, nil
}

// flipCurrent atomically points Prefix/current at the freshly staged release.
// The symlink file is created under HostRoot, but its target is the
// host-absolute release path so host systemd resolves it correctly.
func flipCurrent(cfg Config) error {
	currentLink := filepath.Join(cfg.HostRoot, cfg.Prefix, "current")
	target := filepath.Join(cfg.Prefix, "releases", releaseDirName(cfg))

	if err := os.MkdirAll(filepath.Dir(currentLink), 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(currentLink), err)
	}

	tmp := currentLink + ".tmp"

	_ = os.Remove(tmp) //nolint:errcheck // best-effort cleanup of stale temp link

	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("create symlink %q: %w", tmp, err)
	}

	if err := os.Rename(tmp, currentLink); err != nil {
		_ = os.Remove(tmp) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("activate symlink %q -> %q: %w", currentLink, target, err)
	}

	slog.Info("activated release", "link", currentLink, "target", target)

	return nil
}

// copyTree recursively copies the directory tree rooted at src into dst,
// preserving file modes and symlinks.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}

			return os.MkdirAll(target, info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %q: %w", path, err)
			}

			return os.Symlink(link, target)
		default:
			return copyFile(path, target)
		}
	})
}

// copyFile copies a single regular file from src to dst, preserving its mode.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}

	defer in.Close() //nolint:errcheck // read-only close

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close() //nolint:errcheck // best-effort on error path
		return err
	}

	return out.Close()
}
