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
		part, ok := i.findRootPartition(targetDisk)
		if !ok {
			return errors.New("no root partition found")
		}

		rootPart = part

		return nil
	}); err != nil {
		return fmt.Errorf("no root partition found on %s: %w", targetDisk, err)
	}

	if err := retry(ctx, 5, 2*time.Second, "mount "+rootPart, i.Sleep, i.Logger, func() error {
		return i.withMounted(rootPart, i.MountRoot, rootFilesystems, func() error {
			if err := writeCloudInitFiles(i.MountRoot, cfg); err != nil {
				return err
			}

			i.System.Sync()

			return nil
		})
	}); err != nil {
		return fmt.Errorf("failed to mount %s: %w", rootPart, err)
	}

	i.Logger.Printf("cloud-init configured on %s", rootPart)

	return nil
}

func renderNoCloudConfig(dsURL string) string {
	return fmt.Sprintf("datasource_list: [NoCloud]\ndatasource:\n  NoCloud:\n    seedfrom: %s\n", dsURL)
}

func renderMetalmanConfig(cfg cloudInitConfig) string {
	return fmt.Sprintf("SERVE_URL=%s\nNODE_NAME=%s\nNODE_NAMESPACE=%s\nAPISERVER_URL=%s\n", cfg.ServeURL, cfg.NodeName, cfg.NodeNamespace, cfg.APIServerURL)
}

func writeCloudInitFiles(root string, cfg cloudInitConfig) error {
	if err := os.MkdirAll(filepath.Join(root, "etc/cloud/cloud.cfg.d"), 0o755); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(root, "etc/unbounded-metal"), 0o755); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(root, "etc/cloud/cloud.cfg.d/99-unbounded-metal.cfg"), []byte(renderNoCloudConfig(cfg.DSURL)), 0o644); err != nil {
		return err
	}

	if err := os.Remove(filepath.Join(root, "etc/cloud/cloud-init.disabled")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return os.WriteFile(filepath.Join(root, "etc/unbounded-metal/config"), []byte(renderMetalmanConfig(cfg)), 0o644)
}

func (i *Installer) findRootPartition(targetDisk string) (string, bool) {
	for _, part := range i.partitionsForDisk(targetDisk) {
		var isRoot bool
		if err := i.withMounted(part.Device, i.MountRoot, rootFilesystems, func() error {
			isRoot = pathExists(filepath.Join(i.MountRoot, "etc")) && pathExists(filepath.Join(i.MountRoot, "var"))
			return nil
		}); err != nil {
			continue
		}

		if isRoot {
			return part.Device, true
		}
	}

	return "", false
}
