// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package server implements the Unbounded management dashboard's HTTP surface:
// a server-side-rendered shell plus JSON endpoints, backed by component modules
// discovered from a static registry. It owns navigation, rendering, auth checks,
// and proxying of module data (including live SSE streams).
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"

	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/dashboard/authz"
	"github.com/Azure/unbounded/internal/dashboard/contract"
	"github.com/Azure/unbounded/internal/dashboard/moduleclient"
	"github.com/Azure/unbounded/internal/dashboard/registry"
)

// module pairs a registry entry with its HTTP client.
type module struct {
	id     string
	client *moduleclient.Client
}

// Server is the dashboard HTTP application.
type Server struct {
	modules  []module
	byID     map[string]module
	auth     authz.Authorizer
	renderer *renderer
	static   http.Handler
}

// Options configures a Server.
type Options struct {
	Registry   *registry.Config
	Authorizer authz.Authorizer
	// HTTPClient is used for all module requests; nil uses a default client.
	HTTPClient *http.Client
}

// New builds a Server from the given options.
func New(opts Options) (*Server, error) {
	if opts.Registry == nil {
		return nil, fmt.Errorf("registry is required")
	}

	if opts.Authorizer == nil {
		return nil, fmt.Errorf("authorizer is required")
	}

	r, err := newRenderer()
	if err != nil {
		return nil, err
	}

	static, err := staticHandler()
	if err != nil {
		return nil, err
	}

	var httpClient *http.Client
	if opts.HTTPClient != nil {
		httpClient = opts.HTTPClient
	}

	s := &Server{
		byID:     make(map[string]module, len(opts.Registry.Modules)),
		auth:     opts.Authorizer,
		renderer: r,
		static:   static,
	}

	for _, m := range opts.Registry.Modules {
		mod := module{id: m.ID, client: moduleclient.New(m.BaseURL, httpClient)}
		s.modules = append(s.modules, mod)
		s.byID[m.ID] = mod
	}

	return s, nil
}

// Handler returns the mux serving all dashboard routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", s.static)

	mux.HandleFunc("GET /healthz", writeOK)
	mux.HandleFunc("GET /readyz", writeOK)

	// HTML surfaces.
	mux.HandleFunc("GET /{$}", s.handleOverview)
	mux.HandleFunc("GET /modules", s.handleModules)
	mux.HandleFunc("GET /modules/{id}", s.handleModule)
	mux.HandleFunc("GET /modules/{id}/panels/{key}", s.handlePanel)
	mux.HandleFunc("GET /modules/{id}/resources/{kind}", s.handleResources)
	mux.HandleFunc("GET /modules/{id}/resources/{kind}/{name}", s.handleResourceDetail)
	mux.HandleFunc("GET /modules/{id}/stream", s.handleStream)
	mux.HandleFunc("POST /modules/{id}/actions/{action}", s.handleAction)

	// JSON surfaces.
	mux.HandleFunc("GET /api/dashboard/v1/modules", s.handleAPIModules)
	mux.HandleFunc("GET /api/dashboard/v1/modules/{id}/summary", s.handleAPISummary)
	mux.HandleFunc("GET /api/dashboard/v1/modules/{id}/overview", s.handleAPIOverview)
	mux.HandleFunc("GET /api/dashboard/v1/modules/{id}/graph", s.handleAPIGraph)
	mux.HandleFunc("GET /api/dashboard/v1/modules/{id}/matrix", s.handleAPIMatrix)
	mux.HandleFunc("GET /api/dashboard/v1/modules/{id}/resources/{kind}", s.handleAPIResources)
	mux.HandleFunc("GET /api/dashboard/v1/modules/{id}/resources/{kind}/{name}", s.handleAPIResourceDetail)

	// Graph data for the graph primitive (HTML clients consume this via JS).
	mux.HandleFunc("GET /modules/{id}/graph", s.handleAPIGraph)

	return mux
}

