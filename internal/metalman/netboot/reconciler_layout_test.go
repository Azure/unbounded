// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
)

// pushTestImage stores a single-layer image holding disk/disk.img in an OCI
// layout store, tags it "v1", and returns the store and the manifest digest.
// Like a registry, the store resolves both tags and digests.
func pushTestImage(t *testing.T, payload []byte) (*oci.Store, string) {
	t.Helper()

	ctx := context.Background()

	store, err := oci.New(t.TempDir())
	if err != nil {
		t.Fatalf("create source store: %v", err)
	}

	var layerBuf bytes.Buffer

	tw := tar.NewWriter(&layerBuf)
	for _, h := range []*tar.Header{
		{Name: "disk/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "disk/disk.img", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(payload))},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("write tar header: %v", err)
		}

		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(payload); err != nil {
				t.Fatalf("write tar body: %v", err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	layer := layerBuf.Bytes()
	layerDesc := content.NewDescriptorFromBytes(ispec.MediaTypeImageLayer, layer)

	config, err := json.Marshal(ispec.Image{
		Platform: ispec.Platform{OS: "linux", Architecture: "arm64"},
		RootFS:   ispec.RootFS{Type: "layers", DiffIDs: []godigest.Digest{layerDesc.Digest}},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	configDesc := content.NewDescriptorFromBytes(ispec.MediaTypeImageConfig, config)

	for _, blob := range []struct {
		desc ispec.Descriptor
		data []byte
	}{{layerDesc, layer}, {configDesc, config}} {
		if err := store.Push(ctx, blob.desc, bytes.NewReader(blob.data)); err != nil {
			t.Fatalf("push blob: %v", err)
		}
	}

	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, "", oras.PackManifestOptions{
		ConfigDescriptor: &configDesc,
		Layers:           []ispec.Descriptor{layerDesc},
	})
	if err != nil {
		t.Fatalf("pack manifest: %v", err)
	}

	if err := store.Tag(ctx, manifestDesc, "v1"); err != nil {
		t.Fatalf("tag manifest: %v", err)
	}

	return store, manifestDesc.Digest.String()
}

func TestCopyToLayoutAndUnpack(t *testing.T) {
	payload := []byte("disk image bytes")
	store, digest := pushTestImage(t, payload)

	tests := []struct {
		name string
		ref  string
	}{
		{name: "digest reference", ref: digest},
		{name: "tag reference", ref: "v1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			layoutDir := t.TempDir()
			destDir := filepath.Join(t.TempDir(), "image")

			if err := copyToLayout(ctx, store, tt.ref, layoutDir); err != nil {
				t.Fatalf("copyToLayout: %v", err)
			}

			engine, err := umoci.OpenLayout(layoutDir)
			if err != nil {
				t.Fatalf("open layout: %v", err)
			}

			paths, err := engine.ResolveReference(ctx, layoutTag)
			engine.Close() //nolint:errcheck,gosec // test cleanup

			if err != nil || len(paths) != 1 {
				t.Fatalf("resolve %q = %d paths, %v; want 1 path", layoutTag, len(paths), err)
			}

			// Unpacking chowns the root directory, which needs root.
			if os.Geteuid() != 0 {
				return
			}

			if err := unpackOCILayout(ctx, layoutDir, layoutTag, destDir, "arm64"); err != nil {
				t.Fatalf("unpackOCILayout: %v", err)
			}

			got, err := os.ReadFile(filepath.Join(destDir, "disk", "disk.img"))
			if err != nil {
				t.Fatalf("read unpacked disk image: %v", err)
			}

			if !bytes.Equal(got, payload) {
				t.Fatalf("disk image = %q, want %q", got, payload)
			}
		})
	}
}

func TestCopyToLayoutMissingReference(t *testing.T) {
	store, _ := pushTestImage(t, []byte("x"))

	err := copyToLayout(context.Background(), store, "sha256:"+string(bytes.Repeat([]byte("0"), 64)), t.TempDir())
	if err == nil {
		t.Fatal("copyToLayout succeeded for a missing reference")
	}
}

func TestUnpackOCILayoutMissingTag(t *testing.T) {
	store, digest := pushTestImage(t, []byte("x"))
	layoutDir := t.TempDir()

	if err := copyToLayout(context.Background(), store, digest, layoutDir); err != nil {
		t.Fatalf("copyToLayout: %v", err)
	}

	err := unpackOCILayout(context.Background(), layoutDir, digest, t.TempDir(), "arm64")
	if err == nil {
		t.Fatal("unpackOCILayout found an image by digest name, want not found")
	}
}
