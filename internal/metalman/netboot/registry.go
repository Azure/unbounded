// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	ispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Registry serves the pre-warmed OCI cache as a read-only distribution
// registry on the same HTTP server as the boot artifacts. Proxied nodes pull
// their agent image, their bootstrap bundle and their DaemonSet images from
// here, so a node only needs to reach metalman.
//
// Only images already in the cache index are served. There is no pull-through:
// an unknown reference is a 404, and metalman warms its cache from Kubernetes
// state instead (see OCIReconciler).
type Registry struct {
	Cache *OCICache
}

// ServeHTTP implements the distribution API paths the agent, oras and
// containerd use: the version check, manifest reads and blob reads.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := strings.TrimPrefix(strings.TrimPrefix(req.URL.Path, "/v2"), "/")

	if path == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{}")) //nolint:errcheck // Best-effort registry version response.

		return
	}

	if name, reference, ok := splitRegistryPath(path, "/manifests/"); ok {
		r.serveManifest(w, req, name, reference)

		return
	}

	if _, digest, ok := splitRegistryPath(path, "/blobs/"); ok {
		r.serveBlob(w, req, digest)

		return
	}

	http.NotFound(w, req)
}

// splitRegistryPath splits "<repository>/<separator><reference>" at the last
// occurrence of separator, so repository names may contain slashes.
func splitRegistryPath(path, separator string) (string, string, bool) {
	index := strings.LastIndex(path, separator)
	if index <= 0 {
		return "", "", false
	}

	name := path[:index]
	reference := path[index+len(separator):]

	if name == "" || reference == "" {
		return "", "", false
	}

	return name, reference, true
}

func (r *Registry) serveManifest(w http.ResponseWriter, req *http.Request, name, reference string) {
	for _, repository := range manifestRepositories(name, req.URL.Query()) {
		digest, ok := r.Cache.DigestForRepositoryReference(repository, reference)
		if ok {
			if r.writeManifest(w, digest) {
				return
			}
		}

		// A client that pins a digest rather than a tag is served from any
		// layout that holds it, whatever reference it was pulled under.
		if isDigest(reference) && r.writeManifest(w, reference) {
			return
		}
	}

	http.NotFound(w, req)
}

// manifestRepositories returns the repository spellings a request may use.
// containerd names the upstream registry in ?ns= and the path without it,
// while the agent and oras name the mirror as the registry and keep the
// upstream host in the path.
func manifestRepositories(name string, query url.Values) []string {
	repository := strings.Trim(name, "/")

	if ns := strings.Trim(query.Get("ns"), "/"); ns != "" && !strings.HasPrefix(repository, ns+"/") {
		return []string{ns + "/" + repository, repository}
	}

	return []string{repository}
}

// writeManifest serves the manifest blob for a digest.
func (r *Registry) writeManifest(w http.ResponseWriter, digest string) bool {
	blobPath := r.blobPath(digest)
	if blobPath == "" {
		return false
	}

	data, err := os.ReadFile(blobPath)
	if err != nil {
		return false
	}

	w.Header().Set("Content-Type", manifestMediaType(data))
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))

	// A failed write means the client went away; the digest is still the one
	// that was asked for.
	w.Write(data) //nolint:errcheck // Best-effort manifest response.

	return true
}

// serveBlob serves a content-addressed blob with range support.
func (r *Registry) serveBlob(w http.ResponseWriter, req *http.Request, digest string) {
	blobPath := r.blobPath(digest)
	if blobPath == "" {
		http.NotFound(w, req)

		return
	}

	w.Header().Set("Docker-Content-Digest", digest)
	http.ServeFile(w, req, blobPath)
}

// digestComponentPattern restricts a digest algorithm or encoded value to
// characters that cannot escape the layout directory. It deliberately rejects
// the dot that the OCI algorithm grammar allows, because a dot is enough to
// form a parent-directory reference.
var digestComponentPattern = regexp.MustCompile(`^[A-Za-z0-9_+=-]+$`)

// blobPath returns the cached file for a digest, searching every layout.
func (r *Registry) blobPath(digest string) string {
	algorithm, encoded, ok := strings.Cut(digest, ":")
	if !ok || !digestComponentPattern.MatchString(algorithm) || !digestComponentPattern.MatchString(encoded) {
		return ""
	}

	layouts, err := filepath.Glob(filepath.Join(r.Cache.CacheDir, "layouts", "*"))
	if err != nil {
		return ""
	}

	for _, layoutDir := range layouts {
		candidate := filepath.Join(layoutDir, "blobs", algorithm, encoded)
		if !withinDir(layoutDir, candidate) {
			continue
		}

		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}

	return ""
}

// withinDir reports whether target stays inside dir.
func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// splitImageReference splits an image reference into its repository and its
// tag or digest, following the same rules as oci:// references.
func splitImageReference(imageRef string) (repository, reference string, ok bool) {
	imageRef = strings.TrimPrefix(strings.TrimSpace(imageRef), "oci://")
	if imageRef == "" {
		return "", "", false
	}

	if index := strings.LastIndex(imageRef, "@"); index > 0 && index < len(imageRef)-1 {
		return imageRef[:index], imageRef[index+1:], true
	}

	lastSlash := strings.LastIndex(imageRef, "/")
	lastColon := strings.LastIndex(imageRef, ":")

	if lastColon > lastSlash && lastColon > 0 && lastColon < len(imageRef)-1 {
		return imageRef[:lastColon], imageRef[lastColon+1:], true
	}

	return "", "", false
}

func isDigest(reference string) bool {
	algorithm, encoded, ok := strings.Cut(reference, ":")

	return ok && algorithm == "sha256" && len(encoded) == 64
}

// manifestMediaType reads the media type of a manifest, falling back to the
// shape of the document when the field is missing.
func manifestMediaType(data []byte) string {
	var probe struct {
		MediaType string          `json:"mediaType"`
		Manifests json.RawMessage `json:"manifests"`
	}

	if err := json.Unmarshal(data, &probe); err == nil {
		if probe.MediaType != "" {
			return probe.MediaType
		}

		if len(probe.Manifests) > 0 {
			return ispec.MediaTypeImageIndex
		}
	}

	return ispec.MediaTypeImageManifest
}
