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

	"github.com/pin/tftp/v3"
)

type TFTPServer struct {
	BindAddr string
	FileResolver
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
	ctx := context.Background()
	ip := rf.(tftp.OutgoingTransfer).RemoteAddr().IP.String() //nolint:errcheck // Type is guaranteed by the tftp library.
	filename = strings.TrimPrefix(filename, "/")
	log := slog.With("proto", "tftp", "filename", filename, "ip", ip)

	node, err := t.LookupNodeByIP(ctx, ip)
	if err != nil {
		log.Error("no node for source IP", "err", err)
		return fmt.Errorf("no node for source IP %s: %w", ip, err)
	}

	if node.Spec.PXE == nil {
		log.Error("node has no PXE config", "node", node.Name)
		return fmt.Errorf("node %s has no PXE config", node.Name)
	}

	resolved, err := t.ResolveFileByPath(ctx, filename, node, node.Spec.PXE.ImageRef.Name)
	if err != nil {
		log.Error("resolving file", "node", node.Name, "err", err)
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

		return nil
	}

	log.Info("serving file", "node", node.Name, "size", len(resolved.Data))

	if _, err := rf.ReadFrom(bytes.NewReader(resolved.Data)); err != nil {
		log.Error("transfer failed", "node", node.Name, "err", err)
		return err
	}

	return nil
}
