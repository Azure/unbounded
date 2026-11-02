// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"bytes"
	"context"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/plugins/content/local"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestArchiveImportsIntoContainerd(t *testing.T) {
	image, err := NewImage(map[string][]byte{"amd64": []byte("amd64 binary"), "arm64": []byte("arm64 binary")}, "pause")
	if err != nil {
		t.Fatal(err)
	}

	const ref = "localhost/gantry-multiarch-pause:e2e"

	body, err := image.Archive(ref)
	if err != nil {
		t.Fatal(err)
	}

	store, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	index, err := archive.ImportIndex(t.Context(), store, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("containerd rejected fixture archive: %v", err)
	}

	children, err := images.Children(t.Context(), store, index)
	if err != nil {
		t.Fatal(err)
	}

	if len(children) != 1 || children[0].Digest != image.Descriptor.Digest || archive.FilterRefPrefix(ref)(children[0].Annotations[ocispec.AnnotationRefName]) != ref {
		t.Fatalf("incorrect containerd import name or index: %+v", children)
	}

	seen := make(map[string]bool)

	if err := images.Walk(t.Context(), images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		blob, err := content.ReadBlob(ctx, store, desc)
		if err != nil {
			return nil, err
		}

		if expected := image.Blobs[desc.Digest]; !bytes.Equal(blob, expected) {
			t.Errorf("containerd content differs for %s", desc.Digest)
		}

		seen[desc.Digest.String()] = true

		return images.Children(ctx, store, desc)
	}), image.Descriptor); err != nil {
		t.Fatalf("containerd cannot traverse fixture graph: %v", err)
	}

	if len(seen) != len(image.Blobs) {
		t.Fatalf("containerd traversed %d blobs, want %d", len(seen), len(image.Blobs))
	}
}
