// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package manifest parses OCI v1 / Docker v2 schema-2 image manifests
// just enough to extract the layer and config digests they reference.
//
// This is a deliberately narrow parser. We do NOT validate the full
// OCI schema, do NOT cross-check media types, and do NOT verify
// signatures. The bytes have already been digest-verified by the
// cache pipeline before they reach this code, and containerd is the
// authoritative consumer that performs full validation. The only
// consumer here is the mirror's speculative layer-prefetch path
// (the design doc detailed-design.md L332 / architecture.md L180).
package manifest

import (
	"encoding/json"
	"fmt"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

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
