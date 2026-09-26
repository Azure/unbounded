// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type imageOptions struct {
	Layers     int
	LayerBytes int64
	Jitter     float64
	Seed       string
	Repository string
}

type syntheticImage struct {
	Manifest ocispec.Descriptor
	Config   ocispec.Descriptor
	Layers   []ocispec.Descriptor

	repository string
	manifest   []byte
	blobs      map[digest.Digest]imageBlob
}

type imageBlob struct {
	descriptor ocispec.Descriptor
	data       io.ReaderAt
}

var repositoryPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)

func newImage(ctx context.Context, opts imageOptions) (*syntheticImage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if opts.Layers <= 0 || opts.LayerBytes <= 0 {
		return nil, errors.New("layers and layer payload bytes must be positive")
	}

	if math.IsNaN(opts.Jitter) || opts.Jitter < 0 || opts.Jitter >= 1 {
		return nil, errors.New("jitter must be finite and in [0, 1)")
	}

	if len(opts.Repository) > 255 || !repositoryPattern.MatchString(opts.Repository) {
		return nil, errors.New("repository must be a valid lowercase registry repository name")
	}

	// Reserve room for a PAX header, tar padding, and the two end blocks.
	const maxPayload = int64(math.MaxInt64 - 4096)
	if opts.LayerBytes > maxPayload {
		return nil, errors.New("layer payload is too large")
	}

	delta := int64(math.Floor(float64(opts.LayerBytes) * opts.Jitter))
	if delta >= opts.LayerBytes {
		delta = opts.LayerBytes - 1
	}

	if delta > maxPayload-opts.LayerBytes {
		return nil, errors.New("jittered layer payload is too large")
	}

	img := &syntheticImage{repository: opts.Repository, blobs: make(map[digest.Digest]imageBlob)}
	diffIDs := make([]digest.Digest, 0)
	buffer := make([]byte, 128*1024)

	for index := range opts.Layers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		key := sha256.Sum256(fmt.Appendf(nil, "racer-loadgen/layer/%d/%s", index, opts.Seed))
		sizeHash := sha256.Sum256(append([]byte("racer-loadgen/size/"), key[:]...))
		payloadBytes := opts.LayerBytes - delta + int64(binary.BigEndian.Uint64(sizeHash[:8])%(2*uint64(delta)+1))

		layer, err := newVirtualLayer(index, payloadBytes, key)
		if err != nil {
			return nil, fmt.Errorf("create layer %d: %w", index, err)
		}

		digester := digest.Canonical.Digester()

		for offset := int64(0); offset < layer.size; {
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			n, err := layer.ReadAt(buffer, offset)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("hash layer %d: %w", index, err)
			}

			if _, err := digester.Hash().Write(buffer[:n]); err != nil {
				return nil, fmt.Errorf("hash layer %d: %w", index, err)
			}

			offset += int64(n)
		}

		desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: digester.Digest(), Size: layer.size}
		img.Layers = append(img.Layers, desc)
		img.blobs[desc.Digest] = imageBlob{descriptor: desc, data: layer}
		diffIDs = append(diffIDs, desc.Digest)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Fixed platform and absent timestamps make the image independent of its host.
	config, err := json.Marshal(ocispec.Image{
		Platform: ocispec.Platform{Architecture: "amd64", OS: "linux"},
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: diffIDs},
	})
	if err != nil {
		return nil, fmt.Errorf("encode image config: %w", err)
	}

	img.Config = byteDescriptor(ocispec.MediaTypeImageConfig, config)
	img.blobs[img.Config.Digest] = imageBlob{descriptor: img.Config, data: bytes.NewReader(config)}

	img.manifest, err = json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    img.Config,
		Layers:    img.Layers,
	})
	if err != nil {
		return nil, fmt.Errorf("encode image manifest: %w", err)
	}

	img.Manifest = byteDescriptor(ocispec.MediaTypeImageManifest, img.manifest)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return img, nil
}

func byteDescriptor(mediaType string, data []byte) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
}

// virtualLayer stores only tar metadata and an AES key schedule, regardless of
// payload size. ReadAt is stateless so simultaneous registry requests can share it.
type virtualLayer struct {
	header       []byte
	payloadBytes int64
	size         int64
	block        cipher.Block
}

func newVirtualLayer(index int, payloadBytes int64, key [32]byte) (*virtualLayer, error) {
	var header bytes.Buffer

	w := tar.NewWriter(&header)
	if err := w.WriteHeader(&tar.Header{
		Name: fmt.Sprintf("layer-%06d.bin", index), Mode: 0o644, Size: payloadBytes,
		Typeflag: tar.TypeReg, Format: tar.FormatPAX,
	}); err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}

	padding := (512 - payloadBytes%512) % 512

	return &virtualLayer{
		header: header.Bytes(), payloadBytes: payloadBytes,
		size: int64(header.Len()) + payloadBytes + padding + 1024, block: block,
	}, nil
}

func (layer *virtualLayer) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative layer offset")
	}

	if len(p) == 0 {
		return 0, nil
	}

	if offset >= layer.size {
		return 0, io.EOF
	}

	n := int(min(int64(len(p)), layer.size-offset))
	dst := p[:n]
	clear(dst)

	headerEnd := int64(len(layer.header))
	if offset < headerEnd {
		copied := copy(dst, layer.header[offset:])
		dst = dst[copied:]
		offset += int64(copied)
	}

	if len(dst) > 0 && offset < headerEnd+layer.payloadBytes {
		payloadOffset := offset - headerEnd
		payloadLen := min(int64(len(dst)), layer.payloadBytes-payloadOffset)

		var counter [aes.BlockSize]byte
		binary.BigEndian.PutUint64(counter[8:], uint64(payloadOffset/aes.BlockSize))
		stream := cipher.NewCTR(layer.block, counter[:])

		var skip [aes.BlockSize]byte
		stream.XORKeyStream(skip[:payloadOffset%aes.BlockSize], skip[:payloadOffset%aes.BlockSize])
		stream.XORKeyStream(dst[:payloadLen], dst[:payloadLen])
	}

	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}
