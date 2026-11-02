// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"log/slog"
	"net/http"
)

// Registry answers anonymous registry-auth probes, but never serves image
// content. A peer-only pull cannot succeed through origin fallback.
func Registry(logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
			logger.Info("fixture registry auth probe", "method", r.Method)
			w.Header().Set("Content-Type", "application/json")

			if r.Method == http.MethodGet {
				if _, err := w.Write([]byte(`{}`)); err != nil {
					logger.Error("write registry probe response", "err", err)
				}
			}

			return
		}

		logger.Info("fixture registry content denied", "method", r.Method, "path", r.URL.Path)
		http.NotFound(w, r)
	})
}
