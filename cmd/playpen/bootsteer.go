// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// bootTargetUefiHTTP is the Redfish BootSourceOverrideTarget value that selects
// UEFI HTTP boot. It matches the value metalman PATCHes onto the ComputerSystem.
const bootTargetUefiHTTP = "UefiHttp"

// bootEnabledDisabled is the Redfish BootSourceOverrideEnabled value that means
// no boot override is in effect.
const bootEnabledDisabled = "Disabled"

// bootReaderInterval is how often the client polls the pod's Redfish service for
// the current boot configuration.
const bootReaderInterval = 2 * time.Second

// bootState is the client-side view of the guest's desired network boot,
// derived by polling the pod's Redfish ComputerSystem. It is shared between the
// Redfish reader (writer), the DHCP relay, and the HTTP boot proxy (readers).
type bootState struct {
	mu       sync.RWMutex
	httpBoot bool
	bootURI  string
}

// set records the latest boot configuration observed from Redfish.
func (b *bootState) set(httpBoot bool, bootURI string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.httpBoot = httpBoot
	b.bootURI = bootURI
}

// get returns whether HTTP boot is currently requested and the target boot URI.
func (b *bootState) get() (bool, string) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.httpBoot, b.bootURI
}

// bootReader polls the pod's Redfish ComputerSystem over the overlay stack and
// publishes the current UEFI HTTP boot intent into a shared bootState.
type bootReader struct {
	client   *http.Client
	url      string
	username string
	password string
	state    *bootState
}

// newBootReader builds a bootReader whose HTTP client dials the pod's Redfish
// TLS endpoint through the userspace overlay stack. The pod uses a self-signed
// certificate (trust-on-first-use pinning by metalman), so the reader skips
// verification; it only reads boot state, never credentials or payloads.
func newBootReader(o *overlay, cfg Config, state *bootState) *bootReader {
	remote := tcpip.FullAddress{
		NIC:  overlayNICID,
		Addr: tcpip.AddrFromSlice(o.remoteIP.To4()),
		Port: uint16(cfg.RedfishPort),
	}

	transport := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			conn, err := gonet.DialContextTCP(ctx, o.stack, remote, ipv4.ProtocolNumber)
			if err != nil {
				return nil, fmt.Errorf("dial pod redfish: %w", err)
			}

			tlsConn := tls.Client(conn, &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // emulator self-signed cert; boot-state read only.
				MinVersion:         tls.VersionTLS12,
			})

			if err := tlsConn.HandshakeContext(ctx); err != nil {
				_ = conn.Close() //nolint:errcheck // best-effort close

				return nil, fmt.Errorf("tls handshake pod redfish: %w", err)
			}

			return tlsConn, nil
		},
	}

	return &bootReader{
		client:   &http.Client{Transport: transport, Timeout: 5 * time.Second},
		url:      fmt.Sprintf("https://%s:%d/redfish/v1/Systems/%s", o.remoteIP, cfg.RedfishPort, cfg.RedfishDeviceID),
		username: cfg.RedfishUsername,
		password: cfg.RedfishPassword,
		state:    state,
	}
}

// run polls Redfish until ctx is cancelled, updating the shared bootState.
func (r *bootReader) run(ctx context.Context) {
	ticker := time.NewTicker(bootReaderInterval)
	defer ticker.Stop()

	r.poll(ctx) // prime immediately.

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.poll(ctx)
		}
	}
}

// poll performs one Redfish GET and republishes the derived boot intent. Errors
// are transient (the pod may not be serving yet) and are silently retried.
func (r *bootReader) poll(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return
	}

	req.SetBasicAuth(r.username, r.password)

	resp, err := r.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		return
	}

	var body struct {
		Boot struct {
			BootSourceOverrideTarget  string `json:"BootSourceOverrideTarget"`
			BootSourceOverrideEnabled string `json:"BootSourceOverrideEnabled"`
			HTTPBootURI               string `json:"HttpBootUri"`
		} `json:"Boot"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return
	}

	httpBoot := deriveHTTPBoot(body.Boot.BootSourceOverrideTarget, body.Boot.BootSourceOverrideEnabled, body.Boot.HTTPBootURI)

	r.state.set(httpBoot, body.Boot.HTTPBootURI)
}

// deriveHTTPBoot reports whether the Redfish boot override selects an active
// UEFI HTTP boot: the target must be UefiHttp, the override must not be
// Disabled, and a boot URI must be present.
func deriveHTTPBoot(target, enabled, uri string) bool {
	return strings.EqualFold(target, bootTargetUefiHTTP) &&
		!strings.EqualFold(enabled, bootEnabledDisabled) &&
		uri != ""
}
