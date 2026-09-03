// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package oci hosts shared OCI/Distribution-spec helpers used by more
// than one Gantry subsystem. The mirror and the transfer endpoint were
// each carrying their own parseV2Path; this is their canonical home.
package oci

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// MaxRepositoryNameLength bounds a repository name per the OCI
// Distribution spec (the full <name> must be at most 255 characters).
const MaxRepositoryNameLength = 255

// repositoryNameRe matches the OCI Distribution-spec repository name
// grammar:
//
//	name           := path-component ['/' path-component]*
//	path-component := alphanum [separator alphanum]*
//	alphanum       := [a-z0-9]+
//	separator      := [._] | __ | [-]+
//
// RE2 (Go's regexp engine) is backtracking-free, so matching is
// linear-time and safe against ReDoS even on adversarial input.
var repositoryNameRe = func() *regexp.Regexp {
	const (
		alphaNum  = `[a-z0-9]+`
		separator = `(?:[._]|__|-+)`
		component = alphaNum + `(?:` + separator + alphaNum + `)*`
	)

	return regexp.MustCompile(`^` + component + `(?:/` + component + `)*$`)
}()

// ValidateRepositoryName reports whether repo is a well-formed OCI
// Distribution-spec repository name. It rejects empty values, empty path
// components, names over MaxRepositoryNameLength, and any value outside
// the name grammar - so `..`, `?`, `#`, whitespace, control characters,
// and uppercase are all rejected. Callers use it to keep untrusted
// repository strings from reaching origin URL construction, containerd
// keys, and logs.
func ValidateRepositoryName(repo string) error {
	if repo == "" {
		return fmt.Errorf("oci: repository name is empty")
	}

	if len(repo) > MaxRepositoryNameLength {
		return fmt.Errorf("oci: repository name too long: %d > %d", len(repo), MaxRepositoryNameLength)
	}

	if !repositoryNameRe.MatchString(repo) {
		return fmt.Errorf("oci: invalid repository name %q", repo)
	}

	return nil
}

// ParseV2Path matches a Distribution-spec `/v2/<repo>/(manifests|blobs)/<reference>`
// URL. Returns the repository path (which may itself contain slashes -
// e.g. `library/nginx`), the resource kind (manifest vs blob), the
// reference (tag or digest), and ok=false if the path doesn't match.
//
// The kind separator is the RIGHTMOST `/manifests/` or `/blobs/` in the
// path, not the first one of a fixed kind. A repository path component may
// itself be named `manifests` or `blobs` (both match the OCI name grammar),
// so a path like `/v2/acme/manifests/cache/blobs/<digest>` must parse as
// repo=`acme/manifests/cache`, kind=blob - testing `/manifests/` first would
// wrongly clip it to repo=`acme`. The reference (tag or digest) never
// contains a slash, so the last separator of either kind unambiguously splits
// repo from reference; whichever separator sits further right wins.
//
// The extracted repository is validated against ValidateRepositoryName;
// a path whose repository component is outside the OCI name grammar
// (path traversal, query/fragment characters, control characters, empty
// components, uppercase) returns ok=false so the untrusted value never
// reaches origin URL construction or the peer endpoint.
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

	const (
		manifestsSep = "/manifests/"
		blobsSep     = "/blobs/"
	)

	mIdx := strings.LastIndex(rest, manifestsSep)
	bIdx := strings.LastIndex(rest, blobsSep)

	// Pick the rightmost separator. Indices are distinct when both are
	// present (the two literals can't start at the same offset), so there is
	// no tie to break.
	var (
		sep    string
		sepIdx int
	)

	switch {
	case mIdx < 0 && bIdx < 0:
		return "", 0, "", false
	case bIdx > mIdx:
		kind, sep, sepIdx = ifaces.KindBlob, blobsSep, bIdx
	default:
		kind, sep, sepIdx = ifaces.KindManifest, manifestsSep, mIdx
	}

	repo = rest[:sepIdx]
	if ValidateRepositoryName(repo) != nil {
		return "", 0, "", false
	}

	return repo, kind, rest[sepIdx+len(sep):], true
}
