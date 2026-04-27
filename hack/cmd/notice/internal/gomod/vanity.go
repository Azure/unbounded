// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gomod

import (
	"regexp"
	"strings"
)

// goRepoBase returns the base repository URL and a ref (tag/branch) suitable
// for constructing a `<base>/blob/<ref>/<licenseFile>` URL.
//
// It prefers Origin.URL from `go mod download -json -x` when populated, and
// otherwise falls back to a small table of vanity-import rewrites. Origin info
// is missing for many older versions because the module proxy did not record
// it; in those cases the heuristic is the only viable source.
func goRepoBase(modPath, version string, info *goModDownload) (base, ref string) {
	ref = vcsRef(version)

	if info != nil && info.Origin != nil && info.Origin.URL != "" {
		base = strings.TrimSuffix(info.Origin.URL, ".git")
		base = strings.TrimPrefix(base, "git+")

		if strings.HasPrefix(info.Origin.Ref, "refs/tags/") {
			ref = strings.TrimPrefix(info.Origin.Ref, "refs/tags/")
		} else if strings.HasPrefix(info.Origin.Ref, "refs/heads/") {
			ref = strings.TrimPrefix(info.Origin.Ref, "refs/heads/")
		}

		return base, ref
	}

	base = vanityRepoBase(modPath)

	return base, ref
}

// vcsRef converts a Go module version to a VCS ref usable in a URL path.
// Pseudo-versions (v0.0.0-YYYYMMDDHHMMSS-abcdef123456) are reduced to the
// 12-char commit hash; +incompatible suffixes are stripped; other tagged
// versions pass through unchanged.
func vcsRef(version string) string {
	v := strings.TrimSuffix(version, "+incompatible")
	if pseudoCommit := pseudoVersionCommit(v); pseudoCommit != "" {
		return pseudoCommit
	}

	return v
}

var pseudoRE = regexp.MustCompile(`-([0-9a-f]{12})$`)

func pseudoVersionCommit(version string) string {
	m := pseudoRE.FindStringSubmatch(version)
	if m == nil {
		return ""
	}

	return m[1]
}

// vanityRepoBase derives a best-effort repository base URL from a Go module
// path when the module proxy did not record Origin info. The mappings cover
// the vanity-import domains used by direct deps in this repository; modules
// not matched fall through to a generic GitHub-shaped path which is a safe
// default for github.com/* and a clear "wrong" link for everything else,
// surfaced via the regenerated NOTICE for review.
func vanityRepoBase(modPath string) string {
	switch {
	case strings.HasPrefix(modPath, "github.com/"):
		parts := strings.SplitN(modPath, "/", 4)
		if len(parts) >= 3 {
			return "https://github.com/" + parts[1] + "/" + parts[2]
		}
	case strings.HasPrefix(modPath, "golang.org/x/"):
		// e.g. golang.org/x/crypto -> https://cs.opensource.google/go/x/crypto
		name := strings.TrimPrefix(modPath, "golang.org/x/")
		name = strings.SplitN(name, "/", 2)[0]

		return "https://cs.opensource.google/go/x/" + name
	case strings.HasPrefix(modPath, "k8s.io/"):
		name := strings.TrimPrefix(modPath, "k8s.io/")
		name = strings.SplitN(name, "/", 2)[0]
		// strip /v2, /v3 ... module path segments from the project name.
		name = strings.TrimSuffix(name, ".v0")

		return "https://github.com/kubernetes/" + name
	case strings.HasPrefix(modPath, "sigs.k8s.io/"):
		name := strings.TrimPrefix(modPath, "sigs.k8s.io/")
		name = strings.SplitN(name, "/", 2)[0]

		return "https://github.com/kubernetes-sigs/" + name
	case strings.HasPrefix(modPath, "gopkg.in/"):
		// gopkg.in/yaml.v3 -> https://github.com/go-yaml/yaml
		// gopkg.in/foo.v1 -> https://github.com/go-foo/foo (heuristic)
		// gopkg.in/user/foo.v1 -> https://github.com/user/foo
		rest := strings.TrimPrefix(modPath, "gopkg.in/")
		parts := strings.SplitN(rest, "/", 2)

		var user, name string

		if len(parts) == 1 {
			name = stripVersionSuffix(parts[0])
			// gopkg.in/<name>.vN convention: upstream lives at github.com/go-<name>/<name>.
			user = "go-" + name
		} else {
			user = parts[0]
			name = stripVersionSuffix(parts[1])
		}

		return "https://github.com/" + user + "/" + name
	case strings.HasPrefix(modPath, "google.golang.org/"):
		name := strings.TrimPrefix(modPath, "google.golang.org/")
		name = strings.SplitN(name, "/", 2)[0]

		switch name {
		case "grpc":
			return "https://github.com/grpc/grpc-go"
		case "protobuf":
			return "https://github.com/protocolbuffers/protobuf-go"
		case "genproto":
			return "https://github.com/googleapis/go-genproto"
		case "api":
			return "https://github.com/googleapis/google-api-go-client"
		}
	case strings.HasPrefix(modPath, "oras.land/"):
		// oras.land/oras-go/v2 -> https://github.com/oras-project/oras-go
		rest := strings.TrimPrefix(modPath, "oras.land/")
		name := strings.SplitN(rest, "/", 2)[0]

		return "https://github.com/oras-project/" + name
	case strings.HasPrefix(modPath, "modernc.org/"):
		name := strings.TrimPrefix(modPath, "modernc.org/")
		name = strings.SplitN(name, "/", 2)[0]

		return "https://gitlab.com/cznic/" + name
	case strings.HasPrefix(modPath, "golang.zx2c4.com/wireguard/wgctrl"):
		return "https://github.com/WireGuard/wgctrl-go"
	case strings.HasPrefix(modPath, "golang.zx2c4.com/wireguard"):
		return "https://github.com/WireGuard/wireguard-go"
	}

	return "https://" + modPath
}

func stripVersionSuffix(s string) string {
	// Trim a trailing .vN segment (e.g. "yaml.v3" -> "yaml").
	if i := strings.LastIndex(s, ".v"); i > 0 {
		suffix := s[i+2:]
		allDigits := suffix != ""

		for _, r := range suffix {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}

		if allDigits {
			return s[:i]
		}
	}

	return s
}
