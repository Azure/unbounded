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

	baselineImage, err := b.buildFreshImage(ctx, state, phaseBaseline)
	if err != nil {
		return err
	}

	writeAll(b.stdout, "building fresh Gantry cold image\n")

	gantryImage, err := b.buildFreshImage(ctx, state, phaseGantryCold)
	if err != nil {
		return err
	}

	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 3*b.config.RolloutTimeout)
		defer cancel()

		// Only the containerd host routing changes during a run.
		restoreErr := b.restoreHosts(cleanupContext, state)
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

	baselineKubeletBefore, err := b.fetchKubeletPullMetrics(ctx)
	if err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf("running baseline pull on %d nodes\n", b.config.NodeCount))

	baselineJob, err := b.runPullJob(ctx, state, phaseBaseline, baselineImage)
	if err != nil {
		return err
	}

	baselineIssues, err := b.fetchPullIssues(ctx, baselineJob.JobName)
	if err != nil {
		return err
	}

	baselineACR, err := b.waitForACRPullMetrics(ctx, baselineJob.PhaseStartedAt, baselineJob.PhaseFinishedAt)
	if err != nil {
		return err
	}

	baselineKubeletAfter, err := b.fetchKubeletPullMetrics(ctx)
	if err != nil {
		return err
	}

	baselineResult := phaseResult{
		RunID:        state.RunID,
		Phase:        phaseBaseline,
		Image:        baselineImage,
		ImageSizeMiB: b.config.ImageSizeMiB,
		Origin: originMetrics{
			ACR:            baselineACR,
			EstimatedBytes: estimatedBaselineBytes(len(baselineJob.Nodes), b.config.ImageSizeMiB),
			EstimateMethod: "completed nodes multiplied by configured image payload size",
		},
		Kubelet:    subtractKubeletPullMetrics(baselineKubeletAfter, baselineKubeletBefore),
		Issues:     baselineIssues,
		Job:        baselineJob,
		RecordedAt: time.Now().UTC(),
	}
	if err := b.writeJSONArtifact(state.RunID, "baseline.json", baselineResult); err != nil {
		return err
	}

	state.Status = "gantry-routing"
	if err := b.saveState(ctx, state); err != nil {
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

	gantryKubeletBefore, err := b.fetchKubeletPullMetrics(ctx)
	if err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf("running Gantry cold pull on %d nodes\n", b.config.NodeCount))

	gantryJob, err := b.runPullJob(ctx, state, phaseGantryCold, gantryImage)
	if err != nil {
		return err
	}

	gantryIssues, err := b.fetchPullIssues(ctx, gantryJob.JobName)
	if err != nil {
		return err
	}

	gantryACR, err := b.waitForACRPullMetrics(ctx, gantryJob.PhaseStartedAt, gantryJob.PhaseFinishedAt)
	if err != nil {
		return err
	}

	phaseMetrics, err := b.waitForGantryMetricDelta(ctx, revision, metricsBefore)
	if err != nil {
		return err
	}

	gantryKubeletAfter, err := b.fetchKubeletPullMetrics(ctx)
	if err != nil {
		return err
	}

	gantryResult := phaseResult{
		RunID:        state.RunID,
		Phase:        phaseGantryCold,
		Image:        gantryImage,
		ImageSizeMiB: b.config.ImageSizeMiB,
		Origin: originMetrics{
			ACR: gantryACR,
			EstimatedBytes: estimatedGantryOriginBytes(
				phaseMetrics.OriginLayerPullSuccesses,
				b.config.ImageSizeMiB,
				b.config.ImageLayers,
			),
			EstimateMethod: "successful Gantry origin layer pulls multiplied by average configured layer size",
		},
		Kubelet:    subtractKubeletPullMetrics(gantryKubeletAfter, gantryKubeletBefore),
		Issues:     gantryIssues,
		Gantry:     phaseMetrics,
		Job:        gantryJob,
		RecordedAt: time.Now().UTC(),
	}
	if err := b.writeJSONArtifact(state.RunID, "gantry-cold.json", gantryResult); err != nil {
		return err
	}

	comparison := compareResults(b.config, baselineResult, gantryResult)
	if err := b.writeComparisonArtifacts(comparison); err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf(
		"estimated origin bytes: baseline=%d Gantry=%d reduction=%.2f%%\n",
		baselineResult.Origin.EstimatedBytes,
		gantryResult.Origin.EstimatedBytes,
		100*comparison.OriginByteReduction,
	))
	writeAll(b.stdout, fmt.Sprintf(
		"pod start P95: baseline=%.3fs Gantry=%.3fs\n",
		baselineResult.Job.PodStartLatency.P95Seconds,
		gantryResult.Job.PodStartLatency.P95Seconds,
	))

	if !comparison.Passed {
		return fmt.Errorf("benchmark completed but gating checks failed; see %s", b.config.StateRoot+"/"+state.RunID+"/comparison.json")
	}

	return nil
}
