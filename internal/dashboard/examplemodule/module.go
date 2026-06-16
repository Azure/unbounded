// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package examplemodule implements a self-contained Unbounded dashboard module
// used to exercise and demonstrate the dashboard mechanics. It depends on
// nothing outside the contract package and keeps all state in memory, so it can
// be deployed alongside the dashboard to inspect every surface: summary,
// resources, details, actions, and a live SSE stream.
package examplemodule

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/dashboard/contract"
)

// widget is one in-memory example resource.
type widget struct {
	name    string
	region  string
	healthy bool
	weight  int
}

// Module is the example module's in-memory state and HTTP surface.
type Module struct {
	mu      sync.Mutex
	widgets map[string]*widget
	// subscribers receive a signal whenever state changes, used to push SSE
	// summary updates.
	subscribers map[chan struct{}]struct{}
}

// New returns an example Module seeded with a few widgets.
func New() *Module {
	m := &Module{
		widgets:     make(map[string]*widget),
		subscribers: make(map[chan struct{}]struct{}),
	}

	for _, w := range []*widget{
		{name: "alpha", region: "us-east", healthy: true, weight: 10},
		{name: "bravo", region: "us-west", healthy: true, weight: 20},
		{name: "charlie", region: "eu-central", healthy: false, weight: 5},
	} {
		m.widgets[w.name] = w
	}

	return m
}

// Routes registers the module's contract endpoints under basePath (e.g.
// "/dashboard/v1") on mux.
func (m *Module) Routes(mux *http.ServeMux, basePath string) {
	mux.HandleFunc("GET "+basePath+"/manifest", m.handleManifest)
	mux.HandleFunc("GET "+basePath+"/summary", m.handleSummary)
	mux.HandleFunc("GET "+basePath+"/resources/widgets", m.handleWidgets)
	mux.HandleFunc("GET "+basePath+"/resources/widgets/{name}", m.handleWidgetDetail)
	mux.HandleFunc("POST "+basePath+"/actions/toggle-health", m.handleToggleHealth)
	mux.HandleFunc("POST "+basePath+"/actions/set-weight", m.handleSetWeight)
	mux.HandleFunc("GET "+basePath+"/stream", m.handleStream)
}

func (m *Module) handleManifest(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, contract.Manifest{
		ID:          "example",
		Title:       "Example",
		Description: "A demonstration module exercising the dashboard contract.",
		Capabilities: []contract.Capability{
			contract.CapabilitySummary,
			contract.CapabilityResources,
			contract.CapabilityDetails,
			contract.CapabilityActions,
			contract.CapabilityStream,
		},
		ResourceKinds: []contract.ResourceKind{
			{Kind: "widgets", Title: "Widgets", Singular: "Widget"},
		},
	})
}

func (m *Module) handleSummary(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, m.summary())
}

func (m *Module) summary() contract.Summary {
	m.mu.Lock()
	defer m.mu.Unlock()

	var healthy, unhealthy int

	for _, wg := range m.widgets {
		if wg.healthy {
			healthy++
		} else {
			unhealthy++
		}
	}

	health := contract.HealthOK
	message := "All widgets healthy."

	var alerts []contract.Alert

	if unhealthy > 0 {
		health = contract.HealthWarning
		message = fmt.Sprintf("%d of %d widgets unhealthy.", unhealthy, len(m.widgets))

		for _, wg := range m.sortedWidgetsLocked() {
			if !wg.healthy {
				alerts = append(alerts, contract.Alert{
					Health: contract.HealthWarning,
					Title:  fmt.Sprintf("Widget %q is unhealthy", wg.name),
					Detail: fmt.Sprintf("region %s", wg.region),
					Source: "example",
				})
			}
		}
	}

	return contract.Summary{
		Health:  health,
		Message: message,
		Metrics: []contract.Metric{
			{Label: "Widgets", Value: fmt.Sprintf("%d", len(m.widgets))},
			{Label: "Healthy", Value: fmt.Sprintf("%d", healthy), Health: contract.HealthOK},
			{Label: "Unhealthy", Value: fmt.Sprintf("%d", unhealthy), Health: healthOf(unhealthy == 0)},
		},
		Alerts: alerts,
	}
}

func (m *Module) handleWidgets(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	list := contract.ResourceList{
		Kind:  "widgets",
		Title: "Widgets",
		Columns: []contract.Column{
			{Key: "name", Title: "Name"},
			{Key: "region", Title: "Region"},
			{Key: "weight", Title: "Weight"},
		},
	}

	for _, wg := range m.sortedWidgetsLocked() {
		list.Rows = append(list.Rows, contract.ResourceRow{
			Name:   wg.name,
			Health: healthOf(wg.healthy),
			Cells: map[string]string{
				"name":   wg.name,
				"region": wg.region,
				"weight": fmt.Sprintf("%d", wg.weight),
			},
		})
	}

	writeJSON(w, list)
}

