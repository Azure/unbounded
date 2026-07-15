// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (b *benchmark) runBenchmark(ctx context.Context) (returnErr error) {
	state, err := b.loadState(ctx)
	if err != nil {
		return err
	}

	if state.Status != "preflight-passed" {
		return fmt.Errorf("benchmark state is %q, run preflight before run", state.Status)
	}

	if err := b.requireLock(ctx, state.RunID); err != nil {
		return err
	}

	if err := b.validateContext(ctx); err != nil {
		return err
	}

	if _, err := b.targetNodes(ctx); err != nil {
		return err
	}

	if err := b.validateGantry(ctx); err != nil {
		return err
	}

	if b.config.ACRLoginServer == "" || b.config.ACRUsername == "" || b.config.ACRPassword == "" {
		return fmt.Errorf("ACR build credentials require ACR_LOGIN_SERVER, ACR_USERNAME, and ACR_PASSWORD") //nolint:staticcheck // Environment variable names are intentionally uppercase.
	}

	if b.config.ACRLoginServer != state.ACRLoginServer {
		return fmt.Errorf("configured ACR_LOGIN_SERVER=%q does not match enabled benchmark registry %q", b.config.ACRLoginServer, state.ACRLoginServer)
	}

	if err := b.loginRegistry(ctx); err != nil {
		return err
	}

	writeAll(b.stdout, "building fresh baseline image\n")

	baselineImage, err := b.buildFreshImage(ctx, state, proxyPhaseBaseline)
	if err != nil {
		return err
	}

	writeAll(b.stdout, "building fresh Gantry cold image\n")

	gantryImage, err := b.buildFreshImage(ctx, state, proxyPhaseGantryCold)
	if err != nil {
		return err
	}

	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 3*b.config.RolloutTimeout)
		defer cancel()

		if phaseErr := b.switchProxyPhase(cleanupContext, proxyPhaseIdle); phaseErr != nil {
			writeAll(b.stderr, fmt.Sprintf("warning: switch proxy to idle during cleanup: %v\n", phaseErr))
		}

		hostsErr := b.restoreHosts(cleanupContext, state)
		gantryErr := b.restoreGantry(cleanupContext, &state)

		restoreErr := errors.Join(hostsErr, gantryErr)
		if restoreErr != nil {
			state.Status = "restore-failed"

			returnErr = errors.Join(returnErr, fmt.Errorf("restore benchmark routing: %w", restoreErr))
			if saveErr := b.saveState(cleanupContext, state); saveErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("save failed restoration state: %w", saveErr))
			}

			return
		}

		if returnErr == nil {
			state.Status = "completed"
		} else {
			state.Status = "run-failed-restored"
		}

		if saveErr := b.saveState(cleanupContext, state); saveErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("save final benchmark state: %w", saveErr))
		}
	}()

	state.Status = "baseline-routing"
	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	if err := b.installHosts(ctx, state, hostsModeBaseline); err != nil {
		return err
	}

	if err := b.switchProxyPhase(ctx, proxyPhaseBaseline); err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf("running baseline pull on %d nodes\n", b.config.NodeCount))

	baselineJob, err := b.runPullJob(ctx, state, proxyPhaseBaseline, baselineImage)
	if err != nil {
		return err
	}

	if err := b.switchProxyPhase(ctx, proxyPhaseSetup); err != nil {
		return err
	}

	baselineProxy, err := b.fetchProxyTotals(ctx, state, proxyPhaseBaseline)
	if err != nil {
		return err
	}

	baselineResult := phaseResult{
		RunID:        state.RunID,
		Phase:        proxyPhaseBaseline,
		Image:        baselineImage,
		ImageSizeMiB: b.config.ImageSizeMiB,
		Proxy:        baselineProxy,
		Job:          baselineJob,
		RecordedAt:   time.Now().UTC(),
	}
	if err := b.writeJSONArtifact(state.RunID, "baseline.json", baselineResult); err != nil {
		return err
	}

	state.Status = "gantry-routing"
	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	if err := b.patchGantryForBenchmark(ctx, &state); err != nil {
		return err
	}

	if err := b.installHosts(ctx, state, hostsModeGantry); err != nil {
		return err
	}

	revision, err := b.gantryRevision(ctx)
	if err != nil {
		return err
	}

	if err := b.waitForGantryRevisionScrape(ctx, revision); err != nil {
		return err
	}

	metricsBefore, err := b.fetchGantryRevisionMetrics(ctx, revision)
	if err != nil {
		return err
	}

	if err := b.switchProxyPhase(ctx, proxyPhaseGantryCold); err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf("running Gantry cold pull on %d nodes\n", b.config.NodeCount))

	gantryJob, err := b.runPullJob(ctx, state, proxyPhaseGantryCold, gantryImage)
	if err != nil {
		return err
	}

	if err := b.switchProxyPhase(ctx, proxyPhaseSetup); err != nil {
		return err
	}

	phaseMetrics, err := b.waitForGantryMetricDelta(ctx, revision, metricsBefore)
	if err != nil {
		return err
	}

	gantryProxy, err := b.fetchProxyTotals(ctx, state, proxyPhaseGantryCold)
	if err != nil {
		return err
	}

	gantryResult := phaseResult{
		RunID:        state.RunID,
		Phase:        proxyPhaseGantryCold,
		Image:        gantryImage,
		ImageSizeMiB: b.config.ImageSizeMiB,
		Proxy:        gantryProxy,
		Gantry:       phaseMetrics,
		Job:          gantryJob,
		RecordedAt:   time.Now().UTC(),
	}
	if err := b.writeJSONArtifact(state.RunID, "gantry-cold.json", gantryResult); err != nil {
		return err
	}

	comparison := compareResults(b.config, baselineResult, gantryResult)
	if err := b.writeComparisonArtifacts(comparison); err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf(
		"origin bytes: baseline=%d Gantry=%d reduction=%.2f%%\n",
		baselineResult.Proxy.BytesUpstream,
		gantryResult.Proxy.BytesUpstream,
		100*comparison.OriginByteReduction,
	))
	writeAll(b.stdout, fmt.Sprintf(
		"pod start P95: baseline=%.3fs Gantry=%.3fs\n",
		baselineResult.Job.PodStartLatency.P95Seconds,
		gantryResult.Job.PodStartLatency.P95Seconds,
	))

	if !comparison.Passed {
		return fmt.Errorf("benchmark completed but regression gates failed; see %s", b.config.StateRoot+"/"+state.RunID+"/comparison.json")
	}

	return nil
}
