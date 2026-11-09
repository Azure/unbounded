// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
)

// ControlTransport sends cachemesh control messages as HTTP POSTs to
// ControlPath on the member's address.
type ControlTransport struct {
	Client *http.Client
}

// NewControlTransport returns a ControlTransport with a small dedicated
// keep-alive pool. Send is bounded by the context the Mesh passes.
func NewControlTransport() ControlTransport {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)

	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}

	return ControlTransport{Client: &http.Client{Transport: &http.Transport{
		Protocols:           protocols,
		DialContext:         dialer.DialContext,
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}}}
}

// Send implements cachemesh.ControlTransport.
func (t ControlTransport) Send(ctx context.Context, addr cachemesh.Endpoint, msg []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+ControlPath, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := t.Client.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort close

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) //nolint:errcheck // drain for reuse
		return nil, fmt.Errorf("cachemesh control %s: %s", addr, resp.Status)
	}

	return readBounded(resp.Body)
}

func readBounded(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, cachemesh.MaxControlMessageSize+1))
	if err == nil && len(b) > cachemesh.MaxControlMessageSize {
		err = cachemesh.ErrMessageTooLarge
	}

	return b, err
}

// ControlHandler serves cachemesh control messages for the Mesh in m. A nil
// Mesh, a Mesh that is not running, or an oversized message gets 503 or
// 400, which the sender treats as a transport failure.
func ControlHandler(m *atomic.Pointer[cachemesh.Mesh]) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		mesh := m.Load()
		if mesh == nil {
			http.Error(w, "cachemesh not running", http.StatusServiceUnavailable)
			return
		}

		body, err := readBounded(http.MaxBytesReader(w, r.Body, cachemesh.MaxControlMessageSize+1))
		if err != nil {
			http.Error(w, "bad control message", http.StatusBadRequest)
			return
		}

		resp, err := mesh.ServeControl(r.Context(), r.RemoteAddr, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(resp) //nolint:errcheck // the sender times out on a broken write
	})
}
