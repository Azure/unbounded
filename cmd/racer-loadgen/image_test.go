// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func testImageOptions() imageOptions {
	return imageOptions{Layers: 4, LayerBytes: 4099, Jitter: 0.25, Seed: "test-seed", Repository: "bench/synthetic"}
}

func readImageBlob(t *testing.T, img *syntheticImage, desc ocispec.Descriptor) []byte {
	t.Helper()

	data, err := io.ReadAll(io.NewSectionReader(img.blobs[desc.Digest].data, 0, desc.Size))
	require.NoError(t, err)
	require.Equal(t, desc.Size, int64(len(data)))
	require.Equal(t, desc.Digest, digest.FromBytes(data))

	return data
}

func TestImageDeterministicAndValid(t *testing.T) {
	opts := testImageOptions()
	img, err := newImage(t.Context(), opts)
	require.NoError(t, err)
	other, err := newImage(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, img.Manifest, other.Manifest)
	require.Equal(t, img.Config, other.Config)
	require.Equal(t, img.Layers, other.Layers)
	require.Equal(t, img.Manifest.Digest, digest.FromBytes(img.manifest))
	require.Equal(t, img.Manifest.Size, int64(len(img.manifest)))
	require.Equal(t, ocispec.MediaTypeImageManifest, img.Manifest.MediaType)

	var manifest ocispec.Manifest
	require.NoError(t, json.Unmarshal(img.manifest, &manifest))
	require.Equal(t, 2, manifest.SchemaVersion)
	require.Equal(t, ocispec.MediaTypeImageManifest, manifest.MediaType)
	require.Equal(t, img.Config, manifest.Config)
	require.Equal(t, img.Layers, manifest.Layers)
	require.Len(t, img.Layers, opts.Layers)
	require.Equal(t, ocispec.MediaTypeImageConfig, img.Config.MediaType)

	var config ocispec.Image
	require.NoError(t, json.Unmarshal(readImageBlob(t, img, img.Config), &config))
	require.Equal(t, "layers", config.RootFS.Type)
	require.Equal(t, "linux", config.OS)
	require.Equal(t, "amd64", config.Architecture)
	require.Len(t, config.RootFS.DiffIDs, opts.Layers)

	sizes := make(map[int64]bool)

	for index, desc := range img.Layers {
		require.Equal(t, ocispec.MediaTypeImageLayer, desc.MediaType)
		require.Equal(t, desc.Digest, config.RootFS.DiffIDs[index])
		data := readImageBlob(t, img, desc)
		require.Equal(t, data, readImageBlob(t, other, other.Layers[index]))
		require.Zero(t, len(data)%512)
		require.Equal(t, make([]byte, 1024), data[len(data)-1024:])
		tr := tar.NewReader(bytes.NewReader(data))
		header, err := tr.Next()
		require.NoError(t, err)
		require.Equal(t, byte(tar.TypeReg), header.Typeflag)
		require.GreaterOrEqual(t, float64(header.Size), float64(opts.LayerBytes)*(1-opts.Jitter))
		require.LessOrEqual(t, float64(header.Size), float64(opts.LayerBytes)*(1+opts.Jitter))
		sizes[header.Size] = true
		payload, err := io.ReadAll(tr)
		require.NoError(t, err)
		require.Equal(t, header.Size, int64(len(payload)))

		var compressed bytes.Buffer

		zw := gzip.NewWriter(&compressed)
		_, err = zw.Write(payload)
		require.NoError(t, err)
		require.NoError(t, zw.Close())
		require.Greater(t, compressed.Len(), len(payload)*95/100, "payload should resist compression")

		_, err = tr.Next()
		require.ErrorIs(t, err, io.EOF)
	}

	require.Greater(t, len(sizes), 1, "jitter should vary layer sizes")

	opts.Seed = "different-seed"
	changed, err := newImage(t.Context(), opts)
	require.NoError(t, err)
	require.NotEqual(t, img.Manifest.Digest, changed.Manifest.Digest)

	for index := range img.Layers {
		require.NotEqual(t, img.Layers[index].Digest, changed.Layers[index].Digest)
	}

	opts = testImageOptions()
	opts.Repository = "another/repository"
	other, err = newImage(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, img.Manifest, other.Manifest, "repository is not image content")
}

func TestImagePayloadSizes(t *testing.T) {
	for _, size := range []int64{1, 511, 512, 513} {
		opts := testImageOptions()
		opts.LayerBytes = size
		opts.Jitter = 0
		img, err := newImage(t.Context(), opts)
		require.NoError(t, err)

		for _, desc := range img.Layers {
			tr := tar.NewReader(bytes.NewReader(readImageBlob(t, img, desc)))
			header, err := tr.Next()
			require.NoError(t, err)
			require.Equal(t, size, header.Size)
		}

		require.NotEqual(t, img.Layers[0].Digest, img.Layers[1].Digest)
	}
}

