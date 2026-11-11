// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

//go:embed web/*.html web/*.css web/*.js
var assets embed.FS

type Config struct {
	Client          kubernetes.Interface
	Catalog         ImageCatalog
	Registry        string
	Namespace       string
	NodePool        string
	Architecture    string
	GantryNamespace string
	DirectAgents    bool
	Interval        time.Duration
	Logger          *slog.Logger
	Builder         *ImageBuildConfig
	Reclaimer       *ReclaimConfig
}

type snapshotCache struct {
	mu          sync.RWMutex
	collectMu   sync.Mutex
	snapshot    Snapshot
	at          time.Time
	usedAt      time.Time
	subscribers int
	ctx         context.Context
	cancel      context.CancelFunc
	err         error
}

type Server struct {
	cfg              Config
	catalogMu        sync.Mutex
	catalog          Catalog
	catalogAt        time.Time
	mu               sync.Mutex
	snapshots        map[string]*snapshotCache
	http             *http.Client
	collect          chan struct{}
	buildMu          sync.Mutex
	activityMu       sync.RWMutex
	agentRoster      map[string]string
	agentActivity    map[string]agentActivity
	activityRevision uint64
}

func New(cfg Config) *Server {
	if cfg.Interval < 250*time.Millisecond {
		cfg.Interval = time.Second
	}

	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return &Server{
		cfg: cfg, snapshots: map[string]*snapshotCache{}, collect: make(chan struct{}, collectionConcurrency),
		http: &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
			Proxy:        http.ProxyFromEnvironment,
			DialContext:  (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns: 4096, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second,
		}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, _ *http.Request) {
		s.write(w, http.StatusOK, map[string]any{
			"registry": s.cfg.Registry, "namespace": s.cfg.Namespace,
			"nodePool": s.cfg.NodePool, "architecture": s.cfg.Architecture,
			"generation": map[string]any{"enabled": s.cfg.Builder != nil, "maxLayers": MaxBuildLayers, "maxTotalMB": MaxBuildMB},
			"reclaim":    map[string]any{"enabled": s.cfg.Reclaimer != nil},
		})
	})
	mux.HandleFunc("POST /api/builds", s.createBuild)
	mux.HandleFunc("GET /api/builds", s.builds)
	mux.HandleFunc("GET /api/builds/{name}", s.build)
	mux.HandleFunc("POST /api/reclaim", s.createReclaim)
	mux.HandleFunc("GET /api/reclaim", s.reclaimStatus)
	mux.HandleFunc("GET /api/images", s.images)
	mux.HandleFunc("GET /api/deployments", s.deployments)
	mux.HandleFunc("POST /api/deployments", s.deploy)
	mux.HandleFunc("GET /api/deployments/{id}/events", s.events)
	mux.HandleFunc("GET /api/deployments/{id}/nodes/{node}", s.node)
	mux.HandleFunc("GET /api/deployments/{id}/stream", s.stream)

	web, err := fs.Sub(assets, "web")
	if err != nil {
		panic(err)
	}

	mux.Handle("GET /", http.FileServer(http.FS(web)))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}

		ip := net.ParseIP(strings.Trim(host, "[]"))
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			s.fail(w, http.StatusForbidden, errors.New("this UI accepts loopback hosts only"))
			return
		}

		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || u.Scheme != "http" {
				s.fail(w, http.StatusForbidden, errors.New("cross-origin requests are not allowed"))
				return
			}
		}

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(value); err != nil {
		s.cfg.Logger.Debug("UI response interrupted", slog.Any("error", err))
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, err error) {
	s.cfg.Logger.Warn("Gantry UI request failed", slog.Int("status", status), slog.Any("error", err))
	s.write(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) loadCatalog(ctx context.Context, refresh bool) (Catalog, error) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()

	if !refresh && time.Since(s.catalogAt) < time.Minute {
		return s.catalog, nil
	}

	catalog, err := s.cfg.Catalog.Catalog(ctx)
	if err != nil {
		return Catalog{}, err
	}

	s.catalog, s.catalogAt = catalog, time.Now()

	return catalog, nil
}

func (s *Server) images(w http.ResponseWriter, r *http.Request) {
	catalog, err := s.loadCatalog(r.Context(), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	s.write(w, http.StatusOK, catalog)
}

func (s *Server) deployments(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Client.AppsV1().DaemonSets(s.cfg.Namespace).List(r.Context(), metav1.ListOptions{
		LabelSelector: managerLabel + "=gantry-ui",
	})
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	result := []Deployment{}

	for _, ds := range list.Items {
		deployment, err := describeDeployment(&ds)
		if err != nil {
			s.fail(w, http.StatusConflict, err)
			return
		}

		result = append(result, deployment)
	}

	s.write(w, http.StatusOK, result)
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()

	if active, err := s.reclaimActive(r.Context()); err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	} else if active {
		s.fail(w, http.StatusConflict, errors.New("wait for disk reclamation before starting demos"))
		return
	}

	if r.Header.Get("Content-Type") != "application/json" {
		s.fail(w, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
		return
	}

	var request struct {
		ImageID string `json:"imageId"`
	}

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&request); err != nil {
		s.fail(w, http.StatusBadRequest, errors.New("invalid deployment request"))
		return
	}

	catalog, err := s.loadCatalog(r.Context(), false)
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	index := slices.IndexFunc(catalog.Images, func(image Image) bool { return image.ID == request.ImageID })
	if index < 0 {
		s.fail(w, http.StatusBadRequest, errors.New("image is not in the current ACR catalog"))
		return
	}

	image := catalog.Images[index]
	if image.Architecture != s.cfg.Architecture || image.OS != "linux" {
		s.fail(w, http.StatusBadRequest, errors.New("image does not match the configured Linux worker architecture"))
		return
	}

	ds, err := buildDeployment(s.cfg.Namespace, s.cfg.NodePool, image)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}

	client := s.cfg.Client.AppsV1().DaemonSets(s.cfg.Namespace)
	current, err := client.Get(r.Context(), ds.Name, metav1.GetOptions{})
	status := http.StatusOK

	if apierrors.IsNotFound(err) {
		current, err = client.Create(r.Context(), ds, metav1.CreateOptions{})
		status = http.StatusCreated

		if apierrors.IsAlreadyExists(err) {
			current, err = client.Get(r.Context(), ds.Name, metav1.GetOptions{})
			status = http.StatusOK
		}
	}

	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}

	deployment, err := describeDeployment(current)
	if err != nil || deployment.Image.Reference != image.Reference ||
		current.Spec.Template.Spec.NodeSelector["kubernetes.azure.com/agentpool"] != s.cfg.NodePool {
		s.fail(w, http.StatusConflict, errors.New("an incompatible DaemonSet already occupies this deployment name; it was not changed"))
		return
	}

	s.write(w, status, deployment)
}

var validID = regexp.MustCompile(`^[0-9a-f]{20}$`)

func (s *Server) deployment(ctx context.Context, id string) (Deployment, error) {
	if !validID.MatchString(id) {
		return Deployment{}, errors.New("invalid deployment ID")
	}

	ds, err := s.cfg.Client.AppsV1().DaemonSets(s.cfg.Namespace).Get(ctx, deploymentName(id), metav1.GetOptions{})
	if err != nil {
		return Deployment{}, err
	}

	return describeDeployment(ds)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.deployment(r.Context(), id); err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}

	if _, ok := w.(http.Flusher); !ok {
		s.fail(w, http.StatusInternalServerError, errors.New("streaming is unavailable"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")

	s.streamUpdates(w, r)
}