// nav builds the left-nav module list, marking activeID active.
func (s *Server) nav(ctx context.Context, activeID string) []navItem {
	items := make([]navItem, 0, len(s.modules))

	for _, m := range s.modules {
		title := m.id

		if mf, err := m.client.Manifest(ctx); err == nil {
			title = mf.Title
		}

		items = append(items, navItem{
			Title:  title,
			Href:   "/modules/" + m.id,
			Active: m.id == activeID,
		})
	}

	return items
}

// authorizeManifest checks every permission a module requires. A module with no
// declared permissions is viewable by any authenticated subject.
func (s *Server) authorizeManifest(ctx context.Context, sub authz.Subject, mf *contract.Manifest) bool {
	if mf == nil || len(mf.RequiredPermissions) == 0 {
		return true
	}

	for i := range mf.RequiredPermissions {
		if !s.auth.Allowed(ctx, sub, &mf.RequiredPermissions[i]) {
			return false
		}
	}

	return true
}

// --- HTML handlers -------------------------------------------------------

// moduleCard is the overview/modules view model for a single module.
type moduleCard struct {
	ID           string
	Title        string
	Description  string
	Capabilities []contract.Capability
	Health       contract.Health
	Message      string
	Error        string
}

func (s *Server) collectCards(ctx context.Context, sub authz.Subject) []moduleCard {
	cards := make([]moduleCard, 0, len(s.modules))

	for _, m := range s.modules {
		card := moduleCard{ID: m.id, Title: m.id, Health: contract.HealthUnknown}

		mf, err := m.client.Manifest(ctx)
		if err != nil {
			card.Error = err.Error()
			cards = append(cards, card)

			continue
		}

		if !s.authorizeManifest(ctx, sub, mf) {
			// Hide modules the caller may not view.
			continue
		}

		card.Title = mf.Title
		card.Description = mf.Description
		card.Capabilities = mf.Capabilities

		if mf.HasCapability(contract.CapabilitySummary) {
			if sum, sErr := m.client.Summary(ctx); sErr == nil {
				card.Health = sum.Health
				card.Message = sum.Message
			} else {
				card.Error = sErr.Error()
			}
		}

		cards = append(cards, card)
	}

	sort.Slice(cards, func(i, j int) bool { return cards[i].ID < cards[j].ID })

	return cards
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	sub, ok := s.auth.Subject(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	cards := s.collectCards(r.Context(), sub)
	s.renderer.render(w, http.StatusOK, "overview", pageData{
		Data: struct{ Modules []moduleCard }{Modules: cards},
	})
}

func (s *Server) handleModules(w http.ResponseWriter, r *http.Request) {
	sub, ok := s.auth.Subject(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	cards := s.collectCards(r.Context(), sub)
	s.renderer.render(w, http.StatusOK, "modules", pageData{
		Data: struct{ Modules []moduleCard }{Modules: cards},
	})
}

func (s *Server) handleModule(w http.ResponseWriter, r *http.Request) {
	sub, mod, mf, ok := s.resolveModule(w, r)
	if !ok {
		return
	}

	if !s.authorizeManifest(r.Context(), sub, mf) {
		s.renderError(w, http.StatusForbidden, "Forbidden", "You do not have permission to view this module.")
		return
	}

	data := struct {
		Manifest *contract.Manifest
		Overview *contract.Overview
		Summary  *contract.Summary
		Stream   string
		Error    string
	}{Manifest: mf}

	switch {
	case mf.HasCapability(contract.CapabilityOverview):
		if ov, err := mod.client.Overview(r.Context()); err == nil {
			data.Overview = ov
		} else {
			data.Error = err.Error()
		}
	case mf.HasCapability(contract.CapabilitySummary):
		if sum, err := mod.client.Summary(r.Context()); err == nil {
			data.Summary = sum
		} else {
			data.Error = err.Error()
		}
	}

	if mf.HasCapability(contract.CapabilityStream) {
		data.Stream = "/modules/" + mod.id + "/stream"
	}

	s.renderer.render(w, http.StatusOK, "module", pageData{
		Nav:  s.nav(r.Context(), mod.id),
		Data: data,
	})
}

// handlePanel renders a single overview panel as an HTML fragment, for htmx
// live refetches driven by SSE stream events. The panel is located by its
// StreamKey within the module's current overview.
func (s *Server) handlePanel(w http.ResponseWriter, r *http.Request) {
	sub, mod, mf, ok := s.resolveModule(w, r)
	if !ok {
		return
	}

	if !s.authorizeManifest(r.Context(), sub, mf) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	ov, err := mod.client.Overview(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	key := r.PathValue("key")

	for _, p := range ov.Panels {
		if p.StreamKey == key {
			s.renderer.renderPanel(w, panelView{ModuleID: mod.id, Panel: p})
			return
		}
	}

	http.NotFound(w, r)
}

func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	sub, mod, mf, ok := s.resolveModule(w, r)
	if !ok {
		return
	}

	if !s.authorizeManifest(r.Context(), sub, mf) {
		s.renderError(w, http.StatusForbidden, "Forbidden", "You do not have permission to view this module.")
		return
	}

	list, err := mod.client.Resources(r.Context(), r.PathValue("kind"))
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "Module unreachable", err.Error())
		return
	}

	s.renderer.render(w, http.StatusOK, "resources", pageData{
		Nav: s.nav(r.Context(), mod.id),
		Data: struct {
			ModuleID    string
			ModuleTitle string
			List        *contract.ResourceList
		}{ModuleID: mod.id, ModuleTitle: mf.Title, List: list},
	})
}

