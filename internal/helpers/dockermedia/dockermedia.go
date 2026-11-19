// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package dockermedia lets OCI tooling read images that use Docker V2 media
// types. Images built by `docker build` use those types, and both the agent and
// metalman unpack such images.
package dockermedia

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci/oci/casext/mediatype"
)

// Docker V2 media types that are structurally compatible with OCI equivalents.
const (
	ManifestType     = "application/vnd.docker.distribution.manifest.v2+json"
	ManifestListType = "application/vnd.docker.distribution.manifest.list.v2+json"
	ConfigType       = "application/vnd.docker.container.image.v1+json"
	LayerType        = "application/vnd.docker.image.rootfs.diff.tar"
	LayerGzipType    = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	ForeignLayerType = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
)

var registerOnce sync.Once

// RegisterParsers registers umoci media-type parsers for Docker V2 media
// types so that umoci's FromDescriptor returns parsed Go structs
// (ispec.Manifest, ispec.Index, ispec.Image) instead of raw readers.
//
// Docker V2 manifests and manifest lists are structurally compatible with their
// OCI counterparts, so simple JSON decoding into the OCI types is sufficient.
//
// This function is safe to call multiple times; registration happens only once.
func RegisterParsers() {
	registerOnce.Do(func() {
		mediatype.RegisterTarget(ManifestType)
		mediatype.RegisterParser(ManifestType, parseManifest)
		mediatype.RegisterParser(ManifestListType, parseIndex)
		mediatype.RegisterParser(ConfigType, parseImage)
	})
}

func parseManifest(rdr io.Reader) (any, error) {
	var m ispec.Manifest
	if rdr == nil {
		return m, nil
	}

	if err := json.NewDecoder(rdr).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode Docker manifest: %w", err)
	}

	return m, nil
}

func parseIndex(rdr io.Reader) (any, error) {
	var idx ispec.Index
	if rdr == nil {
		return idx, nil
	}

	if err := json.NewDecoder(rdr).Decode(&idx); err != nil {
		return nil, fmt.Errorf("decode Docker manifest list: %w", err)
	}

	return idx, nil
}

func parseImage(rdr io.Reader) (any, error) {
	var img ispec.Image
	if rdr == nil {
		return img, nil
	}

	if err := json.NewDecoder(rdr).Decode(&img); err != nil {
		return nil, fmt.Errorf("decode Docker image config: %w", err)
	}

	return img, nil
}

// ConvertToOCI rewrites Docker V2 media types in a manifest to their OCI
// equivalents in-place.  Docker V2 and OCI blobs are structurally identical;
// only the MIME types differ.  umoci's UnpackRootfs strictly checks for OCI
// media types, so this conversion is required when pulling images produced by
// `docker build`.
func ConvertToOCI(m *ispec.Manifest) {
	switch m.Config.MediaType {
	case ConfigType:
		m.Config.MediaType = ispec.MediaTypeImageConfig
	}

	for i := range m.Layers {
		switch m.Layers[i].MediaType {
		case LayerGzipType:
			m.Layers[i].MediaType = ispec.MediaTypeImageLayerGzip
		case LayerType:
			m.Layers[i].MediaType = ispec.MediaTypeImageLayer
		case ForeignLayerType:
			m.Layers[i].MediaType = ispec.MediaTypeImageLayerNonDistributableGzip //nolint:staticcheck // matching deprecated OCI type
		}
	}

	switch m.MediaType {
	case ManifestType:
		m.MediaType = ispec.MediaTypeImageManifest
	}
}
