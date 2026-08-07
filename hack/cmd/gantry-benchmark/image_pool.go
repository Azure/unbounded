// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

const (
	gantryImagePoolSchemaVersion = 1
	maxGantryImagePrebuildCount  = 100
)

type gantryImagePoolEntry struct {
	SchemaVersion        int        `json:"schema_version"`
	ID                   string     `json:"id"`
	CreatedAt            time.Time  `json:"created_at"`
	Image                string     `json:"image"`
	PayloadSHA256        string     `json:"payload_sha256"`
	ImageSizeMiB         int        `json:"image_size_mib"`
	ImageLayers          int        `json:"image_layers"`
	ImagePlatform        string     `json:"image_platform"`
	WorkloadRepository   string     `json:"workload_repository"`
	GantryACRLoginServer string     `json:"gantry_acr_login_server"`
	ClaimedByRunID       string     `json:"claimed_by_run_id,omitempty"`
	ClaimedAt            *time.Time `json:"claimed_at,omitempty"`
}

func (b *benchmark) prebuildGantryImages(ctx context.Context, count int) error {
	if count < 1 || count > maxGantryImagePrebuildCount {
		return fmt.Errorf("prebuild count must be between 1 and %d, got %d", maxGantryImagePrebuildCount, count)
	}

	if b.config.usesProxy() {
		return errors.New("prebuild-gantry requires direct dual-ACR mode")
	}

	if b.config.GantryACRLoginServer == "" || b.config.GantryACRUsername == "" || b.config.GantryACRPassword == "" {
		return errors.New("prebuild-gantry requires GANTRY_ACR_LOGIN_SERVER, GANTRY_ACR_USERNAME, and GANTRY_ACR_PASSWORD")
	}

	if err := b.ensureImagePoolDirectories(); err != nil {
		return err
	}

	if err := b.loginRegistry(ctx, b.config.GantryACRLoginServer, b.config.GantryACRUsername, b.config.GantryACRPassword); err != nil {
		return fmt.Errorf("log in to Gantry ACR: %w", err)
	}

	writeAll(b.stdout, fmt.Sprintf(
		"prebuilding %d Gantry images (%s, %d layers) into %s\n",
		count,
		formatMiB(b.config.ImageSizeMiB),
		b.config.ImageLayers,
		b.config.ImagePoolRoot,
	))

	for index := range count {
		entryID, err := newImagePoolEntryID()
		if err != nil {
			return err
		}

		writeAll(b.stdout, fmt.Sprintf("pool image %d/%d: %s\n", index+1, count, entryID))

		entry, taggedImage, err := b.buildGantryPoolImage(ctx, entryID)
		if taggedImage != "" {
			b.removeLocalImage(taggedImage)
		}

		if err != nil {
			return fmt.Errorf("prebuild pool image %s: %w", entryID, err)
		}

		if err := writeJSONAtomic(b.imagePoolReadyPath(entry.ID), entry); err != nil {
			return fmt.Errorf("record pool image %s: %w", entry.ID, err)
		}

		writeAll(b.stdout, fmt.Sprintf(
			"pool image ready: id=%s image=%s payload=%s\n",
			entry.ID,
			entry.Image,
			entry.PayloadSHA256,
		))
	}

	return b.printImagePoolStatus()
}

func (b *benchmark) buildGantryPoolImage(ctx context.Context, entryID string) (gantryImagePoolEntry, string, error) {
	buildDirectory := filepath.Join(b.imagePoolBuildDirectory(), entryID)
	if err := os.RemoveAll(buildDirectory); err != nil {
		return gantryImagePoolEntry{}, "", fmt.Errorf("clear image pool build directory: %w", err)
	}

	defer func() { _ = os.RemoveAll(buildDirectory) }() //nolint:errcheck // Pool metadata and the pushed image are authoritative.

	if err := os.MkdirAll(buildDirectory, 0o750); err != nil {
		return gantryImagePoolEntry{}, "", fmt.Errorf("create image pool build directory: %w", err)
	}

	payloadPaths, err := b.writeImagePayloads(buildDirectory)
	if err != nil {
		return gantryImagePoolEntry{}, "", err
	}
	defer removePayloads(payloadPaths)

	payloadSHA, err := payloadSHA256(payloadPaths)
	if err != nil {
		return gantryImagePoolEntry{}, "", err
	}

	dockerfile := dualACRDockerfile(proxyPhase("gantry-pool-"+entryID), payloadPaths, payloadSHA)
	if err := os.WriteFile(filepath.Join(buildDirectory, "Dockerfile."+string(proxyPhaseGantryCold)), []byte(dockerfile), 0o640); err != nil {
		return gantryImagePoolEntry{}, "", fmt.Errorf("write image pool Dockerfile: %w", err)
	}

	taggedImage := fmt.Sprintf("%s/%s:%s", b.config.GantryACRLoginServer, b.config.WorkloadRepository, entryID)

	imageDigest, err := b.buildAndPushPreparedImage(ctx, buildDirectory, proxyPhaseGantryCold, taggedImage)
	if err != nil {
		return gantryImagePoolEntry{}, taggedImage, fmt.Errorf("build and push image pool entry: %w", err)
	}

	return gantryImagePoolEntry{
		SchemaVersion:        gantryImagePoolSchemaVersion,
		ID:                   entryID,
		CreatedAt:            time.Now().UTC(),
		Image:                fmt.Sprintf("%s/%s@%s", b.config.GantryACRLoginServer, b.config.WorkloadRepository, imageDigest),
		PayloadSHA256:        payloadSHA,
		ImageSizeMiB:         b.config.ImageSizeMiB,
		ImageLayers:          b.config.ImageLayers,
		ImagePlatform:        b.config.ImagePlatform,
		WorkloadRepository:   b.config.WorkloadRepository,
		GantryACRLoginServer: b.config.GantryACRLoginServer,
	}, taggedImage, nil
}

