// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
	"path"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
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
	StatusRecorder StatusRecorder
	Media          *MediaProvider
}

type StatusRecorder interface {
	RecordBootLoaderDownloaded(ctx context.Context, machineName, filename string) error
	RecordBootImageWritten(ctx context.Context, machineName string) error
	RecordCloudInitDone(ctx context.Context, machineName string) error
	RecordOperationCondition(ctx context.Context, machineName string, condition metav1.Condition) error
	RecordPXEDisabled(ctx context.Context, machineName, imageName string) error
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
	mux.HandleFunc("POST /unbounded-agent/install-log", h.handleInstallLog)

	if h.Media != nil {
		// Method-qualified patterns are required: a method-less pattern
		// conflicts with "GET /" in net/http and panics at registration.
		mux.Handle("GET /bootmedia/", h.Media)
		mux.Handle("POST /bootmedia/", h.Media)
	}

	if h.Client != nil {
		mux.HandleFunc("POST /pxe/disable", h.handleDisablePXE)
		mux.HandleFunc("GET /pxe/disable", h.handleDisablePXE)
	}

	mux.HandleFunc("GET /", h.handleFile)

	addr := fmt.Sprintf("%s:%d", h.BindAddr, h.Port)
	srv := &http.Server{Addr: addr, Handler: normalizePath(mux)}

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

// normalizePath canonicalizes paths before routing, authorization, and file
// resolution. When shim is HTTP-booted from the web root it requests
// its second stage as "//grubx64.efi": it appends its absolute-path loader name
// to its boot URL's directory. Go's ServeMux would answer that with a 307
// redirect, which shim refuses to follow (it treats the 3xx as EFI_HTTP_ERROR
// 0x23 and aborts the boot). Normalizing the path here makes the mux serve the
// file directly with a 200.
func normalizePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleaned := cleanRequestPath(r.URL.Path)

		// Capabilities must not gain aliases through path normalization.
		if strings.Contains(r.URL.Path, "bootmedia") && (r.URL.RawPath != "" || cleaned != r.URL.Path) {
			http.NotFound(w, r)
			return
		}

		// ServeMux must route the same decoded path that the handlers check.
		r.URL.Path = cleaned
		r.URL.RawPath = ""

		next.ServeHTTP(w, r)
	})
}

// cleanRequestPath removes duplicate slashes, dot segments, and trailing
// slashes so authorization checks match the cache resolver's path cleaning.
func cleanRequestPath(p string) string {
	if p == "" {
		return "/"
	}

	return path.Clean(p)
}

