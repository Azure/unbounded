// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/Azure/unbounded/api/racer"
)

// Hold a completed inventory read at the API boundary so a heartbeat can change
// the rollout while reconciliation waits. No sleeps or live cluster are needed.
type blockedRolloutInventory struct {
	client.Client
	kind    string
	entered chan struct{}
	release chan struct{}
	err     error
}

func (c *blockedRolloutInventory) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}

	_, pods := list.(*corev1.PodList)

	_, nodes := list.(*corev1.NodeList)
	if c.kind == "pods" && pods || c.kind == "nodes" && nodes {
		close(c.entered)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.release:
			return c.err
		}
	}

	return nil
}

func TestRolloutInventoryDoesNotBlockHeartbeat(t *testing.T) {
	for _, kind := range []string{"pods", "nodes"} {
		for _, missing := range []bool{false, true} {
			t.Run(kind+"/missing="+strconv.FormatBool(missing), func(t *testing.T) {
				ctx := context.Background()
				f := newCoordinationFixture(t, nil)
				f.call(t, 0, 200)

				if missing {
					_, p, _ := fixtures()
					if err := f.api.Delete(ctx, p); err != nil {
						t.Fatal(err)
					}
				}

				blocked := &blockedRolloutInventory{Client: f.api, kind: kind, entered: make(chan struct{}), release: make(chan struct{})}
				f.s.controlStore.client = blocked
				done := make(chan error, 1)

				go func() {
					_, err := f.s.rolloutBusy(ctx, f.index)
					done <- err
				}()

				defer func() {
					close(blocked.release)

					if err := <-done; err != nil {
						t.Error(err)
					}
				}()

				select {
				case <-blocked.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("inventory read not reached")
				}

				req := httptest.NewRequest("GET", "/", nil)
				req.SetPathValue("universe", identity("universe", "default"))
				req.SetPathValue("node", f.node)
				controlTLS(req, "pod-uid")
				req.Header.Set("X-Racer-Boot", strings.Repeat("ab", 32))
				req.Header.Set("X-Racer-Profile", "1")
				req.Header.Set("X-Racer-Digest", f.digest)
				req.Header.Set("X-Racer-Phase", "1")

				heartbeat := make(chan *httptest.ResponseRecorder, 1)

				go func() {
					w := httptest.NewRecorder()
					f.s.control(w, req)

					heartbeat <- w
				}()

				select {
				case w := <-heartbeat:
					var command pb.ControlCommand

					if w.Code != 200 || proto.Unmarshal(w.Body.Bytes(), &command) != nil || command.Phase != 2 {
						t.Fatalf("heartbeat did not advance prepare barrier: HTTP=%d phase=%d", w.Code, command.Phase)
					}
				case <-time.After(time.Second):
					t.Fatal("inventory I/O blocked heartbeat and phase progress")
				}

				// Inspect after the deferred release and reconciliation have completed.
				t.Cleanup(func() {
					want := "2"
					if missing {
						want = "4"
					}

					if got := f.durable(t).Data["phase"]; got != want {
						t.Fatalf("decision used stale phase: got %s want %s", got, want)
					}
				})
			})
		}
	}
}

func TestRolloutInventoryFailurePreservesDecision(t *testing.T) {
	for _, kind := range []string{"pods", "nodes"} {
		t.Run(kind, func(t *testing.T) {
			f := newCoordinationFixture(t, nil)
			f.call(t, 0, 200)
			before := f.durable(t).ResourceVersion
			failure := errors.New("inventory unavailable")
			release := make(chan struct{})
			close(release)

			f.s.controlStore.client = &blockedRolloutInventory{Client: f.api, kind: kind, entered: make(chan struct{}), release: release, err: failure}
			if busy, err := f.s.rolloutBusy(context.Background(), f.index); !busy || !errors.Is(err, failure) {
				t.Fatalf("inventory failure admitted a decision: busy=%v err=%v", busy, err)
			}

			if f.durable(t).ResourceVersion != before {
				t.Fatal("inventory failure changed durable decision")
			}
		})
	}
}

func TestRolloutInventoryRejectsChangedTopology(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run("successor="+strconv.FormatBool(successor), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			f := newCoordinationFixture(t, nil)
			f.call(t, 0, 200)
			before := f.durable(t).ResourceVersion
			blocked := &blockedRolloutInventory{Client: f.api, kind: "nodes", entered: make(chan struct{}), release: make(chan struct{})}
			f.s.controlStore.client = blocked
			done := make(chan error, 1)

			go func() {
				_, err := f.s.rolloutBusy(ctx, f.index)
				done <- err
			}()

			select {
			case <-blocked.entered:
			case <-ctx.Done():
				t.Fatal("inventory read not reached")
			}

			f.s.mu.Lock()
			if successor {
				g := *f.index.g
				g.Revision++

				next, err := indexGeneration(&g)
				if err != nil {
					f.s.mu.Unlock()
					t.Fatal(err)
				}

				if err := f.s.installLocked(next); err != nil {
					f.s.mu.Unlock()
					t.Fatal(err)
				}
			} else {
				// commitCandidate removes publication after an ambiguous write.
				delete(f.s.source.topologies, identityBytes("universe", "default"))
			}
			f.s.mu.Unlock()

			close(blocked.release)

			if err := <-done; err == nil || !strings.Contains(err.Error(), "topology changed") {
				t.Fatalf("stale inventory was used: %v", err)
			}

			if f.durable(t).ResourceVersion != before {
				t.Fatal("stale inventory changed durable decision")
			}
		})
	}
}
