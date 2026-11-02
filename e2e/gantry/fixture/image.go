// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package fixture builds local OCI images for Gantry integration tests.
package fixture

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Image is a digest-addressed, multi-platform image graph. Its archive and
// digests depend only on the supplied binaries, not map order or wall-clock time.
type Image struct {
	Descriptor ocispec.Descriptor
	Blobs      map[godigest.Digest][]byte
}

// NewImage packages one static Linux executable per architecture in a scratch
// image. The executable is the entrypoint; no public base image is involved.
func NewImage(binaries map[string][]byte, executable string) (*Image, error) {
	if len(binaries) == 0 {
		return nil, fmt.Errorf("fixture image requires at least one architecture")
	}

	if executable == "" || strings.ContainsAny(executable, "/\\") || executable == "." || executable == ".." {
		return nil, fmt.Errorf("invalid fixture executable name %q", executable)
	}

	image := &Image{Blobs: make(map[godigest.Digest][]byte)}
	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
	}

	architectures := make([]string, 0, len(binaries))
	for arch := range binaries {
		architectures = append(architectures, arch)
	}

	sort.Strings(architectures)

	for _, arch := range architectures {
		if arch != "amd64" && arch != "arm64" {
			return nil, fmt.Errorf("unsupported fixture architecture %q", arch)
		}

		if len(binaries[arch]) == 0 {
			return nil, fmt.Errorf("empty fixture executable for %s", arch)
		}

		layer, diffID, err := executableLayer(executable, binaries[arch])
		if err != nil {
			return nil, err
		}

		layerDescriptor := image.put(ocispec.MediaTypeImageLayerGzip, layer)
		platform := ocispec.Platform{OS: "linux", Architecture: arch}
		created := time.Unix(0, 0).UTC()
		config := ocispec.Image{
			Created:  &created,
			Platform: platform,
			Config:   ocispec.ImageConfig{Entrypoint: []string{"/" + executable}},
			RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []godigest.Digest{diffID}},
		}

		configDescriptor, err := image.putJSON(ocispec.MediaTypeImageConfig, config)
		if err != nil {
			return nil, err
		}

		manifest := ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2},
			MediaType: ocispec.MediaTypeImageManifest,
			Config:    configDescriptor,
			Layers:    []ocispec.Descriptor{layerDescriptor},
		}

		descriptor, err := image.putJSON(ocispec.MediaTypeImageManifest, manifest)
		if err != nil {
			return nil, err
		}

		descriptor.Platform = &platform
		index.Manifests = append(index.Manifests, descriptor)
	}

	descriptor, err := image.putJSON(ocispec.MediaTypeImageIndex, index)
	if err != nil {
		return nil, err
	}

	image.Descriptor = descriptor

	return image, nil
}

func executableLayer(name string, binary []byte) ([]byte, godigest.Digest, error) {
	var raw bytes.Buffer

	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o755, Size: int64(len(binary)),
		Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0),
	}); err != nil {
		return nil, "", err
	}

	if _, err := tw.Write(binary); err != nil {
		return nil, "", err
	}

	if err := tw.Close(); err != nil {
		return nil, "", err
	}

	var compressed bytes.Buffer

	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		return nil, "", err
	}

	if err := gz.Close(); err != nil {
		return nil, "", err
	}

	return compressed.Bytes(), godigest.FromBytes(raw.Bytes()), nil
}

func (image *Image) put(mediaType string, body []byte) ocispec.Descriptor {
	digest := godigest.FromBytes(body)
	image.Blobs[digest] = body

	return ocispec.Descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(body))}
}

func (image *Image) putJSON(mediaType string, value any) (ocispec.Descriptor, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	return image.put(mediaType, body), nil
}

// Digests returns the entire graph in stable order, including the image index.
func (image *Image) Digests() []godigest.Digest {
	digests := make([]godigest.Digest, 0, len(image.Blobs))
	for digest := range image.Blobs {
		digests = append(digests, digest)
	}

	sort.Slice(digests, func(i, j int) bool { return digests[i] < digests[j] })

	return digests
}

// Archive returns an OCI image-layout tar suitable for ctr images import or
// kind load image-archive. Only the outer layout carries the import name.
func (image *Image) Archive(ref string) ([]byte, error) {
	if ref == "" {
		return nil, fmt.Errorf("fixture archive requires an image reference")
	}

	descriptor := image.Descriptor
	descriptor.Annotations = map[string]string{ocispec.AnnotationRefName: ref}

	index, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{descriptor},
	})
	if err != nil {
		return nil, err
	}

	var archive bytes.Buffer

	tw := tar.NewWriter(&archive)

	entries := []struct {
		name string
		body []byte
	}{
		{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)},
		{"index.json", index},
	}
	for _, digest := range image.Digests() {
		entries = append(entries, struct {
			name string
			body []byte
		}{"blobs/sha256/" + digest.Encoded(), image.Blobs[digest]})
	}

	for _, entry := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: entry.name, Mode: 0o644, Size: int64(len(entry.body)),
			Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0),
		}); err != nil {
			return nil, err
		}

		if _, err := tw.Write(entry.body); err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}

	return archive.Bytes(), nil
}
