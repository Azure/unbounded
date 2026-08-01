// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (b *benchmark) prepareGantryOnly(ctx context.Context, baselineRunID, preparedRunID string) error {
	state, err := b.loadState(ctx)
	if err != nil {
		return err
	}

	if state.Status != "enabled" {
		return fmt.Errorf("benchmark state is %q, run enable before prepare-gantry", state.Status)
	}
	if state.usesProxy() {
		return fmt.Errorf("prepare-gantry requires direct dual-ACR mode")
	}
	if filepath.Base(baselineRunID) != baselineRunID || baselineRunID == "." || baselineRunID == "" {
		return fmt.Errorf("invalid baseline run ID %q", baselineRunID)
	}
	if err := b.requireLock(ctx, state.RunID); err != nil {
		return err
	}
	if err := b.validateContext(ctx); err != nil {
		return err
	}

	baselineState, err := b.readLocalState(baselineRunID)
	if err != nil {
		return fmt.Errorf("read baseline run state: %w", err)
	}
	baselineResult, err := b.readPhaseResult(baselineRunID, "baseline.json")
	if err != nil {
		return fmt.Errorf("read retained baseline result: %w", err)
	}
	if err := validateGantryOnlySource(state, baselineState, baselineResult); err != nil {
		return err
	}
	if preparedRunID != "" {
		if filepath.Base(preparedRunID) != preparedRunID || preparedRunID == "." {
			return fmt.Errorf("invalid prepared run ID %q", preparedRunID)
		}

		preparedState, err := b.readLocalState(preparedRunID)
		if err != nil {
			return fmt.Errorf("read prepared run state: %w", err)
		}
		if err := validatePreparedGantryOnlySource(state, baselineState, preparedState); err != nil {
			return err
		}

		baselineResult.RunID = state.RunID
		state.BaselineImage = baselineState.BaselineImage
		state.GantryColdImage = preparedState.GantryColdImage
		state.WorkloadPayloadSHA256 = baselineState.WorkloadPayloadSHA256
		state.Status = "images-prepared"

		if err := b.writeJSONArtifact(state.RunID, "baseline.json", baselineResult); err != nil {
			return err
		}
		if err := b.saveState(ctx, state); err != nil {
			return err
		}

		writeAll(b.stdout, fmt.Sprintf("reused cache-cold Gantry image from %s for %s\n", preparedRunID, state.RunID))

		return nil
	}

	if b.config.GantryACRUsername == "" || b.config.GantryACRPassword == "" {
		return fmt.Errorf("Gantry-only preparation requires GANTRY_ACR_USERNAME and GANTRY_ACR_PASSWORD")
	}
	if err := b.loginRegistry(ctx, state.GantryACRLoginServer, b.config.GantryACRUsername, b.config.GantryACRPassword); err != nil {
		return fmt.Errorf("log in to Gantry ACR: %w", err)
	}

	writeAll(b.stdout, fmt.Sprintf("rebuilding Gantry image from baseline payload %s\n", baselineState.WorkloadPayloadSHA256))
	gantryImage, err := b.rebuildGantryImage(ctx, state, baselineState)
	if err != nil {
		return err
	}

	baselineResult.RunID = state.RunID
	state.BaselineImage = baselineState.BaselineImage
	state.GantryColdImage = gantryImage
	state.WorkloadPayloadSHA256 = baselineState.WorkloadPayloadSHA256
	state.Status = "images-prepared"

	if err := b.writeJSONArtifact(state.RunID, "baseline.json", baselineResult); err != nil {
		return err
	}
	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf("prepared cache-cold Gantry image for %s using baseline %s\n", state.RunID, baselineRunID))

	return nil
}

