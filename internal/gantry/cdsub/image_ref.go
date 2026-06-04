// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cdsub

import "strings"

// registryFromImage extracts the host (registry) portion of a
// containerd image name. Examples:
//
//	"docker.io/library/alpine:3.18" -> "docker.io"
//	"registry.k8s.io/pause:3.9" -> "registry.k8s.io"
//	"ghcr.io/owner/repo@sha256:abc..." -> "ghcr.io"
//	"localhost:5000/myimg:tag" -> "localhost:5000"
//	"alpine" -> "" (no registry component)
//
// The OCI / docker reference convention is: the registry is the part
// before the first "/" *if* it contains a "." or ":" or is "localhost".
// Otherwise the name is treated as docker.io/library/<name> by tools
// like containerd; we deliberately return "" in that case so the
// caller can decide whether to attach a default.
//
// Build-tag agnostic so the helper is unit-testable on every platform,
// even though it's only invoked from source_containerd.go (linux).
func registryFromImage(name string) string {
	if name == "" {
		return ""
	}

	slash := strings.IndexByte(name, '/')
	if slash < 0 {
		return ""
	}

	prefix := name[:slash]
	if prefix == "localhost" || strings.ContainsAny(prefix, ".:") {
		return prefix
	}

	return ""
}
