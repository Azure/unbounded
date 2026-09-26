// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func registryRequest(handler http.Handler, method, path, byteRange string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if byteRange != "" {
		r.Header.Set("Range", byteRange)
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	return w
}

func TestRegistryGetAndHead(t *testing.T) {
	img, err := newImage(t.Context(), testImageOptions())
	require.NoError(t, err)

	handler := img.handler()
	prefix := "/v2/" + img.repository

	tests := []struct {
		path string
		desc ocispec.Descriptor
		data []byte
	}{
		{prefix + "/manifests/latest", img.Manifest, img.manifest},
		{prefix + "/manifests/" + img.Manifest.Digest.String(), img.Manifest, img.manifest},
		{prefix + "/blobs/" + img.Config.Digest.String(), img.Config, readImageBlob(t, img, img.Config)},
	}
	for _, desc := range img.Layers {
		tests = append(tests, struct {
			path string
			desc ocispec.Descriptor
			data []byte
		}{prefix + "/blobs/" + desc.Digest.String(), desc, readImageBlob(t, img, desc)})
	}

	for _, test := range tests {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := registryRequest(handler, method, test.path, "")
			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, test.desc.MediaType, w.Header().Get("Content-Type"))
			require.Equal(t, test.desc.Digest.String(), w.Header().Get("Docker-Content-Digest"))
			require.Equal(t, "registry/2.0", w.Header().Get("Docker-Distribution-API-Version"))
			require.Equal(t, strconv.FormatInt(test.desc.Size, 10), w.Header().Get("Content-Length"))
			require.Equal(t, "bytes", w.Header().Get("Accept-Ranges"))

			if method == http.MethodHead {
				require.Empty(t, w.Body.Bytes())
			} else {
				require.Equal(t, test.data, w.Body.Bytes())
			}
		}
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := registryRequest(handler, method, "/v2/", "")
		require.Equal(t, http.StatusOK, w.Code)
		require.Equal(t, "registry/2.0", w.Header().Get("Docker-Distribution-API-Version"))
		require.Equal(t, "2", w.Header().Get("Content-Length"))

		if method == http.MethodHead {
			require.Empty(t, w.Body.Bytes())
		} else {
			require.JSONEq(t, "{}", w.Body.String())
		}
	}
}

func TestRegistryRanges(t *testing.T) {
	img, err := newImage(t.Context(), testImageOptions())
	require.NoError(t, err)

	desc := img.Layers[0]
	data := readImageBlob(t, img, desc)
	path := "/v2/" + img.repository + "/blobs/" + desc.Digest.String()
	handler := img.handler()

	tests := []struct {
		value string
		start int64
		end   int64
	}{
		{"bytes=0-0", 0, 0},
		{"bytes=497-529", 497, 529},
		{"bytes=527-544", 527, 544},
		{"bytes=513-", 513, desc.Size - 1},
		{"bytes=-17", desc.Size - 17, desc.Size - 1},
		{fmt.Sprintf("bytes=%d-%d", desc.Size-2, desc.Size+50), desc.Size - 2, desc.Size - 1},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			w := registryRequest(handler, http.MethodGet, path, test.value)
			require.Equal(t, http.StatusPartialContent, w.Code)
			require.Equal(t, fmt.Sprintf("bytes %d-%d/%d", test.start, test.end, desc.Size), w.Header().Get("Content-Range"))
			require.Equal(t, strconv.FormatInt(test.end-test.start+1, 10), w.Header().Get("Content-Length"))
			require.Equal(t, desc.Digest.String(), w.Header().Get("Docker-Content-Digest"))
			require.Equal(t, data[test.start:test.end+1], w.Body.Bytes())
		})
	}

	for _, value := range []string{fmt.Sprintf("bytes=%d-", desc.Size), "bytes=20-10", "bytes=garbage"} {
		w := registryRequest(handler, http.MethodGet, path, value)
		require.Equal(t, http.StatusRequestedRangeNotSatisfiable, w.Code)
	}

	w := registryRequest(handler, http.MethodGet, path, "bytes=0-3,527-544")
	require.Equal(t, http.StatusPartialContent, w.Code)
	mediaType, params, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/byteranges", mediaType)

	mr := multipart.NewReader(w.Body, params["boundary"])
	for _, expected := range [][]byte{data[:4], data[527:545]} {
		part, err := mr.NextPart()
		require.NoError(t, err)
		actual, err := io.ReadAll(part)
		require.NoError(t, err)
		require.Equal(t, expected, actual)
	}

	_, err = mr.NextPart()
	require.ErrorIs(t, err, io.EOF)

	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("If-None-Match", `"`+desc.Digest.String()+`"`)

	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	require.Equal(t, http.StatusNotModified, w.Code)
	require.Empty(t, w.Body.Bytes())

	r = httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Range", "bytes=0-3")
	r.Header.Set("If-Range", `"stale"`)

	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, data, w.Body.Bytes())
}