func validatePreparedGantryOnlySource(current, baseline, prepared benchmarkState) error {
	if prepared.Mode != current.Mode || prepared.NodeCount != current.NodeCount ||
		prepared.ImagePlatform != current.ImagePlatform || prepared.ImageSizeMiB != current.ImageSizeMiB ||
		prepared.ImageLayers != current.ImageLayers || prepared.WorkloadRepository != current.WorkloadRepository ||
		prepared.BaselineACRLoginServer != current.BaselineACRLoginServer ||
		prepared.GantryACRLoginServer != current.GantryACRLoginServer {
		return fmt.Errorf("prepared Gantry run shape does not match current benchmark")
	}
	if prepared.WorkloadPayloadSHA256 != baseline.WorkloadPayloadSHA256 {
		return fmt.Errorf("prepared Gantry payload %q does not match baseline %q", prepared.WorkloadPayloadSHA256, baseline.WorkloadPayloadSHA256)
	}
	if prepared.GantryColdImage == "" {
		return fmt.Errorf("prepared Gantry run has no image")
	}
	repository, _, err := splitImageReference(prepared.GantryColdImage, current.GantryACRLoginServer)
	if err != nil {
		return fmt.Errorf("prepared Gantry image registry mismatch: %w", err)
	}
	if repository != current.WorkloadRepository {
		return fmt.Errorf("prepared Gantry repository %q, want %q", repository, current.WorkloadRepository)
	}

	return nil
}

func validateGantryOnlySource(current, baseline benchmarkState, result phaseResult) error {
	if baseline.Mode != current.Mode || baseline.NodeCount != current.NodeCount ||
		baseline.ImagePlatform != current.ImagePlatform || baseline.ImageSizeMiB != current.ImageSizeMiB ||
		baseline.ImageLayers != current.ImageLayers || baseline.WorkloadRepository != current.WorkloadRepository ||
		baseline.BaselineACRLoginServer != current.BaselineACRLoginServer ||
		baseline.GantryACRLoginServer != current.GantryACRLoginServer {
		return fmt.Errorf("baseline run shape does not match current benchmark")
	}
	if baseline.WorkloadPayloadSHA256 == "" || result.PayloadSHA != baseline.WorkloadPayloadSHA256 {
		return fmt.Errorf("baseline payload fingerprints state=%q result=%q do not match", baseline.WorkloadPayloadSHA256, result.PayloadSHA)
	}
	if result.Phase != proxyPhaseBaseline || len(result.Job.Nodes) != current.NodeCount {
		return fmt.Errorf("retained baseline phase=%q nodes=%d, want baseline with %d nodes", result.Phase, len(result.Job.Nodes), current.NodeCount)
	}
	if result.Image != baseline.BaselineImage {
		return fmt.Errorf("retained baseline image %q does not match state %q", result.Image, baseline.BaselineImage)
	}

	return nil
}

func (b *benchmark) rebuildGantryImage(ctx context.Context, state, baseline benchmarkState) (string, error) {
	buildDirectory := filepath.Join(b.config.StateRoot, state.RunID, "build", "gantry-only")
	if err := os.RemoveAll(buildDirectory); err != nil {
		return "", fmt.Errorf("clear Gantry-only build directory: %w", err)
	}
	if err := os.MkdirAll(buildDirectory, 0o750); err != nil {
		return "", fmt.Errorf("create Gantry-only build directory: %w", err)
	}

	if _, err := b.commands.Run(ctx, nil, b.config.ContainerEngine, "pull", baseline.GantryColdImage); err != nil {
		return "", fmt.Errorf("pull prior Gantry workload image: %w", err)
	}
	containerOutput, err := b.commands.Run(ctx, nil, b.config.ContainerEngine, "create", baseline.GantryColdImage)
	if err != nil {
		return "", fmt.Errorf("create prior workload extraction container: %w", err)
	}
	containerID := strings.TrimSpace(string(containerOutput))
	if containerID == "" {
		return "", fmt.Errorf("container engine returned an empty extraction container ID")
	}
	defer func() {
		_, _ = b.commands.Run(context.Background(), nil, b.config.ContainerEngine, "rm", "-f", containerID)
	}()

	extractDirectory := filepath.Join(buildDirectory, "source")
	if err := os.MkdirAll(extractDirectory, 0o750); err != nil {
		return "", fmt.Errorf("create payload extraction directory: %w", err)
	}
	if _, err := b.commands.Run(ctx, nil, b.config.ContainerEngine, "cp", containerID+":/gantry-benchmark-payload/.", extractDirectory); err != nil {
		return "", fmt.Errorf("extract prior workload payload: %w", err)
	}

	payloadPaths, err := indexedPayloadPaths(extractDirectory, state.ImageLayers)
	if err != nil {
		return "", err
	}
	for index, source := range payloadPaths {
		destination := filepath.Join(buildDirectory, fmt.Sprintf("payload%d.bin", index))
		if err := os.Rename(source, destination); err != nil {
			return "", fmt.Errorf("move extracted payload %d into build context: %w", index, err)
		}
		payloadPaths[index] = destination
	}
	payloadSHA, err := payloadSHA256(payloadPaths)
	if err != nil {
		return "", err
	}
	if payloadSHA != baseline.WorkloadPayloadSHA256 {
		return "", fmt.Errorf("extracted payload fingerprint %s does not match baseline %s", payloadSHA, baseline.WorkloadPayloadSHA256)
	}

	dockerfile := dualACRDockerfile(proxyPhase("gantry-rerun-"+state.RunID), payloadPaths, payloadSHA)
	if err := os.WriteFile(filepath.Join(buildDirectory, "Dockerfile."+string(proxyPhaseGantryCold)), []byte(dockerfile), 0o640); err != nil {
		return "", fmt.Errorf("write Gantry-only Dockerfile: %w", err)
	}

	tag := strings.ReplaceAll(state.RunID+"-gantry-only", "_", "-")
	taggedImage := fmt.Sprintf("%s/%s:%s", state.GantryACRLoginServer, state.WorkloadRepository, tag)
	digest, err := b.buildAndPushPreparedImage(ctx, buildDirectory, proxyPhaseGantryCold, taggedImage)
	if err != nil {
		return "", fmt.Errorf("build and push Gantry-only image: %w", err)
	}

	return fmt.Sprintf("%s/%s@%s", state.GantryACRLoginServer, state.WorkloadRepository, digest), nil
}

