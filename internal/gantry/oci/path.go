// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package oci hosts shared OCI/Distribution-spec helpers used by more
// than one Gantry subsystem. The mirror and the transfer endpoint were
// each carrying their own parseV2Path; this is their canonical home.
package oci

import (
	"strings"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// ParseV2Path matches a Distribution-spec `/v2/<repo>/(manifests|blobs)/<reference>`
// URL. Returns the repository path (which may itself contain slashes -
// e.g. `library/nginx`), the resource kind (manifest vs blob), the
// reference (tag or digest), and ok=false if the path doesn't match.
//
// The match uses `strings.LastIndex` on the kind separators so a repo
// name like `cdn/manifests-mirror/foo` doesn't get clipped at the first
// `/manifests/` substring - the canonical Distribution semantics are
// "last occurrence wins".
//
// Two-package call sites (mirror + transfer) MUST go through this
// function so they stay byte-for-byte aligned; otherwise a path the
// mirror accepts could be rejected by the peer endpoint and vice versa,
// which would manifest as silent peer-fetch 404s.
func ParseV2Path(path string) (repo string, kind ifaces.OriginRefKind, ref string, ok bool) {
	const prefix = "/v2/"
	if !strings.HasPrefix(path, prefix) {
		return "", 0, "", false
	}

	rest := path[len(prefix):]
	if idx := strings.LastIndex(rest, "/manifests/"); idx >= 0 {
		return rest[:idx], ifaces.KindManifest, rest[idx+len("/manifests/"):], true
	}

	if idx := strings.LastIndex(rest, "/blobs/"); idx >= 0 {
		return rest[:idx], ifaces.KindBlob, rest[idx+len("/blobs/"):], true
	}

	return "", 0, "", false
}
