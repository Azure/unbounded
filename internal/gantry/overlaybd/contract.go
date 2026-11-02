// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package overlaybd implements the ACR Artifact Streaming compatibility
// contract currently consumed by the OverlayBD snapshotter.
package overlaybd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	AnnotationBlobDigest = "containerd.io/snapshot/overlaybd/blob-digest"
	AnnotationBlobSize   = "containerd.io/snapshot/overlaybd/blob-size"
	AnnotationBlobFSType = "containerd.io/snapshot/overlaybd/blob-fs-type"
	AnnotationVersion    = "containerd.io/snapshot/overlaybd/version"

	VersionV1 = "0.1.0"
)

var ErrNotArtifactStreaming = errors.New("not an ACR OverlayBD streaming manifest")

// Layer is one immutable ACR-produced OverlayBD lower, in manifest order.
type Layer struct {
	DescriptorDigest digest.Digest
	DataDigest       digest.Digest
	Size             int64
	FSType           string
	Version          string
}

// Manifest is the validated subset of the ACR Artifact Streaming contract
// needed to construct a read-only snapshot.
type Manifest struct {
	ConfigDigest digest.Digest
	Layers       []Layer
}

// ParseManifest validates the current ACR OverlayBD descriptor contract.
// Missing optional version and filesystem annotations retain the snapshotter's
// existing defaults; malformed or partially annotated manifests fail closed.
func ParseManifest(body []byte) (Manifest, error) {
	var manifest ocispec.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}

	if manifest.SchemaVersion != 2 || len(manifest.Layers) == 0 {
		return Manifest{}, ErrNotArtifactStreaming
	}

	if err := manifest.Config.Digest.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("invalid config digest: %w", err)
	}

	result := Manifest{
		ConfigDigest: manifest.Config.Digest,
		Layers:       make([]Layer, 0, len(manifest.Layers)),
	}

	for index, descriptor := range manifest.Layers {
		layer, err := parseLayer(descriptor)
		if err != nil {
			return Manifest{}, fmt.Errorf("layer %d: %w", index, err)
		}

		result.Layers = append(result.Layers, layer)
	}

	return result, nil
}

func parseLayer(descriptor ocispec.Descriptor) (Layer, error) {
	if len(descriptor.URLs) != 0 {
		return Layer{}, ErrNotArtifactStreaming
	}

	dataDigestText := descriptor.Annotations[AnnotationBlobDigest]

	sizeText := descriptor.Annotations[AnnotationBlobSize]
	if dataDigestText == "" || sizeText == "" {
		return Layer{}, ErrNotArtifactStreaming
	}

	dataDigest, err := digest.Parse(dataDigestText)
	if err != nil || dataDigest.Algorithm() != digest.SHA256 {
		return Layer{}, fmt.Errorf("invalid blob digest %q", dataDigestText)
	}

	if err := descriptor.Digest.Validate(); err != nil {
		return Layer{}, fmt.Errorf("invalid descriptor digest: %w", err)
	}

	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || size <= 0 {
		return Layer{}, fmt.Errorf("invalid blob size %q", sizeText)
	}

	if descriptor.Size != size {
		return Layer{}, fmt.Errorf("blob size %d does not match descriptor size %d", size, descriptor.Size)
	}

	version := descriptor.Annotations[AnnotationVersion]
	if version != "" && version != VersionV1 {
		return Layer{}, fmt.Errorf("unsupported OverlayBD version %q", version)
	}

	return Layer{
		DescriptorDigest: descriptor.Digest,
		DataDigest:       dataDigest,
		Size:             size,
		FSType:           descriptor.Annotations[AnnotationBlobFSType],
		Version:          version,
	}, nil
}
