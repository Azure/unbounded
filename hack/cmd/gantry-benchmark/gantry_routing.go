// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
)

func (b *benchmark) restoreGantry(ctx context.Context, state *benchmarkState) error {
	current, err := b.readGantryConfig(ctx)
	if err != nil {
		return err
	}

	currentSHA := gantryConfigSHA(current)
	if currentSHA == state.OriginalGantryConfigSHA {
		if state.PatchedGantryConfigSHA == "" || state.GantryRestored {
			state.GantryRestored = true

			return nil
		}

		if err := b.rolloutGantryAtCurrentSize(ctx); err != nil {
			return err
		}

		state.GantryRestored = true

		return nil
	}

	if state.PatchedGantryConfigSHA == "" || currentSHA != state.PatchedGantryConfigSHA {
		return fmt.Errorf("refusing to overwrite concurrently changed gantry ConfigMap: current sha256=%s, expected benchmark sha256=%s", currentSHA, state.PatchedGantryConfigSHA)
	}

	if err := b.patchGantryConfigMap(ctx, current, state.OriginalGantryConfig); err != nil {
		return err
	}

	if err := b.rolloutGantryAtCurrentSize(ctx); err != nil {
		return err
	}

	restored, err := b.readGantryConfig(ctx)
	if err != nil {
		return err
	}

	if restoredSHA := gantryConfigSHA(restored); restoredSHA != state.OriginalGantryConfigSHA {
		return fmt.Errorf("restored gantry ConfigMap hash is %s, want %s", restoredSHA, state.OriginalGantryConfigSHA)
	}

	state.GantryRestored = true

	return nil
}

func (b *benchmark) patchGantryConfigMap(ctx context.Context, expected, replacement string) error {
	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/data/config.yaml", "value": expected},
		{"op": "replace", "path": "/data/config.yaml", "value": replacement},
	})
	if err != nil {
		return err
	}

	_, err = b.commands.Run(
		ctx,
		nil,
		"kubectl",
		"-n", b.config.GantryNamespace,
		"patch", "configmap", b.config.GantryConfigMap,
		"--type=json",
		"--patch", string(patch),
	)

	return err
}

func (b *benchmark) rolloutGantryAtCurrentSize(ctx context.Context) error {
	return b.rolloutGantryAndValidate(ctx, b.validateGantryAtCurrentSize)
}

func (b *benchmark) rolloutGantryAndValidate(ctx context.Context, validate func(context.Context) error) error {
	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl",
		"-n", b.config.GantryNamespace,
		"rollout", "restart", "daemonset/"+b.config.GantryDaemonSet,
	); err != nil {
		return err
	}

	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl",
		"-n", b.config.GantryNamespace,
		"rollout", "status", "daemonset/"+b.config.GantryDaemonSet,
		"--timeout", b.config.RolloutTimeout.String(),
	); err != nil {
		return err
	}

	return validate(ctx)
}