func (h *HTTPServer) handleFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	ip := clientIP(r)
	log := slog.With("proto", "http", "path", path, "ip", ip)
	// The running installer keeps its original lease even if the Machine is
	// edited. Resolve its artifacts from the operation snapshot, never live input.
	if h.Media != nil {
		switch path {
		case "disk.img.gz", "vmlinuz", "initrd", "init.cpio", "grub.cfg", "grub/grub.cfg":
			remote, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				http.NotFound(w, r)
				return
			}

			o, claimed, err := h.Media.artifactOwner(r.Context(), remote)
			if claimed {
				if err != nil || strings.HasSuffix(path, "grub.cfg") {
					log.Warn("rejected boot media artifact request", "err", err)
					http.NotFound(w, r)

					return
				}

				log = log.With("handler", "bootmedia", "operation", o.op.Name)

				ref := o.status().Netboot.NetbootImage
				if path == "disk.img.gz" {
					ref = o.status().Netboot.Image
				}

				resolved, err := h.Media.Resolver.ResolveFileByPathForIP(r.Context(), path, o.machine, ref, remote)
				if err != nil {
					log.Warn("boot media artifact unavailable", "ref", ref, "err", err)
					http.Error(w, "media artifact unavailable", http.StatusServiceUnavailable)

					return
				}

				log.Info("serving boot media artifact", "ref", ref, "range", r.Header.Get("Range"))

				if resolved.DiskPath != "" {
					// GRUB's gzip arm64 kernel loader needs HTTP Range support.
					// ServeFile preserves it for hybrid ISO boots.
					http.ServeFile(w, r, resolved.DiskPath)
				} else {
					w.Write(resolved.Data) //nolint:errcheck // Best-effort response.
				}

				return
			}
		}
	}

	var (
		node *v1alpha3.Machine
		err  error
	)

	switch path {
	case "cloud-init/meta-data", "cloud-init/user-data", "cloud-init/vendor-data", "cloud-init/network-config":
		var owner *mediaOwner

		node, owner, err = h.firstBootMachine(r)
		if owner != nil {
			ip = owner.status().Netboot.DHCPLeases[0].IPv4
		}
	default:
		node, err = h.LookupNodeByIP(r.Context(), ip)
	}

	if err != nil {
		log.Warn("no node for source IP", "err", err)
		http.NotFound(w, r)

		return
	}

	if node.Spec.Netboot() == nil {
		log.Warn("node has no PXE config", "node", node.Name)
		http.NotFound(w, r)

		return
	}

	if node.Spec.Netboot().TargetBootProtocol() == "VirtualMedia" && strings.HasSuffix(path, "grub.cfg") {
		// Virtual media install intent is available only through its capability.
		http.NotFound(w, r)
		return
	}

	if node.Spec.Netboot().TargetBootProtocol() == "VirtualMedia" {
		switch path {
		case "disk.img.gz", "vmlinuz", "initrd", "init.cpio":
			http.NotFound(w, r)
			return
		}
	}

	imageRef := node.Spec.Netboot().Image
	if path != "disk.img.gz" {
		imageRef = h.NetbootImageRef(node)
	}

	if imageRef == "" {
		log.Warn("node has no image for requested path", "node", node.Name)
		http.NotFound(w, r)

		return
	}

	if node.Spec.Netboot().TargetBootProtocol() == v1alpha3.PXEBootProtocolHTTP &&
		h.isHTTPBootLoaderDownload(imageRef, node.Spec.Netboot().TargetArchitecture(), path) {
		installRequested, err := h.installRequested(r.Context(), node)
		if err != nil {
			log.Warn("checking active install operation", "node", node.Name, "err", err)
			http.Error(w, "checking active install operation", http.StatusServiceUnavailable)

			return
		}

		if !installRequested {
			log.Info("HTTP boot disabled because no install operation is active", "node", node.Name)
			http.NotFound(w, r)

			return
		}
	}

	resolved, err := h.ResolveFileByPathForIP(r.Context(), path, node, imageRef, ip)
	if err != nil {
		if errors.Is(err, ErrNotYetDownloaded) {
			log.Info("file not yet downloaded", "node", node.Name)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "file not yet available, retry later", http.StatusServiceUnavailable)

			return
		}

		if node.Spec.Netboot().TargetBootProtocol() == v1alpha3.PXEBootProtocolHTTP && isOptionalShimRevocationsFile(path) {
			serveMissingShimRevocationsFile(w, log, node, path)

			return
		}

		log.Warn("resolving file", "node", node.Name, "err", err)
		http.NotFound(w, r)

		return
	}

	if resolved.DiskPath != "" {
		log.Info("serving cached file", "node", node.Name)
		http.ServeFile(w, r, resolved.DiskPath)
		h.recordHTTPBootLoaderDownloaded(r.Context(), log, node, imageRef, path)

		return
	}

	log.Info("serving file", "node", node.Name, "size", len(resolved.Data))
	w.Header().Set("Content-Type", resolved.ContentType)
	w.Write(resolved.Data) //nolint:errcheck // Best-effort HTTP response write.
	h.recordHTTPBootLoaderDownloaded(r.Context(), log, node, imageRef, path)
}

const shimRevocationsNotPresentBody = "unbounded: no optional shim revocations file is present\n"

func isOptionalShimRevocationsFile(path string) bool {
	path = strings.Trim(path, "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		path = path[i+1:]
	}

	switch strings.ToLower(path) {
	case "revocations.efi", "revocations_sbat.efi", "revocations_sku.efi":
		return true
	default:
		return false
	}
}

