// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package imagepulltest provides an in-memory OCI registry for tests that
// pull images over HTTP.
package imagepulltest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Docker V2 media types.
const (
	DockerMediaTypeManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	DockerMediaTypeManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	DockerMediaTypeConfig       = "application/vnd.docker.container.image.v1+json"
	DockerMediaTypeLayer        = "application/vnd.docker.image.rootfs.diff.tar"
	DockerMediaTypeLayerGzip    = "application/vnd.docker.image.rootfs.diff.tar.gzip"
)

// Request kinds recorded by the registry.
const (
	KindPing     = "ping"
	KindManifest = "manifest"
	KindBlob     = "blob"
)

// Option configures a Registry.
type Option func(*Registry)

// WithoutRange makes the registry ignore Range headers and always answer
// blob requests with 200 and the full body.
func WithoutRange() Option { return func(r *Registry) { r.noRange = true } }

// WithBasicAuth requires HTTP basic authentication on every /v2/ request.
func WithBasicAuth(username, password string) Option {
	return func(r *Registry) { r.username, r.password = username, password }
}

// WithBlobRedirect makes blob GETs answer with 307 Temporary Redirect to a
// second server (a fake CDN, without authentication) that serves the blob.
func WithBlobRedirect() Option { return func(r *Registry) { r.redirect = true } }

// WithMiddleware wraps the registry handler, for example to inject failures.
// It wraps the CDN handler too when WithBlobRedirect is set.
func WithMiddleware(mw func(http.Handler) http.Handler) Option {
	return func(r *Registry) { r.middleware = mw }
}

// Request is a request observed by the registry.
type Request struct {
	Method string
	Kind   string
	Repo   string
	// Ref is the tag or digest of a manifest or blob request.
	Ref   string
	Range string
	Query url.Values
	// CDN reports whether the request reached the redirect target.
	CDN bool
}

// Registry is an in-memory OCI distribution registry backed by httptest.
type Registry struct {
	srv        *httptest.Server
	cdn        *httptest.Server
	noRange    bool
	username   string
	password   string
	redirect   bool
	middleware func(http.Handler) http.Handler

	mu        sync.Mutex
	blobs     map[digest.Digest][]byte
	manifests map[string]map[string]manifestEntry // repo -> tag or digest
	layers    map[digest.Digest][]digest.Digest   // manifest -> layers
	requests  []Request
	// pending holds manifests built by pushManifest until storeManifest
	// stores them in a repository.
	pending []manifestEntry
}

type manifestEntry struct {
	mediaType string
	body      []byte
}

// NewRegistry starts a registry that is closed when the test ends.
func NewRegistry(t testing.TB, opts ...Option) *Registry {
	t.Helper()

	r := &Registry{
		blobs:     map[digest.Digest][]byte{},
		manifests: map[string]map[string]manifestEntry{},
		layers:    map[digest.Digest][]digest.Digest{},
	}
	for _, o := range opts {
		o(r)
	}

	wrap := func(h http.Handler) http.Handler {
		if r.middleware != nil {
			return r.middleware(h)
		}

		return h
	}

	if r.redirect {
		r.cdn = httptest.NewServer(wrap(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			r.serve(w, req, true)
		})))
		t.Cleanup(r.cdn.Close)
	}

	r.srv = httptest.NewServer(wrap(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.serve(w, req, false)
	})))
	t.Cleanup(r.srv.Close)

	return r
}

// Host returns the registry host:port.
func (r *Registry) Host() string { return r.srv.Listener.Addr().String() }

// URL returns the registry base URL.
func (r *Registry) URL() string { return r.srv.URL }

// Close stops the registry.
func (r *Registry) Close() {
	r.srv.Close()

	if r.cdn != nil {
		r.cdn.Close()
	}
}

// Requests returns a copy of the requests observed so far.
func (r *Registry) Requests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]Request(nil), r.requests...)
}

// ResetRequests clears the request log.
func (r *Registry) ResetRequests() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests = nil
}

// Count returns the number of GET or HEAD requests of the given kind.
func (r *Registry) Count(kind string) int {
	n := 0

	for _, req := range r.Requests() {
		if req.Kind == kind {
			n++
		}
	}

	return n
}

// BlobRequests returns the blob requests for digest d.
func (r *Registry) BlobRequests(d string) []Request {
	var out []Request

	for _, req := range r.Requests() {
		if req.Kind == KindBlob && req.Ref == d {
			out = append(out, req)
		}
	}

	return out
}

