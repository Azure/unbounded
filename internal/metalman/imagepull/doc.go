// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package imagepull reads individual files out of OCI images directly from a
// registry, without unpacking the image to disk.
//
// A Puller resolves an image reference to a single-platform manifest and
// returns an Image pinned to that manifest digest. Image.Open finds a file
// using per-layer indexes, then streams only the layer that holds it,
// decompressing it and skipping to the file's tar entry. Whiteouts, opaque
// directories, and non-directory entries in upper layers hide paths in
// lower layers, as described in the OCI image layer specification.
// Symbolic links (also in parent directories) and hard links are followed
// within the image root. A hard link to a file in a lower layer is
// resolved against the layers up to and including its own, so a later
// layer that replaces or removes the target does not change the link.
//
// # Layer indexes
//
// The first time a layer is needed, it is downloaded and read in full to
// build an in-memory index of its tar entries: name, type, link target,
// size, modification time, and the sha256 of each regular file. An index
// is only kept when the whole layer was read and its digest verified.
// Indexes are keyed by layer digest, shared by all Images from one Puller,
// and kept in a bounded LRU (Puller.MaxIndexedLayers). Concurrent requests
// for the same layer share a single build. Each caller stops waiting for a
// shared build when its own context is done, and a waiter whose context is
// still live retries if the build was canceled by its starter's context.
//
// The first use of a layer therefore costs one full layer download, and each
// file read costs another download of the layer up to the end of the file.
// Once the relevant layers are indexed, a missing file is answered without
// any registry requests, and layers that do not contain the requested name
// are not downloaded.
//
// # Blob downloads
//
// Layer blobs are downloaded with HTTP Range requests when the registry
// supports them. The first request asks for the first chunk; if the registry
// answers with 206 Partial Content and a matching Content-Range, the next
// chunks are prefetched concurrently (bounded by Puller.Concurrency) while
// the first one streams, and delivered in order. Prefetched chunks count
// against a Puller-wide memory budget (Puller.MaxBufferedBytes); a reader
// that cannot get budget streams its next chunk directly instead. If the
// registry answers with 200 OK, for example the Gantry mirror, which ignores
// bounded ranges, the body is consumed as one sequential stream instead.
// Failed chunk requests are retried; no other retry layer is stacked on
// blob requests.
//
// # Mirrors
//
// When Puller.Mirror is set, manifests addressed by digest and blobs are
// requested from the mirror first, with the upstream registry host passed in
// the ns query parameter (the containerd hosts.toml convention used by
// Gantry). Tag references are resolved against the upstream registry
// directly, because Gantry only serves content addressed by digest. A
// mirror failure falls back to the upstream registry; if the mirror fails
// in the middle of a blob, the download resumes from the upstream registry
// at the current offset. Mirror requests are bounded independently of the
// caller's context, so a mirror that accepts connections but stalls still
// leaves time for the upstream registry: Puller.MirrorManifestTimeout and
// Puller.MirrorBlobHeaderTimeout bound the wait for response headers, and
// Puller.MirrorIdleTimeout bounds a stalled response body, after which the
// download resumes from the upstream registry. When the caller's context
// has a deadline, each of these bounds is also capped at half of the time
// remaining when the mirror request starts, so the upstream fallback always
// keeps the other half of the caller's budget. Credentials are sent to the
// mirror too (see Puller.Mirror).
//
// # Integrity
//
// Manifests are verified against their digest. Layer blobs are verified
// against their digest when a layer index is built, which happens before
// any file is served from that layer. File content is then verified
// against the sha256 recorded in the index whenever a File is read
// sequentially from offset 0 to EOF; on a mismatch, the final Read returns
// no bytes and an error wrapping ErrDigestMismatch, so a streaming caller
// never receives the complete corrupted file. Image.ReadFile always reads
// this way, so its result is verified. Reads that start at a non-zero
// offset, such as HTTP Range requests served with http.ServeContent, cannot
// be verified: the bytes returned come from a layer that has been verified
// once, but the registry could serve different bytes on a later request.
//
// When content fails verification after a mirror served any of it, the
// blob is marked on the Image so that later reads skip the mirror and go to
// the upstream registry only. The Puller also remembers the blob for that
// mirror for Puller.MirrorQuarantine (DefaultMirrorQuarantine), so later
// Images it resolves skip the mirror for that blob too. A layer index build that fails this way is
// discarded and the layer is read again from the upstream registry from
// offset 0, and Image.ReadFile likewise retries once from upstream. A File
// being read still returns the error, since it may already have delivered
// unverified bytes; opening the file again reads from upstream. If the
// upstream content also fails verification, the error wraps
// ErrDigestMismatch.
package imagepull
