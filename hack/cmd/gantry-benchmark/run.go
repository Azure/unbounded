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

	if state.usesProxy() {
		if b.config.ACRLoginServer != state.ACRLoginServer {
			return fmt.Errorf("configured ACR_LOGIN_SERVER=%q does not match enabled benchmark registry %q", b.config.ACRLoginServer, state.ACRLoginServer)
		}
	} else if b.config.BaselineACRLoginServer != state.BaselineACRLoginServer || b.config.GantryACRLoginServer != state.GantryACRLoginServer {
		return fmt.Errorf(
			"configured registries baseline=%q Gantry=%q do not match enabled benchmark registries baseline=%q Gantry=%q",
			b.config.BaselineACRLoginServer,
			b.config.GantryACRLoginServer,
			state.BaselineACRLoginServer,
			state.GantryACRLoginServer,
		)
	}

	baselineImage, gantryImage, err := state.preparedImages()
	if err != nil {
		return err
	}

	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 3*b.config.RolloutTimeout)
		defer cancel()

		if phaseErr := b.switchProxyPhase(cleanupContext, proxyPhaseIdle); phaseErr != nil {
			writeAll(b.stderr, fmt.Sprintf("warning: switch proxy to idle during cleanup: %v\n", phaseErr))
		}

		// Gantry is patched at enable and restored at disable, so `run` never
		// restarts Gantry. Only the containerd host routing is restored here.
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

	if err := b.switchProxyPhase(ctx, proxyPhaseBaseline); err != nil {
		return err
	}

	// The Gantry revision is captured once. Proxy mode patched and rolled Gantry
	// at enable time and direct mode never touches it, so the revision is stable
	// across both phases and every counter delta is comparable.
	revision, err := b.gantryRevision(ctx)
	if err != nil {
		return err
	}

	if err := b.waitForGantryRevisionScrape(ctx, revision); err != nil {
		return err
	}

	baselineWindowStart, err := b.beginTelemetryWindow(ctx, proxyPhaseBaseline)
	if err != nil {
		return err
	}

	// Snapshot after the Azure minute boundary so Gantry/peer deltas and
	// PEBytesIn covers the same phase window.
	baselineMetricsBefore, err := b.fetchGantryRevisionMetrics(ctx, revision)
	if err != nil {
		return err
	}

	baselinePeerBefore, err := b.fetchGantryPeerByteSnapshot(ctx, revision)
	if err != nil {
		return err
	}

	baselineDiagnosticsBefore, err := b.fetchGantryDiagnosticSnapshot(ctx, revision)
	if err != nil {
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

	var baselineProxy proxyPhaseTotals

	if state.usesProxy() {
		baselineProxy, err = b.fetchProxyTotals(ctx, state, proxyPhaseBaseline)
		if err != nil {
			return err
		}
	}

	baselineGantry, err := b.waitForGantryMetricSettlement(ctx, revision, baselineMetricsBefore, false)
	if err != nil {
		return err
	}

	baselineWindowFinish, err := b.finishTelemetryWindow(ctx, proxyPhaseBaseline)
	if err != nil {
		return err
	}

	baselinePeerAfter, err := b.fetchGantryPeerByteSnapshot(ctx, revision)
	if err != nil {
		return err
	}

	baselinePeer, err := subtractPeerByteSnapshots(baselinePeerBefore, baselinePeerAfter)
	if err != nil {
		return err
	}

	baselineDiagnosticsAfter, err := b.fetchGantryDiagnosticSnapshot(ctx, revision)
	if err != nil {
		return err
	}
	baselineDiagnosticTimestamps, err := b.fetchGantryDiagnosticTimestamps(ctx, revision, telemetryWindow{
		StartedAt:  baselineJob.PhaseStartedAt,
		FinishedAt: baselineJob.PhaseFinishedAt,
	})
	if err != nil {
		return err
	}
	baselineDiagnostics, err := subtractGantryDiagnosticSnapshots(
		baselineDiagnosticsBefore,
		baselineDiagnosticsAfter,
		baselineDiagnosticTimestamps,
	)
	if err != nil {
		return err
	}

	baselineBytes, baselineBytesSource := deriveOriginBytes(b.config, proxyPhaseBaseline, baselineProxy, baselineGantry, baselineJob)
	baselinePerformance, err := b.capturePhasePerformanceTelemetry(ctx, proxyPhaseBaseline, baselineJob)
	if err != nil {
		return err
	}
	if err := b.writePerformanceTelemetryArtifact(state.RunID, proxyPhaseBaseline, baselinePerformance); err != nil {
		return err
	}

	baselineResult := phaseResult{
		RunID:                  state.RunID,
		Phase:                  proxyPhaseBaseline,
		Image:                  baselineImage,
		ImageSizeMiB:           b.config.ImageSizeMiB,
		ImageLayers:            b.config.ImageLayers,
		PayloadSHA:             state.WorkloadPayloadSHA256,
		WorkloadComparisonMode: workloadComparisonIdenticalPayload,
		Proxy:                  baselineProxy,
		Gantry:                 baselineGantry,
		GantryPeer:             baselinePeer,
		GantryDiagnostics:      baselineDiagnostics,
		Azure: azurePhaseMeasurement{Window: telemetryWindow{
			StartedAt:  baselineWindowStart,
			FinishedAt: baselineWindowFinish,
		}},
		Job:                          baselineJob,
		OriginBytes:                  baselineBytes,
		OriginBytesSource:            baselineBytesSource,
		PerformanceTelemetryArtifact: string(proxyPhaseBaseline) + "-performance.json",
		RecordedAt:                   time.Now().UTC(),
	}
	if err := b.writeJSONArtifact(state.RunID, "baseline.json", baselineResult); err != nil {
		return err
	}

	state.Status = "gantry-routing"
	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	// Gantry was already patched and rolled out at enable time (proxy mode) or is
	// deliberately untouched (direct mode), so its DHT has long since converged.
	// Only the containerd host routing changes here; `run` never restarts Gantry.
	if err := b.installHosts(ctx, state, hostsModeGantry); err != nil {
		return err
	}

	if err := b.switchProxyPhase(ctx, proxyPhaseGantryCold); err != nil {
		return err
	}

	gantryWindowStart, err := b.beginTelemetryWindow(ctx, proxyPhaseGantryCold)
	if err != nil {
		return err
	}

	metricsBefore, err := b.fetchGantryRevisionMetrics(ctx, revision)
	if err != nil {
		return err
	}

	gantryPeerBefore, err := b.fetchGantryPeerByteSnapshot(ctx, revision)
	if err != nil {
		return err
	}

	gantryDiagnosticsBefore, err := b.fetchGantryDiagnosticSnapshot(ctx, revision)
	if err != nil {
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

	gantryWindowFinish, err := b.finishTelemetryWindow(ctx, proxyPhaseGantryCold)
	if err != nil {
		return err
	}

	gantryPeerAfter, err := b.fetchGantryPeerByteSnapshot(ctx, revision)
	if err != nil {
		return err
	}

	gantryPeer, err := subtractPeerByteSnapshots(gantryPeerBefore, gantryPeerAfter)
	if err != nil {
		return err
	}

	gantryDiagnosticsAfter, err := b.fetchGantryDiagnosticSnapshot(ctx, revision)
	if err != nil {
		return err
	}
	gantryDiagnosticTimestamps, err := b.fetchGantryDiagnosticTimestamps(ctx, revision, telemetryWindow{
		StartedAt:  gantryJob.PhaseStartedAt,
		FinishedAt: gantryJob.PhaseFinishedAt,
	})
	if err != nil {
		return err
	}
	if err := requireFinalLayerResponseTimestamps(gantryDiagnosticTimestamps, gantryDiagnosticsAfter.PodNodes); err != nil {
		return err
	}
	gantryDiagnostics, err := subtractGantryDiagnosticSnapshots(
		gantryDiagnosticsBefore,
		gantryDiagnosticsAfter,
		gantryDiagnosticTimestamps,
	)
	if err != nil {
		return err
	}

	var gantryProxy proxyPhaseTotals

	if state.usesProxy() {
		gantryProxy, err = b.fetchProxyTotals(ctx, state, proxyPhaseGantryCold)
		if err != nil {
			return err
		}
	}

	gantryBytes, gantryBytesSource := deriveOriginBytes(b.config, proxyPhaseGantryCold, gantryProxy, phaseMetrics, gantryJob)
	gantryPerformance, err := b.capturePhasePerformanceTelemetry(ctx, proxyPhaseGantryCold, gantryJob)
	if err != nil {
		return err
	}
	if err := b.writePerformanceTelemetryArtifact(state.RunID, proxyPhaseGantryCold, gantryPerformance); err != nil {
		return err
	}

	gantryResult := phaseResult{
		RunID:                  state.RunID,
		Phase:                  proxyPhaseGantryCold,
		Image:                  gantryImage,
		ImageSizeMiB:           b.config.ImageSizeMiB,
		ImageLayers:            b.config.ImageLayers,
		PayloadSHA:             state.WorkloadPayloadSHA256,
		WorkloadComparisonMode: workloadComparisonIdenticalPayload,
		Proxy:                  gantryProxy,
		Gantry:                 phaseMetrics,
		GantryPeer:             gantryPeer,
		GantryDiagnostics:      gantryDiagnostics,
		Azure: azurePhaseMeasurement{Window: telemetryWindow{
			StartedAt:  gantryWindowStart,
			FinishedAt: gantryWindowFinish,
		}},
		Job:                          gantryJob,
		OriginBytes:                  gantryBytes,
		OriginBytesSource:            gantryBytesSource,
		PerformanceTelemetryArtifact: string(proxyPhaseGantryCold) + "-performance.json",
		RecordedAt:                   time.Now().UTC(),
	}
	if err := b.writeJSONArtifact(state.RunID, "gantry-cold.json", gantryResult); err != nil {
		return err
	}

	if b.config.AzureTelemetry {
		writeAll(b.stdout, "waiting for baseline Azure telemetry\n")

		if err := b.collectAndPersistAzurePhase(ctx, &baselineResult, "baseline.json"); err != nil {
			return fmt.Errorf("collect baseline Azure telemetry: %w", err)
		}

		writeAll(b.stdout, "waiting for Gantry-cold Azure telemetry\n")

		if err := b.collectAndPersistAzurePhase(ctx, &gantryResult, "gantry-cold.json"); err != nil {
			return fmt.Errorf("collect Gantry-cold Azure telemetry: %w", err)
		}
	}

	comparison := compareResults(b.config, baselineResult, gantryResult)
	if err := b.writeComparisonArtifacts(comparison); err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf(
		"origin bytes: baseline=%d Gantry=%d reduction=%.2f%% (source %s/%s)\n",
		baselineResult.OriginBytes,
		gantryResult.OriginBytes,
		100*comparison.OriginByteReduction,
		baselineResult.OriginBytesSource,
		gantryResult.OriginBytesSource,
	))
	writeAll(b.stdout, fmt.Sprintf(
		"pod start P95: baseline=%.3fs Gantry=%.3fs\n",
		phaseStartupLatency(baselineResult).P95Seconds,
		phaseStartupLatency(gantryResult).P95Seconds,
	))

	if !comparison.Passed {
		return fmt.Errorf("benchmark completed but regression gates failed; see %s", b.config.StateRoot+"/"+state.RunID+"/comparison.json")
	}

	return nil
}
