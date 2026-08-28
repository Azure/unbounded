// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
)

// runStreaming reports child-process output live. Image builds and pushes run
// for many minutes, so buffering until exit hides all progress.
func (b *benchmark) runStreaming(ctx context.Context, prefix, name string, args ...string) ([]byte, error) {
	streamer, ok := b.commands.(streamingCommandRunner)
	if !ok {
		return b.commands.Run(ctx, nil, name, args...)
	}

	writer := &prefixWriter{target: b.stdout, prefix: prefix}
	defer writer.Flush()

	return streamer.RunStreaming(ctx, nil, writer, name, args...)
}

func formatMiB(sizeMiB int) string {
	if sizeMiB >= 1024 {
		return fmt.Sprintf("%.2f GiB", float64(sizeMiB)/1024)
	}

	return fmt.Sprintf("%d MiB", sizeMiB)
}

func formatElapsed(since time.Time) string {
	return time.Since(since).Round(time.Second).String()
}

func (b *benchmark) loginRegistry(ctx context.Context, loginServer, username, password string) error {
	_, err := b.commands.Run(
		ctx,
		[]byte(password+"\n"),
		b.config.ContainerEngine,
		"login",
		"--username", username,
		"--password-stdin",
		loginServer,
	)
	if err != nil {
		return fmt.Errorf("log in to ACR with %s: %w", b.config.ContainerEngine, err)
	}

	return nil
}

func (b *benchmark) prepareImages(ctx context.Context) error {
	state, err := b.loadState(ctx)
	if err != nil {
		return err
	}

	if state.Status == "images-prepared" {
		if _, _, err := state.preparedImages(); err != nil {
			return err
		}

		writeAll(b.stdout, fmt.Sprintf("images already prepared for %s\n", state.RunID))

		return nil
	}

	if state.Status != "enabled" {
		return fmt.Errorf("benchmark state is %q, run enable before prepare", state.Status)
	}

	if err := b.requireLock(ctx, state.RunID); err != nil {
		return err
	}

	if err := b.validateContext(ctx); err != nil {
		return err
	}

	if state.usesProxy() {
		if b.config.ACRLoginServer == "" || b.config.ACRUsername == "" || b.config.ACRPassword == "" {
			return fmt.Errorf("ACR build credentials require ACR_LOGIN_SERVER, ACR_USERNAME, and ACR_PASSWORD") //nolint:staticcheck // Environment variable names are intentionally uppercase.
		}

		if b.config.ACRLoginServer != state.ACRLoginServer {
			return fmt.Errorf("configured ACR_LOGIN_SERVER=%q does not match enabled benchmark registry %q", b.config.ACRLoginServer, state.ACRLoginServer)
		}

		if err := b.loginRegistry(ctx, b.config.ACRLoginServer, b.config.ACRUsername, b.config.ACRPassword); err != nil {
			return err
		}

		writeAll(b.stdout, "building fresh baseline image\n")

		state.BaselineImage, err = b.buildFreshImage(ctx, state, proxyPhaseBaseline)
		if err != nil {
			return err
		}

		writeAll(b.stdout, "building fresh Gantry cold image\n")

		state.GantryColdImage, err = b.buildFreshImage(ctx, state, proxyPhaseGantryCold)
		if err != nil {
			return err
		}
	} else {
		if b.config.BaselineACRUsername == "" || b.config.BaselineACRPassword == "" || b.config.GantryACRUsername == "" || b.config.GantryACRPassword == "" {
			return fmt.Errorf("dual-ACR preparation requires BASELINE_ACR_USERNAME, BASELINE_ACR_PASSWORD, GANTRY_ACR_USERNAME, and GANTRY_ACR_PASSWORD") //nolint:staticcheck // Environment variable names are intentionally uppercase.
		}

		if b.config.BaselineACRLoginServer != state.BaselineACRLoginServer || b.config.GantryACRLoginServer != state.GantryACRLoginServer {
			return fmt.Errorf(
				"configured registries baseline=%q Gantry=%q do not match enabled benchmark registries baseline=%q Gantry=%q",
				b.config.BaselineACRLoginServer,
				b.config.GantryACRLoginServer,
				state.BaselineACRLoginServer,
				state.GantryACRLoginServer,
			)
		}

		if err := b.loginRegistry(ctx, b.config.BaselineACRLoginServer, b.config.BaselineACRUsername, b.config.BaselineACRPassword); err != nil {
			return fmt.Errorf("log in to baseline ACR: %w", err)
		}

		if err := b.loginRegistry(ctx, b.config.GantryACRLoginServer, b.config.GantryACRUsername, b.config.GantryACRPassword); err != nil {
			return fmt.Errorf("log in to Gantry ACR: %w", err)
		}

		writeAll(b.stdout, "building one shared payload for baseline and Gantry ACRs\n")

		state.BaselineImage, state.GantryColdImage, state.WorkloadPayloadSHA256, err = b.buildDualACRImages(ctx, state)
		if err != nil {
			return err
		}

		state.WorkloadComparisonMode = workloadComparisonIdenticalPayload
	}

	state.Status = "images-prepared"
	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf("prepared digest-pinned images for %s\n", state.RunID))

	return nil
}

