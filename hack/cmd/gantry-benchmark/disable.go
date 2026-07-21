// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"fmt"
)

func (b *benchmark) disable(ctx context.Context) error {
	if err := b.validateContext(ctx); err != nil {
		return err
	}

	exists, err := b.stateExists(ctx)
	if err != nil {
		return err
	}

	if !exists {
		locked, lockErr := b.lockExists(ctx)
		if lockErr != nil {
			return lockErr
		}

		if locked {
			lockedBy, lockErr := b.lockRunID(ctx)
			if lockErr != nil {
				return lockErr
			}

			if _, err := b.commands.Run(
				ctx,
				nil,
				"kubectl", "delete", "namespace", b.config.Namespace,
				"--ignore-not-found=true", "--wait=true",
			); err != nil {
				return err
			}

			if err := b.releaseLock(ctx, lockedBy); err != nil {
				return err
			}

			writeAll(b.stdout, "removed a partial Gantry benchmark enable before cluster routing was changed\n")

			return nil
		}

		writeAll(b.stdout, "Gantry benchmark is already disabled\n")

		return nil
	}

	state, err := b.loadState(ctx)
	if err != nil {
		return err
	}

	lockedBy, err := b.lockRunID(ctx)
	if err != nil {
		return err
	}

	if lockedBy != "" && lockedBy != state.RunID {
		return fmt.Errorf("benchmark lock is owned by %q, state belongs to %q", lockedBy, state.RunID)
	}

	state.Status = "disabling"
	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	if err := b.switchProxyPhase(ctx, proxyPhaseIdle); err != nil {
		writeAll(b.stderr, fmt.Sprintf("warning: could not switch proxy to idle before restoration: %v\n", err))
	}

	hostsErr := b.restoreHosts(ctx, state)

	gantryErr := b.restoreGantry(ctx, &state)
	if restoreErr := errors.Join(hostsErr, gantryErr); restoreErr != nil {
		state.Status = "restore-failed"
		if saveErr := b.saveState(ctx, state); saveErr != nil {
			return errors.Join(restoreErr, fmt.Errorf("save failed restoration state: %w", saveErr))
		}

		return fmt.Errorf("restore benchmark cluster changes: %w", restoreErr)
	}

	if err := b.validateGantryAtCurrentSize(ctx); err != nil {
		state.Status = "restore-failed"

		validationErr := fmt.Errorf("validate Gantry after restoration: %w", err)
		if saveErr := b.saveState(ctx, state); saveErr != nil {
			return errors.Join(validationErr, fmt.Errorf("save failed validation state: %w", saveErr))
		}

		return validationErr
	}

	if err := b.deleteDashboard(ctx); err != nil {
		state.Status = "cleanup-failed"

		cleanupErr := fmt.Errorf("delete Grafana dashboard: %w", err)
		if saveErr := b.saveState(ctx, state); saveErr != nil {
			return errors.Join(cleanupErr, fmt.Errorf("save failed cleanup state: %w", saveErr))
		}

		return cleanupErr
	}

	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "delete", "namespace", b.config.Namespace,
		"--wait=true",
	); err != nil {
		return err
	}

	if err := b.releaseLock(ctx, state.RunID); err != nil {
		return err
	}

	state.Status = "disabled"
	if err := b.writeLocalState(state); err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf("disabled Gantry benchmark %s and restored cluster routing\n", state.RunID))

	return nil
}