// SetBlob replaces the bytes served for digest d, for example to simulate
// corruption.
func (r *Registry) SetBlob(d string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.blobs[digest.Digest(d)] = data
}

// LayerDigests returns the layer digests of a single-platform manifest
// pushed to the registry.
func (r *Registry) LayerDigests(manifestDigest string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []string
	for _, d := range r.layers[digest.Digest(manifestDigest)] {
		out = append(out, d.String())
	}

	return out
}

func (r *Registry) record(req Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests = append(r.requests, req)
}

func (r *Registry) authorized(req *http.Request) bool {
	if r.username == "" {
		return true
	}

	u, p, ok := req.BasicAuth()

	return ok && u == r.username && p == r.password
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request, cdn bool) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")

	p := req.URL.Path
	if p == "/v2/" || p == "/v2" {
		r.record(Request{Method: req.Method, Kind: KindPing, Query: req.URL.Query(), CDN: cdn})

		if !r.authorized(req) {
			r.challenge(w)
			return
		}

		w.WriteHeader(http.StatusOK)

		return
	}

	rest, ok := strings.CutPrefix(p, "/v2/")
	if !ok {
		http.NotFound(w, req)
		return
	}

	var kind, repo, ref string

	if i := strings.LastIndex(rest, "/manifests/"); i >= 0 {
		kind, repo, ref = KindManifest, rest[:i], rest[i+len("/manifests/"):]
	} else if i := strings.LastIndex(rest, "/blobs/"); i >= 0 {
		kind, repo, ref = KindBlob, rest[:i], rest[i+len("/blobs/"):]
	} else {
		http.NotFound(w, req)
		return
	}

	r.record(Request{
		Method: req.Method, Kind: kind, Repo: repo, Ref: ref,
		Range: req.Header.Get("Range"), Query: req.URL.Query(), CDN: cdn,
	})

	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !cdn && !r.authorized(req) {
		r.challenge(w)
		return
	}

	if kind == KindManifest {
		r.serveManifest(w, req, repo, ref)
		return
	}

	if r.redirect && !cdn {
		http.Redirect(w, req, r.cdn.URL+req.URL.Path, http.StatusTemporaryRedirect)
		return
	}

	r.serveBlob(w, req, ref)
}

func (r *Registry) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="imagepulltest"`)
	http.Error(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`, http.StatusUnauthorized)
}

func (r *Registry) serveManifest(w http.ResponseWriter, req *http.Request, repo, ref string) {
	r.mu.Lock()
	m, ok := r.manifests[repo][ref]
	r.mu.Unlock()

	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`)) //nolint:errcheck // test server

		return
	}

	w.Header().Set("Content-Type", m.mediaType)
	w.Header().Set("Docker-Content-Digest", digest.FromBytes(m.body).String())
	w.Header().Set("Content-Length", strconv.Itoa(len(m.body)))
	w.WriteHeader(http.StatusOK)

	if req.Method == http.MethodGet {
		_, _ = w.Write(m.body) //nolint:errcheck // test server
	}
}

func (r *Registry) serveBlob(w http.ResponseWriter, req *http.Request, ref string) {
	r.mu.Lock()
	data, ok := r.blobs[digest.Digest(ref)]
	r.mu.Unlock()

	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"code":"BLOB_UNKNOWN","message":"blob unknown"}]}`)) //nolint:errcheck // test server

		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", ref)

	if !r.noRange {
		w.Header().Set("Accept-Ranges", "bytes")

		if start, end, ok := parseRange(req.Header.Get("Range"), int64(len(data))); ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)

			if req.Method == http.MethodGet {
				_, _ = w.Write(data[start : end+1]) //nolint:errcheck // test server
			}

			return
		}
	}

	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)

	if req.Method == http.MethodGet {
		_, _ = w.Write(data) //nolint:errcheck // test server
	}
}

// parseRange parses a single "bytes=a-b" or "bytes=a-" range.
func parseRange(h string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}

	a, b, ok := strings.Cut(spec, "-")
	if !ok || a == "" {
		return 0, 0, false
	}

	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start >= size {
		return 0, 0, false
	}

	end := size - 1

	if b != "" {
		e, err := strconv.ParseInt(b, 10, 64)
		if err != nil || e < start {
			return 0, 0, false
		}

		end = min(e, size-1)
	}

	return start, end, true
}

// Compression selects a layer encoding.
type Compression int

