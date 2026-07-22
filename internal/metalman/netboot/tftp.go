// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"

	"github.com/pin/tftp/v3"
)

type TFTPServer struct {
	BindAddr string
	Port     int
	FileResolver
	StatusRecorder TFTPStatusRecorder
	Backend        TFTPBackend
}

type TFTPBackend interface {
	Open(ctx context.Context, filename string) (io.ReadCloser, error)
}

type TFTPBackendStatusRecorder interface {
	RecordBootLoaderDownloaded(ctx context.Context, filename string) error
}

type TFTPStatusRecorder interface {
	RecordBootLoaderDownloaded(ctx context.Context, machineName, filename string) error
}

func (t *TFTPServer) NeedLeaderElection() bool { return false }

func (t *TFTPServer) Start(ctx context.Context) error {
	s := tftp.NewServer(t.readHandler, nil)
	s.SetAnticipate(0)

	port := t.Port
	if port == 0 {
		port = 69
	}

	addr := net.JoinHostPort(t.BindAddr, fmt.Sprint(port))

	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	go func() {
		<-ctx.Done()
		s.Shutdown()
	}()

	slog.Info("starting TFTP server", "addr", addr)

	return s.Serve(conn.(*net.UDPConn)) //nolint:errcheck // Type is guaranteed by net.ListenPacket("udp", ...).
}

func (t *TFTPServer) readHandler(filename string, rf io.ReaderFrom) error {
	ctx := context.Background()
	ip := rf.(tftp.OutgoingTransfer).RemoteAddr().IP.String() //nolint:errcheck // Type is guaranteed by the tftp library.
	filename = strings.TrimPrefix(filename, "/")

	log := slog.With("proto", "tftp", "filename", filename, "ip", ip)
	if t.Backend != nil {
		if !validSessionArtifactPath(filename) {
			return fmt.Errorf("invalid session artifact path %q", filename)
		}

		reader, err := t.Backend.Open(ctx, filename)
		if err != nil {
			return fmt.Errorf("opening backend artifact: %w", err)
		}
		defer reader.Close() //nolint:errcheck // Backend stream is no longer needed.

		if _, err := rf.ReadFrom(reader); err != nil {
			return fmt.Errorf("transferring backend artifact: %w", err)
		}

		if recorder, ok := t.Backend.(TFTPBackendStatusRecorder); ok {
			if err := recorder.RecordBootLoaderDownloaded(ctx, filename); err != nil {
				return fmt.Errorf("recording backend transfer: %w", err)
			}
		}

		return nil
	}

	node, err := t.LookupNodeByIP(ctx, ip)
	if err != nil {
		log.Warn("no node for source IP", "err", err)
		return fmt.Errorf("no node for source IP %s: %w", ip, err)
	}

	if node.Spec.Netboot() == nil {
		log.Warn("node has no PXE config", "node", node.Name)
		return fmt.Errorf("node %s has no PXE config", node.Name)
	}

	imageRef := t.NetbootImageRef(node)
	if imageRef == "" {
		log.Warn("node has no netboot image", "node", node.Name)
		return fmt.Errorf("node %s has no netboot image", node.Name)
	}

	resolved, err := t.ResolveFileByPathForIP(ctx, filename, node, imageRef, ip)
	if err != nil {
		log.Warn("resolving file", "node", node.Name, "err", err)
		return err
	}

	if resolved.DiskPath != "" {
		f, err := os.Open(resolved.DiskPath)
		if err != nil {
			log.Error("opening cached file", "node", node.Name, "err", err)
			return fmt.Errorf("opening cached file: %w", err)
		}
		defer f.Close() //nolint:errcheck // Best-effort close of cached file.

		log.Info("serving file", "node", node.Name)

		if _, err := rf.ReadFrom(f); err != nil {
			log.Error("transfer failed", "node", node.Name, "err", err)
			return err
		}

		t.recordBootLoaderDownloaded(ctx, log, node, imageRef, filename)

		return nil
	}

	log.Info("serving file", "node", node.Name, "size", len(resolved.Data))

	if _, err := rf.ReadFrom(bytes.NewReader(resolved.Data)); err != nil {
		log.Error("transfer failed", "node", node.Name, "err", err)
		return err
	}

	t.recordBootLoaderDownloaded(ctx, log, node, imageRef, filename)

	return nil
}

func validSessionArtifactPath(filename string) bool {
	parts := strings.Split(filename, "/")
	return len(parts) >= 7 && parts[0] == "v1" && parts[1] == "netboot" && parts[2] == "sessions" && parts[3] != "" && parts[4] != "" && parts[5] == "artifacts" && parts[6] != ""
}

func (t *TFTPServer) recordBootLoaderDownloaded(ctx context.Context, log *slog.Logger, node *v1alpha3.Machine, imageRef, filename string) {
	if node == nil || t.StatusRecorder == nil || !t.isInitialBootLoaderDownload(imageRef, node.Spec.Netboot().TargetArchitecture(), filename) {
		return
	}

	if err := t.StatusRecorder.RecordBootLoaderDownloaded(ctx, node.Name, filename); err != nil {
		log.Error("recording boot loader download", "node", node.Name, "err", err)
	}
}

func (t *TFTPServer) isInitialBootLoaderDownload(imageRef, architecture, filename string) bool {
	if t.Cache == nil || imageRef == "" {
		return true
	}

	meta, err := t.Cache.MetadataForRefArchitecture(imageRef, architecture)
	if err != nil || meta.DHCPBootImageName == "" {
		return true
	}

	return strings.TrimPrefix(meta.DHCPBootImageName, "/") == filename
}