func TestRegistryErrors(t *testing.T) {
	img, err := newImage(t.Context(), testImageOptions())
	require.NoError(t, err)

	handler := img.handler()
	prefix := "/v2/" + img.repository

	tests := []struct {
		path string
		code string
	}{
		{"/v2/other/manifests/latest", "NAME_UNKNOWN"},
		{prefix + "-extra/manifests/latest", "NAME_UNKNOWN"},
		{prefix + "/manifests/missing", "MANIFEST_UNKNOWN"},
		{prefix + "/manifests/" + img.Config.Digest.String(), "MANIFEST_UNKNOWN"},
		{prefix + "/blobs/sha256:0000", "BLOB_UNKNOWN"},
		{prefix + "/blobs/" + img.Manifest.Digest.String(), "BLOB_UNKNOWN"},
		{prefix + "/tags/list", "UNSUPPORTED"},
	}
	for _, test := range tests {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			w := registryRequest(handler, method, test.path, "")
			require.Equal(t, http.StatusNotFound, w.Code)
			require.Equal(t, "application/json", w.Header().Get("Content-Type"))
			require.Equal(t, "registry/2.0", w.Header().Get("Docker-Distribution-API-Version"))

			if method == http.MethodHead {
				require.Empty(t, w.Body.Bytes())
				continue
			}

			var body struct {
				Errors []registryErrorDetail `json:"errors"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Len(t, body.Errors, 1)
			require.Equal(t, test.code, body.Errors[0].Code)
			require.NotEmpty(t, body.Errors[0].Message)
		}
	}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		for _, path := range []string{"/v2/", prefix + "/manifests/latest", prefix + "/blobs/" + img.Config.Digest.String()} {
			w := registryRequest(handler, method, path, "")
			require.Equal(t, http.StatusMethodNotAllowed, w.Code)
			require.Equal(t, "GET, HEAD", w.Header().Get("Allow"))
			require.Contains(t, w.Body.String(), `"code":"UNSUPPORTED"`)
		}
	}
}

func TestRegistryConcurrentRanges(t *testing.T) {
	img, err := newImage(t.Context(), testImageOptions())
	require.NoError(t, err)

	desc := img.Layers[0]
	data := readImageBlob(t, img, desc)
	server := httptest.NewServer(img.handler())
	t.Cleanup(server.Close)
	url := server.URL + "/v2/" + img.repository + "/blobs/" + desc.Digest.String()

	var wg sync.WaitGroup
	for index := range 16 {
		wg.Go(func() {
			start := 500 + index*7

			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
			if err != nil {
				t.Error(err)
				return
			}

			r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+63))

			response, err := server.Client().Do(r)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()

			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusPartialContent || !bytes.Equal(data[start:start+64], body) {
				t.Errorf("range %d: status=%d error=%v body matches=%t", start, response.StatusCode, err, bytes.Equal(data[start:start+64], body))
			}
		})
	}

	wg.Wait()
}