func (s *Server) handleResourceDetail(w http.ResponseWriter, r *http.Request) {
	sub, mod, mf, ok := s.resolveModule(w, r)
	if !ok {
		return
	}

	if !s.authorizeManifest(r.Context(), sub, mf) {
		s.renderError(w, http.StatusForbidden, "Forbidden", "You do not have permission to view this module.")
		return
	}

	detail, err := mod.client.ResourceDetail(r.Context(), r.PathValue("kind"), r.PathValue("name"))
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "Module unreachable", err.Error())
		return
	}

	s.renderer.render(w, http.StatusOK, "detail", pageData{
		Nav: s.nav(r.Context(), mod.id),
		Data: struct {
			ModuleID    string
			ModuleTitle string
			Detail      *contract.ResourceDetail
		}{ModuleID: mod.id, ModuleTitle: mf.Title, Detail: detail},
	})
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	sub, mod, mf, ok := s.resolveModule(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		s.renderError(w, http.StatusBadRequest, "Bad request", err.Error())
		return
	}

	actionID := r.PathValue("action")

	if perm := s.actionPermission(mf, actionID); perm != nil && !s.auth.Allowed(r.Context(), sub, perm) {
		s.renderError(w, http.StatusForbidden, "Forbidden", "You do not have permission to perform this action.")
		return
	}

	params := make(map[string]string)

	for key := range r.PostForm {
		if key == "_return" {
			continue
		}

		params[key] = r.PostForm.Get(key)
	}

	if _, err := mod.client.Invoke(r.Context(), actionID, params); err != nil {
		s.renderError(w, http.StatusBadGateway, "Action failed", err.Error())
		return
	}

	dest := r.PostForm.Get("_return")
	if dest == "" {
		dest = "/modules/" + mod.id
	}

	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// actionPermission looks up the declared permission for an action across the
// manifest's resource detail actions. The manifest does not carry actions
// directly in the prototype, so a nil result means "no declared permission".
func (s *Server) actionPermission(_ *contract.Manifest, _ string) *contract.Permission {
	return nil
}

