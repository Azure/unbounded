// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/server"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestReplicaObservedHighWaterWithoutImage(t *testing.T) {
	for _, initial := range []string{"installed10", "no image", "suspended"} {
		for _, mutation := range []string{"rollback10", "conflicting11", "membership hash", "membership rollback", "membership jump"} {
			t.Run(initial+"/"+mutation, func(t *testing.T) {
				f := newServingFixture(t)
				r := f.a.Replication
				base := *capturePublication(t, f.a.authority)
				base.record.Sequence, base.record.MembershipVersion = 10, 5

				newer := base.record
				newer.Sequence = 11
				newer.ContentHash = strings.Repeat("a", 64)
				setVersion := func(record VersionRecord) {
					cm := &corev1.ConfigMap{}

					err := r.APIReader.Get(f.ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.VersionConfigMapName}, cm)
					if err != nil {
						t.Fatal(err)
					}

					cm.Data = versionData(record)
					if err := r.Client.Update(f.ctx, cm); err != nil {
						t.Fatal(err)
					}
				}
				setVersion(base.record)

				if initial == "no image" {
					d := fixtureDependencies[f.a.authority]
					a := authority.New(r.Config.authorityConfig(), authority.Dependencies{Reader: d, Writer: d})
					f.a.authority = a
					r.authority = a
					f.a.Lifecycle = server.NewLifecycle(a)
					f.a.Server = server.New(r.Config.serverConfig(), d, a, f.a.Lifecycle, r)
					fixtureTLS(t, f.a.Server, f.ctx, f.serverCertificate)
					f.a.Lifecycle.SetCacheSync(func(context.Context) bool { return true })

					go func() { _ = f.a.Lifecycle.Start(f.ctx) }()

					f.a.Lifecycle.SetServingReady(true)
					f.a.Topology.authority = a
					f.a.Keyring.authority = a
					fixtureDependencies[a] = d
				} else {
					image, err := wire.DecodePublication(strings.NewReader(base.encoded))
					if err != nil {
						t.Fatal(err)
					}

					image.Sequence = 10

					image.MembershipVersion = 5
					if err := r.installReplica(f.ctx, f.ctx, image); err != nil {
						t.Fatal(err)
					}

					if initial == "suspended" {
						restore := withdrawPublication(t, f.a.Topology)
						restore()
					}
				}

				setVersion(newer)
				r.observe(f.ctx)

				bad := newer

				switch mutation {
				case "rollback10":
					bad = base.record
				case "conflicting11":
					bad.ContentHash = strings.Repeat("b", 64)
				case "membership hash":
					bad.Sequence++
					bad.MembershipHash = strings.Repeat("b", 64)
				case "membership rollback":
					bad.Sequence++
					bad.MembershipVersion--
				case "membership jump":
					bad.Sequence++
					bad.MembershipVersion += 2
				}

				setVersion(bad)
				r.observe(f.ctx)

				if f.a.authority.PublicationReady() == nil || f.a.Server.Ready(nil) == nil {
					t.Fatal("invalid authority failed to suspend or erased high-water")
				}

				if err := r.authority.TrustReady(); err == nil {
					t.Fatal("invalid authority retained trust")
				}

				image, decodeErr := wire.DecodePublication(strings.NewReader(base.encoded))
				if decodeErr != nil {
					t.Fatal(decodeErr)
				}

				image.Sequence = 10

				image.MembershipVersion = 5
				if err := r.installReplica(f.ctx, f.ctx, image); err == nil {
					t.Fatal("install bypassed observed high-water")
				}

				// Exercise the public serving boundary, not only store readiness:
				// rejected authority must not expose even the previously valid image.
				request := httptest.NewRequest(http.MethodGet, wire.SnapshotPath, nil)
				request.TLS = f.requestState(t)
				response := httptest.NewRecorder()
				f.a.Server.Handler().ServeHTTP(response, request)

				if response.Code != http.StatusServiceUnavailable {
					t.Fatalf("invalid authority still served snapshot: %d", response.Code)
				}

				// Restoring the last valid authority permits a new reconcile/CAS,
				// without forgetting the observed watermark or reusing revoked bytes.
				setVersion(newer)
				runKeys(t, f.a.Keyring)

				recovered := reconcileTopology(t, f.a.Topology, f.ctx)
				if recovered.record.Sequence != newer.Sequence+1 || recovered.record.MembershipVersion != newer.MembershipVersion {
					t.Fatal("recovery reset counters or changed unchanged membership")
				}

				response = httptest.NewRecorder()
				f.a.Server.Handler().ServeHTTP(response, request.Clone(f.ctx))

				if response.Code != http.StatusOK || response.Body.String() != recovered.encoded {
					t.Fatalf("reconciled authority not served: %d", response.Code)
				}
			})
		}
	}
}

type firstChunkWriter struct {
	calls            int
	entered, unblock chan struct{}
}

func (w *firstChunkWriter) Write(b []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		close(w.entered)
		<-w.unblock
	}

	return len(b), nil
}

func TestPublicationWriteAuthorityRevocation(t *testing.T) {
	for _, change := range []string{"superseded", "suspend recover", "confirmation after write admission"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := initializedTopology(t)
				image := reconcileTopology(t, r, t.Context())
				copy := *image
				copy.encoded = strings.Repeat("x", 64*1024)
				w := &firstChunkWriter{entered: make(chan struct{}), unblock: make(chan struct{})}
				done := make(chan error, 1)

				writeCtx, stopWrite, err := copy.writeContext(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stopWrite()

				go func() { _, err := copy.ForBase("").writeTo(writeCtx, w); done <- err }()

				<-w.entered

				switch change {
				case "superseded":
					next := *image
					next.record.Sequence++

					next.record.ContentHash = strings.Repeat("a", 64)

					advanceFixturePublication(t, r)
				case "suspend recover":
					restore := withdrawPublication(t, r)
					restore()
					reconcileTopology(t, r, t.Context())
				case "confirmation after write admission":
					time.Sleep(20 * time.Second)

					reconcileTopology(t, r, t.Context())

					time.Sleep(11 * time.Second)
				}

				if r.authority.PublicationReady() != nil {
					t.Fatal("replacement or confirmed image unavailable")
				}

				close(w.unblock)

				if err := <-done; err == nil || w.calls != 1 {
					t.Fatalf("revoked response continued: calls=%d err=%v", w.calls, err)
				}
			})
		})
	}
}
