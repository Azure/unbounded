// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
)

// cloudInitEvent represents a cloud-init webhook reporting event.
type cloudInitEvent struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	EventType   string  `json:"event_type"`
	Origin      string  `json:"origin"`
	Timestamp   float64 `json:"timestamp"`
	Result      string  `json:"result,omitempty"`
}

type HTTPServer struct {
	BindAddr string
	Port     int
	Client   client.Client
	Mux      *http.ServeMux
	FileResolver
}

func (h *HTTPServer) NeedLeaderElection() bool { return false }

func (h *HTTPServer) Start(ctx context.Context) error {
	mux := h.Mux
	if mux == nil {
		mux = http.NewServeMux()
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok")) //nolint:errcheck // Best-effort health check response.
	})
	mux.HandleFunc("POST /cloudinit/log", h.handleCloudInitLog)

	if h.Client != nil {
		mux.HandleFunc("POST /pxe/disable", h.handleDisablePXE)
		mux.HandleFunc("GET /pxe/disable", h.handleDisablePXE)
	}

	mux.HandleFunc("GET /", h.handleFile)

	addr := fmt.Sprintf("%s:%d", h.BindAddr, h.Port)
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		srv.Close() //nolint:errcheck // Best-effort shutdown of HTTP server.
	}()

	slog.Info("starting HTTP server", "addr", addr)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}

func (h *HTTPServer) handleFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	ip := clientIP(r)
	log := slog.With("proto", "http", "path", path, "ip", ip)

	node, err := h.LookupNodeByIP(r.Context(), ip)
	if err != nil {
		log.Warn("no node for source IP", "err", err)
		http.NotFound(w, r)

		return
	}

	if node.Spec.PXE == nil {
		log.Warn("node has no PXE config", "node", node.Name)
		http.NotFound(w, r)

		return
	}

	resolved, err := h.ResolveFileByPath(r.Context(), path, node, node.Spec.PXE.Image)
	if err != nil {
		if errors.Is(err, ErrNotYetDownloaded) {
			log.Info("file not yet downloaded", "node", node.Name)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "file not yet available, retry later", http.StatusServiceUnavailable)

			return
		}

		log.Warn("resolving file", "node", node.Name, "err", err)
		http.NotFound(w, r)

		return
	}

	if resolved.DiskPath != "" {
		log.Info("serving cached file", "node", node.Name)
		http.ServeFile(w, r, resolved.DiskPath)

		return
	}

	log.Info("serving file", "node", node.Name, "size", len(resolved.Data))
	w.Header().Set("Content-Type", resolved.ContentType)
	w.Write(resolved.Data) //nolint:errcheck // Best-effort HTTP response write.
}

func (h *HTTPServer) handleCloudInitLog(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		slog.Error("reading cloud-init log body", "ip", ip, "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	var ev cloudInitEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		slog.Warn("cloud-init log: unparseable event", "ip", ip, "body", string(body))
		w.WriteHeader(http.StatusOK)

		return
	}

	log := slog.With("handler", "cloudinit-log", "ip", ip, "stage", ev.Name)

	switch ev.EventType {
	case "start":
		log.Info("cloud-init stage started", "description", ev.Description)
	case "finish":
		log.Info("cloud-init stage finished", "description", ev.Description, "result", ev.Result)
	default:
		log.Info("cloud-init event", "type", ev.EventType, "description", ev.Description)
	}

	h.updateCloudInitCondition(r.Context(), log, ip, &ev)

	w.WriteHeader(http.StatusOK)
}

// cloudInitLastStage is the final cloud-init stage. When this stage
// finishes successfully the CloudInitDone condition transitions to True.
const cloudInitLastStage = "modules-final"

