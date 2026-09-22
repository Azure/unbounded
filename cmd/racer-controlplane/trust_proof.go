// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/racer/pki"
)

// Each proof uses a new TLS handshake and exactly one bounded HTTP exchange.
// The opaque proof is minted by the local TLS layer, never from wire headers.
func serveTrustProof(ctx context.Context, listener net.Listener, hot *pki.HotTLS, ca *pki.Manager) error {
	var workers sync.WaitGroup
	defer workers.Wait()

	capacity := make(chan struct{}, 64)

	go func() { <-ctx.Done(); closeTLSResource(listener) }()

	for {
		raw, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return err
		}

		select {
		case capacity <- struct{}{}:
			workers.Add(1)

			go func() {
				defer workers.Done()
				defer func() { <-capacity }()
				defer closeTLSResource(raw)

				handleTrustProof(ctx, raw, hot, ca)
			}()
		default:
			closeTLSResource(raw)
		}
	}
}

func handleTrustProof(ctx context.Context, raw net.Conn, hot *pki.HotTLS, ca *pki.Manager) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := raw.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return
	}

	conn, finish, err := hot.HandshakeProof(ctx, raw, true, "")
	if err != nil {
		return
	}
	defer closeTLSResource(conn)

	reader := bufio.NewReader(io.LimitReader(conn, 16384))

	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	defer closeTLSResource(req.Body)

	reply := func(status int) {
		if _, err := fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status, http.StatusText(status)); err != nil {
			log.Printf("write trust proof response: %v", err)
		}
	}
	if req.Method != "POST" || req.URL.Path != "/v3/proof" || req.ContentLength > 0 || len(req.TransferEncoding) > 0 {
		reply(http.StatusBadRequest)
		return
	}

	state := conn.ConnectionState()
	req.TLS = &state

	ack, err := trustAcknowledgment(req)
	if err != nil {
		reply(http.StatusBadRequest)
		return
	}

	if len(state.PeerCertificates) != 0 && len(state.PeerCertificates[0].URIs) == 1 {
		parts := strings.Split(state.PeerCertificates[0].URIs[0].Path, "/")
		if len(parts) == 7 && parts[1] == "universe" && parts[3] == "node" && parts[5] == "pod" {
			req.SetPathValue("universe", parts[2])
			req.SetPathValue("node", parts[4])
		}
	}

	uid, err := authenticateControl(req)
	if err != nil {
		reply(http.StatusForbidden)
		return
	}

	boot, err := hex.DecodeString(req.Header.Get("X-Racer-Boot"))
	if err != nil || len(boot) != 32 {
		reply(http.StatusBadRequest)
		return
	}

	proof, err := finish(ack)
	if err == nil {
		err = ca.RecordTLSProof(ctx, pki.MemberKey{PodUID: uid, BootID: hex.EncodeToString(boot)}, proof)
	}

	if err != nil {
		reply(http.StatusConflict)
		return
	}

	reply(http.StatusNoContent)
}