const (
	// Gzip is a tar+gzip layer (the default).
	Gzip Compression = iota
	// Uncompressed is a plain tar layer.
	Uncompressed
	// Zstd is a tar+zstd layer. It has no Docker media type.
	Zstd
)

// EntryType is the type of a layer tar entry.
type EntryType int

const (
	// TypeFile is a regular file.
	TypeFile EntryType = iota
	// TypeDir is a directory.
	TypeDir
	// TypeSymlink is a symbolic link to Linkname.
	TypeSymlink
	// TypeHardlink is a hard link to Linkname.
	TypeHardlink
)

// Entry is one tar entry in a layer.
type Entry struct {
	Name     string
	Type     EntryType
	Content  []byte
	Linkname string
}

// File returns a regular file entry.
func File(name string, content []byte) Entry {
	return Entry{Name: name, Type: TypeFile, Content: content}
}

// Dir returns a directory entry.
func Dir(name string) Entry { return Entry{Name: name, Type: TypeDir} }

// Symlink returns a symbolic link entry.
func Symlink(name, target string) Entry {
	return Entry{Name: name, Type: TypeSymlink, Linkname: target}
}

// Hardlink returns a hard link entry.
func Hardlink(name, target string) Entry {
	return Entry{Name: name, Type: TypeHardlink, Linkname: target}
}

// Whiteout returns an entry that deletes path from lower layers.
func Whiteout(path string) Entry {
	dir, base := splitPath(path)
	return File(dir+".wh."+base, nil)
}

// Opaque returns an entry that hides the contents of dir in lower layers.
func Opaque(dir string) Entry {
	dir = strings.Trim(dir, "/")
	if dir != "" {
		dir += "/"
	}

	return File(dir+".wh..wh..opq", nil)
}

func splitPath(p string) (string, string) {
	p = strings.Trim(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i+1], p[i+1:]
	}

	return "", p
}

// Layer describes one image layer.
type Layer struct {
	// Entries are written first, in order.
	Entries []Entry
	// Files are written after Entries, sorted by path.
	Files       map[string][]byte
	Compression Compression
}

// ImageSpec describes an image to push.
type ImageSpec struct {
	// Layers builds a single-platform (linux/amd64) manifest. Ignored when
	// Platforms is set.
	Layers []Layer
	// Platforms maps an architecture (such as "arm64") to its layers and
	// builds an image index with one manifest per architecture.
	Platforms map[string][]Layer
	// Docker uses Docker V2 media types instead of OCI media types.
	Docker bool
}

// PushFiles pushes a single-layer, single-platform gzip image and returns
// its reference and manifest digest.
func (r *Registry) PushFiles(t testing.TB, repo, tag string, files map[string][]byte) (string, string) {
	t.Helper()
	return r.Push(t, repo, tag, ImageSpec{Layers: []Layer{{Files: files}}})
}

// PushMultiArch pushes an image index with one single-layer gzip manifest
// per architecture.
func (r *Registry) PushMultiArch(t testing.TB, repo, tag string, files map[string]map[string][]byte) (string, string) {
	t.Helper()

	platforms := map[string][]Layer{}
	for arch, f := range files {
		platforms[arch] = []Layer{{Files: f}}
	}

	return r.Push(t, repo, tag, ImageSpec{Platforms: platforms})
}

// Push pushes img under repo:tag and returns the reference
// (host/repo:tag) and the top-level manifest or index digest.
func (r *Registry) Push(t testing.TB, repo, tag string, img ImageSpec) (string, string) {
	t.Helper()

	var top manifestEntry

	if img.Platforms == nil {
		_, top = r.pushManifest(t, "amd64", img.Layers, img.Docker)
	} else {
		archs := make([]string, 0, len(img.Platforms))
		for a := range img.Platforms {
			archs = append(archs, a)
		}

		sort.Strings(archs)

		idx := ispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ispec.MediaTypeImageIndex}
		if img.Docker {
			idx.MediaType = DockerMediaTypeManifestList
		}

		for _, a := range archs {
			desc, _ := r.pushManifest(t, a, img.Platforms[a], img.Docker)
			desc.Platform = &ispec.Platform{OS: "linux", Architecture: a}
			idx.Manifests = append(idx.Manifests, desc)
		}

		top = manifestEntry{mediaType: idx.MediaType, body: mustJSON(t, idx)}
	}

	d := digest.FromBytes(top.body).String()
	r.storeManifest(repo, d, top)
	r.storeManifest(repo, tag, top)

	return fmt.Sprintf("%s/%s:%s", r.Host(), repo, tag), d
}

