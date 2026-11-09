// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
	"github.com/Azure/unbounded/internal/cachemesh/memtransport"
)

// httpTransport is a ControlTransport over HTTP: each control message is a
// POST to /cachemesh on the target endpoint (host:port).
type httpTransport struct{ client *http.Client }

func (t httpTransport) Send(ctx context.Context, addr cachemesh.Endpoint, msg []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/cachemesh", bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cachemesh control: %s", resp.Status)
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

// controlHandler serves control messages for the Mesh in m.
func controlHandler(m *atomic.Pointer[cachemesh.Mesh]) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mesh := m.Load()

		body, err := readBounded(r.Body)
		if mesh == nil || err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		resp, err := mesh.ServeControl(r.Context(), r.RemoteAddr, body)
		if err != nil {
			// ErrNotRunning or ErrMessageTooLarge: the sender retries.
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		_, _ = w.Write(resp)
	})
}

// Example_httpTransport wires the control plane over net/http: every member
// listens on its own server and names it in Config.Addr.
func Example_httpTransport() {
	lease := memtransport.NewLeaseStore() // in production: a *kubelease.Store
	client := httpTransport{client: &http.Client{}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		meshes []*cachemesh.Mesh
		seed   string
	)

	for i := range 3 {
		var m atomic.Pointer[cachemesh.Mesh]

		srv := httptest.NewServer(controlHandler(&m))
		defer srv.Close()

		addr := strings.TrimPrefix(srv.URL, "http://")

		var seeds []string
		if seed != "" {
			seeds = []string{seed}
		}

		mesh, err := cachemesh.New(cachemesh.Config{
			ID:            fmt.Sprintf("node-%d", i),
			Addr:          addr,
			Transport:     client,
			Seeds:         seeds,
			Lease:         lease,
			ExpectedSize:  3,
			Logger:        slog.New(slog.DiscardHandler),
			PartitionBits: 12,
			Tuning:        exampleTuning(),
		})
		if err != nil {
			panic(err)
		}

		m.Store(mesh)
		meshes = append(meshes, mesh)

		go func() { _ = mesh.Run(ctx) }()

		if i == 0 {
			seed = addr

			waitUntil(mesh.Leading)
		}
	}

	waitUntil(func() bool {
		for _, m := range meshes {
			if !m.Ready() || m.Members() != 3 {
				return false
			}
		}

		return true
	})

	l, _ := meshes[2].Leader()
	fmt.Println("leader:", l.ID)
	fmt.Println("members:", meshes[2].Members())

	// Invalidate waits until this member's view shows the bump.
	meta := cachemesh.MustKeyspace("manifests", cachemesh.MinTTL)
	stamp := meshes[1].Stamp(meta)

	ictx, icancel := context.WithTimeout(ctx, 10*time.Second)
	defer icancel()

	if err := meshes[1].Invalidate(ictx, meta); err != nil && !errors.Is(err, context.Canceled) {
		panic(err)
	}

	fmt.Println("fresh after invalidate:", meshes[1].Fresh(meta, stamp))

	// Output:
	// leader: node-0
	// members: 3
	// fresh after invalidate: false
}

// exampleTuning shortens the timers so examples finish quickly. Production
// code leaves Tuning zero.
func exampleTuning() cachemesh.Tuning {
	return fastTuning()
}

func waitUntil(f func() bool) {
	deadline := time.Now().Add(20 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			panic("timed out")
		}

		time.Sleep(10 * time.Millisecond)
	}
}