func (b *benchmark) prepareAdoptedImages(ctx context.Context, baselineImage, gantryImage, payloadSHA string) error {
	state, err := b.loadState(ctx)
	if err != nil {
		return err
	}

	if state.Status != "enabled" {
		return fmt.Errorf("benchmark state is %q, run enable before prepare-adopt", state.Status)
	}

	if state.usesProxy() {
		return fmt.Errorf("prepare-adopt requires direct dual-ACR mode")
	}

	if err := b.requireLock(ctx, state.RunID); err != nil {
		return err
	}

	if err := b.validateContext(ctx); err != nil {
		return err
	}

	state, err = adoptPreparedImages(state, baselineImage, gantryImage, payloadSHA)
	if err != nil {
		return err
	}

	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf("adopted digest-pinned images for %s using shared payload %s\n", state.RunID, payloadSHA))

	return nil
}

func adoptPreparedImages(state benchmarkState, baselineImage, gantryImage, payloadSHA string) (benchmarkState, error) {
	payloadDigest, err := digest.Parse(payloadSHA)
	if err != nil || payloadDigest.Algorithm() != digest.SHA256 {
		return benchmarkState{}, fmt.Errorf("adopted payload fingerprint %q must be a valid sha256 digest", payloadSHA)
	}

	state.BaselineImage = baselineImage
	state.GantryColdImage = gantryImage
	state.WorkloadPayloadSHA256 = payloadSHA

	state.WorkloadComparisonMode = workloadComparisonIdenticalPayload
	if _, _, err := state.preparedImages(); err != nil {
		return benchmarkState{}, fmt.Errorf("validate adopted images: %w", err)
	}

	state.Status = "images-prepared"

	return state, nil
}

func (b *benchmark) buildDualACRImages(ctx context.Context, state benchmarkState) (string, string, string, error) {
	buildDirectory := filepath.Join(b.config.StateRoot, state.RunID, "build", "shared-payload")
	if err := os.RemoveAll(buildDirectory); err != nil {
		return "", "", "", fmt.Errorf("clear shared image build directory: %w", err)
	}

	if err := os.MkdirAll(buildDirectory, 0o750); err != nil {
		return "", "", "", fmt.Errorf("create shared image build directory: %w", err)
	}

	payloadPaths, err := b.writeImagePayloads(buildDirectory)
	if err != nil {
		return "", "", "", err
	}

	defer removePayloads(payloadPaths)

	hashStarted := time.Now()

	writeAll(b.stdout, fmt.Sprintf("hashing %s shared payload\n", formatMiB(b.config.ImageSizeMiB)))

	payloadSHA, err := payloadSHA256(payloadPaths)
	if err != nil {
		return "", "", "", err
	}

	writeAll(b.stdout, fmt.Sprintf("shared payload fingerprint %s in %s\n", payloadSHA, formatElapsed(hashStarted)))

	for _, phase := range []proxyPhase{proxyPhaseBaseline, proxyPhaseGantryCold} {
		dockerfile := dualACRDockerfile(phase, payloadPaths, payloadSHA)
		if err := os.WriteFile(filepath.Join(buildDirectory, "Dockerfile."+string(phase)), []byte(dockerfile), 0o640); err != nil {
			return "", "", "", fmt.Errorf("write %s workload Dockerfile: %w", phase, err)
		}
	}

	tag := strings.ReplaceAll(state.RunID, "_", "-")
	baselineTagged := fmt.Sprintf("%s/%s:%s", state.BaselineACRLoginServer, b.config.WorkloadRepository, tag)
	gantryTagged := fmt.Sprintf("%s/%s:%s", state.GantryACRLoginServer, b.config.WorkloadRepository, tag)

	writeAll(b.stdout, fmt.Sprintf("image 1 of 2: baseline -> %s\n", baselineTagged))

	baselineDigest, err := b.buildAndPushPreparedImage(ctx, buildDirectory, proxyPhaseBaseline, baselineTagged)
	if err != nil {
		return "", "", "", fmt.Errorf("build and push baseline ACR image: %w", err)
	}

	writeAll(b.stdout, fmt.Sprintf("image 2 of 2: Gantry -> %s\n", gantryTagged))

	gantryDigest, err := b.buildAndPushPreparedImage(ctx, buildDirectory, proxyPhaseGantryCold, gantryTagged)
	if err != nil {
		return "", "", "", fmt.Errorf("build and push Gantry ACR image: %w", err)
	}

	if baselineDigest == gantryDigest {
		return "", "", "", fmt.Errorf("phase image digests are identical and would reuse node cache: %s", baselineDigest)
	}

	return fmt.Sprintf("%s/%s@%s", state.BaselineACRLoginServer, b.config.WorkloadRepository, baselineDigest),
		fmt.Sprintf("%s/%s@%s", state.GantryACRLoginServer, b.config.WorkloadRepository, gantryDigest),
		payloadSHA,
		nil
}

