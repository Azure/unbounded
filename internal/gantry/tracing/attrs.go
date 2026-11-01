// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"go.opentelemetry.io/otel/attribute"
)

// Attribute keys used across the Gantry pull path. Names follow OTel
// semantic-conventions naming (dotted, lowercase) with a gantry.* namespace
// for the domain-specific ones, so a collector can filter Gantry hops without
// matching unrelated instrumentation.
const (
	// AttrDigest is the content digest being served, e.g. sha256:abcd....
	AttrDigest = "gantry.digest"

	// AttrMediaType is the OCI media type of the blob or manifest.
	AttrMediaType = "gantry.media_type"

	// AttrBytes is the number of bytes moved by the span.
	AttrBytes = "gantry.bytes"

	// AttrCacheResult is "hit" or "miss" for the local content-store lookup.
	AttrCacheResult = "gantry.cache.result"

	// AttrSource is where the bytes came from: local, peer, or upstream.
	AttrSource = "gantry.source"

	// AttrPeerID identifies the peer that served or was attempted.
	AttrPeerID = "gantry.peer.id"

	// AttrRegistry is the upstream registry name the request resolved to.
	AttrRegistry = "gantry.registry"

	// AttrRepository is the OCI repository in the request path.
	AttrRepository = "gantry.repository"

	// AttrKind is "blob" or "manifest".
	AttrKind = "gantry.kind"

	// AttrStatusCode is the HTTP status Gantry returned or received.
	AttrStatusCode = "http.response.status_code"
	// AttrURLFull is the request URL recorded on a client span. It uses the
	// OpenTelemetry semantic-convention key so Gantry's spans line up with the
	// ones containerd and other OTel-instrumented components emit.
	//
	// Gantry always records this value redacted. The URL that goes on the wire
	// is a different URL; see the note on AttrURLRedacted.
	AttrURLFull = "url.full"
	// AttrURLRedacted reports whether any query value on AttrURLFull was
	// replaced with the REDACTED placeholder.
	//
	// It exists so a redaction bug is visible in the data rather than silently
	// plausible. A trace reader who knows Gantry redacts signed URLs cannot tell
	// a correctly redacted url.full from a matcher that stopped matching after a
	// provider renamed its parameters, and neither can an assertion. This flag
	// says whether the matcher fired for this request, so a regression that
	// starts exporting signatures shows up as url.redacted=false on a request
	// that also carries a signature-bearing parameter name.
	AttrURLRedacted = "gantry.url.redacted"
)

// Cache and source vocabulary. Exported so mirror, transfer, and origin all
// emit the same strings; a collector query that says source=peer has to mean
// the same thing on every hop.
const (
	CacheHit  = "hit"
	CacheMiss = "miss"

	SourceLocal    = "local"
	SourcePeer     = "peer"
	SourceUpstream = "upstream"
)

// Digest returns the digest attribute.
func Digest(value string) attribute.KeyValue {
	return attribute.String(AttrDigest, value)
}

// MediaType returns the media-type attribute.
func MediaType(value string) attribute.KeyValue {
	return attribute.String(AttrMediaType, value)
}

// Bytes returns the byte-count attribute.
func Bytes(n int64) attribute.KeyValue {
	return attribute.Int64(AttrBytes, n)
}

// Cache returns the hit/miss attribute.
func Cache(hit bool) attribute.KeyValue {
	if hit {
		return attribute.String(AttrCacheResult, CacheHit)
	}

	return attribute.String(AttrCacheResult, CacheMiss)
}

// Source returns the local/peer/upstream attribute.
func Source(value string) attribute.KeyValue {
	return attribute.String(AttrSource, value)
}

// PeerID returns the peer identifier attribute.
func PeerID(value string) attribute.KeyValue {
	return attribute.String(AttrPeerID, value)
}

// Registry returns the upstream registry attribute.
func Registry(value string) attribute.KeyValue {
	return attribute.String(AttrRegistry, value)
}

// Repository returns the repository attribute.
func Repository(value string) attribute.KeyValue {
	return attribute.String(AttrRepository, value)
}

// Kind returns the blob/manifest attribute.
func Kind(value string) attribute.KeyValue {
	return attribute.String(AttrKind, value)
}

// StatusCode returns the HTTP status attribute.
func StatusCode(code int) attribute.KeyValue {
	return attribute.Int(AttrStatusCode, code)
}
