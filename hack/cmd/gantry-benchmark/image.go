// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
)

func (b *benchmark) loginRegistry(ctx context.Context) error {
	_, err := b.commands.Run(
		ctx,
		[]byte(b.config.ACRPassword+"\n"),
		b.config.ContainerEngine,
		"login",
		"--username", b.config.ACRUsername,
		"--password-stdin",
		b.config.ACRLoginServer,
	)
	if err != nil {
		return fmt.Errorf("log in to ACR with %s: %w", b.config.ContainerEngine, err)
	}

	return nil
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