func serveMissingShimRevocationsFile(w http.ResponseWriter, log *slog.Logger, node *v1alpha3.Machine, path string) {
	// shim treats these files as optional for netboot, but its HTTP fetch path
	// requires a 200 response with a non-empty body. Returning a small invalid
	// EFI payload preserves the "not present" semantics without sending a 404.
	log.Info("serving no-op body for missing optional shim revocations file", "node", node.Name, "path", path)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(shimRevocationsNotPresentBody)))
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(shimRevocationsNotPresentBody)) //nolint:errcheck // Best-effort HTTP response write.
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

	var node *v1alpha3.Machine

	if h.Media != nil || h.Reader != nil {
		var owner *mediaOwner

		node, owner, err = h.firstBootMachine(r)
		if err != nil {
			log.Warn("cloud-init report has no authorized Machine", "err", err)

			if errors.Is(err, errMediaFirstBootUnauthorized) {
				http.NotFound(w, r)
			} else {
				w.WriteHeader(http.StatusOK)
			}

			return
		}

		if owner != nil {
			if err := h.Media.recordFirstBootCondition(r.Context(), owner, &ev); err != nil {
				log.Error("recording media cloud-init condition", "operation", owner.op.Name, "err", err)
				w.Header().Set("Retry-After", "5")
				http.Error(w, "cannot save media cloud-init condition", http.StatusServiceUnavailable)

				return
			}

			w.WriteHeader(http.StatusOK)

			return
		}
	}

	machineName := h.recordCloudInitCondition(r.Context(), log, node, &ev)
	h.recordCloudInitStatus(r.Context(), log, machineName, &ev)

	w.WriteHeader(http.StatusOK)
}

func (h *HTTPServer) handleInstallLog(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		slog.Error("reading install log body", "ip", ip, "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	log := slog.With("ip", ip)

	if h.Media != nil || h.Reader != nil {
		node, owner, err := h.firstBootMachine(r)
		if err != nil {
			log.Warn("install log has no authorized Machine", "err", err)

			if errors.Is(err, errMediaFirstBootUnauthorized) {
				http.NotFound(w, r)
			} else {
				w.WriteHeader(http.StatusOK)
			}

			return
		}

		log = log.With("node", node.Name)
		if owner != nil {
			log = log.With("operation", owner.op.Name)
		}
	}

	log.Warn("unbounded-agent install log", "body", strings.TrimSpace(string(body)))
	w.WriteHeader(http.StatusOK)
}

var errMediaFirstBootUnauthorized = errors.New("no authorized media first-boot owner")

// Only the direct peer can claim a frozen lease. Live-IP fallback, including
// forwarding headers, is allowed only for Machines and leases without media owners.
func (h *HTTPServer) firstBootMachine(r *http.Request) (*v1alpha3.Machine, *mediaOwner, error) {
	if h.Media != nil {
		remote, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %w", errMediaFirstBootUnauthorized, err)
		}

		o, claimed, err := h.Media.firstBootOwner(r.Context(), remote)
		if claimed {
			if err != nil {
				return nil, nil, fmt.Errorf("%w: %w", errMediaFirstBootUnauthorized, err)
			}

			return o.machine, o, nil
		}
	}

	node, err := h.LookupNodeByIP(r.Context(), clientIP(r))
	if node != nil && node.Spec.Netboot() != nil && node.Spec.Netboot().TargetBootProtocol() == "VirtualMedia" {
		return nil, nil, errMediaFirstBootUnauthorized
	}

	if h.Media != nil {
		if mediaErr := h.Media.rejectFirstBootFallback(r.Context(), node, clientIP(r)); mediaErr != nil {
			return nil, nil, fmt.Errorf("%w: %w", errMediaFirstBootUnauthorized, mediaErr)
		}
	}

	return node, nil, err
}

// cloudInitLastStage is the final cloud-init stage. When this stage
// finishes successfully the CloudInitDone condition transitions to True.
const cloudInitLastStage = "modules-final"

// recordCloudInitCondition records the CloudInitDone condition for the active
// MachineOperation targeting the Machine that matches the request source IP.
func (h *HTTPServer) recordCloudInitCondition(ctx context.Context, log *slog.Logger, node *v1alpha3.Machine, ev *cloudInitEvent) string {
	if node == nil {
		return ""
	}

	cond := buildCloudInitCondition(ev, node.Generation)
	if cond != nil && h.StatusRecorder != nil {
		if err := h.StatusRecorder.RecordOperationCondition(ctx, node.Name, *cond); err != nil {
			log.Error("recording cloud-init condition", "node", node.Name, "err", err)
		}
	}

	return node.Name
}

