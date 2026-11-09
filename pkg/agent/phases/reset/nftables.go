// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type removeNFTablesFlush struct {
	log *slog.Logger
	// unitDir and clearPath are set at construction so tests can run the task
	// against a temporary tree.
	unitDir   string
	clearPath string
}

// RemoveNFTablesFlush returns a task that disables and removes the
// nftables-flush unit bootstrap installs, and the rules file it loads.
//
// Every isolation mode installs the unit. Left enabled, it runs `flush ruleset`
// on every boot of the reset host and erases the host's own firewall: the none
// and container variants are wanted by multi-user.target, so nothing else has
// to start for them to run. The nspawn variant only runs with a machine, but it
// is the agent's file all the same. The caller reloads systemd afterwards.
func RemoveNFTablesFlush(log *slog.Logger) phases.Task {
	return &removeNFTablesFlush{
		log:       log,
		unitDir:   goalstates.SystemdSystemDir,
		clearPath: goalstates.NFTablesClearPath,
	}
}

func (t *removeNFTablesFlush) Name() string { return "remove-nftables-flush" }

func (t *removeNFTablesFlush) Do(ctx context.Context) error {
	unitPath := filepath.Join(t.unitDir, goalstates.NFTablesFlushUnit)

	if _, err := os.Lstat(unitPath); err == nil {
		t.log.Info("removing nftables flush unit", "unit", goalstates.NFTablesFlushUnit)

		// Disable before removing the file, so the [Install] links are removed
		// with it rather than left dangling.
		if err := executil.RunCmd(ctx, t.log, executil.Systemctl(), "disable", goalstates.NFTablesFlushUnit); err != nil {
			return fmt.Errorf("disable %s: %w", goalstates.NFTablesFlushUnit, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	for _, path := range []string{unitPath, t.clearPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	return nil
}