// updateCloudInitCondition sets the CloudInitDone condition on the Machine
// that matches the request source IP. The condition reflects the
// cloud-init lifecycle reported through webhook events:
func (h *HTTPServer) updateCloudInitCondition(ctx context.Context, log *slog.Logger, ip string, ev *cloudInitEvent) {
	if h.Client == nil {
		return
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := h.LookupNodeByIP(ctx, ip)
		if err != nil {
			return err
		}

		cond := buildCloudInitCondition(ev, node.Generation)
		if cond == nil {
			return nil
		}

		// TODO(jordan): Rename all `node` references in this file to `machine`
		meta.SetStatusCondition(&node.Status.Conditions, *cond)

		return h.Client.Status().Update(ctx, node)
	})
	if err != nil {
		log.Error("cloud-init condition: updating Machine status", "ip", ip, "err", err)
	}
}

const maxConditionMessageLen = 1024

// buildCloudInitCondition returns the metav1.Condition to set for a
// cloud-init webhook event, or nil if no update is needed.
func buildCloudInitCondition(ev *cloudInitEvent, generation int64) *metav1.Condition {
	switch ev.EventType {
	case "start":
		return &metav1.Condition{
			Type:               v1alpha3.MachineConditionCloudInitDone,
			Status:             metav1.ConditionFalse,
			Reason:             "Running",
			Message:            fmt.Sprintf("stage %q started: %s", ev.Name, ev.Description),
			ObservedGeneration: generation,
		}

	case "finish":
		if !strings.EqualFold(ev.Result, "SUCCESS") {
			msg := fmt.Sprintf("stage %q failed with result %q: %s", ev.Name, ev.Result, ev.Description)
			if len(msg) > maxConditionMessageLen {
				msg = msg[:maxConditionMessageLen-3] + "..."
			}

			return &metav1.Condition{
				Type:               v1alpha3.MachineConditionCloudInitDone,
				Status:             metav1.ConditionFalse,
				Reason:             "Failed",
				Message:            msg,
				ObservedGeneration: generation,
			}
		}

		if ev.Name == cloudInitLastStage {
			return &metav1.Condition{
				Type:               v1alpha3.MachineConditionCloudInitDone,
				Status:             metav1.ConditionTrue,
				Reason:             "Succeeded",
				Message:            "cloud-init completed successfully",
				ObservedGeneration: generation,
			}
		}

		// An earlier stage succeeded - cloud-init is still running.
		return &metav1.Condition{
			Type:               v1alpha3.MachineConditionCloudInitDone,
			Status:             metav1.ConditionFalse,
			Reason:             "Running",
			Message:            fmt.Sprintf("stage %q finished successfully, waiting for remaining stages", ev.Name),
			ObservedGeneration: generation,
		}

	default:
		return nil
	}
}

func (h *HTTPServer) handleDisablePXE(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	log := slog.With("handler", "pxe-disable", "ip", ip)

	node, err := h.LookupNodeByIP(r.Context(), ip)
	if err != nil {
		log.Warn("no node for source IP", "err", err)
		http.NotFound(w, r)

		return
	}

	var specRepave, statusRepave int64
	if node.Spec.Operations != nil {
		specRepave = node.Spec.Operations.RepaveCounter
	}

	if node.Status.Operations != nil {
		statusRepave = node.Status.Operations.RepaveCounter
	}

	if specRepave <= statusRepave {
		log.Info("repave already cleared", "node", node.Name)
		w.WriteHeader(http.StatusOK)

		return
	}

	if node.Status.Operations == nil {
		node.Status.Operations = &v1alpha3.OperationsStatus{}
	}

	node.Status.Operations.RepaveCounter = specRepave

	imageName := ""
	if node.Spec.PXE != nil {
		imageName = node.Spec.PXE.Image
	}

	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineConditionRepaved,
		Status:             metav1.ConditionTrue,
		Reason:             "Succeeded",
		Message:            "image=" + imageName,
		ObservedGeneration: node.Generation,
	})

	if err := h.Client.Status().Update(r.Context(), node); err != nil {
		log.Error("updating Machine status", "node", node.Name, "err", err)
		http.Error(w, "failed to disable PXE", http.StatusInternalServerError)

		return
	}

	log.Info("repave cleared", "node", node.Name)
	w.WriteHeader(http.StatusOK)
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.SplitN(fwd, ",", 2)
		return strings.TrimSpace(parts[0])
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}
