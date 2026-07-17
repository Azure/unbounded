// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// httpBootProxyPort is the TCP port on the client's overlay IP where the HTTP
// boot reverse proxy listens. The DHCP relay rewrites the guest's boot URI
// (option 67) to point here so guest HTTP-boot requests are proxied to the real
// boot server over the client's host network.
const httpBootProxyPort uint16 = 8090

// startHTTPBootProxy installs a reverse proxy on the overlay stack that serves
// the guest's UEFI HTTP boot requests. The guest reaches it at
// <client-overlay-ip>:httpBootProxyPort (over the shared VXLAN segment); each
// request is forwarded to the boot server named by the current bootState URI,
// reachable from the client's host network. The proxy runs until ctx is
// cancelled.
func startHTTPBootProxy(ctx context.Context, o *overlay, state *bootState) error {
	addr := tcpip.FullAddress{
		NIC:  overlayNICID,
		Addr: tcpip.AddrFromSlice(o.localIP.To4()),
		Port: httpBootProxyPort,
	}

	ln, err := gonet.ListenTCP(o.stack, addr, ipv4.ProtocolNumber)
	if err != nil {
		return fmt.Errorf("listen http boot proxy on overlay: %w", err)
	}

	srv := &http.Server{
		Handler:           newBootProxy(state),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = srv.Shutdown(shutdownCtx) //nolint:errcheck // best-effort shutdown
	}()

	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			fmt.Printf("  http boot proxy: serve error: %v\n", serveErr)
		}
	}()

	return nil
}

// newBootProxy builds the reverse proxy that serves guest HTTP-boot requests. It
// rewrites each request to target the boot server named by the current bootState
// URI (reachable from the client's host network), preserving the request path.
// If the URI is empty or unparseable the request is left untouched so the
// transport fails fast rather than looping back to the proxy.
func newBootProxy(state *bootState) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			_, bootURI := state.get()

			target, perr := url.Parse(bootURI)
			if perr != nil || target.Host == "" {
				return
			}

			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
		},
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		},
	}
}