func (m *Module) handleWidgetDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	m.mu.Lock()
	wg, ok := m.widgets[name]
	m.mu.Unlock()

	if !ok {
		http.Error(w, "widget not found", http.StatusNotFound)
		return
	}

	writeJSON(w, contract.ResourceDetail{
		Kind:   "widgets",
		Name:   wg.name,
		Title:  "Widget " + wg.name,
		Health: healthOf(wg.healthy),
		Sections: []contract.DetailSection{
			{
				Title: "Configuration",
				Fields: []contract.DetailField{
					{Label: "Name", Value: wg.name},
					{Label: "Region", Value: wg.region},
					{Label: "Weight", Value: fmt.Sprintf("%d", wg.weight)},
				},
			},
			{
				Title: "Status",
				Fields: []contract.DetailField{
					{Label: "Health", Value: healthText(wg.healthy), Health: healthOf(wg.healthy)},
				},
			},
		},
		Actions: []contract.ActionRef{
			{
				ID:          "toggle-health",
				Title:       "Toggle Health",
				Description: "Flip this widget's health to exercise summary/stream updates.",
				Fields: []contract.ActionField{
					{Name: "name", Label: "Widget", Type: "text", Required: true, Default: wg.name},
				},
			},
			{
				ID:          "set-weight",
				Title:       "Set Weight",
				Description: "Change this widget's weight.",
				Fields: []contract.ActionField{
					{Name: "name", Label: "Widget", Type: "text", Required: true, Default: wg.name},
					{Name: "weight", Label: "Weight", Type: "number", Required: true, Default: fmt.Sprintf("%d", wg.weight)},
				},
			},
		},
	})
}

func (m *Module) handleToggleHealth(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")

	m.mu.Lock()

	wg, ok := m.widgets[name]
	if ok {
		wg.healthy = !wg.healthy
	}
	m.mu.Unlock()

	if !ok {
		writeJSON(w, contract.ActionResult{Health: contract.HealthError, Message: "no such widget: " + name})
		return
	}

	m.notify()
	writeJSON(w, contract.ActionResult{
		Health:  contract.HealthOK,
		Message: fmt.Sprintf("widget %q is now %s", name, healthText(wg.healthy)),
	})
}

func (m *Module) handleSetWeight(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")

	var weight int
	if _, err := fmt.Sscanf(r.FormValue("weight"), "%d", &weight); err != nil {
		writeJSON(w, contract.ActionResult{Health: contract.HealthError, Message: "invalid weight"})
		return
	}

	m.mu.Lock()

	wg, ok := m.widgets[name]
	if ok {
		wg.weight = weight
	}
	m.mu.Unlock()

	if !ok {
		writeJSON(w, contract.ActionResult{Health: contract.HealthError, Message: "no such widget: " + name})
		return
	}

	m.notify()
	writeJSON(w, contract.ActionResult{Health: contract.HealthOK, Message: fmt.Sprintf("widget %q weight set to %d", name, weight)})
}

// handleStream emits the current summary immediately and then on every state
// change, as Server-Sent Events.
func (m *Module) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := m.subscribe()
	defer m.unsubscribe(ch)

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	m.sendSummaryEvent(w, flusher)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			m.sendSummaryEvent(w, flusher)
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n") //nolint:errcheck // best-effort SSE keepalive
			flusher.Flush()
		}
	}
}

func (m *Module) sendSummaryEvent(w http.ResponseWriter, flusher http.Flusher) {
	summary := m.summary()

	payload, err := json.Marshal(contract.StreamEvent{Surface: "summary", Summary: &summary})
	if err != nil {
		return
	}

	fmt.Fprintf(w, "event: summary\ndata: %s\n\n", payload) //nolint:errcheck // best-effort SSE write
	flusher.Flush()
}

func (m *Module) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)

	m.mu.Lock()
	m.subscribers[ch] = struct{}{}
	m.mu.Unlock()

	return ch
}

func (m *Module) unsubscribe(ch chan struct{}) {
	m.mu.Lock()
	delete(m.subscribers, ch)
	m.mu.Unlock()
}

func (m *Module) notify() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for ch := range m.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// sortedWidgetsLocked returns widgets ordered by name. Caller must hold m.mu.
func (m *Module) sortedWidgetsLocked() []*widget {
	out := make([]*widget, 0, len(m.widgets))
	for _, wg := range m.widgets {
		out = append(out, wg)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })

	return out
}

func healthOf(ok bool) contract.Health {
	if ok {
		return contract.HealthOK
	}

	return contract.HealthError
}

func healthText(ok bool) string {
	if ok {
		return "healthy"
	}

	return "unhealthy"
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck // best-effort response write
}
