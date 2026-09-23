// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
)

const imageRepository = "loadgen/images"

type imageCatalog struct {
	Version    int              `json:"version"`
	Repository string           `json:"repository"`
	Images     []oci.Descriptor `json:"images"`
}

type imageObject struct {
	descriptor oci.Descriptor
	source     io.ReaderAt
}

type imageRegistry struct {
	catalog imageCatalog
	objects map[string]imageObject
	ready   atomic.Bool
	metrics *imageMetrics
}

func newImageRegistry(m *imageMetrics) *imageRegistry {
	return &imageRegistry{catalog: imageCatalog{Version: 1, Repository: imageRepository}, objects: make(map[string]imageObject), metrics: m}
}

// prepare publishes an immutable catalog only after hashing every layer. Bodies
// are random-access synthetic bytes, not materialized files or unpackable tar.
func (s *imageRegistry) prepare(ctx context.Context, c config) error {
	count := c.footprint / c.objectSize / int64(c.layersPerImage)
	for image := int64(0); image < count; image++ {
		manifest := oci.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: oci.MediaTypeImageManifest}
		config := oci.Image{Platform: oci.Platform{Architecture: "amd64", OS: "linux"}, RootFS: oci.RootFS{Type: "layers"}}

		for layer := 0; layer < c.layersPerImage; layer++ {
			identity := fmt.Sprintf("loadgen/oci/v1/%d/%d/%d", c.footprint, c.objectSize, image*int64(c.layersPerImage)+int64(layer))
			key := sha256.Sum256([]byte(identity))
			source := syntheticSource{size: c.objectSize, key: binary.LittleEndian.Uint64(key[:])}

			sum, err := checksum(ctx, source, source.size)
			if err != nil {
				return err
			}

			desc := oci.Descriptor{MediaType: oci.MediaTypeImageLayer, Digest: digest.Digest(fmt.Sprintf("sha256:%x", sum)), Size: source.size}
			s.objects["blobs/"+desc.Digest.String()] = imageObject{descriptor: desc, source: source}
			manifest.Layers = append(manifest.Layers, desc)
			config.RootFS.DiffIDs = append(config.RootFS.DiffIDs, desc.Digest)
		}

		manifest.Config = s.addJSON("blobs", oci.MediaTypeImageConfig, config)
		s.catalog.Images = append(s.catalog.Images, s.addJSON("manifests", oci.MediaTypeImageManifest, manifest))

		if image%100 == 0 || image+1 == count {
			slog.Info("registry preparation", "images_ready", image+1, "images_total", count)
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	s.ready.Store(true)

	return nil
}

func (s *imageRegistry) addJSON(kind, mediaType string, value any) oci.Descriptor {
	// All callers pass OCI structs containing only JSON-supported fields.
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}

	desc := oci.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	s.objects[kind+"/"+desc.Digest.String()] = imageObject{descriptor: desc, source: bytes.NewReader(data)}

	return desc
}

func (s *imageRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	if !s.ready.Load() {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "preparing dataset", http.StatusServiceUnavailable)

		return
	}

	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")

	switch r.URL.Path {
	case "/v2/":
		w.Header().Set("Content-Type", "application/json")
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader("{}"))

		return
	case "/loadgen/catalog":
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(s.catalog) //nolint:errcheck // A disconnected client needs no further response.
		}

		return
	}

	key, ok := strings.CutPrefix(r.URL.Path, "/v2/"+imageRepository+"/")

	object, found := s.objects[key]
	if !ok || !found {
		http.NotFound(w, r)
		return
	}

	kind := "blob"
	if strings.HasPrefix(key, "manifests/") {
		kind = "manifest"
	}

	request := "full"
	if r.Method == http.MethodHead {
		request = "head"
	} else if r.Header.Get("Range") != "" {
		request = "range"
	}

	s.metrics.registryRequests.WithLabelValues(kind, request).Inc()
	w.Header().Set("Content-Type", object.descriptor.MediaType)
	w.Header().Set("Docker-Content-Digest", object.descriptor.Digest.String())
	w.Header().Set("ETag", `"`+object.descriptor.Digest.String()+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	counter := &registryWriter{ResponseWriter: w}
	http.ServeContent(counter, r, "", time.Time{}, io.NewSectionReader(object.source, 0, object.descriptor.Size))
	s.metrics.registryBytes.WithLabelValues(kind, request).Add(float64(counter.bytes))
}

type registryWriter struct {
	http.ResponseWriter
	bytes int64
}

func (w *registryWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)

	return n, err
}