func (b *benchmark) writeImagePayloads(buildDirectory string) ([]string, error) {
	layers := b.config.ImageLayers
	perLayerMiB := b.config.ImageSizeMiB / layers
	remainderMiB := b.config.ImageSizeMiB % layers
	payloadPaths := make([]string, 0, layers)

	started := time.Now()
	writtenMiB := 0

	writeAll(b.stdout, fmt.Sprintf(
		"generating %s of random payload across %d layers in %s\n",
		formatMiB(b.config.ImageSizeMiB), layers, buildDirectory,
	))

	for index := range layers {
		sizeMiB := perLayerMiB
		if index == layers-1 {
			sizeMiB += remainderMiB
		}

		path := filepath.Join(buildDirectory, fmt.Sprintf("payload%d.bin", index))

		layerStarted := time.Now()

		if err := writeRandomPayload(path, int64(sizeMiB)*mibibyte); err != nil {
			removePayloads(payloadPaths)

			return nil, fmt.Errorf("write shared random workload payload %d: %w", index, err)
		}

		payloadPaths = append(payloadPaths, path)
		writtenMiB += sizeMiB

		writeAll(b.stdout, fmt.Sprintf(
			"  payload layer %d/%d: %s in %s (%s of %s, %.0f%%)\n",
			index+1, layers, formatMiB(sizeMiB), formatElapsed(layerStarted),
			formatMiB(writtenMiB), formatMiB(b.config.ImageSizeMiB),
			float64(writtenMiB)/float64(b.config.ImageSizeMiB)*100,
		))
	}

	writeAll(b.stdout, fmt.Sprintf(
		"payload generation complete: %s in %s\n",
		formatMiB(b.config.ImageSizeMiB), formatElapsed(started),
	))

	return payloadPaths, nil
}

func dualACRDockerfile(phase proxyPhase, payloadPaths []string, payloadSHA string) string {
	var copyLines strings.Builder

	phasePath := strings.ReplaceAll(string(phase), "_", "-")

	for _, path := range payloadPaths {
		name := filepath.Base(path)
		fmt.Fprintf(&copyLines, "COPY %s /gantry-benchmark-payload/%s/%s\n", name, phasePath, name)
	}

	return fmt.Sprintf(`FROM docker.io/library/alpine:3.20
LABEL io.unbounded.gantry-benchmark.payload-sha256=%q
%sCMD ["sh", "-c", "date -u +%%Y-%%m-%%dT%%H:%%M:%%S.%%NZ"]
`, payloadSHA, copyLines.String())
}

func payloadSHA256(paths []string) (string, error) {
	hasher := sha256.New()

	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open payload for hashing: %w", err)
		}

		_, copyErr := io.Copy(hasher, file)

		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return "", fmt.Errorf("hash payload %s: %w", filepath.Base(path), err)
		}
	}

	return fmt.Sprintf("sha256:%x", hasher.Sum(nil)), nil
}

func removePayloads(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path) //nolint:errcheck // The pushed image is authoritative; stale local payload cleanup is best effort.
	}
}

