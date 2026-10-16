// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

const azureSidecarVersion = "2026-02-06"

func azureSidecarQuery(r *http.Request) (string, sidecarFailure) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", http.StatusBadRequest
	}
	// Validate all values first so malformed input always takes precedence.
	for name, values := range query {
		if name == "" || !utf8.ValidString(name) || len(values) != 1 || values[0] == "" || !utf8.ValidString(values[0]) {
			return "", http.StatusBadRequest
		}

		for _, c := range []byte(values[0]) {
			if c < 0x20 || c == 0x7f {
				return "", http.StatusBadRequest
			}
		}

		if name == "timeout" {
			if n, ok := sidecarRangeNumber(values[0]); !ok || n == 0 {
				return "", http.StatusBadRequest
			}
		}
	}

	for name := range query {
		if name != "versionid" && name != "timeout" {
			return "", http.StatusNotImplemented
		}
	}

	return query.Get("versionid"), 0
}

func azureSidecarSupportedHeaders(h http.Header) bool {
	for name := range h {
		name = strings.ToLower(name)
		switch name {
		case "if-modified-since", "if-unmodified-since", "if-range":
			return false
		case "x-ms-range", "x-ms-version", "x-ms-date", "x-ms-client-request-id", "x-ms-return-client-request-id", "x-ms-useragent":
			continue
		}
		// Lease, encryption, tags, checksums and unknown requirements are not ignored.
		if strings.HasPrefix(name, "x-ms-") {
			return false
		}
	}

	return true
}

func azureSidecarRangeHeaders(r *http.Request) http.Header {
	// Get Blob Properties always reports the full size, even with range headers.
	if r.Method == http.MethodHead {
		return nil
	}

	if values, present := r.Header[http.CanonicalHeaderKey("x-ms-range")]; present {
		return http.Header{"Range": values}
	}

	return r.Header
}

func writeAzureSidecarHeaders(h http.Header, version string) {
	h.Set("x-ms-blob-type", "BlockBlob")
	h.Set("x-ms-version", azureSidecarVersion)

	if version != "" {
		h.Set("x-ms-version-id", version)
	}
}

var azureSidecarCodes = map[sidecarFailure]string{
	http.StatusBadRequest:                   "InvalidInput",
	http.StatusForbidden:                    "AuthorizationFailure",
	http.StatusNotFound:                     "BlobNotFound",
	http.StatusMethodNotAllowed:             "UnsupportedHttpVerb",
	http.StatusPreconditionFailed:           "ConditionNotMet",
	http.StatusRequestedRangeNotSatisfiable: "InvalidRange",
	http.StatusRequestHeaderFieldsTooLarge:  "InvalidInput",
	http.StatusInternalServerError:          "InternalError",
	http.StatusNotImplemented:               "NotImplemented",
	http.StatusBadGateway:                   "BadGateway",
	http.StatusServiceUnavailable:           "ServerBusy",
}

func writeAzureSidecarFailure(w http.ResponseWriter, r *http.Request, failure sidecarFailure) {
	code := azureSidecarCodes[failure]
	w.Header().Set("x-ms-error-code", code)
	failure.writeCode(w, r, code)
}
