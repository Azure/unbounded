// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const copyBufferBytes = 64 * 1024

type pathClass string

const (
	pathClassBlob             pathClass = "blob"
	pathClassManifestByDigest pathClass = "manifest_by_digest"
	pathClassManifestByTag    pathClass = "manifest_by_tag"
	pathClassPing             pathClass = "ping"
	pathClassOther            pathClass = "other"
)

var allPathClasses = []pathClass{
	pathClassBlob,
	pathClassManifestByDigest,
	pathClassManifestByTag,
	pathClassPing,
	pathClassOther,
}

type clientClass string

const (
	clientClassGantry     clientClass = "gantry"
	clientClassContainerd clientClass = "containerd"
	clientClassOther      clientClass = "other"
)

var allClientClasses = []clientClass{
	clientClassGantry,
	clientClassContainerd,
	clientClassOther,
}

func classifyClient(userAgent string) clientClass {
	lower := strings.ToLower(userAgent)
	switch {
	case lower == "":
		return clientClassOther
	case strings.Contains(lower, "containerd"):
		return clientClassContainerd
	case strings.Contains(lower, "gantry"), strings.HasPrefix(lower, "go-http-client"):
		return clientClassGantry
	default:
		return clientClassOther
	}
}

func proxyHandler(cfg *config, cache *tokenCache, observer *observer, client *http.Client) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		path, digest := classifyRequest(r.URL.EscapedPath())
		caller := classifyClient(r.Header.Get("User-Agent"))
		started := time.Now()
		attribution := observer.begin(r.Method, path, caller)

		if shouldThrottle(cfg, observer, path) {
			observer.recordSyntheticThrottle(attribution, "blob_inflight")

			body := "synthetic throttle\n"

			w.Header().Set("Retry-After", strconv.Itoa(cfg.throttleRetryAfterSec))
			w.WriteHeader(http.StatusTooManyRequests)
			written, writeErr := io.WriteString(w, body)

			status := strconv.Itoa(http.StatusTooManyRequests)
			if writeErr != nil {
				status = "client_closed"
			}

			observer.finish(attribution, r.Method, path, caller, digest, status, 0, int64(written), time.Since(started))

			return
		}

		status := "upstream_error"

		var (
			upstreamBytes int64
			clientBytes   int64
		)

		defer func() {
			observer.finish(attribution, r.Method, path, caller, digest, status, upstreamBytes, clientBytes, time.Since(started))
		}()

		r.Header.Del("Authorization")

		target := *cfg.upstream
		target.Path = singleJoiningSlash(cfg.upstream.Path, r.URL.EscapedPath())
		target.RawQuery = r.URL.RawQuery

		response, refreshed, err := doWithAuth(
			r.Context(),
			client,
			cfg,
			cache,
			observer,
			attribution,
			r.Method,
			&target,
			r.Header,
			r.Body,
		)
		if err != nil {
			status = strconv.Itoa(http.StatusBadGateway)
			errorReason := classifyUpstreamError(err)
			observer.recordUpstreamError(attribution, r.Method, path, caller, errorReason)

			slog.Error(
				"proxy upstream request failed",
				"method", r.Method,
				"path", r.URL.Path,
				"phase", attribution.Phase,
				"error_class", errorReason,
				"error", err,
			)
			http.Error(w, "bad gateway", http.StatusBadGateway)

			return
		}

		defer func() {
			if err := response.Body.Close(); err != nil {
				slog.Warn("close upstream response body", "error", err)
			}
		}()

		copyResponseHeaders(w.Header(), response.Header)
		w.WriteHeader(response.StatusCode)
		status = strconv.Itoa(response.StatusCode)

		countedUpstream := &countingReader{reader: response.Body}
		countedClient := &countingWriter{writer: w}
		buffer := make([]byte, copyBufferBytes)
		_, copyErr := io.CopyBuffer(countedClient, countedUpstream, buffer)
		upstreamBytes = countedUpstream.bytes
		clientBytes = countedClient.bytes

		if copyErr != nil {
			status = "client_closed"
		}

		slog.Info(
			"proxied registry request",
			"run_id", attribution.RunID,
			"phase", attribution.Phase,
			"method", r.Method,
			"path", r.URL.Path,
			"path_class", path,
			"status", status,
			"upstream_bytes", upstreamBytes,
			"client_bytes", clientBytes,
			"auth_refreshed", refreshed,
			"latency", time.Since(started).Round(time.Millisecond),
			"copy_error", copyErr,
		)
	})
}

func shouldThrottle(cfg *config, observer *observer, class pathClass) bool {
	return class == pathClassBlob && cfg.throttleBlobInflight > 0 && observer.currentInflight(class) > cfg.throttleBlobInflight
}

type countingReader struct {
	reader io.Reader
	bytes  int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	r.bytes += int64(read)

	return read, err
}

type countingWriter struct {
	writer io.Writer
	bytes  int64
}

func (w *countingWriter) Write(buffer []byte) (int, error) {
	written, err := w.writer.Write(buffer)
	w.bytes += int64(written)

	return written, err
}

func classifyRequest(rawPath string) (pathClass, string) {
	path := rawPath
	if index := strings.IndexByte(path, '?'); index >= 0 {
		path = path[:index]
	}

	if path == "/v2" || path == "/v2/" {
		return pathClassPing, ""
	}

	if !strings.HasPrefix(path, "/v2/") {
		return pathClassOther, ""
	}

	rest := strings.TrimPrefix(path, "/v2/")

	separator, kind := rightmostOCIBoundary(rest)
	if separator <= 0 {
		return pathClassOther, ""
	}

	switch kind {
	case "blobs":
		ref := rest[separator+len("/blobs/"):]
		if ref == "uploads" || strings.HasPrefix(ref, "uploads/") {
			return pathClassOther, ""
		}

		if isDigest(ref) {
			return pathClassBlob, strings.ToLower(ref)
		}
	case "manifests":
		ref := rest[separator+len("/manifests/"):]
		if strings.Contains(ref, "/") || ref == "" {
			return pathClassOther, ""
		}

		if isDigest(ref) {
			return pathClassManifestByDigest, strings.ToLower(ref)
		}

		return pathClassManifestByTag, ""
	}

	return pathClassOther, ""
}

func rightmostOCIBoundary(rest string) (int, string) {
	blobIndex := strings.LastIndex(rest, "/blobs/")

	manifestIndex := strings.LastIndex(rest, "/manifests/")
	if blobIndex > manifestIndex {
		return blobIndex, "blobs"
	}

	if manifestIndex >= 0 {
		return manifestIndex, "manifests"
	}

	return -1, ""
}

var digestPattern = regexp.MustCompile(`(?i)^sha256:[a-f0-9]{64}$`)

func isDigest(ref string) bool {
	return digestPattern.MatchString(ref)
}

func copyForwardedHeaders(destination, source http.Header) {
	for key, values := range source {
		canonical := http.CanonicalHeaderKey(key)
		if hopByHopHeader[canonical] {
			continue
		}

		for _, value := range values {
			destination.Add(canonical, value)
		}
	}
}

func copyResponseHeaders(destination, source http.Header) {
	copyForwardedHeaders(destination, source)
}

var hopByHopHeader = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Host":                true,
	"Authorization":       true,
}

func singleJoiningSlash(left, right string) string {
	leftSlash := strings.HasSuffix(left, "/")

	rightSlash := strings.HasPrefix(right, "/")
	switch {
	case leftSlash && rightSlash:
		return left + right[1:]
	case !leftSlash && !rightSlash:
		return left + "/" + right
	default:
		return left + right
	}
}
