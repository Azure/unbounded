// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Azure/unbounded/internal/racer/wire"
)

type completionResponse struct {
	*httptest.ResponseRecorder
	onWrite func()
	onFlush func()
}

func (w *completionResponse) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if w.onWrite != nil {
		w.onWrite()
	}

	return n, err
}

func (w *completionResponse) Flush() {
	w.ResponseRecorder.Flush()

	if w.onFlush != nil {
		w.onFlush()
	}
}

func TestTrustAuthorityImmediateCompletion(t *testing.T) {
	for _, route := range []string{"bootstrap", "keyring", "keyring empty", "snapshot"} {
		for _, stage := range []string{"write", "flush"} {
			if route == "keyring empty" && stage == "write" {
				continue
			}

			for _, change := range []string{"invalidate", "recover", "rotate"} {
				t.Run(route+"/"+stage+"/"+change, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						f := newServingFixture(t)
						s := f.a.Server
						_, bundle, _, _ := keyState(t, f.a.Keyring)

						roots, err := s.Trust.pool()
						if err != nil {
							t.Fatal(err)
						}

						request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)

						switch route {
						case "bootstrap":
							encoded, err := wire.EncodeBootstrapRequest(f.request)
							if err != nil {
								t.Fatal(err)
							}

							request = httptest.NewRequest(http.MethodPost, wire.BootstrapPath, bytes.NewReader(encoded))
							request.Header.Set("Content-Type", "application/json")
							request.Header.Set("Authorization", "Bearer "+f.token)
						case "keyring":
							request = httptest.NewRequest(http.MethodGet, wire.KeyringPath, nil)
						case "keyring empty":
							request = httptest.NewRequest(http.MethodGet, wire.KeyringPath+"?after=1", nil)
							// Keep both stores fresh throughout the no-change poll.
							s.Trust.maxAge = 2 * wire.PollWait
							s.Publications.maxAge = 2 * wire.PollWait
						}

						request.TLS = f.requestState(t)
						called := false
						changeAuthority := func() {
							called = true

							if change != "rotate" {
								s.Trust.invalidate()
							}

							if change != "invalidate" {
								bundle.Generation++
								if err := s.Trust.install(t.Context(), roots, bundle); err != nil {
									t.Fatal(err)
								}
							}
							// Deliberately do not yield or wait for cancellation callbacks.
						}

						w := &completionResponse{ResponseRecorder: httptest.NewRecorder()}
						if stage == "write" {
							w.onWrite = changeAuthority
						} else {
							w.onFlush = changeAuthority
						}

						var aborted any

						func() {
							defer func() { aborted = recover() }()

							s.Handler().ServeHTTP(w, request)
						}()

						if !called {
							t.Fatal("completion hook not reached", w.Code)
						}

						if change == "rotate" {
							if aborted != nil {
								t.Fatalf("ordinary rotation aborted admitted response: %v", aborted)
							}
						} else if aborted != http.ErrAbortHandler {
							t.Fatalf("revoked response completed: %v", aborted)
						}

						if len(s.writes) != 0 || len(s.bootstrapSlots) != 0 || s.keyringPolls.count() != 0 || s.polls.count() != 0 {
							t.Fatal("completion leaked admission")
						}
					})
				})
			}
		}
	}
}

func TestTrustAuthoritySynchronousRevocation(t *testing.T) {
	f := newServingFixture(t)

	ctx, cancel, err := f.a.Server.Trust.writeContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	f.a.Server.Trust.invalidate()

	if ctx.Err() != context.Canceled {
		t.Fatal("revocation depends on callback scheduling")
	}
}

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
