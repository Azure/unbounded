// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package imagepull reads individual files out of OCI images directly from a
// registry, without unpacking the image to disk.
//
// A Puller resolves an image reference to a single-platform manifest and
// returns an Image pinned to that manifest digest. Image.Open streams the
// image layers top-down (the last layer first), decompressing each layer and
// scanning its tar entries until the requested file is found. Whiteouts in
// upper layers hide files in lower layers, as described in the OCI image
// layer specification.
//
// # Blob downloads
//
// Layer blobs are downloaded with HTTP Range requests when the registry
// supports them. The first request asks for the first chunk; if the registry
// answers with 206 Partial Content and a matching Content-Range, the remaining
// chunks are fetched concurrently (bounded by Puller.Concurrency) and
// delivered in order. If the registry answers with 200 OK, for example the
// Gantry mirror, which ignores bounded ranges, the body is consumed as one
// sequential stream instead.
//
// # Mirrors
//
// When Puller.Mirror is set, manifests and blobs are requested from the
// mirror first, with the upstream registry host passed in the ns query
// parameter (the containerd hosts.toml convention used by Gantry). Any mirror
// failure before bytes are handed to the caller falls back to the upstream
// registry. Gantry answers tag requests with 503, so tag resolution always
// falls back to the upstream registry.
//
// # Integrity
//
// Manifests are verified against their digest. Layer blobs are verified
// against their digest only when they are read to EOF. Because Image.Open
// stops reading a layer as soon as the requested file has been read, most
// reads never reach the end of the blob, and the digest of the bytes served
// cannot be verified in that case. Callers that need verified content must
// pull from a trusted registry or mirror over a trusted transport.
package imagepull
