// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package agentbinary installs unbounded-agent binaries from release archives.
package agentbinary

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

const verifyTimeout = 30 * time.Second

// InstallFromTarGz downloads a remote .tar.gz archive and installs binaryName
// from it to targetPath.
func InstallFromTarGz(ctx context.Context, downloadURL, targetPath, binaryName string, perm os.FileMode) error {
	parsedURL, err := url.Parse(downloadURL)
	if err != nil {
		return fmt.Errorf("parse download URL %q: %w", downloadURL, err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return fmt.Errorf("unsupported agent download URL scheme %q", parsedURL.Scheme)
	}

	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
		if err != nil {
			return err
		}
		if filepath.Base(tarFile.Name) != binaryName {
			continue
		}
		if tarFile.Size == 0 {
			return fmt.Errorf("agent binary %q in archive %q is empty", binaryName, downloadURL)
		}

		if err := utilio.InstallFile(targetPath, tarFile.Body, perm); err != nil {
			return fmt.Errorf("install %s from %q: %w", binaryName, downloadURL, err)
		}

		if err := Verify(ctx, targetPath); err != nil {
			return err
		}

		return nil
	}

	return fmt.Errorf("agent binary %q not found in archive %q", binaryName, downloadURL)
}

// InstallFromFile installs a local agent binary to targetPath.
func InstallFromFile(sourcePath, targetPath string, perm os.FileMode) (err error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", sourcePath, err)
	}
	defer func() {
		if closeErr := source.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", sourcePath, closeErr)
		}
	}()

	if err := utilio.InstallFile(targetPath, source, perm); err != nil {
		return fmt.Errorf("install %s to %s: %w", sourcePath, targetPath, err)
	}

	return nil
}

// InstallAndSwitchFromTarGz installs the next agent binary and switches daemon links.
func InstallAndSwitchFromTarGz(ctx context.Context, downloadURL string, paths goalstates.AgentUpgradePaths, perm os.FileMode) error {
	targetPath := paths.NextTargetPath()
	if err := InstallFromTarGz(ctx, downloadURL, targetPath, goalstates.AgentUpgradeBinaryName, perm); err != nil {
		return fmt.Errorf("install upgraded daemon binary to %s: %w", targetPath, err)
	}

	if err := UpdateSymlink(paths.LastGoodPath, paths.CurrentTargetPath); err != nil {
		return fmt.Errorf("update last-good daemon symlink: %w", err)
	}

	if err := UpdateSymlink(paths.CurrentPath, targetPath); err != nil {
		return fmt.Errorf("update current daemon symlink: %w", err)
	}

	return nil
}

// Verify runs the installed agent binary's version command.
func Verify(ctx context.Context, path string) error {
	verifyCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	for {
		output, err := exec.CommandContext(verifyCtx, path, "version").CombinedOutput()
		if err == nil {
			return nil
		}

		if errors.Is(err, syscall.ETXTBSY) {
			select {
			case <-verifyCtx.Done():
				return fmt.Errorf("verify agent binary %s: %w", path, err)
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}

		details := strings.TrimSpace(string(output))
		if details != "" {
			return fmt.Errorf("verify agent binary %s: %w: %s", path, err, details)
		}
		return fmt.Errorf("verify agent binary %s: %w", path, err)
	}
}

// UpdateSymlink atomically updates linkPath to point at targetPath.
func UpdateSymlink(linkPath, targetPath string) error {
	return utilio.UpdateSymlink(linkPath, targetPath)
}