const maxConditionMessageLen = 1024

// buildCloudInitCondition returns the metav1.Condition to set for a
// cloud-init webhook event, or nil if no update is needed.
func buildCloudInitCondition(ev *cloudInitEvent, generation int64) *metav1.Condition {
	switch ev.EventType {
	case "start":
		return &metav1.Condition{
			Type:               v1alpha3.MachineOperationConditionCloudInitDone,
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
				Type:               v1alpha3.MachineOperationConditionCloudInitDone,
				Status:             metav1.ConditionFalse,
				Reason:             "Failed",
				Message:            msg,
				ObservedGeneration: generation,
			}
		}

		if ev.Name == cloudInitLastStage {
			return &metav1.Condition{
				Type:               v1alpha3.MachineOperationConditionCloudInitDone,
				Status:             metav1.ConditionTrue,
				Reason:             "Succeeded",
				Message:            "cloud-init completed successfully",
				ObservedGeneration: generation,
			}
		}

		// An earlier stage succeeded - cloud-init is still running.
		return &metav1.Condition{
			Type:               v1alpha3.MachineOperationConditionCloudInitDone,
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

	if node.Spec.Netboot() != nil && node.Spec.Netboot().TargetBootProtocol() == "VirtualMedia" {
		http.NotFound(w, r)
		return
	}

	h.recordBootImageWritten(r.Context(), log, node.Name)

	imageName := ""
	if node.Spec.Netboot() != nil {
		imageName = node.Spec.Netboot().Image
	}

	if h.StatusRecorder == nil {
		log.Error("recording PXE disabled", "node", node.Name, "err", "status recorder is not configured")
		http.Error(w, "recording PXE disabled", http.StatusServiceUnavailable)

		return
	}

	if err := h.StatusRecorder.RecordPXEDisabled(r.Context(), node.Name, imageName); err != nil {
		log.Error("recording PXE disabled", "node", node.Name, "err", err)
		http.Error(w, "recording PXE disabled", http.StatusServiceUnavailable)

		return
	}

	log.Info("repave cleared", "node", node.Name)
	w.WriteHeader(http.StatusOK)
}

func (h *HTTPServer) recordBootImageWritten(ctx context.Context, log *slog.Logger, machineName string) {
	if h.StatusRecorder == nil {
		return
	}

	if err := h.StatusRecorder.RecordBootImageWritten(ctx, machineName); err != nil {
		log.Error("recording boot image written", "node", machineName, "err", err)
	}
}

func (h *HTTPServer) recordHTTPBootLoaderDownloaded(ctx context.Context, log *slog.Logger, node *v1alpha3.Machine, imageRef, path string) {
	if h.StatusRecorder == nil || node == nil || node.Spec.Netboot() == nil || node.Spec.Netboot().TargetBootProtocol() != v1alpha3.PXEBootProtocolHTTP {
		return
	}

	if !h.isHTTPBootLoaderDownload(imageRef, node.Spec.Netboot().TargetArchitecture(), path) {
		return
	}

	if err := h.StatusRecorder.RecordBootLoaderDownloaded(ctx, node.Name, path); err != nil {
		log.Error("recording boot loader download", "node", node.Name, "err", err)
	}
}

func (h *HTTPServer) isHTTPBootLoaderDownload(imageRef, architecture, path string) bool {
	if h.Cache == nil || imageRef == "" {
		return false
	}

	meta, err := h.Cache.MetadataForRefArchitecture(imageRef, architecture)
	if err != nil {
		return false
	}

	return HTTPBootPathFromMetadata(meta) == strings.TrimPrefix(path, "/")
}

func (h *HTTPServer) recordCloudInitStatus(ctx context.Context, log *slog.Logger, machineName string, ev *cloudInitEvent) {
	if h.StatusRecorder == nil || machineName == "" || ev.EventType != "finish" || ev.Name != cloudInitLastStage || !strings.EqualFold(ev.Result, "SUCCESS") {
		return
	}

	if err := h.StatusRecorder.RecordCloudInitDone(ctx, machineName); err != nil {
		log.Error("recording cloud-init status", "node", machineName, "err", err)
	}
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
