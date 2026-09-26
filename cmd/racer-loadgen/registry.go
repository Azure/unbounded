// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func (img *syntheticImage) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			registryError(w, r, http.StatusMethodNotAllowed, "UNSUPPORTED", "registry is read-only")

			return
		}

		if r.URL.Path == "/v2/" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "2")
			w.WriteHeader(http.StatusOK)

			if r.Method == http.MethodGet {
				if _, err := io.WriteString(w, "{}"); err != nil {
					return
				}
			}

			return
		}

		prefix := "/v2/" + img.repository + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			registryError(w, r, http.StatusNotFound, "NAME_UNKNOWN", "repository unknown")

			return
		}

		path := strings.TrimPrefix(r.URL.Path, prefix)
		if ref, ok := strings.CutPrefix(path, "manifests/"); ok {
			if ref != "latest" && ref != img.Manifest.Digest.String() {
				registryError(w, r, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")

				return
			}

			serveRegistryContent(w, r, img.Manifest, bytes.NewReader(img.manifest))

			return
		}

		if ref, ok := strings.CutPrefix(path, "blobs/"); ok {
			blob, found := img.blobs[digest.Digest(ref)]
			if !found {
				registryError(w, r, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown")

				return
			}

			serveRegistryContent(w, r, blob.descriptor, io.NewSectionReader(blob.data, 0, blob.descriptor.Size))

			return
		}

		registryError(w, r, http.StatusNotFound, "UNSUPPORTED", "registry endpoint unknown")
	})
}

func serveRegistryContent(w http.ResponseWriter, r *http.Request, desc ocispec.Descriptor, reader io.ReadSeeker) {
	w.Header().Set("Content-Type", desc.MediaType)
	w.Header().Set("Docker-Content-Digest", desc.Digest.String())
	w.Header().Set("ETag", `"`+desc.Digest.String()+`"`)
	http.ServeContent(w, r, "", time.Time{}, reader)
}

func registryError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if r.Method == http.MethodHead {
		return
	}

	if err := json.NewEncoder(w).Encode(struct {
		Errors []registryErrorDetail `json:"errors"`
	}{Errors: []registryErrorDetail{{Code: code, Message: message}}}); err != nil {
		return
	}
}

type registryErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
