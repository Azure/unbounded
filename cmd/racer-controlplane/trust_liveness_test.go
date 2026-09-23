// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/Azure/unbounded/api/racer"
)

func TestTrustHeartbeatDoesNotBlockControlAndRechecksSelection(t *testing.T) {
	for _, change := range []string{"heartbeat", "replacement", "unpublished", "unavailable", "canceled", "trust-error"} {
		t.Run(change, func(t *testing.T) {
			f := newCoordinationFixture(t, nil)
			f.call(t, 0, 200)
			before := f.durable(t).ResourceVersion
			ack := f.s.rollouts["default"].acks[f.node]
			entered, release := make(chan struct{}), make(chan struct{})

			var once sync.Once

			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()

			f.s.trustHeartbeat = func(req *http.Request, _ string) error {
				if req.Header.Get("X-Test-Block") == "1" {
					close(entered)
					<-release
				}

				if change == "trust-error" {
					return errors.New("trust unavailable")
				}

				return nil
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			req := httptest.NewRequest("GET", "/", nil).WithContext(ctx)
			req.SetPathValue("universe", identity("universe", "default"))
			req.SetPathValue("node", f.node)
			controlTLS(req, "pod-uid")
			req.Header.Set("X-Racer-Boot", strings.Repeat("ab", 32))
			req.Header.Set("X-Racer-Profile", "1")
			req.Header.Set("X-Racer-Digest", f.digest)
			req.Header.Set("X-Racer-Phase", "1")
			req.Header.Set("X-Test-Block", "1")

			done := make(chan *httptest.ResponseRecorder, 1)

			go func() {
				w := httptest.NewRecorder()
				f.s.control(w, req)

				done <- w
			}()

			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("trust callback not reached")
			}

			progress := make(chan error, 1)

			go func() {
				if change == "heartbeat" {
					next := req.Clone(t.Context())
					next.Header.Del("X-Test-Block")

					w := httptest.NewRecorder()
					f.s.control(w, next)

					var command pb.ControlCommand
					if w.Code != 200 || proto.Unmarshal(w.Body.Bytes(), &command) != nil || command.Phase != 2 {
						progress <- errors.New("independent heartbeat failed to advance prepare barrier")
						return
					}
				} else {
					f.s.mu.Lock()
					defer f.s.mu.Unlock()

					switch change {
					case "replacement":
						g := *f.index.g
						g.Revision++
						g.Nodes = maps.Clone(g.Nodes)
						name := f.index.byID[f.node]
						member := g.Nodes[name]
						member.PodUID = "replacement-pod"
						g.Nodes[name] = member

						next, err := indexGeneration(&g)
						if err != nil {
							progress <- err
							return
						}

						if err := f.s.installLocked(next); err != nil {
							progress <- err
							return
						}
					case "unpublished":
						delete(f.s.source.topologies, identityBytes("universe", "default"))
					case "unavailable":
						f.s.source = nil
					case "canceled":
						cancel()
					}
				}

				progress <- nil
			}()

			select {
			case err := <-progress:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("trust callback held the server lock")
			}

			unblock()

			w := <-done

			want := map[string]int{"heartbeat": 200, "replacement": 403, "unpublished": 404, "unavailable": 503, "canceled": 200, "trust-error": 503}[change]
			if w.Code != want {
				t.Fatalf("HTTP %d, want %d: %s", w.Code, want, w.Body.String())
			}

			if change != "heartbeat" {
				if f.durable(t).ResourceVersion != before || f.s.rollouts["default"].acks[f.node] != ack {
					t.Fatal("rejected heartbeat changed durable rollout or refreshed acknowledgment")
				}

				if change == "canceled" && w.Body.Len() != 0 {
					t.Fatal("canceled heartbeat produced a command")
				}
			}
		})
	}
}