func indexedPayloadPaths(root string, layers int) ([]string, error) {
	matches := make(map[string][]string, layers)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "payload") && strings.HasSuffix(entry.Name(), ".bin") {
			matches[entry.Name()] = append(matches[entry.Name()], path)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan extracted payloads: %w", err)
	}

	paths := make([]string, 0, layers)
	for index := range layers {
		name := fmt.Sprintf("payload%d.bin", index)
		if len(matches[name]) != 1 {
			return nil, fmt.Errorf("extracted payload %s has %d matches, want 1", name, len(matches[name]))
		}
		paths = append(paths, matches[name][0])
	}

	return paths, nil
}

func (b *benchmark) readLocalState(runID string) (benchmarkState, error) {
	var state benchmarkState
	if err := readJSONFile(filepath.Join(b.config.StateRoot, runID, "state.json"), &state); err != nil {
		return benchmarkState{}, err
	}

	return state, nil
}

func (b *benchmark) readPhaseResult(runID, filename string) (phaseResult, error) {
	var result phaseResult
	if err := readJSONFile(filepath.Join(b.config.StateRoot, runID, filename), &result); err != nil {
		return phaseResult{}, err
	}

	return result, nil
}

func readJSONFile(path string, target any) error {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}

	return nil
}

func (b *benchmark) runGantryOnly(ctx context.Context) (returnErr error) {
	state, err := b.loadState(ctx)
	if err != nil {
		return err
	}
	if state.Status != "preflight-passed" {
		return fmt.Errorf("benchmark state is %q, run preflight before run-gantry", state.Status)
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

	baselineResult, err := b.readPhaseResult(state.RunID, "baseline.json")
	if err != nil {
		return fmt.Errorf("read retained baseline result: %w", err)
	}
	_, gantryImage, err := state.preparedImages()
	if err != nil {
		return err
	}
	if baselineResult.RunID != state.RunID || baselineResult.PayloadSHA != state.WorkloadPayloadSHA256 {
		return fmt.Errorf("retained baseline does not belong to current Gantry-only run")
	}

	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 3*b.config.RolloutTimeout)
		defer cancel()

		if phaseErr := b.switchProxyPhase(cleanupContext, proxyPhaseIdle); phaseErr != nil {
			writeAll(b.stderr, fmt.Sprintf("warning: switch proxy to idle during cleanup: %v\n", phaseErr))
		}
		restoreErr := b.restoreHosts(cleanupContext, state)
		if restoreErr != nil {
			state.Status = "restore-failed"
			returnErr = errors.Join(returnErr, fmt.Errorf("restore benchmark routing: %w", restoreErr))
		} else if returnErr == nil {
			state.Status = "completed"
		} else {
			state.Status = "run-failed-restored"
		}
		if saveErr := b.saveState(cleanupContext, state); saveErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("save final benchmark state: %w", saveErr))
		}
	}()

	state.Status = "gantry-routing"
	if err := b.saveState(ctx, state); err != nil {
		return err
	}
	if err := b.installHosts(ctx, state, hostsModeGantry); err != nil {
		return err
	}
	if err := b.switchProxyPhase(ctx, proxyPhaseGantryCold); err != nil {
		return err
	}

	revision, err := b.gantryRevision(ctx)
	if err != nil {
		return err
	}
	if err := b.waitForGantryRevisionScrape(ctx, revision); err != nil {
		return err
	}
	windowStart, err := b.beginTelemetryWindow(ctx, proxyPhaseGantryCold)
	if err != nil {
		return err
	}
	metricsBefore, err := b.fetchGantryRevisionMetrics(ctx, revision)
	if err != nil {
		return err
	}
	peerBefore, err := b.fetchGantryPeerByteSnapshot(ctx, revision)
	if err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf("running Gantry-only cold pull on %d nodes\n", b.config.NodeCount))
	job, err := b.runPullJob(ctx, state, proxyPhaseGantryCold, gantryImage)
	if err != nil {
		return err
	}
	if err := b.switchProxyPhase(ctx, proxyPhaseSetup); err != nil {
		return err
	}
	metrics, err := b.waitForGantryMetricDelta(ctx, revision, metricsBefore)
	if err != nil {
		return err
	}
	windowFinish, err := b.finishTelemetryWindow(ctx, proxyPhaseGantryCold)
	if err != nil {
		return err
	}
	peerAfter, err := b.fetchGantryPeerByteSnapshot(ctx, revision)
	if err != nil {
		return err
	}
	peer, err := subtractPeerByteSnapshots(peerBefore, peerAfter)
	if err != nil {
		return err
	}

	bytes, bytesSource := deriveOriginBytes(b.config, proxyPhaseGantryCold, proxyPhaseTotals{}, metrics, job)
	gantryResult := phaseResult{
		RunID: state.RunID, Phase: proxyPhaseGantryCold, Image: gantryImage,
		ImageSizeMiB: b.config.ImageSizeMiB, PayloadSHA: state.WorkloadPayloadSHA256,
		Gantry: metrics, GantryPeer: peer,
		Azure: azurePhaseMeasurement{Window: telemetryWindow{StartedAt: windowStart, FinishedAt: windowFinish}},
		Job:   job, OriginBytes: bytes, OriginBytesSource: bytesSource, RecordedAt: time.Now().UTC(),
	}
	if b.config.AzureTelemetry {
		gantryResult.Azure, err = b.collectAzurePhaseUntilStable(ctx, gantryResult)
		if err != nil {
			return fmt.Errorf("collect Gantry-only Azure telemetry: %w", err)
		}
		gantryResult.OriginBytes = gantryResult.Azure.PrivateEndpoint.BytesFromACR
		gantryResult.OriginBytesSource = originBytesPrivateEndpoint
		gantryResult.PodStartupLatency = gantryResult.Azure.Audit.PodStartupLatency
		gantryResult.PodStartupLatencySource = gantryResult.Azure.Audit.Source
	}
	if err := b.writeJSONArtifact(state.RunID, "gantry-cold.json", gantryResult); err != nil {
		return err
	}

	comparison := compareResults(b.config, baselineResult, gantryResult)
	if err := b.writeComparisonArtifacts(comparison); err != nil {
		return err
	}
	writeAll(b.stdout, fmt.Sprintf("Gantry-only origin bytes=%d reduction=%.2f%%; P95 baseline=%.3fs Gantry=%.3fs\n",
		gantryResult.OriginBytes, 100*comparison.OriginByteReduction,
		phaseStartupLatency(baselineResult).P95Seconds, phaseStartupLatency(gantryResult).P95Seconds))
	if !comparison.Passed {
		return fmt.Errorf("Gantry-only benchmark completed but regression gates failed; see %s/%s/comparison.json", b.config.StateRoot, state.RunID)
	}

	return nil
}
