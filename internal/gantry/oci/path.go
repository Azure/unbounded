// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
// The match uses `strings.LastIndex` on the kind separators so a repo
// name like `cdn/manifests-mirror/foo` doesn't get clipped at the first
// `/manifests/` substring - the canonical Distribution semantics are
// "last occurrence wins".
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
	if idx := strings.LastIndex(rest, "/manifests/"); idx >= 0 {
		repo = rest[:idx]
		if ValidateRepositoryName(repo) != nil {
			return "", 0, "", false
		}

		return repo, ifaces.KindManifest, rest[idx+len("/manifests/"):], true
	}

	if idx := strings.LastIndex(rest, "/blobs/"); idx >= 0 {
		repo = rest[:idx]
		if ValidateRepositoryName(repo) != nil {
			return "", 0, "", false
		}

		return repo, ifaces.KindBlob, rest[idx+len("/blobs/"):], true
	}

	return "", 0, "", false
}