// pushManifest stores the blobs of a single-platform manifest and queues
// the manifest for the next storeManifest call.
func (r *Registry) pushManifest(t testing.TB, arch string, layers []Layer, docker bool) (ispec.Descriptor, manifestEntry) {
	t.Helper()

	cfgMediaType := ispec.MediaTypeImageConfig
	manifestMediaType := ispec.MediaTypeImageManifest

	if docker {
		cfgMediaType = DockerMediaTypeConfig
		manifestMediaType = DockerMediaTypeManifest
	}

	m := ispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: manifestMediaType}

	var diffIDs []digest.Digest

	for _, l := range layers {
		raw, blob, mediaType := buildLayer(t, l, docker)
		diffIDs = append(diffIDs, digest.FromBytes(raw))
		m.Layers = append(m.Layers, r.storeBlob(mediaType, blob))
	}

	cfg := ispec.Image{
		Platform: ispec.Platform{OS: "linux", Architecture: arch},
		RootFS:   ispec.RootFS{Type: "layers", DiffIDs: diffIDs},
	}
	m.Config = r.storeBlob(cfgMediaType, mustJSON(t, cfg))

	body := mustJSON(t, m)
	d := digest.FromBytes(body)
	entry := manifestEntry{mediaType: manifestMediaType, body: body}

	r.mu.Lock()
	for _, l := range m.Layers {
		r.layers[d] = append(r.layers[d], l.Digest)
	}

	r.pending = append(r.pending, entry)
	r.mu.Unlock()

	return ispec.Descriptor{MediaType: manifestMediaType, Digest: d, Size: int64(len(body))}, entry
}

func (r *Registry) storeBlob(mediaType string, data []byte) ispec.Descriptor {
	d := digest.FromBytes(data)

	r.mu.Lock()
	r.blobs[d] = data
	r.mu.Unlock()

	return ispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(data))}
}

func (r *Registry) storeManifest(repo, ref string, m manifestEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.manifests[repo] == nil {
		r.manifests[repo] = map[string]manifestEntry{}
	}

	r.manifests[repo][ref] = m

	for _, p := range r.pending {
		r.manifests[repo][digest.FromBytes(p.body).String()] = p
	}

	r.pending = nil
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return b
}

// buildLayer returns the uncompressed tar, the encoded blob, and the media
// type.
func buildLayer(t testing.TB, l Layer, docker bool) ([]byte, []byte, string) {
	t.Helper()

	var tarBuf bytes.Buffer

	tw := tar.NewWriter(&tarBuf)
	mtime := time.Unix(1700000000, 0).UTC()

	entries := append([]Entry(nil), l.Entries...)

	names := make([]string, 0, len(l.Files))
	for n := range l.Files {
		names = append(names, n)
	}

	sort.Strings(names)

	for _, n := range names {
		entries = append(entries, File(n, l.Files[n]))
	}

	for _, e := range entries {
		hdr := &tar.Header{Name: e.Name, Mode: 0o644, ModTime: mtime, Linkname: e.Linkname}

		switch e.Type {
		case TypeFile:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.Content))
		case TypeDir:
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		case TypeSymlink:
			hdr.Typeflag = tar.TypeSymlink
		case TypeHardlink:
			hdr.Typeflag = tar.TypeLink
		}

		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header: %v", err)
		}

		if e.Type == TypeFile {
			if _, err := tw.Write(e.Content); err != nil {
				t.Fatalf("tar write: %v", err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}

	raw := tarBuf.Bytes()

	switch l.Compression {
	case Uncompressed:
		if docker {
			return raw, raw, DockerMediaTypeLayer
		}

		return raw, raw, ispec.MediaTypeImageLayer
	case Zstd:
		var buf bytes.Buffer

		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatalf("zstd: %v", err)
		}

		_, _ = zw.Write(raw) //nolint:errcheck // in-memory buffer

		if err := zw.Close(); err != nil {
			t.Fatalf("zstd close: %v", err)
		}

		return raw, buf.Bytes(), ispec.MediaTypeImageLayerZstd
	default:
		var buf bytes.Buffer

		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(raw) //nolint:errcheck // in-memory buffer

		if err := zw.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}

		if docker {
			return raw, buf.Bytes(), DockerMediaTypeLayerGzip
		}

		return raw, buf.Bytes(), ispec.MediaTypeImageLayerGzip
	}
}

// BasicAuthHeader returns the Authorization header value for basic auth.
func BasicAuthHeader(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}