func (b *benchmark) removeLocalImage(taggedImage string) {
	var args []string

	switch b.config.ContainerEngine {
	case "podman", "docker":
		args = []string{"image", "rm", "-f", taggedImage}
	default:
		return
	}

	cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if _, err := b.commands.Run(cleanupContext, nil, b.config.ContainerEngine, args...); err != nil {
		writeAll(b.stderr, fmt.Sprintf("warning: remove local pool image %s: %v\n", taggedImage, err))
	}
}

func (b *benchmark) prepareGantryOnlyFromPool(ctx context.Context, baselineRunID string) error {
	state, err := b.loadState(ctx)
	if err != nil {
		return err
	}

	if state.Status != "enabled" {
		return fmt.Errorf("benchmark state is %q, run enable before prepare-gantry-pool", state.Status)
	}

	if state.usesProxy() {
		return errors.New("prepare-gantry-pool requires direct dual-ACR mode")
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

	entry, readyPath, claimedPath, err := b.claimImagePoolEntry(state, baselineState)
	if err != nil {
		return err
	}

	adopted := false

	defer func() {
		if !adopted {
			entry.ClaimedByRunID = ""
			entry.ClaimedAt = nil

			if err := writeJSONAtomic(claimedPath, entry); err == nil {
				_ = os.Rename(claimedPath, readyPath) //nolint:errcheck // Preserve the primary adoption error.
			}
		}
	}()

	baselineResult.RunID = state.RunID
	baselineResult.ImageLayers = state.ImageLayers
	baselineResult.WorkloadComparisonMode = workloadComparisonRandomShape
	state.BaselineImage = baselineState.BaselineImage
	state.GantryColdImage = entry.Image
	state.WorkloadPayloadSHA256 = entry.PayloadSHA256
	state.WorkloadComparisonMode = workloadComparisonRandomShape
	state.Status = "images-prepared"

	if err := b.writeJSONArtifact(state.RunID, "baseline.json", baselineResult); err != nil {
		return err
	}

	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	adopted = true

	writeAll(b.stdout, fmt.Sprintf(
		"claimed prebuilt Gantry image %s for %s from pool entry %s\n",
		entry.Image,
		state.RunID,
		entry.ID,
	))

	return nil
}

func (b *benchmark) claimImagePoolEntry(current, baseline benchmarkState) (gantryImagePoolEntry, string, string, error) {
	if err := b.ensureImagePoolDirectories(); err != nil {
		return gantryImagePoolEntry{}, "", "", err
	}

	entries, err := os.ReadDir(b.imagePoolReadyDirectory())
	if err != nil {
		return gantryImagePoolEntry{}, "", "", fmt.Errorf("list ready image pool entries: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	compatible := 0

	for _, file := range entries {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}

		readyPath := filepath.Join(b.imagePoolReadyDirectory(), file.Name())

		entry, err := readImagePoolEntry(readyPath)
		if err != nil || file.Name() != entry.ID+".json" || validateImagePoolEntry(entry, b.config, current, baseline) != nil {
			continue
		}

		compatible++

		claimedName := current.RunID + "--" + file.Name()

		claimedPath := filepath.Join(b.imagePoolClaimedDirectory(), claimedName)
		if err := os.Rename(readyPath, claimedPath); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			return gantryImagePoolEntry{}, "", "", fmt.Errorf("claim image pool entry %s: %w", entry.ID, err)
		}

		entry.ClaimedByRunID = current.RunID
		claimedAt := time.Now().UTC()

		entry.ClaimedAt = &claimedAt
		if err := writeJSONAtomic(claimedPath, entry); err != nil {
			_ = os.Rename(claimedPath, readyPath) //nolint:errcheck // Preserve metadata write error.

			return gantryImagePoolEntry{}, "", "", fmt.Errorf("record image pool claim %s: %w", entry.ID, err)
		}

		return entry, readyPath, claimedPath, nil
	}

	return gantryImagePoolEntry{}, "", "", fmt.Errorf(
		"no compatible ready Gantry image in %s (examined %d entries, %d compatible before claim races)",
		b.imagePoolReadyDirectory(),
		len(entries),
		compatible,
	)
}

func validateImagePoolEntry(entry gantryImagePoolEntry, config benchmarkConfig, current, baseline benchmarkState) error {
	if entry.SchemaVersion != gantryImagePoolSchemaVersion {
		return fmt.Errorf("pool entry schema %d, want %d", entry.SchemaVersion, gantryImagePoolSchemaVersion)
	}

	if entry.ID == "" || entry.ID == "." || entry.ID == ".." || filepath.Base(entry.ID) != entry.ID {
		return fmt.Errorf("invalid pool entry ID %q", entry.ID)
	}

	if entry.ImageSizeMiB != current.ImageSizeMiB || entry.ImageLayers != current.ImageLayers ||
		entry.ImagePlatform != current.ImagePlatform || entry.WorkloadRepository != current.WorkloadRepository ||
		entry.GantryACRLoginServer != current.GantryACRLoginServer {
		return errors.New("pool entry shape does not match current benchmark")
	}

	if entry.ImageSizeMiB != config.ImageSizeMiB || entry.ImageLayers != config.ImageLayers ||
		entry.ImagePlatform != config.ImagePlatform || entry.WorkloadRepository != config.WorkloadRepository ||
		entry.GantryACRLoginServer != config.GantryACRLoginServer {
		return errors.New("pool entry shape does not match configured builder")
	}

	return validateAdoptedFreshGantryImage(current, baseline, entry.Image, entry.PayloadSHA256)
}

func (b *benchmark) printImagePoolStatus() error {
	if err := b.ensureImagePoolDirectories(); err != nil {
		return err
	}

	ready, err := readImagePoolDirectory(b.imagePoolReadyDirectory())
	if err != nil {
		return err
	}

	claimed, err := readImagePoolDirectory(b.imagePoolClaimedDirectory())
	if err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf("Gantry image pool: root=%s ready=%d claimed=%d\n", b.config.ImagePoolRoot, len(ready), len(claimed)))

	for _, entry := range ready {
		writeAll(b.stdout, fmt.Sprintf(
			"READY   %s %s %s %s/%d\n",
			entry.ID,
			entry.CreatedAt.Format(time.RFC3339),
			entry.Image,
			formatMiB(entry.ImageSizeMiB),
			entry.ImageLayers,
		))
	}

	for _, entry := range claimed {
		claimedAt := "unknown"
		if entry.ClaimedAt != nil {
			claimedAt = entry.ClaimedAt.Format(time.RFC3339)
		}

		writeAll(b.stdout, fmt.Sprintf(
			"CLAIMED %s run=%s at=%s image=%s\n",
			entry.ID,
			entry.ClaimedByRunID,
			claimedAt,
			entry.Image,
		))
	}

	return nil
}

