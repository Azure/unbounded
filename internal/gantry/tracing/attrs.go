// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"go.opentelemetry.io/otel/attribute"
)

const (
	AttrDigest = "gantry.digest"

	AttrMediaType = "gantry.media_type"

	AttrBytes = "gantry.bytes"

	AttrCacheResult = "gantry.cache.result"

	AttrSource = "gantry.source"

	AttrPeerID = "gantry.peer.id"

	AttrRegistry = "gantry.registry"

	AttrRepository = "gantry.repository"

	AttrKind = "gantry.kind"

	AttrStatusCode = "http.response.status_code"

	AttrURLFull = "url.full"

	AttrURLRedacted = "gantry.url.redacted"
)

const (
	CacheHit  = "hit"
	CacheMiss = "miss"

	SourceLocal    = "local"
	SourcePeer     = "peer"
	SourceUpstream = "upstream"
)

func Digest(value string) attribute.KeyValue {
	return attribute.String(AttrDigest, value)
}

func MediaType(value string) attribute.KeyValue {
	return attribute.String(AttrMediaType, value)
}

func Bytes(n int64) attribute.KeyValue {
	return attribute.Int64(AttrBytes, n)
}

func Cache(hit bool) attribute.KeyValue {
	if hit {
		return attribute.String(AttrCacheResult, CacheHit)
	}

	return attribute.String(AttrCacheResult, CacheMiss)
}

func Source(value string) attribute.KeyValue {
	return attribute.String(AttrSource, value)
}

func PeerID(value string) attribute.KeyValue {
	return attribute.String(AttrPeerID, value)
}

func Registry(value string) attribute.KeyValue {
	return attribute.String(AttrRegistry, value)
}

func Repository(value string) attribute.KeyValue {
	return attribute.String(AttrRepository, value)
}

func Kind(value string) attribute.KeyValue {
	return attribute.String(AttrKind, value)
}

func StatusCode(code int) attribute.KeyValue {
	return attribute.Int(AttrStatusCode, code)
}
