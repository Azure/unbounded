// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// forwardMaxInFlight bounds the number of half-open forwarded connections the
// overlay TCP forwarder tracks at once.
const forwardMaxInFlight = 16

// startForwarder installs a userspace TCP forwarder on the overlay stack that
// proxies connections addressed to the client's overlay IP to the client's
// loopback interface. Remote processes that dial <overlay-ip>:overlayPort are
// spliced to 127.0.0.1:loopbackPort on the client. Ports without a rule are
// reset. The forwarder runs until ctx is cancelled.
func (o *overlay) startForwarder(ctx context.Context, rules []forwardRule) {
	ports := make(map[uint16]uint16, len(rules))
	for _, r := range rules {
		ports[r.overlayPort] = r.loopbackPort
	}

	handler := func(req *tcp.ForwarderRequest) {
		overlayPort := req.ID().LocalPort

		loopbackPort, ok := ports[overlayPort]
		if !ok {
			req.Complete(true) // no rule: reset.
			return
		}

		go o.serveForward(ctx, req, overlayPort, loopbackPort)
	}

	fwd := tcp.NewForwarder(o.stack, 0, forwardMaxInFlight, handler)
	o.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
}

// serveForward completes one forwarded TCP connection: it dials the client's
// loopback target, accepts the overlay endpoint, and splices the two together.
func (o *overlay) serveForward(ctx context.Context, req *tcp.ForwarderRequest, overlayPort, loopbackPort uint16) {
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(loopbackPort)))

	var dialer net.Dialer
	if o.proxySrc != nil {
		// Bind the egress source to the guest's real overlay IP so metalman
		// (bound on the host loopback) observes that IP as the request source.
		dialer.LocalAddr = &net.TCPAddr{IP: o.proxySrc}
	}

	local, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		fmt.Printf("  forward %d -> %s: dial error: %v\n", overlayPort, target, err)
		req.Complete(true) // reset: nothing is listening.

		return
	}

	var wq waiter.Queue

	ep, tcpErr := req.CreateEndpoint(&wq)
	if tcpErr != nil {
		fmt.Printf("  forward %d -> %s: create endpoint: %s\n", overlayPort, target, tcpErr)
		req.Complete(true)

		_ = local.Close() //nolint:errcheck // best-effort close

		return
	}

	req.Complete(false)

	remote := gonet.NewTCPConn(&wq, ep)

	proxyConn(ctx, remote, local)
}

// proxyConn splices two connections bidirectionally, closing both once either
// direction finishes or the context is cancelled.
func proxyConn(ctx context.Context, a, b net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		<-ctx.Done()

		_ = a.Close() //nolint:errcheck // best-effort close
		_ = b.Close() //nolint:errcheck // best-effort close
	}()

	var wg sync.WaitGroup

	wg.Add(2)

	go copyHalf(&wg, cancel, a, b)
	go copyHalf(&wg, cancel, b, a)

	wg.Wait()
}

// copyHalf copies from src to dst until EOF or error, then cancels so the peer
// direction and the connections are torn down.
func copyHalf(wg *sync.WaitGroup, cancel context.CancelFunc, dst, src net.Conn) {
	defer wg.Done()
	defer cancel()

	_, _ = io.Copy(dst, src) //nolint:errcheck // splice best-effort; cancel handles teardown
}
