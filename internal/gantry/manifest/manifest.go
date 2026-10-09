// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package manifest parses OCI v1 / Docker v2 schema-2 image manifests
// just enough to classify them and extract the layer and config digests they
// reference.
//
// This is a deliberately narrow parser. We do NOT validate the full
// OCI schema, do NOT cross-check media types, and do NOT verify
// signatures. The bytes have already been digest-verified by the
// cache pipeline before they reach this code, and containerd is the
// authoritative consumer that performs full validation.
package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

const (
	ociImageIndexMediaType      = "application/vnd.oci.image.index.v1+json"
	ociImageManifestMediaType   = "application/vnd.oci.image.manifest.v1+json"
	dockerManifestListMediaType = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerManifestMediaType     = "application/vnd.docker.distribution.manifest.v2+json"
)

// Keep detection within the repository's 4 MiB manifest parsing bound so a
// blob requested through a /manifests/ URL cannot force an unbounded scan.
const maxContentTypeDetectionBytes int64 = 4 * 1024 * 1024

// schema is the subset of the OCI / Docker schema-2 manifest layout
// the prefetch path needs.
type schema struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
	// Manifests is populated for image indexes (multi-arch manifest
	// lists). When non-empty, the body is an index and we skip
	// prefetch: containerd will request the architecture-specific
	// manifest next.
	Manifests []descriptor `json:"manifests"`
}

type descriptor struct {
	MediaType string   `json:"mediaType"`
	Digest    string   `json:"digest"`
	Size      int64    `json:"size"`
	URLs      []string `json:"urls"`
}

// DetectContentType returns the OCI/Docker Content-Type described by a
// manifest body or body prefix. It returns an empty string when the bytes do
// not look like a schema-2 manifest envelope.
func DetectContentType(prefix []byte) string {
	return DetectContentTypeFromReader(bytes.NewReader(prefix))
}

// DetectContentTypeFromReader classifies the top-level fields of a manifest
// JSON stream. It does not match nested extension fields or annotation values,
// and it stops as soon as the top-level mediaType or object shape is known.
func DetectContentTypeFromReader(body io.Reader) string {
	decoder := json.NewDecoder(io.LimitReader(body, maxContentTypeDetectionBytes))

	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ""
	}

	schemaVersionTwo := false
	hasManifestFields := false
	hasManifests := false

	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			break
		}

		field, ok := token.(string)
		if !ok {
			break
		}

		switch field {
		case "mediaType":
			var mediaType string
			if err := decoder.Decode(&mediaType); err != nil {
				return contentTypeForShape(hasManifestFields, hasManifests, schemaVersionTwo)
			}

			if contentType := knownContentType(mediaType); contentType != "" {
				return contentType
			}
		case "manifests":
			hasManifests = true
			if err := discardJSONValue(decoder); err != nil {
				return contentTypeForShape(hasManifestFields, hasManifests, schemaVersionTwo)
			}
		case "config", "layers":
			hasManifestFields = true
			if err := discardJSONValue(decoder); err != nil {
				return contentTypeForShape(hasManifestFields, hasManifests, schemaVersionTwo)
			}
		case "schemaVersion":
			var schemaVersion int
			if err := decoder.Decode(&schemaVersion); err != nil {
				return ""
			}

			schemaVersionTwo = schemaVersion == 2
		default:
			if err := discardJSONValue(decoder); err != nil {
				return contentTypeForShape(hasManifestFields, hasManifests, schemaVersionTwo)
			}
		}
	}

	return contentTypeForShape(hasManifestFields, hasManifests, schemaVersionTwo)
}

func knownContentType(mediaType string) string {
	switch mediaType {
	case ociImageIndexMediaType,
		ociImageManifestMediaType,
		dockerManifestListMediaType,
		dockerManifestMediaType:
		return mediaType
	default:
		return ""
	}
}

func contentTypeForShape(hasManifestFields, hasManifests, schemaVersionTwo bool) string {
	if !schemaVersionTwo {
		return ""
	}

	switch {
	case hasManifestFields:
		return ociImageManifestMediaType
	case hasManifests:
		return ociImageIndexMediaType
	default:
		return ociImageManifestMediaType
	}
}

func discardJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}

	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delim {
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}

			if err := discardJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := discardJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("manifest: unexpected JSON delimiter %q", delim)
	}

	_, err = decoder.Token()

	return err
}

// ChildDigests parses body as an OCI / Docker schema-2 image manifest
// and returns the digests of every content blob the manifest
// references - its config blob plus every layer descriptor. The
// returned slice preserves source order: config first, then layers
// top-to-bottom (which is also the order containerd will request
// them).
//
// When body is an image index (manifest list) the function returns
// (nil, nil): no prefetch can happen until containerd requests the
// architecture-specific manifest.
//
// Foreign-layer descriptors (those with a non-empty `urls` array) are
// skipped: they point at non-OCI hosts (Microsoft base layers) and
// MUST NOT be fetched through Gantry.
//
// The function does not error on individual malformed digest strings
// inside the manifest; those entries are silently skipped. A parse
// failure on the manifest envelope itself is returned as an error.
//
// Prefer TypedChildren over ChildDigests for new callers that need
// the per-digest kind (image-config blob vs layer blob) - e.g. so
// the per-kind metric label survives end-to-end through the prefetch
// fan-out into please_pull and StartLocalPull batches.
func ChildDigests(body []byte) ([]digest.Digest, error) {
	typed, err := TypedChildren(body)
	if err != nil {
		return nil, err
	}

	if typed == nil {
		return nil, nil
	}

	out := make([]digest.Digest, 0, len(typed))
	for _, c := range typed {
		out = append(out, c.Digest)
	}

	return out, nil
}

// TypedChild pairs a child digest with the OCI URL-family kind the
// puller MUST target. Kind is one of ifaces.KindConfig (the manifest's
// image-config blob, served from /v2/<repo>/blobs/<digest> per the
// OCI Distribution Spec) or ifaces.KindBlob (every layer descriptor,
// also served from /v2/<repo>/blobs/<digest>). The two kinds are
// bytes-equivalent at the registry level but carried separately so
// observability counters can keep
//
//	p2p_origin_pull_total{kind="manifest|config|layer"}
//
// honest end-to-end through the prefetch fan-out and the please_pull
// wire boundary. Without the split every child digest of a manifest
// would label as "blob" and the "config" bucket would always be
// empty in practice - the design intent the observability
// recommendation calls out.
type TypedChild struct {
	Digest digest.Digest
	Kind   ifaces.OriginRefKind
}

// TypedChildren returns config first, then layers top-to-bottom.
// Foreign-layer descriptors (non-empty `urls`) are skipped; image indexes
// (.manifests with no .layers) return no children. Invalid child digests are
// skipped, but malformed JSON returns an error. The config digest is
// tagged KindConfig; every layer is tagged KindBlob (KindLayer is
// intentionally NOT introduced - the OCI URL family is /blobs/ for
// both and downstream pullers do not need to distinguish, only the
// metric label needs to).
func TypedChildren(body []byte) ([]TypedChild, error) {
	var m schema
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("manifest: parse: %w", err)
	}
	// Image index detection: index has .manifests,
	// image manifest has .layers. If both happen to be populated,
	// prefer image-manifest interpretation (defensive against weird
	// hand-crafted bodies).
	if len(m.Manifests) > 0 && len(m.Layers) == 0 {
		return nil, nil
	}

	out := make([]TypedChild, 0, 1+len(m.Layers))
	if m.Config.Digest != "" {
		if d, err := digest.Parse(m.Config.Digest); err == nil {
			out = append(out, TypedChild{Digest: d, Kind: ifaces.KindConfig})
		}
	}

	for _, l := range m.Layers {
		if l.Digest == "" {
			continue
		}

		if len(l.URLs) > 0 {
			// Foreign layer (Windows base, Microsoft-hosted) - skip.
			continue
		}

		if d, err := digest.Parse(l.Digest); err == nil {
			out = append(out, TypedChild{Digest: d, Kind: ifaces.KindBlob})
		}
	}

	return out, nil
}

// IsImageIndex reports whether body parses as an OCI / Docker schema-2
// image index (manifest list). Provided for callers that want to
// short-circuit before walking child digests.
func IsImageIndex(body []byte) bool {
	var m schema
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}

	return len(m.Manifests) > 0 && len(m.Layers) == 0
}
