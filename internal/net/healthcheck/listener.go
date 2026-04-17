// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package healthcheck

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"k8s.io/klog/v2"

	pb "github.com/Azure/unbounded-kube/internal/net/healthcheck/proto"
)

const maxPacketSize = 1500

// packetHandler is called by the listener for each valid incoming packet.
type packetHandler func(pkt *pb.HealthCheckPacket, from net.Addr)

// Listener handles incoming health check packets on a UDP port.
// When a valid request is received matching the local hostname,
// it sends a reply with swapped source/destination.
//
// The handler and all packet processing are lock-free by design.
// SetHandler must be called before Start; after that the handler
// field is read-only and safe for concurrent access from the
// read loop goroutine.
type Listener struct {
	localHostname string
	port          int
	conn          net.PacketConn

	handler  packetHandler
	stopOnce sync.Once
	done     chan struct{}
}

// NewListener creates a Listener bound to the given UDP port.
func NewListener(localHostname string, port int) (*Listener, error) {
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port: %d", port)
	}

	return &Listener{
		localHostname: localHostname,
		port:          port,
		done:          make(chan struct{}),
	}, nil
}

// SetHandler registers a callback for incoming reply packets destined to the
// local hostname. It must be called before Start; the handler is not safe to
// change after the read loop begins.
func (l *Listener) SetHandler(h packetHandler) {
	l.handler = h
}

// Start begins listening for health check packets. It blocks until ctx is
// cancelled or Stop is called.
func (l *Listener) Start(ctx context.Context) error {
	if l.conn == nil {
		addr := fmt.Sprintf(":%d", l.port)

		conn, err := net.ListenPacket("udp", addr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", addr, err)
		}

		l.conn = conn
	}

	klog.Infof("healthcheck listener started on %s", l.conn.LocalAddr())

	go func() {
		select {
		case <-ctx.Done():
			l.Stop()
		case <-l.done:
		}
	}()

	buf := make([]byte, maxPacketSize)
	for {
		n, remoteAddr, err := l.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-l.done:
				return nil
			default:
				klog.Errorf("healthcheck listener read error: %v", err)
				return err
			}
		}

		l.handlePacket(buf[:n], remoteAddr)
	}
}

// Stop gracefully shuts down the listener.
func (l *Listener) Stop() {
	l.stopOnce.Do(func() {
		close(l.done)

		if l.conn != nil {
			_ = l.conn.Close() //nolint:errcheck
		}

		klog.Info("healthcheck listener stopped")
	})
}

func (l *Listener) handlePacket(data []byte, from net.Addr) {
	pkt := &pb.HealthCheckPacket{}
	if err := proto.Unmarshal(data, pkt); err != nil {
		klog.V(4).Infof("healthcheck: failed to unmarshal packet from %s: %v", from, err)
		return
	}

	if pkt.DestinationHostname != l.localHostname {
		klog.V(4).Infof("healthcheck: ignoring packet for %q (local=%q) from %s",
			pkt.DestinationHostname, l.localHostname, from)

		return
	}

	switch pkt.Type {
	case pb.PacketType_REQUEST:
		// Lock-free path: sendReply only reads immutable fields and writes
		// to the shared UDP conn (which is goroutine-safe).
		l.sendReply(pkt, from)
	case pb.PacketType_REPLY:
		// Lock-free: handler is set once before Start and never changes.
		if l.handler != nil {
			l.handler(pkt, from)
		}
	}
}

func (l *Listener) sendReply(req *pb.HealthCheckPacket, to net.Addr) {
	reply := &pb.HealthCheckPacket{
		SourceHostname:      l.localHostname,
		DestinationHostname: req.SourceHostname,
		SequenceNumber:      req.SequenceNumber,
		TimestampNs:         time.Now().UnixNano(),
		Type:                pb.PacketType_REPLY,
	}

	data, err := proto.Marshal(reply)
	if err != nil {
		klog.Errorf("healthcheck: failed to marshal reply: %v", err)
		return
	}

	if _, err := l.conn.WriteTo(data, to); err != nil {
		klog.V(4).Infof("healthcheck: failed to send reply to %s: %v", to, err)
	}
}
