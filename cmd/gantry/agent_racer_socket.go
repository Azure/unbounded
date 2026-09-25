// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/internal/gantry/racer"
	"github.com/Azure/unbounded/pkg/racersdk"
)

func serveGantryRacerOrigin(ctx context.Context, config racersdk.OriginConfig, origin racersdk.Origin) error {
	if err := prepareRacerOriginDirectory("/run/racer"); err != nil {
		return fmt.Errorf("prepare origin directory: %w", err)
	}

	return racersdk.ServeOrigin(ctx, config, origin)
}

// Gantry owns the origin endpoint; Racer creates only the client endpoint. The
// shared mount must exist, but Gantry may start before Racer creates the cache
// directory. Match Racer's 0755 directory mode without changing existing modes
// or touching either socket. As with the SDK, ancestors must be trusted against
// concurrent replacement, and symlink traversal is refused.
func prepareRacerOriginDirectory(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("invalid socket root %q", root)
	}

	parent := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(root, parent), parent) {
		parent = filepath.Join(parent, part)
		if err := racerSocketDirectory(parent); err != nil {
			return err
		}
	}

	for _, part := range []string{racer.CacheName, "origin"} {
		parent = filepath.Join(parent, part)
		if err := os.Mkdir(parent, 0o755); err != nil && !os.IsExist(err) {
			return err
		}

		if err := racerSocketDirectory(parent); err != nil {
			return err
		}
	}

	return nil
}

func racerSocketDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return fmt.Errorf("socket directory %q is not a directory or is a symlink", path)
	}

	return nil
}
