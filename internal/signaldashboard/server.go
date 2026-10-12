// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaldashboard

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type Server struct {
	Collector *Collector
	Logger    *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /api/summary", s.summary)
	mux.HandleFunc("GET /api/runs", s.runs)
	mux.HandleFunc("GET /api/runs/{name}", s.run)
	mux.HandleFunc("GET /api/nodes", s.nodes)

	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}

	mux.Handle("GET /", http.FileServer(http.FS(static)))

	return securityHeaders(mux)
}

func (s *Server) health(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(http.StatusOK)

	if _, err := response.Write([]byte("ok\n")); err != nil {
		s.logger().Error("write health response", "error", err)
	}
}

func (s *Server) ready(response http.ResponseWriter, request *http.Request) {
	snapshot := s.Collector.Collect(request.Context())
	for _, item := range snapshot.errors {
		switch item.Resource {
		case "CertificationRun", "CertificationProfile", "Workflow":
			writeJSON(response, http.StatusServiceUnavailable, map[string]any{
				"ready":  false,
				"errors": snapshot.errors,
			})

			return
		}
	}

	writeJSON(response, http.StatusOK, map[string]any{"ready": true, "lastRefresh": snapshot.refreshed})
}

func (s *Server) summary(response http.ResponseWriter, request *http.Request) {
	snapshot := s.Collector.Collect(request.Context())
	writeJSON(response, http.StatusOK, s.Collector.Summary(snapshot))
}

func (s *Server) runs(response http.ResponseWriter, request *http.Request) {
	snapshot := s.Collector.Collect(request.Context())
	snapshot = filterRuns(snapshot, request)
	offset := queryInt(request, "offset", 0)
	limit := queryInt(request, "limit", defaultPageLimit)
	writeJSON(response, http.StatusOK, s.Collector.Runs(snapshot, offset, limit))
}

func (s *Server) run(response http.ResponseWriter, request *http.Request) {
	name := request.PathValue("name")
	if name == "" || strings.ContainsAny(name, "/\\") {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid run name"})

		return
	}

	snapshot := s.Collector.Collect(request.Context())

	detail, err := s.Collector.Run(request.Context(), snapshot, name)
	if err != nil {
		if IsNotFound(err) {
			writeJSON(response, http.StatusNotFound, map[string]string{"error": "run not found"})

			return
		}

		s.logger().Error("build run detail", "error", err)
		writeJSON(response, http.StatusInternalServerError, map[string]string{"error": "could not build run detail"})

		return
	}

	writeJSON(response, http.StatusOK, detail)
}

func (s *Server) nodes(response http.ResponseWriter, request *http.Request) {
	snapshot := s.Collector.Collect(request.Context())
	writeJSON(response, http.StatusOK, s.Collector.Nodes(snapshot))
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}

	return slog.Default()
}

func filterRuns(snapshot collected, request *http.Request) collected {
	phase := strings.ToLower(request.URL.Query().Get("phase"))
	verdict := strings.ToLower(request.URL.Query().Get("verdict"))
	profile := strings.ToLower(request.URL.Query().Get("profile"))

	hardware := strings.ToLower(request.URL.Query().Get("hardware"))
	view := strings.ToLower(request.URL.Query().Get("view"))
	if phase == "" && verdict == "" && profile == "" && hardware == "" && view == "" {
		return snapshot
	}

	filtered := snapshot

	filtered.runs = nil
	for i := range snapshot.runs {
		run := &snapshot.runs[i]
		if phase != "" && strings.ToLower(string(run.Status.Phase)) != phase {
			continue
		}

		if verdict != "" && strings.ToLower(run.Status.Verdict) != verdict {
			continue
		}

		if profile != "" && !strings.Contains(strings.ToLower(run.Spec.Profile.Name), profile) {
			continue
		}

		if view != "" {
			classification, _ := runClassification(run, snapshot.runs)
			if strings.ToLower(classification) != view {
				continue
			}
		}

		if hardware != "" {
			matched := false

			for _, participant := range run.Spec.Participants {
				node := snapshot.nodes[participant.NodeName]
				if node != nil && strings.Contains(strings.ToLower(hardwareClass(node)), hardware) {
					matched = true
					break
				}
			}

			if !matched {
				continue
			}
		}

		filtered.runs = append(filtered.runs, *run)
	}

	return filtered
}

func queryInt(request *http.Request, name string, fallback int) int {
	value, err := strconv.Atoi(request.URL.Query().Get(name))
	if err != nil {
		return fallback
	}

	return value
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)

	if err := json.NewEncoder(response).Encode(value); err != nil && !errors.Is(err, http.ErrHandlerTimeout) {
		slog.Error("write dashboard response", "error", err)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		response.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		response.Header().Set("Cache-Control", "no-store")
		response.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(response, request)
	})
}

func NewHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}
