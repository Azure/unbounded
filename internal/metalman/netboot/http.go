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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machinestatus"
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
	Recorder events.EventRecorder
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

	h.setPXEBootCondition(r.Context(), node, metav1.ConditionFalse, "Booting", fmt.Sprintf("Machine requested PXE artifact %s from %s", path, ip), corev1.EventTypeNormal, false)

	resolved, err := h.ResolveFileByPath(r.Context(), path, node, node.Spec.PXE.Image)
	if err != nil {
		if errors.Is(err, ErrNotYetDownloaded) {
			log.Info("file not yet downloaded", "node", node.Name)
			h.setPXEBootCondition(r.Context(), node, metav1.ConditionFalse, "WaitingForImage", fmt.Sprintf("PXE artifact %s is waiting for image %s to be cached", path, node.Spec.PXE.Image), corev1.EventTypeNormal, false)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "file not yet available, retry later", http.StatusServiceUnavailable)

			return
		}

		log.Warn("resolving file", "node", node.Name, "err", err)
		h.setPXEBootCondition(r.Context(), node, metav1.ConditionFalse, "FileResolveFailed", fmt.Sprintf("failed to resolve PXE artifact %s: %v", path, err), corev1.EventTypeWarning, false)
		http.NotFound(w, r)

		return
	}

	if resolved.DiskPath != "" {
		log.Info("serving cached file", "node", node.Name)
		h.setPXEBootCondition(r.Context(), node, metav1.ConditionTrue, "Served", fmt.Sprintf("served PXE artifact %s to %s", path, ip), corev1.EventTypeNormal, true)
		http.ServeFile(w, r, resolved.DiskPath)

		return
	}

	log.Info("serving file", "node", node.Name, "size", len(resolved.Data))
	h.setPXEBootCondition(r.Context(), node, metav1.ConditionTrue, "Served", fmt.Sprintf("served PXE artifact %s to %s", path, ip), corev1.EventTypeNormal, true)
	w.Header().Set("Content-Type", resolved.ContentType)
	w.Write(resolved.Data) //nolint:errcheck // Best-effort HTTP response write.
}

func (h *HTTPServer) setPXEBootCondition(ctx context.Context, machine *v1alpha3.Machine, status metav1.ConditionStatus, reason, message, eventType string, keepServedStable bool) {
	if h.Client == nil || machine == nil {
		return
	}

	key := client.ObjectKey{Name: machine.Name}
	changed := false

	err := machinestatus.Update(ctx, h.Client, key, func(latest *v1alpha3.Machine) bool {
		if latest.Spec.PXE == nil {
			return false
		}

		existing := meta.FindStatusCondition(latest.Status.Conditions, v1alpha3.MachineConditionPXEBoot)
		if status == metav1.ConditionFalse && reason == "Booting" && existing != nil &&
			existing.Status == metav1.ConditionTrue && (existing.Reason == "Served" || existing.Reason == "BootDisabled") {
			return false
		}

		if keepServedStable {
			if existing != nil && existing.Status == metav1.ConditionTrue && (existing.Reason == "Served" || existing.Reason == "BootDisabled") {
				return false
			}
		}

		changed = machinestatus.SetConditionIfChanged(latest, machinestatus.Condition(
			v1alpha3.MachineConditionPXEBoot,
			status,
			reason,
			message,
			latest.Generation,
		))

		return changed
	})
	if err != nil {
		slog.Error("updating PXE boot condition", "node", machine.Name, "err", err)
		return
	}

	if changed {
		machinestatus.Event(h.Recorder, machine, eventType, "PXEBoot"+reason, message)
	}
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

	node, err := h.LookupNodeByIP(ctx, ip)
	if err != nil {
		log.Error("cloud-init condition: looking up Machine", "ip", ip, "err", err)
		return
	}

	err = machinestatus.Update(ctx, h.Client, client.ObjectKey{Name: node.Name}, func(latest *v1alpha3.Machine) bool {
		cond := buildCloudInitCondition(ev, latest.Generation)
		if cond == nil {
			return false
		}

		return machinestatus.SetConditionIfChanged(latest, *cond)
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

	var requestRepave int64
	if node.Spec.Operations != nil {
		requestRepave = node.Spec.Operations.RepaveCounter
	}

	requestImage := ""
	if node.Spec.PXE != nil {
		requestImage = node.Spec.PXE.Image
	}

	updated := false
	eventMessage := ""
	if err := machinestatus.Update(r.Context(), h.Client, client.ObjectKey{Name: node.Name}, func(latest *v1alpha3.Machine) bool {
		updated = false
		var specRepave, statusRepave int64
		if latest.Spec.Operations != nil {
			specRepave = latest.Spec.Operations.RepaveCounter
		}

		if latest.Status.Operations != nil {
			statusRepave = latest.Status.Operations.RepaveCounter
		}

		imageName := ""
		if latest.Spec.PXE != nil {
			imageName = latest.Spec.PXE.Image
		}

		if specRepave != requestRepave || imageName != requestImage || !pxeHasIP(latest, ip) {
			return false
		}

		if specRepave <= statusRepave {
			return false
		}

		if latest.Status.Operations == nil {
			latest.Status.Operations = &v1alpha3.OperationsStatus{}
		}

		repavedMessage := "PXE disabled for image=" + imageName
		eventMessage = "PXE disabled after successful boot for image=" + imageName
		latest.Status.Operations.RepaveCounter = specRepave
		meta.SetStatusCondition(&latest.Status.Conditions, machinestatus.Condition(
			v1alpha3.MachineConditionRepaved,
			metav1.ConditionTrue,
			"Succeeded",
			repavedMessage,
			latest.Generation,
		))
		meta.SetStatusCondition(&latest.Status.Conditions, machinestatus.Condition(
			v1alpha3.MachineConditionPXEBoot,
			metav1.ConditionTrue,
			"BootDisabled",
			eventMessage,
			latest.Generation,
		))

		updated = true

		return true
	}); err != nil {
		log.Error("updating Machine status", "node", node.Name, "err", err)
		http.Error(w, "failed to disable PXE", http.StatusInternalServerError)

		return
	}

	if updated {
		log.Info("repave cleared", "node", node.Name)
		machinestatus.Event(h.Recorder, node, corev1.EventTypeNormal, "PXEBootDisabled", eventMessage)
	} else {
		log.Info("repave already cleared", "node", node.Name)
	}

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

func pxeHasIP(machine *v1alpha3.Machine, ip string) bool {
	if machine.Spec.PXE == nil {
		return false
	}

	for _, lease := range machine.Spec.PXE.DHCPLeases {
		if lease.IPv4 == ip {
			return true
		}
	}

	return false
}
