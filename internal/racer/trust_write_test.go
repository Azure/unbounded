// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestPublicBlockedWriteTrustAuthority(t *testing.T) {
	for _, route := range []string{"snapshot", "bootstrap", "keyring"} {
		for _, change := range []string{"invalidate", "invalidate recover", "expire", "reconfirm"} {
			t.Run(route+"/"+change, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newServingFixture(t)
					s := f.a.Server
					s.Trust.maxAge = 5 * time.Second
					_, bundle, _, _ := keyState(t, f.a.Keyring)

					roots, err := s.Trust.pool()
					if err != nil {
						t.Fatal(err)
					}

					server, peer := net.Pipe()
					defer server.Close()
					defer peer.Close()

					request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
					if route == "keyring" {
						request = httptest.NewRequest(http.MethodGet, wire.KeyringPath, nil)
					}

					if route == "bootstrap" {
						encoded, err := wire.EncodeBootstrapRequest(f.request)
						if err != nil {
							t.Fatal(err)
						}

						request = httptest.NewRequest(http.MethodPost, wire.BootstrapPath, bytes.NewReader(encoded))
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("Authorization", "Bearer "+f.token)
					}

					request.TLS = f.requestState(t)
					request = request.WithContext(connectionContext(f.ctx, server))
					w := &pipeResponse{ResponseRecorder: httptest.NewRecorder(), conn: server}
					done := make(chan any, 1)

					go func() { defer func() { done <- recover() }(); s.Handler().ServeHTTP(w, request) }()

					synctest.Wait()

					if len(s.writes) != 1 {
						t.Fatal("response not blocked in write")
					}

					start := time.Now()

					switch change {
					case "invalidate":
						s.Trust.invalidate()
					case "invalidate recover":
						s.Trust.invalidate()

						if err := s.Trust.install(t.Context(), roots, bundle); err != nil {
							t.Fatal(err)
						}
					case "expire":
						time.Sleep(5 * time.Second)
					case "reconfirm":
						time.Sleep(3 * time.Second)

						if err := s.Trust.install(t.Context(), roots, bundle); err != nil {
							t.Fatal(err)
						}

						time.Sleep(2 * time.Second)
					}

					if aborted := <-done; aborted != http.ErrAbortHandler {
						t.Fatalf("blocked response did not abort: %v", aborted)
					}

					want := time.Duration(0)
					if change == "expire" || change == "reconfirm" {
						want = 5 * time.Second
					}

					if elapsed := time.Since(start); elapsed != want {
						t.Fatalf("trust cancellation took %s, want %s", elapsed, want)
					}

					if s.Publications.Ready(nil) != nil {
						t.Fatal("test must retain fresh publication independently of trust")
					}

					if len(s.writes) != 0 || len(s.bootstrapSlots) != 0 {
						t.Fatal("write or bootstrap admission leaked")
					}

					if change == "invalidate recover" || change == "reconfirm" {
						if _, err := s.Trust.pool(); err != nil {
							t.Fatal("new requests should have usable trust", err)
						}
					}
				})
			})
		}
	}
}

func TestTrustRotationKeepsAdmittedResponseBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trust := &Trust{maxAge: 5 * time.Second}
		f := newServingFixture(t)

		_, bundle, _, _ := keyState(t, f.a.Keyring)
		if err := trust.install(t.Context(), x509.NewCertPool(), bundle); err != nil {
			t.Fatal(err)
		}

		ctx, cancel, err := trust.writeContext(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()

		deadline, _ := ctx.Deadline()

		time.Sleep(3 * time.Second)

		bundle.Generation++
		if err := trust.install(t.Context(), x509.NewCertPool(), bundle); err != nil {
			t.Fatal(err)
		}

		if ctx.Err() != nil {
			t.Fatal("normal rotation revoked admitted response")
		}

		if got, _ := ctx.Deadline(); got != deadline {
			t.Fatal("rotation extended admitted freshness")
		}

		time.Sleep(2 * time.Second)
		synctest.Wait()

		if ctx.Err() == nil {
			t.Fatal("admitted trust outlived pinned freshness")
		}

		if _, err := trust.pool(); err != nil {
			t.Fatal("rotation should admit fresh requests", err)
		}
	})
}
