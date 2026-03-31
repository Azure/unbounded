package netboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
)

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
		log.Error("no node for source IP", "err", err)
		http.NotFound(w, r)

		return
	}

	if node.Spec.PXE == nil {
		log.Error("node has no PXE config", "node", node.Name)
		http.NotFound(w, r)

		return
	}

	resolved, err := h.ResolveFileByPath(r.Context(), path, node, node.Spec.PXE.ImageRef.Name)
	if err != nil {
		if errors.Is(err, ErrNotYetDownloaded) {
			log.Info("file not yet downloaded", "node", node.Name)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "file not yet available, retry later", http.StatusServiceUnavailable)

			return
		}

		log.Error("resolving file", "node", node.Name, "err", err)
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
		slog.Error("reading cloudinit log body", "ip", ip, "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	slog.Info("cloudinit", "ip", ip, "event", string(body))
	w.WriteHeader(http.StatusOK)
}

func (h *HTTPServer) handleDisablePXE(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	log := slog.With("handler", "pxe-disable", "ip", ip)

	node, err := h.LookupNodeByIP(r.Context(), ip)
	if err != nil {
		log.Error("no node for source IP", "err", err)
		http.NotFound(w, r)

		return
	}

	var specReimage, statusReimage int64
	if node.Spec.Operations != nil {
		specReimage = node.Spec.Operations.ReimageCounter
	}

	if node.Status.Operations != nil {
		statusReimage = node.Status.Operations.ReimageCounter
	}

	if specReimage <= statusReimage {
		log.Info("reimage already cleared", "node", node.Name)
		w.WriteHeader(http.StatusOK)

		return
	}

	if node.Status.Operations == nil {
		node.Status.Operations = &v1alpha3.OperationsStatus{}
	}

	node.Status.Operations.ReimageCounter = specReimage

	imageName := ""
	if node.Spec.PXE != nil {
		imageName = node.Spec.PXE.ImageRef.Name
	}

	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineConditionReimaged,
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

	log.Info("reimage cleared", "node", node.Name)
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
