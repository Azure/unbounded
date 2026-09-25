// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/Azure/unbounded/internal/gantry/config"
	streamingapi "github.com/Azure/unbounded/internal/gantry/streaming"
)

func newCanonicalOriginResolver(registries []config.UpstreamRegistry) func(streamingapi.OriginURL) (streamingapi.OriginURL, bool) {
	endpoints := make(map[string]*url.URL, len(registries))
	for _, registry := range registries {
		endpoint, err := url.Parse(registry.Endpoint)
		if err != nil || !endpoint.IsAbs() || endpoint.Host == "" {
			continue
		}
		endpoints[strings.ToLower(registry.Name)] = endpoint
		if registry.NSAlias != "" {
			endpoints[strings.ToLower(registry.NSAlias)] = endpoint
		}
	}

	return func(origin streamingapi.OriginURL) (streamingapi.OriginURL, bool) {
		canonical, err := url.Parse(origin.Raw)
		if err != nil {
			return streamingapi.OriginURL{}, false
		}
		endpoint, ok := endpoints[strings.ToLower(canonical.Hostname())]
		if !ok {
			return streamingapi.OriginURL{}, false
		}

		target := *endpoint
		target.Path = strings.TrimRight(endpoint.Path, "/") + canonical.Path
		target.RawPath = ""

		return streamingapi.OriginURL{Raw: target.String(), Digest: origin.Digest}, true
	}
}

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
