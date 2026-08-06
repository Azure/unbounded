// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
)

type proxyPhase string

const (
	proxyPhaseSetup      proxyPhase = "setup"
	proxyPhaseBaseline   proxyPhase = "baseline"
	proxyPhaseGantryCold proxyPhase = "gantry_cold"
	proxyPhaseIdle       proxyPhase = "idle"
)

func (p proxyPhase) valid() bool {
	switch p {
	case proxyPhaseSetup, proxyPhaseBaseline, proxyPhaseGantryCold, proxyPhaseIdle:
		return true
	default:
		return false
	}
}

func (b *benchmark) patchGantryForBenchmark(ctx context.Context, state *benchmarkState) error {
	current, err := b.readGantryConfig(ctx)
	if err != nil {
		return err
	}

	currentSHA := gantryConfigSHA(current)
	if currentSHA != state.OriginalGantryConfigSHA {
		if state.PatchedGantryConfigSHA != "" && currentSHA == state.PatchedGantryConfigSHA {
			return nil
		}

		return fmt.Errorf("gantry ConfigMap changed after enable: current sha256=%s, original sha256=%s", currentSHA, state.OriginalGantryConfigSHA)
	}

	endpoint := fmt.Sprintf("http://acr-origin-proxy.%s.svc.cluster.local:5002", b.config.Namespace)
	namespaceAlias := state.ProxyClusterIP + ":5002"

	patched, err := patchGantryRegistry([]byte(current), state.ACRLoginServer, endpoint, namespaceAlias)
	if err != nil {
		return err
	}

	state.PatchedGantryConfigSHA = gantryConfigSHA(string(patched))
	state.GantryRestored = false

	state.Status = "patching-gantry"
	if err := b.saveState(ctx, *state); err != nil {
		return err
	}

	if err := b.patchGantryConfigMap(ctx, current, string(patched)); err != nil {
		return err
	}

	return b.rolloutGantry(ctx)
}

func (b *benchmark) restoreGantry(ctx context.Context, state *benchmarkState) error {
	current, err := b.readGantryConfig(ctx)
	if err != nil {
		return err
	}

	currentSHA := gantryConfigSHA(current)

	// Direct mode never patches Gantry, so there is nothing to restore. Still
	// verify the ConfigMap is byte-identical to what enable recorded: a drift
	// here means something outside the benchmark changed Gantry mid-run, which
	// invalidates the comparison.
	if !state.usesProxy() {
		if currentSHA != state.OriginalGantryConfigSHA {
			return fmt.Errorf(
				"gantry ConfigMap changed during a direct-mode run, which never patches it: current sha256=%s, recorded sha256=%s",
				currentSHA,
				state.OriginalGantryConfigSHA,
			)
		}

		state.GantryRestored = true

		return nil
	}

	if currentSHA == state.OriginalGantryConfigSHA {
		if state.PatchedGantryConfigSHA == "" || state.GantryRestored {
			state.GantryRestored = true

			return nil
		}

		if err := b.rolloutGantry(ctx); err != nil {
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

	if err := b.rolloutGantry(ctx); err != nil {
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

func (b *benchmark) rolloutGantry(ctx context.Context) error {
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

	return b.validateGantry(ctx)
}

func (b *benchmark) switchProxyPhase(ctx context.Context, phase proxyPhase) error {
	if !phase.valid() {
		return fmt.Errorf("invalid proxy phase %q", phase)
	}

	// Direct mode has no proxy to hold phase state. Phases are attributed by
	// counter deltas over the recorded phase window instead.
	if !b.config.usesProxy() {
		return nil
	}

	if _, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"exec", "deployment/acr-origin-proxy", "--",
		"/usr/local/bin/acr-origin-proxy",
		"set-phase", string(phase), b.config.RolloutTimeout.String(),
	); err != nil {
		return fmt.Errorf("switch proxy phase to %s: %w", phase, err)
	}

	return nil
}