func (b *benchmark) buildAndPushPreparedImage(ctx context.Context, buildDirectory string, phase proxyPhase, taggedImage string) (digest.Digest, error) {
	dockerfilePath := filepath.Join(buildDirectory, "Dockerfile."+string(phase))
	digestPath := filepath.Join(buildDirectory, "push-digest."+string(phase)+".txt")
	metadataPath := filepath.Join(buildDirectory, "buildx-metadata."+string(phase)+".json")

	var imageDigest string

	switch b.config.ContainerEngine {
	case "docker":
		buildStarted := time.Now()

		writeAll(b.stdout, fmt.Sprintf("  [%s] docker buildx build and push starting\n", phase))

		if _, err := b.runStreaming(
			ctx,
			"  ["+string(phase)+"] ",
			"docker", "buildx", "build",
			"--platform", b.config.ImagePlatform,
			"--progress", "plain",
			"--file", dockerfilePath,
			"--tag", taggedImage,
			"--output", "type=image,push=true,oci-mediatypes=true",
			"--provenance=false",
			"--sbom=false",
			"--metadata-file", metadataPath,
			buildDirectory,
		); err != nil {
			return "", err
		}

		writeAll(b.stdout, fmt.Sprintf("  [%s] build and push complete in %s\n", phase, formatElapsed(buildStarted)))

		metadata, err := os.ReadFile(metadataPath)
		if err != nil {
			return "", fmt.Errorf("read Buildx metadata: %w", err)
		}

		var parsed struct {
			Digest string `json:"containerimage.digest"`
		}
		if err := json.Unmarshal(metadata, &parsed); err != nil {
			return "", fmt.Errorf("decode Buildx metadata: %w", err)
		}

		imageDigest = parsed.Digest
	case "podman":
		buildStarted := time.Now()

		writeAll(b.stdout, fmt.Sprintf("  [%s] podman build starting\n", phase))

		if _, err := b.runStreaming(
			ctx,
			"  ["+string(phase)+" build] ",
			"podman", "build",
			"--isolation", "chroot",
			"--platform", b.config.ImagePlatform,
			"--format", "oci",
			"--file", dockerfilePath,
			"--tag", taggedImage,
			buildDirectory,
		); err != nil {
			return "", err
		}

		writeAll(b.stdout, fmt.Sprintf("  [%s] build complete in %s\n", phase, formatElapsed(buildStarted)))

		pushStarted := time.Now()

		writeAll(b.stdout, fmt.Sprintf("  [%s] pushing %s to %s\n", phase, formatMiB(b.config.ImageSizeMiB), taggedImage))

		if _, err := b.runStreaming(
			ctx,
			"  ["+string(phase)+" push] ",
			"podman", "push", "--digestfile", digestPath, taggedImage,
		); err != nil {
			return "", err
		}

		writeAll(b.stdout, fmt.Sprintf("  [%s] push complete in %s\n", phase, formatElapsed(pushStarted)))

		pushedDigest, err := os.ReadFile(digestPath)
		if err != nil {
			return "", fmt.Errorf("read Podman push digest: %w", err)
		}

		imageDigest = strings.TrimSpace(string(pushedDigest))
	default:
		return "", fmt.Errorf("unsupported CONTAINER_ENGINE %q; use podman or docker", b.config.ContainerEngine)
	}

	parsedDigest, err := digest.Parse(imageDigest)
	if err != nil {
		return "", fmt.Errorf("parse pushed image digest %q: %w", imageDigest, err)
	}

	if parsedDigest.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("pushed image digest uses %s, want sha256", parsedDigest.Algorithm())
	}

	return parsedDigest, nil
}

