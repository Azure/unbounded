// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/pin/tftp/v3"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/imagepull"
)

type TFTPServer struct {
	BindAddr string
	FileResolver
	StatusRecorder TFTPStatusRecorder
	Media          *MediaProvider

	// LookupTimeout bounds machine lookup and file resolution for one
	// request. Zero means defaultTFTPLookupTimeout.
	LookupTimeout time.Duration
	// TransferTimeout caps one whole transfer, including streaming the file.
	// Zero means defaultTFTPTransferTimeout.
	TransferTimeout time.Duration
}

const (
	defaultTFTPLookupTimeout   = 3 * time.Minute
	defaultTFTPTransferTimeout = time.Hour
)

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}

	return d
}

type TFTPStatusRecorder interface {
	RecordBootLoaderDownloaded(ctx context.Context, machineName, filename string) error
}

func (t *TFTPServer) NeedLeaderElection() bool { return false }

func (t *TFTPServer) Start(ctx context.Context) error {
	s := tftp.NewServer(t.readHandler, nil)
	s.SetAnticipate(0)

	addr := net.JoinHostPort(t.BindAddr, "69")

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
	// transferCtx lives for the whole transfer: opened files are bound to it
	// and stream until the transfer finishes. ctx only bounds lookups.
	transferCtx, cancelTransfer := context.WithTimeout(context.Background(), orDefault(t.TransferTimeout, defaultTFTPTransferTimeout))
	defer cancelTransfer()

	ctx, cancel := context.WithTimeout(transferCtx, orDefault(t.LookupTimeout, defaultTFTPLookupTimeout))
	defer cancel()

	// openWithin runs open with transferCtx, aborting it if the lookup
	// timeout expires first. Once open returns, the lookup timeout no longer
	// applies to the returned file.
	openWithin := func(open func(context.Context) error) error {
		stop := context.AfterFunc(ctx, cancelTransfer)
		err := open(transferCtx)

		if !stop() && err == nil {
			return fmt.Errorf("opening %s: %w", filename, context.DeadlineExceeded)
		}

		return err
	}
	// Rack managers fetch the ISO using their own IP, not the host lease.
	if t.Media != nil && (strings.HasPrefix(filename, "ub-") || strings.HasSuffix(filename, ".iso")) {
		log := slog.With("proto", "tftp", "filename", filename)
		if transfer, ok := rf.(tftp.OutgoingTransfer); ok {
			log = log.With("ip", transfer.RemoteAddr().IP.String())
		}

		file, err := t.Media.OpenISO(ctx, filename)
		if err != nil {
			log.Warn("rejecting boot media transfer", "err", err)
			return err
		}
		defer file.Close() //nolint:errcheck // Best-effort close after transfer.

		if transfer, ok := rf.(tftp.OutgoingTransfer); ok {
			if info, err := file.Stat(); err == nil {
				transfer.SetSize(info.Size())
			}
		}

		log.Info("serving boot media")

		n, err := sendWithin(transferCtx, rf, file)
		if err != nil {
			log.Error("boot media transfer failed", "bytes", n, "err", err)
			return err
		}

		log.Info("boot media transfer complete", "bytes", n)

		return nil
	}

	ip := rf.(tftp.OutgoingTransfer).RemoteAddr().IP.String() //nolint:errcheck // Type is guaranteed by the tftp library.
	filename = strings.TrimPrefix(filename, "/")
	log := slog.With("proto", "tftp", "filename", filename, "ip", ip)

	if t.Media != nil {
		if _, claimed, err := t.Media.artifactOwner(ctx, ip); claimed {
			log.Warn("rejecting boot media artifact transfer", "err", err)
			return fmt.Errorf("source IP is claimed by boot media; artifacts require HTTP")
		}
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

	if node.Spec.Netboot().TargetBootProtocol() == "VirtualMedia" {
		return fmt.Errorf("virtual media requires an operation-scoped ISO")
	}

	src := t.NetbootImageSource(node)
	if src.Ref == "" {
		log.Warn("node has no netboot image", "node", node.Name)
		return fmt.Errorf("node %s has no netboot image", node.Name)
	}

	var (
		resolved *ResolvedFile
		// img pins the request to one digest for content and the boot
		// loader classification below.
		img *imagepull.Image
	)

	err = openWithin(func(c context.Context) error {
		var err error

		if filename == userDataPath && node != nil {
			resolved, err = t.ResolveFileByPathForIP(c, filename, node, src, ip)
			return err
		}

		img, err = t.resolveImage(c, src)
		if err != nil {
			return err
		}

		resolved, err = t.ResolveFileFromImage(c, filename, node, img, ip)

		return err
	})
	if err != nil {
		if resolved != nil {
			resolved.Close() //nolint:errcheck,gosec // Best-effort close on failure.
		}

		log.Warn("resolving file", "node", node.Name, "err", err)

		return err
	}
	defer resolved.Close() //nolint:errcheck // read-only

	var content io.Reader = bytes.NewReader(resolved.Data)

	if resolved.File != nil {
		content = resolved.File

		if transfer, ok := rf.(tftp.OutgoingTransfer); ok {
			transfer.SetSize(resolved.File.Size())
		}

		log.Info("serving image file", "node", node.Name, "size", resolved.File.Size())
	} else {
		if transfer, ok := rf.(tftp.OutgoingTransfer); ok {
			transfer.SetSize(int64(len(resolved.Data)))
		}

		log.Info("serving file", "node", node.Name, "size", len(resolved.Data))
	}

	if _, err := sendWithin(transferCtx, rf, content); err != nil {
		log.Error("transfer failed", "node", node.Name, "err", err)
		return err
	}

	// The lookup ctx may have expired during a long transfer.
	recordCtx, cancelRecord := context.WithTimeout(context.Background(), orDefault(t.LookupTimeout, defaultTFTPLookupTimeout))
	defer cancelRecord()

	t.recordBootLoaderDownloaded(recordCtx, log, node, img, filename)

	return nil
}

// sendWithin streams src through rf and reports success only if the whole
// source was read before ctx ended. The tftp library treats
// io.ErrUnexpectedEOF from the source as a short final block and reports
// success, and it never sees ctx, so both are checked here.
func sendWithin(ctx context.Context, rf io.ReaderFrom, src io.Reader) (int64, error) {
	cr := &ctxReader{ctx: ctx, r: src}

	n, err := rf.ReadFrom(cr)
	if err != nil {
		return n, err
	}

	if cr.err != nil {
		return n, cr.err
	}

	if err := ctx.Err(); err != nil {
		return n, fmt.Errorf("transfer deadline: %w", err)
	}

	if !cr.eof {
		return n, fmt.Errorf("transfer ended before end of file: %w", io.ErrUnexpectedEOF)
	}

	return n, nil
}

// ctxReader fails reads once ctx ends and remembers the first read error
// other than io.EOF, so a truncated or late transfer is not reported as
// success.
type ctxReader struct {
	ctx context.Context //nolint:containedctx // Bounds reads of one transfer.
	r   io.Reader
	err error
	eof bool
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}

	if err := c.ctx.Err(); err != nil {
		c.err = fmt.Errorf("transfer deadline: %w", err)
		return 0, c.err
	}

	n, err := c.r.Read(p)
	switch {
	case err == nil:
	case errors.Is(err, io.EOF):
		c.eof = true
	case errors.Is(err, io.ErrUnexpectedEOF):
		// Reported as a distinct error so the tftp library aborts instead of
		// treating it as a short final block.
		c.err = fmt.Errorf("reading source: %w: %w", errTruncatedSource, err)
		return n, c.err
	default:
		c.err = err
	}

	return n, err
}

var errTruncatedSource = errors.New("source truncated")

func (t *TFTPServer) recordBootLoaderDownloaded(ctx context.Context, log *slog.Logger, node *v1alpha3.Machine, img *imagepull.Image, filename string) {
	if node == nil || t.StatusRecorder == nil || !t.isInitialBootLoaderDownload(ctx, img, filename) {
		return
	}

	if err := t.StatusRecorder.RecordBootLoaderDownloaded(ctx, node.Name, filename); err != nil {
		log.Error("recording boot loader download", "node", node.Name, "err", err)
	}
}

// isInitialBootLoaderDownload reports whether filename is the image's DHCP
// boot loader. Transfers without a resolved image, such as generated
// cloud-init user-data, are never boot loader downloads. When the image
// metadata cannot be read or names no boot loader, any image file counts.
func (t *TFTPServer) isInitialBootLoaderDownload(ctx context.Context, img *imagepull.Image, filename string) bool {
	if img == nil || filename == userDataPath {
		return false
	}

	meta, err := t.PinnedImageMetadata(ctx, img)
	if err != nil || meta.DHCPBootImageName == "" {
		return true
	}

	return strings.TrimPrefix(meta.DHCPBootImageName, "/") == filename
}
