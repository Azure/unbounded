// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestImageDeterministicMultiArchGraph(t *testing.T) {
	binaries := map[string][]byte{"amd64": []byte("amd64 executable"), "arm64": []byte("arm64 executable")}

	image, err := NewImage(binaries, "pause")
	if err != nil {
		t.Fatal(err)
	}

	other, err := NewImage(map[string][]byte{"arm64": binaries["arm64"], "amd64": binaries["amd64"]}, "pause")
	if err != nil {
		t.Fatal(err)
	}

	if image.Descriptor.Digest != other.Descriptor.Digest {
		t.Fatal("map order changed the fixture digest")
	}

	var index ocispec.Index
	if err := json.Unmarshal(image.Blobs[image.Descriptor.Digest], &index); err != nil {
		t.Fatal(err)
	}

	if image.Descriptor.MediaType != ocispec.MediaTypeImageIndex || index.MediaType != ocispec.MediaTypeImageIndex || len(index.Manifests) != 2 {
		t.Fatalf("not a two-platform OCI image index: %+v", index)
	}

	for i, descriptor := range index.Manifests {
		arch := []string{"amd64", "arm64"}[i]
		if descriptor.Platform == nil || descriptor.Platform.OS != "linux" || descriptor.Platform.Architecture != arch {
			t.Fatalf("incorrect platform descriptor: %+v", descriptor)
		}

		var manifest ocispec.Manifest
		if err := json.Unmarshal(image.Blobs[descriptor.Digest], &manifest); err != nil {
			t.Fatal(err)
		}

		if manifest.MediaType != ocispec.MediaTypeImageManifest || len(manifest.Layers) != 1 {
			t.Fatalf("invalid fixture manifest: %+v", manifest)
		}

		var config ocispec.Image
		if err := json.Unmarshal(image.Blobs[manifest.Config.Digest], &config); err != nil {
			t.Fatal(err)
		}

		if config.OS != "linux" || config.Architecture != arch || len(config.Config.Entrypoint) != 1 || config.Config.Entrypoint[0] != "/pause" {
			t.Fatalf("invalid executable config: %+v", config)
		}

		layer := manifest.Layers[0]
		if layer.MediaType != ocispec.MediaTypeImageLayerGzip {
			t.Fatalf("layer media type = %q", layer.MediaType)
		}

		gz, err := gzip.NewReader(bytes.NewReader(image.Blobs[layer.Digest]))
		if err != nil {
			t.Fatal(err)
		}

		raw, err := io.ReadAll(gz)
		if err != nil {
			t.Fatal(err)
		}

		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}

		if config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != 1 || config.RootFS.DiffIDs[0] != godigest.FromBytes(raw) {
			t.Fatalf("incorrect uncompressed layer digest: %+v", config.RootFS)
		}

		tr := tar.NewReader(bytes.NewReader(raw))

		header, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}

		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}

		if header.Name != "pause" || header.Mode != 0o755 || header.Uid != 0 || header.Gid != 0 || header.ModTime.Unix() != 0 || !bytes.Equal(body, binaries[arch]) {
			t.Fatalf("incorrect executable layer: %+v, body=%q", header, body)
		}

		if _, err := tr.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("scratch layer has unexpected extra entries: %v", err)
		}
	}

	for digest, body := range image.Blobs {
		if godigest.FromBytes(body) != digest {
			t.Fatalf("blob digest mismatch: %s", digest)
		}
	}

	archive, err := image.Archive("localhost/pause:e2e")
	if err != nil {
		t.Fatal(err)
	}

	second, err := other.Archive("localhost/pause:e2e")
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(archive, second) {
		t.Fatal("fixture archive is not deterministic")
	}

	entries := readArchive(t, archive)
	if string(entries["oci-layout"]) != `{"imageLayoutVersion":"1.0.0"}` {
		t.Fatal("missing OCI layout version")
	}

	var layout ocispec.Index
	if err := json.Unmarshal(entries["index.json"], &layout); err != nil {
		t.Fatal(err)
	}

	if len(layout.Manifests) != 1 || layout.Manifests[0].Digest != image.Descriptor.Digest || layout.Manifests[0].Annotations[ocispec.AnnotationRefName] != "localhost/pause:e2e" {
		t.Fatalf("incorrect named index: %+v", layout)
	}

	if len(entries) != len(image.Blobs)+2 {
		t.Fatalf("archive has %d files, want %d", len(entries), len(image.Blobs)+2)
	}

	for digest, body := range image.Blobs {
		if !bytes.Equal(entries["blobs/sha256/"+digest.Encoded()], body) {
			t.Fatalf("archive missing blob %s", digest)
		}
	}

	if len(image.Descriptor.Annotations) != 0 {
		t.Fatal("archive name mutated the image descriptor")
	}
}

func TestImageRejectsInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		binaries   map[string][]byte
		executable string
	}{
		{name: "no platforms", executable: "pause"},
		{name: "empty binary", binaries: map[string][]byte{"amd64": nil}, executable: "pause"},
		{name: "unsupported platform", binaries: map[string][]byte{"s390x": []byte("binary")}, executable: "pause"},
		{name: "empty path", binaries: map[string][]byte{"amd64": []byte("binary")}},
		{name: "traversal", binaries: map[string][]byte{"amd64": []byte("binary")}, executable: "../pause"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewImage(tc.binaries, tc.executable); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}

	image, err := NewImage(map[string][]byte{"amd64": []byte("binary")}, "pause")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := image.Archive(""); err == nil {
		t.Fatal("empty archive reference accepted")
	}

	registry, err := NewImage(map[string][]byte{"amd64": []byte("binary")}, "registry")
	if err != nil {
		t.Fatal(err)
	}

	for digest := range image.Blobs {
		if _, overlaps := registry.Blobs[digest]; overlaps {
			t.Fatalf("side-loaded registry would warm workload content %s", digest)
		}
	}
}

func readArchive(t *testing.T, body []byte) map[string][]byte {
	t.Helper()

	entries := make(map[string][]byte)

	tr := tar.NewReader(bytes.NewReader(body))
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatal(err)
		}

		if _, duplicate := entries[header.Name]; duplicate {
			t.Fatalf("duplicate archive entry %q", header.Name)
		}

		entry, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}

		entries[header.Name] = entry
	}

	return entries
}
