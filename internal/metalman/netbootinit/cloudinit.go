// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type cloudInitConfig struct {
	DSURL         string
	ServeURL      string
	NodeName      string
	NodeNamespace string
	APIServerURL  string
}

func (i *Installer) injectCloudInit(ctx context.Context, targetDisk string, cfg cloudInitConfig) error {
	i.Logger.Printf("injecting cloud-init datasource: %s", cfg.DSURL)

	var rootPart string

	if err := retry(ctx, 20, 2*time.Second, "find root partition", i.Sleep, i.Logger, func() error {
		part, ok := i.findRootPartition(ctx, targetDisk)
		if !ok {
			return errors.New("no root partition found")
		}

		rootPart = part

		return nil
	}); err != nil {
		return fmt.Errorf("no root partition found on %s: %w", targetDisk, err)
	}

	if err := retry(ctx, 5, 2*time.Second, "mount "+rootPart, i.Sleep, i.Logger, func() error {
		return i.Runner.Run(ctx, "mount", rootPart, i.MountRoot)
	}); err != nil {
		return fmt.Errorf("failed to mount %s: %w", rootPart, err)
	}

	mounted := true
	defer func() {
		if mounted {
			runBestEffort(context.Background(), i.Runner, "umount", i.MountRoot)
		}
	}()

	if err := os.MkdirAll(filepath.Join(i.MountRoot, "etc/cloud/cloud.cfg.d"), 0o755); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(i.MountRoot, "etc/metalman"), 0o755); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(i.MountRoot, "etc/cloud/cloud.cfg.d/99-metalman.cfg"), []byte(renderNoCloudConfig(cfg.DSURL)), 0o644); err != nil {
		return err
	}

	if err := os.Remove(filepath.Join(i.MountRoot, "etc/cloud/cloud-init.disabled")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := os.WriteFile(filepath.Join(i.MountRoot, "etc/metalman/config"), []byte(renderMetalmanConfig(cfg)), 0o644); err != nil {
		return err
	}

	unix.Sync()

	if err := i.Runner.Run(ctx, "umount", i.MountRoot); err != nil {
		return err
	}

	mounted = false

	i.Logger.Printf("cloud-init configured on %s", rootPart)

	return nil
}

func renderNoCloudConfig(dsURL string) string {
	return fmt.Sprintf("datasource_list: [NoCloud]\ndatasource:\n  NoCloud:\n    seedfrom: %s\n", dsURL)
}

func renderMetalmanConfig(cfg cloudInitConfig) string {
	return fmt.Sprintf("SERVE_URL=%s\nNODE_NAME=%s\nNODE_NAMESPACE=%s\nAPISERVER_URL=%s\n", cfg.ServeURL, cfg.NodeName, cfg.NodeNamespace, cfg.APIServerURL)
}

func (i *Installer) findRootPartition(ctx context.Context, targetDisk string) (string, bool) {
	for _, part := range i.partsForDisk(targetDisk) {
		if err := i.Runner.Run(ctx, "mount", part, i.MountRoot); err != nil {
			continue
		}

		isRoot := pathExists(filepath.Join(i.MountRoot, "etc")) && pathExists(filepath.Join(i.MountRoot, "var"))
		runBestEffort(ctx, i.Runner, "umount", i.MountRoot)

		if isRoot {
			return part, true
		}
	}

	return "", false
}
