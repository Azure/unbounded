// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"strings"

	streamingapi "github.com/Azure/unbounded/internal/gantry/streaming"
)

// routeNodeLocalHandlers dispatches /blobs/ before the mirror's ServeMux can
// clean repeated slashes in the embedded origin URL.
func routeNodeLocalHandlers(streamingHandler, mirrorHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.RequestURI, streamingapi.HandlerPrefix) || r.URL.Path == streamingapi.ReadinessPath {
			streamingHandler.ServeHTTP(w, r)

			return
		}

		mirrorHandler.ServeHTTP(w, r)
	})
}
