// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fakeblob

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()

	s := &Server{Size: 3*patternSize + 11, Modified: time.Unix(0, 0)}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)

	return s, h
}

func do(t *testing.T, method, url string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	require.NoError(t, err)

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp, body
}

func expected(key string, first, n int64) []byte {
	b := make([]byte, n)
	Fill(key, first, b)

	return b
}

func TestHead(t *testing.T) {
	s, h := newServer(t)
	resp, body := do(t, http.MethodHead, h.URL+"/acct/c/k/1", map[string]string{"x-ms-version": APIVersion})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, body)
	require.Equal(t, s.Size, resp.ContentLength)
	require.Equal(t, s.ETag("k/1"), resp.Header.Get("ETag"))
	require.Equal(t, "BlockBlob", resp.Header.Get("x-ms-blob-type"))
	require.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
	require.NotEmpty(t, resp.Header.Get("Last-Modified"))
	require.Equal(t, APIVersion, resp.Header.Get("x-ms-version"))
	require.Equal(t, int64(1), s.Stats.Heads.Load())
}

func TestGetFullAndRanges(t *testing.T) {
	s, h := newServer(t)
	resp, body := do(t, http.MethodGet, h.URL+"/a/c/key", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, expected("key", 0, s.Size), body)

	for _, header := range []string{"x-ms-range", "Range"} {
		resp, body = do(t, http.MethodGet, h.URL+"/a/c/key", map[string]string{header: "bytes=5-1048600", "If-Match": s.ETag("key")})
		require.Equal(t, http.StatusPartialContent, resp.StatusCode)
		require.Equal(t, "bytes 5-1048600/"+strconv.FormatInt(s.Size, 10), resp.Header.Get("Content-Range"))
		require.Equal(t, expected("key", 5, 1048596), body)
	}

	// Ranges past the end are clamped, as Azure does.
	resp, body = do(t, http.MethodGet, h.URL+"/a/c/key", map[string]string{"Range": "bytes=10-99999999999"})
	require.Equal(t, http.StatusPartialContent, resp.StatusCode)
	require.Equal(t, expected("key", 10, s.Size-10), body)
	require.Positive(t, s.Stats.Bytes.Load())
}

func TestEmptyAndUnsatisfiable(t *testing.T) {
	s, h := newServer(t)
	resp, _ := do(t, http.MethodGet, h.URL+"/a/c/size-0/x", map[string]string{"Range": "bytes=0-16777215"})
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, resp.StatusCode)
	require.Equal(t, "bytes */0", resp.Header.Get("Content-Range"))

	resp, _ = do(t, http.MethodHead, h.URL+"/a/c/size-0/x", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int64(0), resp.ContentLength)

	resp, body := do(t, http.MethodGet, h.URL+"/a/c/size-0/x", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, body)

	resp, _ = do(t, http.MethodGet, h.URL+"/a/c/size-5/x", map[string]string{"x-ms-range": "bytes=5-"})
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, resp.StatusCode)
	require.Equal(t, int64(2), s.Stats.Errors.Load())
}

func TestFailures(t *testing.T) {
	_, h := newServer(t)
	resp, _ := do(t, http.MethodGet, h.URL+"/a/c/k", map[string]string{"If-Match": `"other"`})
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)

	resp, _ = do(t, http.MethodHead, h.URL+"/a/c/k", map[string]string{"If-Match": `"other"`})
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)

	resp, _ = do(t, http.MethodGet, h.URL+"/a/c/k", map[string]string{"Range": "bytes=9-3"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, _ = do(t, http.MethodGet, h.URL+"/a/c", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, _ = do(t, http.MethodPut, h.URL+"/a/c/k", nil)
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)

	resp, body := do(t, http.MethodGet, h.URL+"/a/c/k?versionid=v1", nil)
	require.Equal(t, "v1", resp.Header.Get("x-ms-version-id"))
	require.NotEmpty(t, body)

	resp, body = do(t, http.MethodGet, h.URL+StatsPath, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), `"errors":5`)
}

func TestVerifier(t *testing.T) {
	data := expected("k", 0, 2*patternSize+3)
	v := NewVerifier("k")

	for rest := data; len(rest) > 0; {
		n := min(len(rest), 777777)
		_, err := v.Write(rest[:n])
		require.NoError(t, err)

		rest = rest[n:]
	}

	require.Equal(t, int64(len(data)), v.Bytes())

	bad := NewVerifier("k")
	_, err := bad.Write(expected("other", 0, 100))
	require.Error(t, err)
	require.True(t, bad.Mismatch())
	require.False(t, v.Mismatch())
}

func TestETagDiffersPerKey(t *testing.T) {
	s := &Server{Size: 1}
	require.NotEqual(t, s.ETag("a"), s.ETag("b"))
	require.Equal(t, s.SizeOf("size-42/x"), int64(42))
	require.Equal(t, s.SizeOf("size-x/x"), int64(1))
}