// handleStream proxies a module's server-sent event stream to the browser.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	mod, ok := s.byID[r.PathValue("id")]
	if !ok {
		http.NotFound(w, r)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, mod.client.StreamURL(), nil)
	if err != nil {
		http.Error(w, "bad stream request", http.StatusInternalServerError)
		return
	}

	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "module stream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close() //nolint:errcheck

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	buf := make([]byte, 4096)

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				return
			}

			flusher.Flush()
		}

		if readErr != nil {
			if readErr != io.EOF {
				klog.V(4).Infof("dashboard: stream read from module %q: %v", mod.id, readErr)
			}

			return
		}
	}
}

// --- JSON handlers -------------------------------------------------------

func (s *Server) handleAPIModules(w http.ResponseWriter, r *http.Request) {
	manifests := make([]*contract.Manifest, 0, len(s.modules))

	for _, m := range s.modules {
		if mf, err := m.client.Manifest(r.Context()); err == nil {
			manifests = append(manifests, mf)
		}
	}

	writeJSON(w, manifests)
}

func (s *Server) handleAPISummary(w http.ResponseWriter, r *http.Request) {
	mod, ok := s.byID[r.PathValue("id")]
	if !ok {
		http.NotFound(w, r)
		return
	}

	sum, err := mod.client.Summary(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, sum)
}

func (s *Server) handleAPIOverview(w http.ResponseWriter, r *http.Request) {
	mod, ok := s.byID[r.PathValue("id")]
	if !ok {
		http.NotFound(w, r)
		return
	}

	ov, err := mod.client.Overview(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, ov)
}

func (s *Server) handleAPIGraph(w http.ResponseWriter, r *http.Request) {
	mod, ok := s.byID[r.PathValue("id")]
	if !ok {
		http.NotFound(w, r)
		return
	}

	g, err := mod.client.Graph(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, g)
}

func (s *Server) handleAPIMatrix(w http.ResponseWriter, r *http.Request) {
	mod, ok := s.byID[r.PathValue("id")]
	if !ok {
		http.NotFound(w, r)
		return
	}

	m, err := mod.client.Matrix(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, m)
}

func (s *Server) handleAPIResources(w http.ResponseWriter, r *http.Request) {
	mod, ok := s.byID[r.PathValue("id")]
	if !ok {
		http.NotFound(w, r)
		return
	}

	list, err := mod.client.Resources(r.Context(), r.PathValue("kind"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, list)
}

func (s *Server) handleAPIResourceDetail(w http.ResponseWriter, r *http.Request) {
	mod, ok := s.byID[r.PathValue("id")]
	if !ok {
		http.NotFound(w, r)
		return
	}

	detail, err := mod.client.ResourceDetail(r.Context(), r.PathValue("kind"), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, detail)
}

// --- helpers -------------------------------------------------------------

// resolveModule extracts the subject and module for a request, fetching the
// module manifest. It writes the appropriate error response and returns ok=false
// when the request cannot proceed.
func (s *Server) resolveModule(w http.ResponseWriter, r *http.Request) (authz.Subject, module, *contract.Manifest, bool) {
	sub, authed := s.auth.Subject(r)
	if !authed {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return authz.Subject{}, module{}, nil, false
	}

	mod, ok := s.byID[r.PathValue("id")]
	if !ok {
		s.renderError(w, http.StatusNotFound, "Not found", "No such module.")
		return authz.Subject{}, module{}, nil, false
	}

	mf, err := mod.client.Manifest(r.Context())
	if err != nil {
		s.renderError(w, http.StatusBadGateway, "Module unreachable", err.Error())
		return authz.Subject{}, module{}, nil, false
	}

	return sub, mod, mf, true
}

func (s *Server) renderError(w http.ResponseWriter, status int, title, message string) {
	s.renderer.render(w, status, "error", pageData{
		Data: struct {
			Title   string
			Message string
		}{Title: title, Message: message},
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(v); err != nil {
		klog.V(4).Infof("dashboard: encoding JSON response: %v", err)
	}
}

// writeOK serves a trivial health/readiness probe response.
func writeOK(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write([]byte("ok")); err != nil {
		klog.V(4).Infof("dashboard: writing probe response: %v", err)
	}
}
