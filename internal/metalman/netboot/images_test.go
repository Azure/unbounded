// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"net"
	"net/http"
	"path"
	"testing"

	"oras.land/oras-go/v2/registry"

	"github.com/Azure/unbounded/internal/metalman/imagepull"
	"github.com/Azure/unbounded/internal/metalman/imagepull/imagepulltest"
)

// testImages is an in-memory registry for netboot tests. Its Puller sends
// requests for every registry host to the in-memory registry, so tests can
// use realistic references such as ghcr.io/test/image:v1.
type testImages struct {
	Registry *imagepulltest.Registry
	Puller   *imagepull.Puller
}

func newTestImages(t *testing.T, opts ...imagepulltest.Option) *testImages {
	t.Helper()

	reg := imagepulltest.NewRegistry(t, opts...)

	return &testImages{
		Registry: reg,
		Puller: &imagepull.Puller{
			HTTPClient: &http.Client{Transport: &redirectTransport{host: reg.Host()}},
		},
	}
}

// redirectTransport sends every request to host over plain HTTP.
type redirectTransport struct {
	host string
}

func (rt *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = "http"
	r.URL.Host = rt.host
	r.Host = rt.host

	resp, err := http.DefaultTransport.RoundTrip(r)
	if resp != nil {
		// Report the original request so that the registry client treats
		// the response as coming from the original host.
		resp.Request = req
	}

	return resp, err
}

// unreachablePuller returns a Puller whose registry connections fail.
func unreachablePuller(t *testing.T) *imagepull.Puller {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	host := l.Addr().String()
	l.Close() //nolint:errcheck // only the free address is needed

	return &imagepull.Puller{HTTPClient: &http.Client{Transport: &redirectTransport{host: host}}}
}

// diskFiles prefixes file names with the image disk directory.
func diskFiles(files map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(files))
	for name, data := range files {
		out[path.Join(imageDiskDir, name)] = data
	}

	return out
}

func parseTestRef(t *testing.T, ref string) registry.Reference {
	t.Helper()

	parsed, err := registry.ParseReference(ref)
	if err != nil {
		t.Fatalf("parse reference %q: %v", ref, err)
	}

	if parsed.Reference == "" {
		parsed.Reference = "latest"
	}

	return parsed
}

// Push pushes files, relative to the disk directory, as a single-platform
// amd64 image tagged as ref, and returns the digest-pinned reference.
func (ti *testImages) Push(t *testing.T, ref string, files map[string][]byte) string {
	t.Helper()

	parsed := parseTestRef(t, ref)
	_, dgst := ti.Registry.PushFiles(t, parsed.Repository, parsed.Reference, diskFiles(files))

	return parsed.Registry + "/" + parsed.Repository + "@" + dgst
}

// PushLayers pushes an amd64 image with one layer per file set, from the
// lowest layer to the highest, and returns the digest-pinned reference.
func (ti *testImages) PushLayers(t *testing.T, ref string, layers ...map[string][]byte) string {
	t.Helper()

	parsed := parseTestRef(t, ref)

	spec := imagepulltest.ImageSpec{}
	for _, files := range layers {
		spec.Layers = append(spec.Layers, imagepulltest.Layer{Files: diskFiles(files)})
	}

	_, dgst := ti.Registry.Push(t, parsed.Repository, parsed.Reference, spec)

	return parsed.Registry + "/" + parsed.Repository + "@" + dgst
}

// PushArch pushes an image index with one image per architecture. files
// maps architecture to files relative to the disk directory. It returns the
// digest-pinned reference of the index.
func (ti *testImages) PushArch(t *testing.T, ref string, files map[string]map[string][]byte) string {
	t.Helper()

	parsed := parseTestRef(t, ref)

	platforms := make(map[string]map[string][]byte, len(files))
	for arch, f := range files {
		platforms[arch] = diskFiles(f)
	}

	_, dgst := ti.Registry.PushMultiArch(t, parsed.Repository, parsed.Reference, platforms)

	return parsed.Registry + "/" + parsed.Repository + "@" + dgst
}

// setupImagePuller pushes files as ref and returns a Puller that pulls from
// the test registry.
func setupImagePuller(t *testing.T, ref string, files map[string][]byte) *imagepull.Puller {
	t.Helper()

	images := newTestImages(t)
	images.Push(t, ref, files)

	return images.Puller
}
