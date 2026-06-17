// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/dashboard/contract"
	"github.com/Azure/unbounded/internal/net/authn"
	webhookpkg "github.com/Azure/unbounded/internal/net/webhook"
)

// dashboardModuleBasePath is the root under which the net controller exposes the
// generic dashboard module contract consumed by cmd/dashboard. It lives next to
// the existing /status endpoints and is gated by the same dashboard auth.
const dashboardModuleBasePath = "/dashboard/v1"

// registerDashboardModuleHandlers wires the net module's dashboard-contract
// endpoints onto the shared mux. Every handler is gated by the existing
// dashboard authorization (HMAC viewer token or trusted aggregated request),
// matching the /status/json surface.
func registerDashboardModuleHandlers(
	mux *http.ServeMux,
	health *healthState,
	requireDashboardAuth bool,
	webhookServer *webhookpkg.Server,
	dashAuthorizer *dashboardAuthorizer,
	tokenIssuer *authn.TokenIssuer,
) {
	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !authorizeDashboardOrAggregated(requireDashboardAuth, tokenIssuer, dashAuthorizer, webhookServer, r) {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			next(w, r)
		}
	}

	mux.HandleFunc("GET "+dashboardModuleBasePath+"/manifest", authed(func(w http.ResponseWriter, _ *http.Request) {
		writeModuleJSON(w, netManifest())
	}))

	mux.HandleFunc("GET "+dashboardModuleBasePath+"/summary", authed(func(w http.ResponseWriter, r *http.Request) {
		withStatus(health, w, r, func(status *ClusterStatusResponse) {
			writeModuleJSON(w, netSummary(status))
		})
	}))

	mux.HandleFunc("GET "+dashboardModuleBasePath+"/overview", authed(func(w http.ResponseWriter, r *http.Request) {
		withStatus(health, w, r, func(status *ClusterStatusResponse) {
			writeModuleJSON(w, netOverview(status))
		})
	}))

	mux.HandleFunc("GET "+dashboardModuleBasePath+"/graph", authed(func(w http.ResponseWriter, r *http.Request) {
		withStatus(health, w, r, func(status *ClusterStatusResponse) {
			writeModuleJSON(w, netGraph(status))
		})
	}))

	mux.HandleFunc("GET "+dashboardModuleBasePath+"/matrix", authed(func(w http.ResponseWriter, r *http.Request) {
		withStatus(health, w, r, func(status *ClusterStatusResponse) {
			writeModuleJSON(w, netMatrix(status))
		})
	}))

	mux.HandleFunc("GET "+dashboardModuleBasePath+"/resources/{kind}", authed(func(w http.ResponseWriter, r *http.Request) {
		withStatus(health, w, r, func(status *ClusterStatusResponse) {
			list, ok := netResourceList(status, r.PathValue("kind"))
			if !ok {
				http.Error(w, "unknown resource kind", http.StatusNotFound)
				return
			}

			writeModuleJSON(w, list)
		})
	}))

	mux.HandleFunc("GET "+dashboardModuleBasePath+"/resources/{kind}/{name}", authed(func(w http.ResponseWriter, r *http.Request) {
		withStatus(health, w, r, func(status *ClusterStatusResponse) {
			detail, ok := netResourceDetail(status, r.PathValue("kind"), r.PathValue("name"))
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}

			writeModuleJSON(w, detail)
		})
	}))

	mux.HandleFunc("POST "+dashboardModuleBasePath+"/actions/set-pull-enabled", authed(func(w http.ResponseWriter, r *http.Request) {
		handleSetPullEnabledAction(health, w, r)
	}))

	mux.HandleFunc("GET "+dashboardModuleBasePath+"/stream", authed(func(w http.ResponseWriter, r *http.Request) {
		handleModuleStream(health, w, r)
	}))
}

// withStatus fetches the current cluster status (leader-only) and invokes fn,
// or writes an appropriate error.
func withStatus(health *healthState, w http.ResponseWriter, _ *http.Request, fn func(*ClusterStatusResponse)) {
	if !health.isLeader.Load() {
		http.Error(w, "not the leader", http.StatusServiceUnavailable)
		return
	}

	if health.clusterStatusCache == nil {
		http.Error(w, "status not yet available", http.StatusServiceUnavailable)
		return
	}

	status := health.clusterStatusCache.Get()
	if status == nil {
		http.Error(w, "status not yet available", http.StatusServiceUnavailable)
		return
	}

	fn(status)
}

// handleSetPullEnabledAction toggles the controller's pull-fallback mode. This
// is the stateful action surface: it mutates real controller state and triggers
// a status rebuild so connected dashboards see the change.
func handleSetPullEnabledAction(health *healthState, w http.ResponseWriter, r *http.Request) {
	if !health.isLeader.Load() {
		writeModuleJSON(w, contract.ActionResult{Health: contract.HealthError, Message: "not the leader"})
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	enabled := r.FormValue("enabled") == "true"
	health.pullEnabled.Store(enabled)

	if health.clusterStatusCache != nil {
		health.clusterStatusCache.MarkDirty()
	}

	klog.V(2).Infof("dashboard module: pull-enabled set to %v", enabled)
	writeModuleJSON(w, contract.ActionResult{
		Health:  contract.HealthOK,
		Message: fmt.Sprintf("pull mode %s", map[bool]string{true: "enabled", false: "disabled"}[enabled]),
	})
}

// handleModuleStream emits SSE signal events when the cluster status sequence
// changes. It polls the status cache sequence (decoupled from the WS broadcaster)
// and signals the panels that may have changed, implementing the "signal +
// refetch" live-update model.
func handleModuleStream(health *healthState, w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	if health.clusterStatusCache == nil {
		http.Error(w, "status not yet available", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	lastSeq := health.clusterStatusCache.GetSeq()

	emitModuleEvents(w, flusher)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			seq := health.clusterStatusCache.GetSeq()

			if seq != lastSeq {
				lastSeq = seq

				emitModuleEvents(w, flusher)
			} else {
				fmt.Fprint(w, ": keepalive\n\n") //nolint:errcheck // best-effort SSE keepalive
				flusher.Flush()
			}
		}
	}
}

// emitModuleEvents signals every live panel key. The dashboard refetches each
// panel's HTML on receipt.
func emitModuleEvents(w http.ResponseWriter, flusher http.Flusher) {
	for _, key := range []string{"summary", "graph", "matrix", "nodes"} {
		fmt.Fprintf(w, "event: %s\ndata: {}\n\n", key) //nolint:errcheck // best-effort SSE write
	}

	flusher.Flush()
}

func writeModuleJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(v); err != nil {
		klog.V(4).Infof("dashboard module: json encode failed: %v", err)
	}
}