func (b *benchmark) buildFreshImage(ctx context.Context, state benchmarkState, phase proxyPhase) (string, error) {
	if phase != proxyPhaseBaseline && phase != proxyPhaseGantryCold {
		return "", fmt.Errorf("cannot build image for phase %q", phase)
	}

	tag := strings.ReplaceAll(state.RunID+"-"+string(phase), "_", "-")
	taggedImage := fmt.Sprintf("%s/%s:%s", b.config.ACRLoginServer, b.config.WorkloadRepository, tag)

	buildDirectory := filepath.Join(b.config.StateRoot, state.RunID, "build", string(phase))
	if err := os.RemoveAll(buildDirectory); err != nil {
		return "", fmt.Errorf("clear image build directory: %w", err)
	}

	if err := os.MkdirAll(buildDirectory, 0o750); err != nil {
		return "", fmt.Errorf("create image build directory: %w", err)
	}

	// Split the total image payload across ImageLayers separate COPY layers so
	// the workload resembles a real multi-layer image (each COPY instruction
	// produces one layer). Distributing layer-by-layer is what lets the P2P
	// cascade pipeline across layers; a single giant layer is the pathological
	// M=1 case. Each payload is fresh random bytes per run so every layer
	// digest is unique and the pull is genuinely cold.
	layers := b.config.ImageLayers
	if layers < 1 {
		layers = 1
	}

	perLayerMiB := b.config.ImageSizeMiB / layers
	remainderMiB := b.config.ImageSizeMiB % layers

	var (
		copyLines    strings.Builder
		payloadPaths []string
	)

	for i := 0; i < layers; i++ {
		sizeMiB := perLayerMiB
		if i == layers-1 {
			sizeMiB += remainderMiB // last layer absorbs the remainder
		}

		name := fmt.Sprintf("payload%d.bin", i)
		path := filepath.Join(buildDirectory, name)

		if err := writeRandomPayload(path, int64(sizeMiB)*1024*1024); err != nil {
			return "", fmt.Errorf("write random workload payload %d: %w", i, err)
		}

		payloadPaths = append(payloadPaths, path)

		fmt.Fprintf(&copyLines, "COPY %s /%s\n", name, name)
	}

	dockerfile := fmt.Sprintf(`FROM docker.io/library/alpine:3.20
%sCMD ["sh", "-c", "date -u +%%Y-%%m-%%dT%%H:%%M:%%S.%%NZ"]
`, copyLines.String())

	if err := os.WriteFile(filepath.Join(buildDirectory, "Dockerfile"), []byte(dockerfile), 0o640); err != nil {
		return "", fmt.Errorf("write workload Dockerfile: %w", err)
	}

	defer func() {
		for _, p := range payloadPaths {
			_ = os.Remove(p) //nolint:errcheck // The pushed image is authoritative; stale local payload cleanup is best effort.
		}
	}()

	var imageDigest string

	switch b.config.ContainerEngine {
	case "docker":
		metadataPath := filepath.Join(buildDirectory, "buildx-metadata.json")
		if _, err := b.commands.Run(
			ctx,
			nil,
			"docker", "buildx", "build",
			"--platform", b.config.ImagePlatform,
			"--tag", taggedImage,
			"--output", "type=image,push=true,oci-mediatypes=true",
			"--provenance=false",
			"--sbom=false",
			"--metadata-file", metadataPath,
			buildDirectory,
		); err != nil {
			return "", fmt.Errorf("build and push workload image: %w", err)
		}

		metadata, err := os.ReadFile(metadataPath)
		if err != nil {
			return "", fmt.Errorf("read Buildx metadata: %w", err)
		}

		var parsed struct {
			Digest string `json:"containerimage.digest"`
		}
		if err := json.Unmarshal(metadata, &parsed); err != nil {
			return "", fmt.Errorf("decode Buildx metadata: %w", err)
		}

		imageDigest = parsed.Digest
	case "podman":
		if _, err := b.commands.Run(
			ctx,
			nil,
			"podman", "build",
			"--isolation", "chroot",
			"--platform", b.config.ImagePlatform,
			"--format", "oci",
			"--tag", taggedImage,
			buildDirectory,
		); err != nil {
			return "", fmt.Errorf("build workload image: %w", err)
		}

		digestPath := filepath.Join(buildDirectory, "push-digest.txt")
		if _, err := b.commands.Run(
			ctx,
			nil,
			"podman", "push",
			"--digestfile", digestPath,
			taggedImage,
		); err != nil {
			return "", fmt.Errorf("push workload image: %w", err)
		}

		pushedDigest, err := os.ReadFile(digestPath)
		if err != nil {
			return "", fmt.Errorf("read Podman push digest: %w", err)
		}

		imageDigest = strings.TrimSpace(string(pushedDigest))
	default:
		return "", fmt.Errorf("unsupported CONTAINER_ENGINE %q; use podman or docker", b.config.ContainerEngine)
	}

	parsedDigest, err := digest.Parse(imageDigest)
	if err != nil {
		return "", fmt.Errorf("parse pushed image digest %q: %w", imageDigest, err)
	}

	if parsedDigest.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("pushed image digest uses %s, want sha256", parsedDigest.Algorithm())
	}

	return fmt.Sprintf("%s/%s@%s", b.config.ACRLoginServer, b.config.WorkloadRepository, parsedDigest), nil
}

func writeRandomPayload(path string, size int64) (returnErr error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()

	buffer := make([]byte, 1024*1024)

	var written int64
	for written < size {
		chunk := buffer

		remaining := size - written
		if remaining < int64(len(chunk)) {
			chunk = chunk[:remaining]
		}

		if _, err := rand.Read(chunk); err != nil {
			return err
		}

		count, err := file.Write(chunk)
		if err != nil {
			return err
		}

		written += int64(count)
	}

	return file.Sync()
}