func readImagePoolDirectory(directory string) ([]gantryImagePoolEntry, error) {
	files, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read image pool directory %s: %w", directory, err)
	}

	entries := make([]gantryImagePoolEntry, 0, len(files))
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}

		entry, err := readImagePoolEntry(filepath.Join(directory, file.Name()))
		if err != nil {
			return nil, err
		}

		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })

	return entries, nil
}

func readImagePoolEntry(path string) (gantryImagePoolEntry, error) {
	var entry gantryImagePoolEntry
	if err := readJSONFile(path, &entry); err != nil {
		return gantryImagePoolEntry{}, fmt.Errorf("read image pool entry %s: %w", path, err)
	}

	return entry, nil
}

func writeJSONAtomic(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".pool-entry-*.tmp")
	if err != nil {
		return err
	}

	temporaryPath := temporary.Name()

	defer func() { _ = os.Remove(temporaryPath) }() //nolint:errcheck // Rename removes successful temporary files.

	if err := temporary.Chmod(0o640); err != nil {
		return errors.Join(err, temporary.Close())
	}

	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		return errors.Join(err, temporary.Close())
	}

	if err := temporary.Sync(); err != nil {
		return errors.Join(err, temporary.Close())
	}

	if err := temporary.Close(); err != nil {
		return err
	}

	return os.Rename(temporaryPath, path)
}

func newImagePoolEntryID() (string, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}

	return "pool-" + time.Now().UTC().Format("20060102-150405.000000000") + "-" + suffix, nil
}

func (b *benchmark) ensureImagePoolDirectories() error {
	for _, directory := range []string{b.imagePoolReadyDirectory(), b.imagePoolClaimedDirectory(), b.imagePoolBuildDirectory()} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return fmt.Errorf("create image pool directory %s: %w", directory, err)
		}
	}

	return nil
}

func (b *benchmark) imagePoolReadyDirectory() string {
	return filepath.Join(b.config.ImagePoolRoot, "ready")
}

func (b *benchmark) imagePoolClaimedDirectory() string {
	return filepath.Join(b.config.ImagePoolRoot, "claimed")
}

func (b *benchmark) imagePoolBuildDirectory() string {
	if b.config.ImagePoolBuildRoot != "" {
		return b.config.ImagePoolBuildRoot
	}

	return filepath.Join(b.config.ImagePoolRoot, "build")
}

func (b *benchmark) imagePoolReadyPath(entryID string) string {
	return filepath.Join(b.imagePoolReadyDirectory(), entryID+".json")
}

func parsePrebuildCount(value string) (int, error) {
	count, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse prebuild count %q: %w", value, err)
	}

	return count, nil
}