func TestVirtualLayerRandomAccess(t *testing.T) {
	key := sha256.Sum256([]byte("random-access-test"))
	layer, err := newVirtualLayer(0, 2051, key)
	require.NoError(t, err)
	all, err := io.ReadAll(io.NewSectionReader(layer, 0, layer.size))
	require.NoError(t, err)

	// Independently generate the stream in one pass. Offsets deliberately cross
	// AES blocks, tar headers, payload padding, and the end of the archive.
	block, err := aes.NewCipher(key[:])
	require.NoError(t, err)

	payload := make([]byte, layer.payloadBytes)
	cipher.NewCTR(block, make([]byte, aes.BlockSize)).XORKeyStream(payload, payload)
	require.Equal(t, payload, all[len(layer.header):int64(len(layer.header))+layer.payloadBytes])

	reader := io.NewSectionReader(layer, 0, layer.size)
	for _, offset := range []int64{0, 1, 497, 511, 512, 513, 527, 528, 529, 1023, 2559, 2562, 2563, layer.size - 17} {
		for _, count := range []int{1, 15, 16, 17, 513} {
			position, err := reader.Seek(offset, io.SeekStart)
			require.NoError(t, err)
			require.Equal(t, offset, position)

			buffer := bytes.Repeat([]byte{0xff}, count)
			n, err := reader.Read(buffer)
			require.True(t, err == nil || errors.Is(err, io.EOF))
			require.Equal(t, all[offset:min(offset+int64(count), layer.size)], buffer[:n])
		}
	}

	_, err = reader.Seek(-17, io.SeekEnd)
	require.NoError(t, err)
	_, err = reader.Seek(3, io.SeekCurrent)
	require.NoError(t, err)
	end, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, all[len(all)-14:], end)

	_, err = reader.Seek(-1, io.SeekStart)
	require.Error(t, err)
	_, err = reader.Seek(0, 999)
	require.Error(t, err)
	_, err = layer.ReadAt(make([]byte, 1), -1)
	require.Error(t, err)
	n, err := layer.ReadAt(make([]byte, 16), layer.size-3)
	require.Equal(t, 3, n)
	require.ErrorIs(t, err, io.EOF)
	_, err = layer.ReadAt(make([]byte, 1), layer.size+1)
	require.ErrorIs(t, err, io.EOF)
	n, err = layer.ReadAt(nil, 0)
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestVirtualLayerLargePayloadMetadata(t *testing.T) {
	// A terabyte layer must support seeking without allocating its payload.
	const size = int64(1 << 40)

	layer, err := newVirtualLayer(0, size, sha256.Sum256([]byte("large")))
	require.NoError(t, err)
	require.Less(t, len(layer.header), 4096)
	tr := tar.NewReader(io.NewSectionReader(layer, 0, layer.size))
	header, err := tr.Next()
	require.NoError(t, err)
	require.Equal(t, size, header.Size)

	buffer := make([]byte, 32)
	n, err := layer.ReadAt(buffer, int64(len(layer.header))+size-16)
	require.NoError(t, err)
	require.Equal(t, 32, n)
	require.NotEqual(t, make([]byte, 16), buffer[:16])
	require.Equal(t, make([]byte, 16), buffer[16:])
}

func TestImageInvalidOptions(t *testing.T) {
	tests := map[string]func(*imageOptions){
		"zero layers":       func(o *imageOptions) { o.Layers = 0 },
		"negative layers":   func(o *imageOptions) { o.Layers = -1 },
		"zero bytes":        func(o *imageOptions) { o.LayerBytes = 0 },
		"negative bytes":    func(o *imageOptions) { o.LayerBytes = -1 },
		"negative jitter":   func(o *imageOptions) { o.Jitter = -0.1 },
		"unit jitter":       func(o *imageOptions) { o.Jitter = 1 },
		"nan jitter":        func(o *imageOptions) { o.Jitter = math.NaN() },
		"infinite jitter":   func(o *imageOptions) { o.Jitter = math.Inf(1) },
		"size overflow":     func(o *imageOptions) { o.LayerBytes = math.MaxInt64 },
		"jitter overflow":   func(o *imageOptions) { o.LayerBytes = math.MaxInt64 / 4 * 3; o.Jitter = 0.9 },
		"empty repository":  func(o *imageOptions) { o.Repository = "" },
		"uppercase repo":    func(o *imageOptions) { o.Repository = "Bad/repo" },
		"empty component":   func(o *imageOptions) { o.Repository = "bad//repo" },
		"leading slash":     func(o *imageOptions) { o.Repository = "/repo" },
		"trailing slash":    func(o *imageOptions) { o.Repository = "repo/" },
		"tag in repository": func(o *imageOptions) { o.Repository = "repo:latest" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			opts := testImageOptions()
			change(&opts)
			img, err := newImage(t.Context(), opts)
			require.Error(t, err)
			require.Nil(t, img)
		})
	}
}

type cancelDuringHashContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (ctx *cancelDuringHashContext) Err() error {
	ctx.checks++
	if ctx.checks == 4 {
		ctx.cancel()
	}

	return ctx.Context.Err()
}

func TestImageCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	img, err := newImage(ctx, testImageOptions())
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, img)

	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()

	opts := testImageOptions()
	opts.LayerBytes = 1 << 20
	midHash := &cancelDuringHashContext{Context: ctx, cancel: cancel}
	img, err = newImage(midHash, opts)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, img)
	require.Equal(t, 4, midHash.checks)
}
