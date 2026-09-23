// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/Azure/unbounded/api/racer"
)

// Authenticated control requests and durable rollout barriers.

type tokenClient struct{ client.Client }

func (c tokenClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if review, ok := obj.(*authenticationv1.TokenReview); ok {
		review.Status.Authenticated = review.Spec.Token == "pod-token"
		review.Status.Audiences = review.Spec.Audiences
		review.Status.User.Extra = map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-uid": {"pod-uid"}}

		return nil
	}

	return c.Client.Create(ctx, obj, opts...)
}

// API-boundary fault injection and shared coordination fixtures.

var errLostRolloutResponse = errors.New("injected rollout API response loss")

// Effects occur at the API boundary, including a real successful write whose
// returned object/response never reaches the caller. All accesses are serialized
// by Server.mu (or by the single-threaded transition tests).
type rolloutAPI struct {
	client.Client
	fault                   string
	readFailures            int
	writes, conflicts, hits int
}

func (c *rolloutAPI) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if strings.HasSuffix(key.Name, "-rollout") && c.readFailures > 0 {
		c.readFailures--
		return errors.New("injected uncached read failure")
	}

	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *rolloutAPI) write(ctx context.Context, obj client.Object, write func(client.Object) error) error {
	if !strings.HasSuffix(obj.GetName(), "-rollout") {
		return write(obj)
	}

	c.writes++
	fault := c.fault

	c.fault = ""
	if fault != "" {
		c.hits++
	}

	if fault == "no-commit" {
		return errLostRolloutResponse
	}

	if fault == "conflict" || fault == "history-conflict" {
		other := &corev1.ConfigMap{}
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(obj), other); err != nil {
			return err
		}

		if fault == "conflict" {
			other.Data["phase"] = "2" // A competing durable decision, not just a new RV.
		} else {
			other.Annotations = map[string]string{"test-writer": "competing history CAS"}
		}

		if err := c.Client.Update(ctx, other); err != nil {
			return err
		}
	}

	if fault == "lost" || fault == "lost-read" {
		if err := write(obj.DeepCopyObject().(client.Object)); err != nil {
			return err
		}

		if fault == "lost-read" {
			c.readFailures = 2
		}

		return errLostRolloutResponse
	}

	err := write(obj)
	if apierrors.IsConflict(err) {
		c.conflicts++
	}

	return err
}

func (c *rolloutAPI) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return c.write(ctx, obj, func(o client.Object) error { return c.Client.Create(ctx, o, opts...) })
}

func (c *rolloutAPI) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.write(ctx, obj, func(o client.Object) error { return c.Client.Update(ctx, o, opts...) })
}

type coordinationFixture struct {
	s            *Server
	api          *rolloutAPI
	index        *topologyIndex
	node, digest string
	podUID       string
}

func newCoordinationFixture(t *testing.T, kube client.Client) *coordinationFixture {
	t.Helper()

	n, p, svc := fixtures()
	p.UID = "pod-uid"

	if kube == nil {
		kube = fakeKube(n, p, svc)
	}

	api := &rolloutAPI{Client: tokenClient{kube}}

	g, _, err := buildCacheFixture("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, svc)
	if err != nil {
		t.Fatal(err)
	}

	g.Revision = 1

	store := stateStore{client: api, namespace: "state"}
	if err := store.commit(context.Background(), g, nil); err != nil {
		t.Fatal(err)
	}

	index, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{controlStore: store}
	if err := s.install(index); err != nil {
		t.Fatal(err)
	}

	return &coordinationFixture{s: s, api: api, index: index, node: g.Nodes[n.Name].ID}
}

func (f *coordinationFixture) call(t *testing.T, phase uint32, code int) *pb.ControlCommand {
	t.Helper()

	req := httptest.NewRequest("GET", "/v3/"+identity("universe", "default")+"/"+f.node, nil)
	req.SetPathValue("universe", identity("universe", "default"))
	req.SetPathValue("node", f.node)

	podUID := f.podUID
	if podUID == "" {
		podUID = "pod-uid"
	}

	controlTLS(req, podUID)
	req.Header.Set("X-Racer-Boot", strings.Repeat("ab", 32))
	req.Header.Set("X-Racer-Profile", "1")
	req.Header.Set("X-Racer-Phase", strconv.Itoa(int(phase)))
	req.Header.Set("X-Racer-Digest", f.digest)

	w := httptest.NewRecorder()
	f.s.control(w, req)

	if w.Code != code {
		t.Fatalf("HTTP %d, want %d: %s", w.Code, code, w.Body.String())
	}

	if code != 200 {
		return nil
	}

	var command pb.ControlCommand

	if err := proto.Unmarshal(w.Body.Bytes(), &command); err != nil {
		t.Fatal(err)
	}

	f.digest = hex.EncodeToString(command.SnapshotDigest)

	cm := f.durable(t)
	if cm.Data["revision"] != strconv.FormatUint(command.Revision, 10) || cm.Data["phase"] != strconv.Itoa(int(command.Phase)) {
		t.Fatalf("delivery before durable decision: command=%v durable=%v", &command, cm.Data)
	}

	return &command
}

func (f *coordinationFixture) durable(t *testing.T) *corev1.ConfigMap {
	t.Helper()

	cm := &corev1.ConfigMap{}
	if err := f.api.Client.Get(context.Background(), client.ObjectKey{Namespace: "state", Name: stateName("default") + "-rollout"}, cm); err != nil {
		t.Fatal(err)
	}

	return cm
}

// CAS recovery: ambiguous writes, competing decisions and concurrent readers.

func TestB14AmbiguousUpdate(t *testing.T) {
	f := newCoordinationFixture(t, nil)
	f.call(t, 0, 200)
	r := f.s.rollouts["default"]
	oldRV := r.pointer.ResourceVersion
	f.api.fault = "lost"
	f.call(t, 1, 503)

	if cm := f.durable(t); cm.Data["phase"] != "2" || cm.ResourceVersion == oldRV {
		t.Fatal("fault did not commit")
	}
	// Baseline keeps r.pointer stale and repeatedly conflicts. Exercise the same
	// decision path three times without a controller restart.
	for i := 0; i < 3; i++ {
		_, err := f.s.rolloutFor(context.Background(), f.index)
		if err != nil {
			t.Fatal(err)
		}

		current := f.s.rollouts["default"]
		if current.phase < 2 {
			t.Errorf("attempt %d: stale phase %d after durable receive", i, current.phase)
		}

		if err := f.s.persistPhase(context.Background(), "default", current, 3); err != nil {
			t.Errorf("attempt %d: repeated CAS: %v", i, err)
		}
	}

	if f.api.conflicts != 0 {
		t.Fatalf("stale resourceVersion caused %d repeated conflicts", f.api.conflicts)
	}
}

func TestB14WriteOutcomes(t *testing.T) {
	for _, fault := range []string{"lost", "no-commit", "conflict", "lost-read"} {
		t.Run(fault, func(t *testing.T) {
			f := newCoordinationFixture(t, nil)
			f.call(t, 0, 200)
			f.api.fault = fault
			f.call(t, 1, 503)

			writes := f.api.writes
			if fault == "lost-read" {
				f.call(t, 1, 503)
				f.call(t, 1, 503)

				if f.api.writes != writes {
					t.Fatal("readback failure allowed a decision/write")
				}
			}

			got := f.call(t, 0, 200)

			want := uint32(2)
			if fault == "no-commit" {
				want = 1
			}

			if got.Phase != want {
				t.Fatalf("reloaded phase %d, want %d", got.Phase, want)
			}

			if want == 1 {
				f.call(t, 1, 200)
			}

			if got := f.call(t, 2, 200); got.Phase != 3 {
				t.Fatal("receive barrier stalled")
			}

			if got := f.call(t, 3, 200); got.Phase != 4 {
				t.Fatal("activation barrier stalled")
			}

			f.call(t, 4, 200)
		})
	}
}

func TestB14CreateLostResponse(t *testing.T) {
	f := newCoordinationFixture(t, nil)
	f.api.fault = "lost"
	f.call(t, 0, 503)

	rv := f.durable(t).ResourceVersion
	if got := f.call(t, 0, 200); got.Phase != 1 {
		t.Fatal("create recovery")
	}

	if f.durable(t).ResourceVersion != rv {
		t.Fatal("recovery rewrote committed create")
	}

	f.call(t, 1, 200)
}

func TestB14LostResponseEveryBarrier(t *testing.T) {
	for _, phase := range []uint32{1, 2, 3} {
		t.Run(strconv.Itoa(int(phase)), func(t *testing.T) {
			f := newCoordinationFixture(t, nil)
			for ack := uint32(0); ack < phase; ack++ {
				f.call(t, ack, 200)
			}

			f.api.fault = "lost"
			f.call(t, phase, 503)

			if got := f.call(t, 0, 200); got.Phase != phase+1 {
				t.Fatalf("lost barrier %d reloaded as %d", phase+1, got.Phase)
			}

			for ack := phase + 1; ack <= 4; ack++ {
				f.call(t, ack, 200)
			}

			if busy, err := f.s.rolloutBusy(context.Background(), f.index); err != nil || busy {
				t.Fatalf("retirement stalled: %v %v", busy, err)
			}
		})
	}
}

func TestB14RecoveringTransition(t *testing.T) {
	for _, phase := range []uint32{2, 4} {
		for _, fault := range []string{"no-commit", "lost", "lost-read"} {
			t.Run(strconv.Itoa(int(phase))+"/"+fault, func(t *testing.T) {
				f := newCoordinationFixture(t, nil)
				for ack := uint32(0); ack < phase; ack++ {
					f.call(t, ack, 200)
				}

				r := f.s.rollouts["default"]

				_, pod, _ := fixtures()
				if err := f.api.Delete(context.Background(), pod); err != nil {
					t.Fatal(err)
				}

				f.api.fault = fault
				if _, err := f.s.rolloutBusy(context.Background(), f.index); err == nil {
					t.Fatal("missing write error")
				}

				if r.recovering {
					t.Fatal("exposed unconfirmed recovering transition")
				}

				if fault == "lost-read" {
					for i := 0; i < 2; i++ {
						if busy, err := f.s.rolloutBusy(context.Background(), f.index); !busy || err == nil {
							t.Fatal("read failure admitted decisions")
						}
					}
				}

				if busy, err := f.s.rolloutBusy(context.Background(), f.index); err != nil || busy {
					t.Fatalf("forward recovery stalled: %v %v", busy, err)
				}

				cm := f.durable(t)
				if cm.Data["phase"] != "4" || cm.Data["recovering"] != "true" {
					t.Fatal("must recover forward", cm.Data)
				}
			})
		}
	}
}

func TestB14NoAbortAfterAmbiguousReceive(t *testing.T) {
	f := newCoordinationFixture(t, nil)
	f.call(t, 0, 200)
	f.s.rollouts["default"].since = time.Now().Add(-time.Hour)
	f.api.fault = "lost"
	f.call(t, 1, 503)

	_, pod, _ := fixtures()
	if err := f.api.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}

	if _, err := f.s.rolloutBusy(context.Background(), f.index); err != nil {
		t.Fatal(err)
	}

	if f.durable(t).Data["phase"] != "4" {
		t.Fatal("aborted durable receive")
	}
}

func TestB14ReadbackValidation(t *testing.T) {
	for _, field := range []string{"revision", "newer", "phase", "recovering", "since", "rollback", "abort", "deleted"} {
		t.Run(field, func(t *testing.T) {
			f := newCoordinationFixture(t, nil)
			f.call(t, 0, 200)
			f.call(t, 1, 200)
			f.api.fault = "no-commit"
			f.call(t, 2, 503)
			cm := f.durable(t)

			switch field {
			case "newer":
				cm.Data["revision"] = "2"
			case "rollback":
				cm.Data["phase"] = "1"
			case "abort":
				cm.Data["phase"] = "5"
			case "deleted":
				if err := f.api.Delete(context.Background(), cm); err != nil {
					t.Fatal(err)
				}
			default:
				cm.Data[field] = "malformed"
			}

			if field != "deleted" {
				if err := f.api.Client.Update(context.Background(), cm); err != nil {
					t.Fatal(err)
				}
			}

			writes := f.api.writes
			for i := 0; i < 3; i++ {
				f.call(t, 2, 503)
			}

			if f.api.writes != writes {
				t.Fatal("invalid readback authorized write")
			}
		})
	}
}

func TestB14ConcurrentSubscriptionAndReconcileRecovery(t *testing.T) {
	f := newCoordinationFixture(t, nil)
	f.call(t, 0, 200)
	f.api.fault = "lost"
	f.call(t, 1, 503)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()
			// Independent HTTP caller state; the production server serializes the
			// invalidation/reload against the reconcile barrier under its mutex.
			local := *f
			for j := 0; j < 4; j++ {
				if busy, err := f.s.rolloutBusy(context.Background(), f.index); err != nil || !busy {
					t.Errorf("receive must remain busy: %v %v", busy, err)
				}

				if got := local.call(t, 0, 200); got.Phase != 2 {
					t.Errorf("stale phase %d", got.Phase)
				}
			}
		}()
	}

	wg.Wait()

	if f.api.conflicts != 0 {
		t.Fatal("recovery retried stale CAS")
	}

	f.call(t, 2, 200)
	f.call(t, 3, 200)
	f.call(t, 4, 200)
}

// Removal catch-up: identity, retained decisions and history write faults.

func catchupRequest(t *testing.T, f *coordinationFixture, boot, digest string, ack uint32, code int) *pb.ControlCommand {
	t.Helper()

	req := httptest.NewRequest("GET", "/", nil)
	req.SetPathValue("universe", identity("universe", "default"))
	req.SetPathValue("node", f.node)
	controlTLS(req, "pod-uid")
	req.Header.Set("X-Racer-Profile", "1")
	req.Header.Set("X-Racer-Boot", boot)
	req.Header.Set("X-Racer-Digest", digest)
	req.Header.Set("X-Racer-Phase", strconv.Itoa(int(ack)))

	w := httptest.NewRecorder()
	f.s.control(w, req)

	if w.Code != code {
		t.Fatalf("HTTP %d want %d: %s", w.Code, code, w.Body.String())
	}

	if code != 200 {
		return nil
	}

	var command pb.ControlCommand

	if err := proto.Unmarshal(w.Body.Bytes(), &command); err != nil {
		t.Fatal(err)
	}

	cm := f.durable(t)
	if command.Revision == f.index.g.Revision {
		if fmt.Sprint(command.Phase) != cm.Data["phase"] {
			t.Fatal("latest before durability")
		}
	} else {
		entries, err := removalHistory(cm.Data["removals"], "default", f.index.g.Revision)
		if err != nil {
			t.Fatal(err)
		}

		found := false

		for _, d := range entries {
			hash := sha256.Sum256(d.Snapshot)
			if hex.EncodeToString(hash[:]) == hex.EncodeToString(command.SnapshotDigest) && d.Boot == boot && d.Phase == command.Phase {
				found = true
			}
		}

		if !found {
			t.Fatal("historical command before durability")
		}
	}

	return &command
}

// Unit requests intentionally supply acknowledgments; the separate hybrid
// fixture is the evidence for actual worker execution.
func TestB13HistoryFaults(t *testing.T) {
	for _, fault := range []string{"lost", "lost-read", "no-commit", "history-conflict"} {
		for _, operation := range []string{"terminal", "gc"} {
			t.Run(operation+"/"+fault, func(t *testing.T) { historyFault(t, nil, operation, fault) })
		}
	}
}

func historyFault(t *testing.T, kube client.Client, operation, fault string) {
	f := newCoordinationFixture(t, kube)
	ctx := context.Background()
	boot := strings.Repeat("ab", 32)

	f.generation(t, false)

	r, err := f.s.rolloutFor(ctx, f.index)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.s.persistPhase(ctx, "default", r, 2); err != nil {
		t.Fatal(err)
	}

	snapshot, _ := marshalSnapshot(f.index.snapshot(f.node))
	hash := sha256.Sum256(snapshot)

	digest := hex.EncodeToString(hash[:])
	if operation != "bind" {
		if _, _, err := f.s.catchup(ctx, r, "default", f.node, "pod-uid", boot, digest, 0); err != nil {
			t.Fatal(err)
		}
	}

	if operation == "terminal" {
		f.api.fault = fault
		if err := f.s.persistPhase(ctx, "default", r, 4); err == nil {
			t.Fatal("missing terminal write failure")
		}
	} else {
		if err := f.s.persistPhase(ctx, "default", r, 4); err != nil {
			t.Fatal(err)
		}

		f.generation(t, true)

		r, err = f.s.rolloutFor(ctx, f.index)
		if err != nil {
			t.Fatal(err)
		}

		f.api.fault = fault
		catchupRequest(t, f, boot, digest, 4, 503)
	}

	if !r.invalid {
		t.Fatal("uncertain history reused")
	}

	writes := f.api.writes
	if fault == "lost-read" {
		catchupRequest(t, f, boot, digest, 2, 503)
		catchupRequest(t, f, boot, digest, 2, 503)

		if f.api.writes != writes {
			t.Fatal("read failure authorized write")
		}
	}

	if operation == "terminal" {
		r, err = f.s.rolloutFor(ctx, f.index)
		if err != nil {
			t.Fatal(err)
		}

		if err := f.s.persistPhase(ctx, "default", r, 4); err != nil {
			t.Fatal(err)
		}

		f.generation(t, true)
	}
	// Restart loses all heartbeat/cache state, but not the removal obligation.
	f.s = &Server{controlStore: f.s.controlStore}
	if err := f.s.install(f.index); err != nil {
		t.Fatal(err)
	}

	c := catchupRequest(t, f, boot, digest, 2, 200)

	gcCommitted := operation == "gc" && (fault == "lost" || fault == "lost-read")
	if !gcCommitted && (c.Revision != 2 || c.Phase != 4) {
		t.Fatal("missed historical terminal", c)
	}

	if !gcCommitted {
		catchupRequest(t, f, strings.Repeat("cd", 32), digest, 4, 409) // heartbeat boot collision

		if got := catchupRequest(t, f, boot, digest, 4, 200); got.Revision != 3 || got.Phase != 1 {
			t.Fatal("retirement did not release latest", got)
		}
	}

	wantConflicts := 0
	if fault == "history-conflict" {
		wantConflicts = 1
	}

	if f.api.conflicts != wantConflicts {
		t.Fatal("stale history CAS retry")
	}
}

func TestB13HistoryIdentityBoundsAndMixedTargets(t *testing.T) {
	f := newCoordinationFixture(t, nil)
	f.generation(t, false)
	// A selected peer keeps the global barrier incomplete. The excluded
	// recipient must never obtain activation merely by reconnecting.
	f.index.g.Nodes["peer"] = member{ID: identity("node", "peer"), IP: "10.0.0.2", PodUID: "peer-pod"}
	f.index.byID[identity("node", "peer")] = "peer"
	boot := strings.Repeat("ab", 32)
	c := catchupRequest(t, f, boot, "", 0, 200)

	digest := hex.EncodeToString(c.SnapshotDigest)
	if c.Phase != 1 {
		t.Fatal("excluded ack bypassed selected preparation")
	}

	r := f.s.rollouts["default"]
	if err := f.s.persistPhase(context.Background(), "default", r, 2); err != nil {
		t.Fatal(err)
	}

	c = catchupRequest(t, f, boot, digest, 2, 200)
	if c.Phase != 2 {
		t.Fatal("excluded ack bypassed selected receive")
	}

	entries, err := removalHistory(r.pointer.Data["removals"], "default", 2)
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}

	for _, field := range []string{"pod", "boot", "node", "digest"} {
		pod, node, b, d := "pod-uid", f.node, boot, digest

		switch field {
		case "pod":
			pod = "other"
		case "boot":
			b = strings.Repeat("cd", 32)
		case "node":
			node = identity("node", "other")
		case "digest":
			d = strings.Repeat("00", 32)
		}
		// Historical boot mismatch (current revision may bind a second boot).
		oldRevision := r.revision
		r.revision++
		e, _, err := f.s.catchup(context.Background(), r, "default", node, pod, b, d, 4)
		r.revision = oldRevision

		if field == "digest" {
			if e != nil || err != nil {
				t.Fatal("unknown digest inherited authority")
			}
		} else if err == nil {
			t.Fatalf("%s accepted", field)
		}
	}

	full := make([]removalDecision, catchupLimit)
	for i := range full {
		full[i] = entries[0]
		full[i].Boot = fmt.Sprintf("%064x", i+1)
	}

	if _, err := encodeRemovals(full); err != nil {
		t.Fatal(err)
	}

	if _, err := encodeRemovals(append(full, entries[0])); err == nil {
		t.Fatal("unbounded history")
	}

	if err := f.s.saveRemovals(context.Background(), r, full); err != nil {
		t.Fatal(err)
	}

	writes := f.api.writes
	catchupRequest(t, f, boot, digest, 2, 503)

	if f.api.writes != writes {
		t.Fatal("capacity overflow attempted API write")
	}

	if err := f.s.saveRemovals(context.Background(), r, entries); err != nil {
		t.Fatal(err)
	}
	// Replacement changes the authenticated Pod identity and releases only
	// obligations for that Pod, without relying on wall-clock expiry.
	name := f.index.byID[f.node]
	m := f.index.g.Nodes[name]
	m.PodUID = "replacement"
	f.index.g.Nodes[name] = m

	raw, err := f.s.advanceRemovals("default", r, 1, r.pointer.Data["removals"])
	if err != nil {
		t.Fatal(err)
	}

	remaining, err := removalHistory(raw, "default", 2)
	if err != nil || len(remaining) != 1 || remaining[0].PodUID != "replacement" || remaining[0].Boot != "" {
		t.Fatal("Pod replacement retained inaccessible history", remaining, err)
	}

	full[0].Snapshot = make([]byte, catchupBytes)
	if _, err := encodeRemovals(full[:1]); err == nil {
		t.Fatal("unbounded bytes")
	}
}

func TestB13UndeliveredHistoryDoesNotAccumulate(t *testing.T) {
	f := newCoordinationFixture(t, nil)
	for i := 0; i < 8; i++ {
		f.generation(t, false)

		if busy, err := f.s.rolloutBusy(context.Background(), f.index); busy || err != nil {
			t.Fatal(busy, err)
		}

		entries, err := removalHistory(f.durable(t).Data["removals"], "default", f.index.g.Revision)
		if err != nil || len(entries) != 1 {
			t.Fatal("undelivered per-revision accumulation", entries, err)
		}
	}
}

func TestB13LostRetirementAckAndNewBoot(t *testing.T) {
	f := newCoordinationFixture(t, nil)
	ctx := context.Background()

	f.generation(t, false)

	r, err := f.s.rolloutFor(ctx, f.index)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.s.persistPhase(ctx, "default", r, 4); err != nil {
		t.Fatal(err)
	}

	raw, _ := marshalSnapshot(f.index.snapshot(f.node))
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])

	first, second := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	for _, boot := range []string{first, second} {
		if _, _, err := f.s.catchup(ctx, r, "default", f.node, "pod-uid", boot, digest, 0); err != nil {
			t.Fatal(err)
		}
	}

	f.generation(t, true)

	r, err = f.s.rolloutFor(ctx, f.index)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.s.collectRemovals(ctx, r, "default", f.node, first); err != nil {
		t.Fatal(err)
	}

	entries, err := removalHistory(r.pointer.Data["removals"], "default", r.revision)
	if err != nil || len(entries) != 1 || entries[0].Boot != second {
		t.Fatal("newer candidate collected another boot", entries, err)
	}

	if e, phase, err := f.s.catchup(ctx, r, "default", f.node, "pod-uid", second, digest, 2); err != nil || e == nil || phase != 4 {
		t.Fatal("remaining boot lost terminal", e, phase, err)
	}
}

// Catch-up admission: repeated boots, ambiguous commits and capacity recovery.

func TestB13ReviewRepeatedBootHandler(t *testing.T) {
	reviewRepeatedBoot(t, nil)
}

type reviewCommitLoss struct {
	client.Client
	lose bool
}

func (c *reviewCommitLoss) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.lose && obj.GetName() == stateName("default") {
		c.lose = false
		if err := c.Client.Update(ctx, obj.DeepCopyObject().(client.Object), opts...); err != nil {
			return err
		}

		return errors.New("lost topology commit response")
	}

	return c.Client.Update(ctx, obj, opts...)
}

func TestB13ReviewAmbiguousCandidateAdmission(t *testing.T) {
	ctx := context.Background()
	f := newCoordinationFixture(t, nil)
	f.generation(t, false)

	if busy, err := f.s.rolloutBusy(ctx, f.index); busy || err != nil {
		t.Fatal(busy, err)
	}

	_, _, svc := fixtures()
	if err := f.api.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}

	node := &corev1.Node{}
	if err := f.api.Get(ctx, client.ObjectKey{Name: "node"}, node); err != nil {
		t.Fatal(err)
	}

	node.Annotations = map[string]string{annotationPrefix + "fabric": "changed"}
	if err := f.api.Update(ctx, node); err != nil {
		t.Fatal(err)
	}

	api := &reviewCommitLoss{Client: f.api, lose: true}
	r := newTestReconciler(api)
	r.server = f.s

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("commit response loss not injected")
	}

	g, _, err := r.store.load(ctx, "default")
	if err != nil || g.Revision != 3 {
		t.Fatal(g, err)
	}

	catchupRequest(t, f, strings.Repeat("ab", 32), "", 0, 404)

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal("durable admission reload", err)
	}

	f.index, _ = indexGeneration(r.loaded["default"])
	catchupRequest(t, f, strings.Repeat("ab", 32), "", 0, 200)
}

func reviewRepeatedBoot(t *testing.T, kube client.Client) {
	f := newCoordinationFixture(t, kube)
	f.generation(t, false)

	a, b := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	c := catchupRequest(t, f, a, "", 0, 200)
	digest := hex.EncodeToString(c.SnapshotDigest)
	// Restart controller to discard the short heartbeat collision window, not
	// either durable boot obligation. B then repeatedly uses the real handler.
	f.s = &Server{controlStore: f.s.controlStore}
	if err := f.s.install(f.index); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		catchupRequest(t, f, b, digest, 2, 200)
	}

	entries, err := removalHistory(f.durable(t).Data["removals"], "default", 2)
	if err != nil || len(entries) != 2 {
		t.Fatal("boot binding not idempotent", entries, err)
	}

	f.s.rollouts = nil
	catchupRequest(t, f, b, digest, 4, 200)
}

func TestB13ReviewCapacityReconcile(t *testing.T) {
	reviewCapacityReconcile(t, nil)
}

// Inventory is deterministic; persistence and CAS can use envtest's real API.
type reviewInventory struct {
	client.Client
	inventory client.Client
}

func (c reviewInventory) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	// Historical recipient deletion proofs use GET, so they must see the same
	// deterministic Pod inventory as rollout liveness LISTs. Only persistence
	// and its resource-version CAS belong to the real API in this fixture.
	switch obj.(type) {
	case *corev1.Pod, *corev1.Node:
		return c.inventory.Get(ctx, key, obj, opts...)
	default:
		return c.Client.Get(ctx, key, obj, opts...)
	}
}

func (c reviewInventory) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.PodList); ok {
		return c.inventory.List(ctx, list, opts...)
	}

	if _, ok := list.(*corev1.NodeList); ok {
		return c.inventory.List(ctx, list, opts...)
	}

	return c.Client.List(ctx, list, opts...)
}

func reviewCapacityReconcile(t *testing.T, state client.Client) {
	ctx := context.Background()

	var objects []client.Object

	for i := 0; i < catchupLimit+1; i++ {
		n, p, _ := fixtures()
		n.Name = fmt.Sprintf("node-%d", i)
		n.UID = types.UID(n.Name)
		p.Name = fmt.Sprintf("pod-%d", i)
		p.UID = types.UID(p.Name)
		p.Spec.NodeName = n.Name
		p.Status.PodIP = fmt.Sprintf("10.1.%d.%d", i/250, i%250+1)
		objects = append(objects, n, p)
	}

	_, _, svc := fixtures()
	objects = append(objects, svc)
	kube := fakeKube(objects...)

	r := newTestReconciler(kube)
	if state != nil {
		r.store.client = reviewInventory{state, kube}
	}

	r.server.controlStore = r.store
	// Certificates identify each selected Pod as in the shared TLS harness.
	r.server.controlStore.client = tokenClient{r.store.client}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}

	g := r.loaded["default"]
	index, _ := indexGeneration(g)

	roll, err := r.server.rolloutFor(ctx, index)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.server.persistPhase(ctx, "default", roll, 4); err != nil {
		t.Fatal(err)
	}

	for _, m := range g.Nodes {
		roll.acks[m.ID] = rolloutAck{phase: 4}
	}

	if err := kube.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}

	for _, object := range objects {
		if node, ok := object.(*corev1.Node); ok {
			node.Labels[annotationPrefix+"exclude"] = "true"
			if err := kube.Update(ctx, node); err != nil {
				t.Fatal(err)
			}
		}
	}

	for i := 0; i < 2; i++ {
		_, err := r.Reconcile(ctx, req)

		durable, _, loadErr := r.store.load(ctx, "default")
		if err == nil || loadErr != nil || durable.Revision != 1 {
			t.Fatalf("capacity candidate committed before admission: err=%v load=%v revision=%d", err, loadErr, durable.Revision)
		}
	}
	// API replacement and selector correction must be reachable while the
	// inadmissible desired removal is rejected. Keep one new selected process.
	p := objects[1].(*corev1.Pod)
	if err := kube.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}

	p = p.DeepCopy()
	p.ResourceVersion = ""
	p.UID = "pod-uid"

	node := objects[0].(*corev1.Node)
	delete(node.Labels, annotationPrefix+"exclude")

	if err := kube.Update(ctx, node); err != nil {
		t.Fatal(err)
	}

	if err := kube.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	svc = svc.DeepCopy()
	svc.ResourceVersion = ""

	if err := kube.Create(ctx, svc); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal("replacement could not recover admission", err)
	}

	g = r.loaded["default"]
	if g.Revision != 2 || g.Nodes["node-0"].PodUID != "pod-uid" {
		t.Fatal("replacement not installed")
	}

	index, _ = indexGeneration(g)
	f := &coordinationFixture{s: r.server, api: &rolloutAPI{Client: r.server.controlStore.client}, index: index, node: g.Nodes["node-0"].ID}
	boot := strings.Repeat("ab", 32)
	c := catchupRequest(t, f, boot, "", 0, 200)

	digest := hex.EncodeToString(c.SnapshotDigest)
	for phase := uint32(1); phase <= 4; phase++ {
		catchupRequest(t, f, boot, digest, phase, 200)
	}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
}

func TestB13ReviewCumulativeBootCapacity(t *testing.T) {
	for _, recovery := range []string{"retirement", "replacement"} {
		t.Run(recovery, func(t *testing.T) {
			ctx := context.Background()
			f := newCoordinationFixture(t, nil)
			f.generation(t, false)

			first := fmt.Sprintf("%064x", 1)
			c := catchupRequest(t, f, first, "", 0, 200)
			digest := hex.EncodeToString(c.SnapshotDigest)

			roll := f.s.rollouts["default"]
			if err := f.s.persistPhase(ctx, "default", roll, 4); err != nil {
				t.Fatal(err)
			}

			entries, err := removalHistory(roll.pointer.Data["removals"], "default", 2)
			if err != nil {
				t.Fatal(err)
			}
			// Populate earlier boots, then exercise the final admission and overflow
			// through the real handler, including duplicate polling and durable reload.
			full := make([]removalDecision, catchupLimit-1)
			for i := range full {
				full[i] = entries[0]
				full[i].Boot = fmt.Sprintf("%064x", i+1)
			}

			if err := f.s.saveRemovals(ctx, roll, full); err != nil {
				t.Fatal(err)
			}

			last := fmt.Sprintf("%064x", catchupLimit)
			f.s.rollouts = nil
			catchupRequest(t, f, last, digest, 2, 200)
			catchupRequest(t, f, last, digest, 2, 200)
			f.s.rollouts = nil
			catchupRequest(t, f, fmt.Sprintf("%064x", catchupLimit+1), digest, 2, 503)
			f.s.rollouts = nil
			catchupRequest(t, f, last, digest, 2, 200) // overflow does not poison existing delivery

			_, _, svc := fixtures()
			if err := f.api.Delete(ctx, svc); err != nil {
				t.Fatal(err)
			}

			node := &corev1.Node{}
			if err := f.api.Get(ctx, client.ObjectKey{Name: "node"}, node); err != nil {
				t.Fatal(err)
			}

			node.Annotations = map[string]string{annotationPrefix + "fabric": "changed"}

			node.Status.Conditions[0].Status = corev1.ConditionFalse
			if err := f.api.Update(ctx, node); err != nil {
				t.Fatal(err)
			}

			node.Status.Conditions[0].Status = corev1.ConditionFalse
			if err := f.api.Status().Update(ctx, node); err != nil {
				t.Fatal(err)
			}

			r := newTestReconciler(f.api)
			r.server = f.s

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
			if _, err := r.Reconcile(ctx, req); err == nil {
				t.Fatal("cumulative bound not admitted before commit")
			}

			g, _, err := r.store.load(ctx, "default")
			if err != nil || g.Revision != 2 {
				t.Fatal("cumulative overflow committed", g, err)
			}

			if recovery == "retirement" {
				catchupRequest(t, f, last, digest, 4, 200)
			} else {
				_, p, svc := fixtures()

				node.Status.Conditions[0].Status = corev1.ConditionTrue
				if err := f.api.Status().Update(ctx, node); err != nil {
					t.Fatal(err)
				}

				if err := f.api.Delete(ctx, p); err != nil {
					t.Fatal(err)
				}

				p.UID = "replacement"
				if err := f.api.Create(ctx, p); err != nil {
					t.Fatal(err)
				}

				if err := f.api.Create(ctx, svc); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal("full ledger recovery failed", err)
			}

			if r.loaded["default"].Revision != 3 {
				t.Fatal("capacity not reclaimed")
			}
		})
	}
}

// Forward recovery: durable intent, boot binding, grants and capacity.

func forwardFixture(t *testing.T) (*coordinationFixture, *rollout, string) {
	t.Helper()
	f := newCoordinationFixture(t, nil)

	r, err := f.s.rolloutFor(context.Background(), f.index)
	if err != nil {
		t.Fatal(err)
	}

	if err = f.s.persistPhase(context.Background(), "default", r, 2); err != nil {
		t.Fatal(err)
	}

	n, p, _ := fixtures()
	p.UID = "pod-uid"

	g, _, err := buildGeneration("default", f.index.g, []corev1.Node{*n}, []corev1.Pod{*p}, nil)
	if err != nil {
		t.Fatal(err)
	}

	next, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	if err = f.s.planForward(context.Background(), f.index, next); err != nil {
		t.Fatal(err)
	}

	data, _ := marshalSnapshot(f.index.snapshot(f.node))
	hash := sha256.Sum256(data)

	f.generation(t, false)

	r, err = f.s.rolloutFor(context.Background(), f.index)
	if err != nil {
		t.Fatal(err)
	}

	return f, r, hex.EncodeToString(hash[:])
}

func TestB15ForwardDurability(t *testing.T) {
	for _, op := range []string{"bind", "grant", "collect"} {
		for _, fault := range []string{"lost", "lost-read", "no-commit", "history-conflict"} {
			t.Run(op+"/"+fault, func(t *testing.T) {
				f, r, digest := forwardFixture(t)
				ctx := context.Background()
				boot := strings.Repeat("ab", 32)

				hold := ""
				if op == "grant" {
					hold = digest
				}

				if op == "collect" {
					if _, _, _, err := f.s.forward(ctx, r, "default", f.node, "pod-uid", boot, digest, digest, 0); err != nil {
						t.Fatal(err)
					}
				}

				f.api.fault = fault

				act := func(r *rollout) error {
					if op == "collect" {
						return f.s.collectForwards(ctx, r, "default", f.node, "pod-uid", boot)
					}

					_, _, _, err := f.s.forward(ctx, r, "default", f.node, "pod-uid", boot, digest, hold, 0)

					return err
				}
				if err := act(r); err == nil || !r.invalid {
					t.Fatal("uncertain write did not fail closed")
				}

				if err := act(r); err == nil {
					t.Fatal("invalid handle permitted authority")
				}

				for i := 0; i < 4; i++ {
					var err error

					r, err = f.s.rolloutFor(ctx, f.index)
					if err == nil {
						break
					}
				}

				if r == nil || r.invalid {
					t.Fatal("readback failed")
				}

				if err := act(r); err != nil {
					t.Fatal(err)
				}

				ds, err := forwardHistory(f.durable(t).Data["forwards"], "default", 2)
				if err != nil {
					t.Fatal(err)
				}

				want := 2
				if op == "collect" {
					want = 1
				}

				if len(ds) != want {
					t.Fatalf("history %d want %d", len(ds), want)
				}

				if f.api.hits != 1 {
					t.Fatal("fault not hit")
				}
			})
		}
	}
}

func TestB15IntentWriteRecovery(t *testing.T) {
	for _, fault := range []string{"lost", "lost-read", "no-commit", "history-conflict"} {
		t.Run(fault, func(t *testing.T) {
			f := newCoordinationFixture(t, nil)
			ctx := context.Background()

			r, err := f.s.rolloutFor(ctx, f.index)
			if err != nil {
				t.Fatal(err)
			}

			if err = f.s.persistPhase(ctx, "default", r, 2); err != nil {
				t.Fatal(err)
			}

			n, p, _ := fixtures()
			p.UID = "pod-uid"

			g, _, err := buildGeneration("default", f.index.g, []corev1.Node{*n}, []corev1.Pod{*p}, nil)
			if err != nil {
				t.Fatal(err)
			}

			next, _ := indexGeneration(g)

			f.api.fault = fault
			if err = f.s.planForward(ctx, f.index, next); err == nil {
				t.Fatal("intent fault did not fire")
			}

			for i := 0; i < 4; i++ {
				err = f.s.planForward(ctx, f.index, next)
				if err == nil {
					break
				}
			}

			if err != nil {
				t.Fatal(err)
			}

			if f.durable(t).Data["phase"] != "2" {
				t.Fatal("intent advanced old decision")
			}

			data, _ := marshalSnapshot(f.index.snapshot(f.node))
			h := sha256.Sum256(data)
			r = f.s.rollouts["default"]

			e, grant, _, err := f.s.forward(ctx, r, "default", f.node, "pod-uid", strings.Repeat("ab", 32), hex.EncodeToString(h[:]), hex.EncodeToString(h[:]), 0)
			if err != nil || e != nil || len(grant) > 0 {
				t.Fatal("uncommitted successor granted authority")
			}
		})
	}
}

func TestB15FirstBootCapacityReserved(t *testing.T) {
	f, r, digest := forwardFixture(t)

	ds, _ := forwardHistory(r.pointer.Data["forwards"], "default", 2)
	if len(ds) != 1 {
		t.Fatal("missing wildcard")
	}

	full := make([]forwardDecision, catchupLimit)
	for i := range full {
		full[i] = ds[0]
	}

	if _, err := encodeForwards(full); err == nil {
		t.Fatal("wildcards admitted without boot reservations")
	}

	_, _, _, err := f.s.forward(context.Background(), r, "default", f.node, "pod-uid", strings.Repeat("ab", 32), digest, digest, 0)
	if err != nil {
		t.Fatal("reserved first boot failed", err)
	}
}

func TestB15HistorySurvivesChunkGC(t *testing.T) {
	f, r, digest := forwardFixture(t)
	ctx := context.Background()

	var m manifest
	if err := json.Unmarshal([]byte(r.pointer.Data["serving"]), &m); err != nil {
		t.Fatal(err)
	}

	if len(m.Chunks) == 0 {
		t.Fatal("fixture did not retain old serving manifest")
	}

	boot := strings.Repeat("ab", 32)
	if _, _, _, err := f.s.forward(ctx, r, "default", f.node, "pod-uid", boot, digest, "", 0); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if busy, err := f.s.rolloutBusy(ctx, f.index); err != nil || busy {
			t.Fatal(busy, err)
		}

		f.generation(t, false)
	}

	for _, name := range m.Chunks {
		if err := f.api.Client.Get(ctx, client.ObjectKey{Namespace: "state", Name: name}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			t.Fatal("old chunk survived GC", err)
		}
	}

	f.s = &Server{controlStore: f.s.controlStore}
	if err := f.s.install(f.index); err != nil {
		t.Fatal(err)
	}

	r, err := f.s.rolloutFor(ctx, f.index)
	if err != nil {
		t.Fatal(err)
	}

	e, grant, _, err := f.s.forward(ctx, r, "default", f.node, "pod-uid", boot, digest, "", 2)
	if err != nil || e == nil || len(grant) != 0 {
		t.Fatal("old receive obligation lost after GC", err)
	}

	h := sha256.Sum256(e.snapshot)
	if hex.EncodeToString(h[:]) != digest {
		t.Fatal("historical bytes changed")
	}
}

func TestB15PhaseZeroAndBootAreNotFences(t *testing.T) {
	f, r, digest := forwardFixture(t)
	ctx := context.Background()

	for _, boot := range []string{strings.Repeat("ab", 32), strings.Repeat("cd", 32)} {
		for i := 0; i < 3; i++ {
			e, grant, _, err := f.s.forward(ctx, r, "default", f.node, "pod-uid", boot, digest, "", 0)
			if err != nil || e == nil || len(grant) != 0 {
				t.Fatalf("ambiguous phase 0 discarded receive: %v", err)
			}
		}
	}

	ds, _ := forwardHistory(r.pointer.Data["forwards"], "default", 2)
	if len(ds) != 3 {
		t.Fatalf("boot obligations lost or duplicated: %d", len(ds))
	}

	for _, binding := range []string{"node", "pod", "digest"} {
		n, p, d := f.node, "pod-uid", digest

		switch binding {
		case "node":
			n = strings.Repeat("ff", 32)
		case "pod":
			p = "another"
		case "digest":
			d = strings.Repeat("ff", 32)
		}

		e, grant, _, err := f.s.forward(ctx, r, "default", n, p, strings.Repeat("ab", 32), d, d, 0)
		if err != nil || e != nil || len(grant) != 0 {
			t.Fatalf("bad binding %s granted", binding)
		}
	}
}

func TestB15NotReadyStalePod(t *testing.T) {
	for _, phase := range []uint32{2, 3, 4} {
		t.Run(fmt.Sprint(phase), func(t *testing.T) {
			f := newCoordinationFixture(t, nil)
			ctx := context.Background()

			r, err := f.s.rolloutFor(ctx, f.index)
			if err != nil {
				t.Fatal(err)
			}

			if err = f.s.persistPhase(ctx, "default", r, phase); err != nil {
				t.Fatal(err)
			}

			n, _, _ := fixtures()
			if err = f.api.Client.Get(ctx, client.ObjectKeyFromObject(n), n); err != nil {
				t.Fatal(err)
			}

			n.Status.Conditions[0].Status = corev1.ConditionFalse
			if err = f.api.Client.Status().Update(ctx, n); err != nil {
				t.Fatal(err)
			}

			f.s = &Server{controlStore: f.s.controlStore}
			rec := newTestReconciler(f.api)

			rec.server, rec.store = f.s, f.s.controlStore
			if _, err = rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err != nil {
				t.Fatal(err)
			}

			g := rec.loaded["default"]
			if g.Revision != 2 || g.Nodes["node"].IP != "" {
				t.Fatal("NotReady stale Running Pod blocked correction")
			}

			if f.durable(t).Data["forwards"] == "" {
				t.Fatal("NotReady discarded old authority")
			}
		})
	}
}

func TestB15CapacityBeforeCommit(t *testing.T) {
	f := newCoordinationFixture(t, nil)
	ctx := context.Background()

	r, err := f.s.rolloutFor(ctx, f.index)
	if err != nil {
		t.Fatal(err)
	}

	if err = f.s.persistPhase(ctx, "default", r, 2); err != nil {
		t.Fatal(err)
	}

	data, _ := marshalSnapshot(f.index.snapshot(f.node))

	ds := make([]forwardDecision, catchupLimit)
	for i := range ds {
		ds[i] = forwardDecision{Snapshot: data, PodUID: "pod-uid", Boot: fmt.Sprintf("%064x", i+1)}
	}
	// Byte bound may be stricter than count bound for nonempty snapshots.
	for {
		if _, err = encodeForwards(ds); err == nil {
			break
		}

		ds = ds[:len(ds)-1]
	}

	if err = f.s.saveForwards(ctx, r, ds); err != nil {
		t.Fatal(err)
	}

	large := append([]forwardDecision(nil), ds...)
	for i := 0; i < catchupLimit; i++ {
		large = append(large, forwardDecision{Snapshot: data, PodUID: "pod-uid", Boot: fmt.Sprintf("%064x", i+1000)})
	}

	if err = f.s.saveForwards(ctx, r, large); err == nil {
		t.Fatal("capacity admitted")
	}

	if f.durable(t).Data["revision"] != "1" {
		t.Fatal("capacity advanced topology")
	}
	// Exercise actual Reconcile, not only the encoder: a full ledger cannot
	// admit an additional old recipient, even when its payload can be chunked.
	f.index.g.Volume.ID = strings.Repeat("x", forwardBytes)

	_, pointer, _ := f.s.controlStore.load(ctx, "default")
	if err = f.s.controlStore.commit(ctx, f.index.g, pointer); err != nil {
		t.Fatal(err)
	}

	_, _, svc := fixtures()
	if err = f.api.Delete(ctx, svc); err != nil {
		t.Fatal(err)
	}

	f.s = &Server{controlStore: f.s.controlStore}
	rec := newTestReconciler(f.api)

	rec.server, rec.store = f.s, f.s.controlStore
	if _, err = rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err == nil {
		t.Fatal("full forward ledger admitted another obligation")
	}

	g, _, err := f.s.controlStore.load(ctx, "default")
	if err != nil || g.Revision != 1 {
		t.Fatal("over-capacity candidate changed commit point", err)
	}
}

// Forward selection and history collection must follow the topology commit.

// A successful ledger write followed by a failed topology pointer write.
type forwardCommitFailure struct {
	client.Client
	armed                       bool
	ledgerWrites, failedCommits int
}

func (c *forwardCommitFailure) Update(ctx context.Context, o client.Object, opts ...client.UpdateOption) error {
	if c.armed && o.GetName() == stateName("default") {
		c.failedCommits++
		return errors.New("injected topology no-commit after forward ledger persistence")
	}

	err := c.Client.Update(ctx, o, opts...)
	if c.armed && err == nil && o.GetName() == stateName("default")+"-rollout" {
		c.ledgerWrites++
	}

	return err
}

func TestB15ReviewSelectionBeforeCommit(t *testing.T) {
	for _, mode := range []string{"crash-after-ledger", "failed-commit"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f, r, digest := forwardFixture(t) // P is still entitled to old R1 terminal delivery at R2.

			boot := strings.Repeat("ab", 32)
			if _, _, _, err := f.s.forward(ctx, r, "default", f.node, "pod-uid", boot, digest, "", 2); err != nil {
				t.Fatal(err)
			}

			f.generation(t, true) // R3 selects the same P; do not deliver P's outstanding R1.

			r, err := f.s.rolloutFor(ctx, f.index)
			if err != nil {
				t.Fatal(err)
			}

			if err = f.s.persistPhase(ctx, "default", r, 2); err != nil {
				t.Fatal(err)
			}
			// Include one already-inaccessible entry so even the fixed planner must
			// successfully write the ledger before the simulated crash/commit error.
			ds, err := forwardHistory(r.pointer.Data["forwards"], "default", r.revision)
			if err != nil {
				t.Fatal(err)
			}

			inaccessible := ds[0]

			inaccessible.Snapshot, err = f.s.controlStore.readForwardSnapshot(ctx, "default", inaccessible)
			if err != nil {
				t.Fatal(err)
			}

			inaccessible.Ref = nil

			var snap pb.Snapshot
			if err = proto.Unmarshal(inaccessible.Snapshot, &snap); err != nil {
				t.Fatal(err)
			}

			snap.Node = bytes.Repeat([]byte{0xee}, 32)

			inaccessible.Snapshot, err = marshalSnapshot(&snap)
			if err != nil {
				t.Fatal(err)
			}

			if err = f.s.saveForwards(ctx, r, append(ds, inaccessible)); err != nil {
				t.Fatal(err)
			}

			before := f.durable(t).Data["forwards"]
			n, p, svc := fixtures()
			p.UID = "Q"

			next, _, err := buildCacheFixture("default", f.index.g, []corev1.Node{*n}, []corev1.Pod{*p}, svc)
			if err != nil {
				t.Fatal(err)
			}

			next.Revision++

			target, err := indexGeneration(next)
			if err != nil {
				t.Fatal(err)
			}

			fault := &forwardCommitFailure{Client: f.s.controlStore.client, armed: true}

			f.s.controlStore.client = fault
			if mode == "crash-after-ledger" {
				if err = f.s.planForward(ctx, f.index, target); err != nil {
					t.Fatal(err)
				}
			} else {
				rec := newTestReconciler(fault)
				rec.server, rec.store = f.s, f.s.controlStore

				_, rec.pointers["default"], err = rec.store.load(ctx, "default")
				if err != nil {
					t.Fatal(err)
				}

				if err = rec.commitCandidate(ctx, target); err == nil || fault.failedCommits != 1 {
					t.Fatal("no-commit fault not reached", err)
				}
			}
			// Simulate controller crash; Q never became selected, then disappears.
			fault.armed = false
			if fault.ledgerWrites < 1 {
				t.Fatal("successful ledger write before interruption not exercised")
			}

			g, _, err := f.s.controlStore.load(ctx, "default")
			if err != nil || g.Revision != 3 || g.Nodes["node"].PodUID != "pod-uid" {
				t.Fatal("unexpected durable selection", g, err)
			}

			after := f.durable(t).Data["forwards"]
			if !strings.Contains(after, `"podUID":"pod-uid"`) {
				t.Errorf("pre-commit ledger deleted P obligations: before=%s after=%s", before, after)
			}

			f.s = &Server{controlStore: f.s.controlStore}
			rec := newTestReconciler(fault)
			rec.server, rec.store = f.s, f.s.controlStore
			// Desired inventory was never committed to Q: real reconcile reloads R3/P.
			if _, err = rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest("GET", "/", nil)
			req.SetPathValue("universe", identity("universe", "default"))
			req.SetPathValue("node", f.node)
			controlTLS(req, "pod-uid")
			req.Header.Set("X-Racer-Profile", "1")
			req.Header.Set("X-Racer-Boot", boot)
			req.Header.Set("X-Racer-Digest", digest)
			req.Header.Set("X-Racer-Phase", "2")

			rr := httptest.NewRecorder()
			f.s.control(rr, req)

			var command pb.ControlCommand

			_ = proto.Unmarshal(rr.Body.Bytes(), &command)
			if rr.Code != 200 || command.Revision != 1 || command.Phase != 4 || hex.EncodeToString(command.SnapshotDigest) != digest {
				t.Fatalf("P reconnect lost old obligation after %s: HTTP=%d revision=%d phase=%d", mode, rr.Code, command.Revision, command.Phase)
			}

			t.Logf("%s preserved P; successful ledger writes=%d failed topology commits=%d", mode, fault.ledgerWrites, fault.failedCommits)
		})
	}
}

func TestB15ReviewReplacementCapacity(t *testing.T) {
	ctx := context.Background()
	f, r, _ := forwardFixture(t)

	ds, err := forwardHistory(r.pointer.Data["forwards"], "default", r.revision)
	if err != nil {
		t.Fatal(err)
	}
	// Fill to the real byte/entry admission boundary using distinct old boots.
	full := []forwardDecision{}

	for i := 1; i <= catchupLimit; i++ {
		d := ds[0]

		d.Boot = fmt.Sprintf("%064x", i)
		if _, err := encodeForwards(append(full, d)); err != nil {
			break
		}

		full = append(full, d)
	}

	if err = f.s.saveForwards(ctx, r, full); err != nil {
		t.Fatal(err)
	}

	n, p, svc := fixtures()
	p.UID = "Q"

	next, _, err := buildCacheFixture("default", f.index.g, []corev1.Node{*n}, []corev1.Pod{*p}, svc)
	if err != nil {
		t.Fatal(err)
	}

	target, _ := indexGeneration(next)

	if err = f.s.persistPhase(ctx, "default", r, 4); err != nil {
		t.Fatal(err)
	}

	before := f.durable(t).Data["forwards"]
	if err = f.s.planForward(ctx, f.index, target); err != nil {
		t.Fatal(err)
	}

	if got := f.durable(t).Data["forwards"]; got != before {
		t.Fatalf("replacement proposal reclaimed %d old obligations before commit", len(full))
	}
	// A committed replacement is allowed to reclaim P, without increasing caps.
	next.Revision++
	rec := newTestReconciler(f.api)
	rec.server, rec.store = f.s, f.s.controlStore

	_, rec.pointers["default"], err = rec.store.load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}

	if err = rec.commitCandidate(ctx, target); err != nil {
		t.Fatal(err)
	}

	f.s = &Server{controlStore: f.s.controlStore}
	if err = f.s.install(target); err != nil {
		t.Fatal(err)
	}

	r, err = f.s.rolloutFor(ctx, target)
	if err != nil {
		t.Fatal(err)
	}

	kept, err := forwardHistory(r.pointer.Data["forwards"], "default", next.Revision)
	if err != nil || len(kept) != 0 {
		t.Fatalf("committed replacement did not reclaim history: %d %v", len(kept), err)
	}
}

func TestB15ReviewPostCommitGCUncertainty(t *testing.T) {
	for _, fault := range []string{"lost", "lost-read", "no-commit", "history-conflict"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			f, r, _ := forwardFixture(t)
			n, p, svc := fixtures()
			p.UID = "Q"

			g, _, err := buildCacheFixture("default", f.index.g, []corev1.Node{*n}, []corev1.Pod{*p}, svc)
			if err != nil {
				t.Fatal(err)
			}

			g.Revision++
			target, _ := indexGeneration(g)

			if err = f.s.persistPhase(ctx, "default", r, 4); err != nil {
				t.Fatal(err)
			}

			rec := newTestReconciler(f.api)
			rec.server, rec.store = f.s, f.s.controlStore

			_, rec.pointers["default"], err = rec.store.load(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}

			if err = rec.commitCandidate(ctx, target); err != nil {
				t.Fatal(err)
			}

			f.api.fault = fault
			if _, err = f.s.rolloutFor(ctx, target); err == nil {
				t.Fatal("GC uncertainty did not fail closed")
			}

			for i := 0; i < 4; i++ {
				r, err = f.s.rolloutFor(ctx, target)
				if err == nil {
					break
				}
			}

			if err != nil {
				t.Fatal(err)
			}

			ds, err := forwardHistory(r.pointer.Data["forwards"], "default", g.Revision)
			if err != nil || len(ds) != 0 {
				t.Fatal("post-commit GC did not recover", err)
			}

			if f.api.hits != 1 {
				t.Fatal("fault not hit")
			}
		})
	}
}

func TestB15ReviewRetainedHistoryStillCountsAtAdmission(t *testing.T) {
	ctx := context.Background()
	f, r, _ := forwardFixture(t)

	ds, err := forwardHistory(r.pointer.Data["forwards"], "default", r.revision)
	if err != nil {
		t.Fatal(err)
	}

	full := make([]forwardDecision, catchupLimit)
	for i := range full {
		full[i] = ds[0]
		full[i].Boot = fmt.Sprintf("%064x", i+1)
	}

	if err = f.s.saveForwards(ctx, r, full); err != nil {
		t.Fatal(err)
	}

	f.generation(t, true)

	r, err = f.s.rolloutFor(ctx, f.index)
	if err != nil {
		t.Fatal(err)
	}

	if err = f.s.persistPhase(ctx, "default", r, 2); err != nil {
		t.Fatal(err)
	}

	before := f.durable(t).Data["forwards"]
	n, p, _ := fixtures()
	p.UID = "pod-uid"

	g, _, err := buildGeneration("default", f.index.g, []corev1.Node{*n}, []corev1.Pod{*p}, nil)
	if err != nil {
		t.Fatal(err)
	}

	g.Revision++
	target, _ := indexGeneration(g)
	rec := newTestReconciler(f.api)
	rec.server, rec.store = f.s, f.s.controlStore

	_, rec.pointers["default"], err = rec.store.load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}

	if err = rec.commitCandidate(ctx, target); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatal("retained history did not backpressure admission", err)
	}

	durable, _, err := rec.store.load(ctx, "default")
	if err != nil || durable.Revision != 3 || f.durable(t).Data["forwards"] != before {
		t.Fatal("capacity failure changed history/commit", err)
	}
}

// Forward payload storage: default geometry, immutable chunks and GC integrity.

// Real default geometry, including production Reconcile's pre-commit planner.
func defaultForwardFixture(t *testing.T) (*coordinationFixture, *reconciler, *corev1.Node) {
	t.Helper()

	ctx := context.Background()
	n, p, svc := fixtures()
	p.UID = "pod-uid"
	n2, p2 := n.DeepCopy(), p.DeepCopy()
	n2.Name, n2.UID = "second", "second-node"
	p2.Name, p2.UID, p2.Spec.NodeName, p2.Status.PodIP = "second", "second-pod", "second", "10.1.1.2"

	api := &rolloutAPI{Client: tokenClient{fakeKube(n, n2, p, p2, svc)}}
	s := &Server{controlStore: stateStore{api, "state"}}
	rec := newTestReconciler(api)

	rec.server, rec.store = s, s.controlStore
	if _, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err != nil {
		t.Fatal(err)
	}

	index, err := indexGeneration(rec.loaded["default"])
	if err != nil || index.g.Volume.Slots != defaultSlots || len(index.g.Nodes) != 2 {
		t.Fatal("not default two-node geometry", err)
	}

	r, err := s.rolloutFor(ctx, index)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.persistPhase(ctx, "default", r, 2); err != nil {
		t.Fatal(err)
	}

	return &coordinationFixture{s: s, api: api, index: index, node: index.g.Nodes[n.Name].ID}, rec, n
}

func TestForwardStorageDefaultCorrection(t *testing.T) {
	ctx := context.Background()
	f, rec, n := defaultForwardFixture(t)

	old, err := marshalSnapshot(f.index.snapshot(f.node))
	if err != nil || len(old) < 4*1024*1024 {
		t.Fatal("fixture does not exercise multi-MiB payload", len(old), err)
	}

	h := sha256.Sum256(old)
	digest := hex.EncodeToString(h[:])
	// A NotReady node with an API-stale Running Pod keeps its old Pod UID.
	if err := f.api.Get(ctx, client.ObjectKeyFromObject(n), n); err != nil {
		t.Fatal(err)
	}

	n.Status.Conditions[0].Status = corev1.ConditionFalse
	if err := f.api.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}

	if _, err := rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err != nil {
		t.Fatal("default correction stalled", err)
	}

	g := rec.loaded["default"]
	if g.Revision != 2 || g.Nodes[n.Name].IP != "" || g.Nodes[n.Name].PodUID != "pod-uid" {
		t.Fatal("correction lost retained Pod binding", g.Nodes[n.Name])
	}

	raw := f.durable(t).Data["forwards"]
	if strings.Contains(raw, `"snapshot"`) || len(raw) > forwardBytes {
		t.Fatal("inline/oversized ledger", len(raw))
	}

	t.Logf("snapshot=%d bytes, two-recipient ledger=%d bytes", len(old), len(raw))

	var chunks corev1.ConfigMapList
	if err := f.api.List(ctx, &chunks, client.MatchingLabels{stateLabel: "forward"}); err != nil || len(chunks.Items) != 20 {
		t.Fatal("expected ten bounded chunks per recipient", len(chunks.Items), err)
	}

	for _, part := range chunks.Items {
		if len(part.BinaryData["snapshot"]) > stateChunkSize || part.Immutable == nil || !*part.Immutable {
			t.Fatal("unbounded or mutable snapshot chunk")
		}
	}

	index, _ := indexGeneration(g)
	// Repeated topology GC ages out the original topology chunks. Forward data
	// must remain independently readable even after every controller object dies.
	var original manifest

	_ = json.Unmarshal([]byte(f.durable(t).Data["serving"]), &original)
	for i := 0; i < 3; i++ {
		if err := f.s.install(index); err != nil {
			t.Fatal(err)
		}

		r, err := f.s.rolloutFor(ctx, index)
		if err != nil {
			t.Fatal(err)
		}

		if err := f.s.persistPhase(ctx, "default", r, 2); err != nil {
			t.Fatal(err)
		}

		current, pointer, err := rec.store.load(ctx, "default")
		if err != nil {
			t.Fatal(err)
		}

		current.Revision++
		if err := rec.store.commit(ctx, current, pointer); err != nil {
			t.Fatal(err)
		}

		index, _ = indexGeneration(current)
	}

	for _, name := range original.Chunks {
		if err := f.api.Get(ctx, client.ObjectKey{Namespace: "state", Name: name}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			t.Fatal("old topology not GC'd", err)
		}
	}

	f.s = &Server{controlStore: rec.store}
	if err := f.s.install(index); err != nil {
		t.Fatal(err)
	}
	// Exercise authenticated production delivery and exact snapshot bytes.
	req := httptest.NewRequest("GET", "/", nil)
	req.SetPathValue("universe", identity("universe", "default"))
	req.SetPathValue("node", f.node)
	controlTLS(req, "pod-uid")
	req.Header.Set("X-Racer-Profile", "1")
	req.Header.Set("X-Racer-Boot", strings.Repeat("ab", 32))
	req.Header.Set("X-Racer-Digest", digest)
	req.Header.Set("X-Racer-Phase", "2")
	req.Header.Set("X-Racer-Needs-Config", "1")

	w := httptest.NewRecorder()
	f.s.control(w, req)

	var command pb.ControlCommand

	_ = proto.Unmarshal(w.Body.Bytes(), &command)
	if w.Code != 200 || command.Revision != 1 || command.Phase != 4 || command.PodUid != "pod-uid" || !bytes.Equal(command.SnapshotDigest, h[:]) || !bytes.Equal(configurationSnapshot(t, command.Configuration), old) {
		t.Fatalf("historical delivery changed bytes/bindings: HTTP=%d revision=%d phase=%d", w.Code, command.Revision, command.Phase)
	}
	// Exact pre-receive capability also survives restart and is persisted first.
	r := f.s.rollouts["default"]

	_, grant, revision, err := f.s.forward(ctx, r, "default", f.node, "pod-uid", strings.Repeat("cd", 32), digest, digest, 0)
	if err != nil || !bytes.Equal(grant, h[:]) || revision != 1 {
		t.Fatal("conditional grant", err)
	}

	ds, err := forwardHistory(f.durable(t).Data["forwards"], "default", index.g.Revision)
	if err != nil || len(ds) != 4 || ds[3].Grant != index.g.Revision {
		t.Fatal("grant not durable", err)
	}
}

type forwardChunkFault struct {
	client.Client
	mode string
	hits int
}

func (c *forwardChunkFault) Create(ctx context.Context, o client.Object, opts ...client.CreateOption) error {
	if o.GetLabels()[stateLabel] == "forward" && c.mode != "" {
		mode := c.mode
		c.mode = ""

		c.hits++
		if mode == "lost" {
			if err := c.Client.Create(ctx, o, opts...); err != nil {
				return err
			}
		}

		return errors.New("injected chunk create failure")
	}

	return c.Client.Create(ctx, o, opts...)
}

func TestForwardStorageChunkFailures(t *testing.T) {
	for _, mode := range []string{"lost", "no-commit", "ledger-lost", "ledger-lost-read", "ledger-no-commit", "ledger-history-conflict"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f, rec, _ := defaultForwardFixture(t)
			r := f.s.rollouts["default"]
			data, _ := marshalSnapshot(f.index.snapshot(f.node))
			ds := []forwardDecision{{Snapshot: data, PodUID: "pod-uid"}}

			fault := &forwardChunkFault{Client: f.api, mode: mode}
			if strings.HasPrefix(mode, "ledger-") {
				fault.mode = ""
				f.api.fault = strings.TrimPrefix(mode, "ledger-")
			}

			f.s.controlStore.client = fault
			if err := f.s.saveForwards(ctx, r, ds); err == nil || !r.invalid || fault.hits+f.api.hits != 1 {
				t.Fatal("chunk error did not invalidate", err)
			}

			if (!strings.HasPrefix(mode, "ledger-lost") && f.durable(t).Data["forwards"] != "") || rec.loaded["default"].Revision != 1 {
				t.Fatal("partial payload exposed")
			}

			if err := f.s.saveForwards(ctx, r, ds); err == nil {
				t.Fatal("stale decision retried")
			}

			r, err := f.s.rolloutFor(ctx, f.index)
			for i := 0; err != nil && i < 4; i++ {
				r, err = f.s.rolloutFor(ctx, f.index)
			}

			if err != nil {
				t.Fatal(err)
			}

			if err := f.s.saveForwards(ctx, r, ds); err != nil {
				t.Fatal("retry did not verify/reuse chunks", err)
			}

			refs, _ := forwardHistory(r.pointer.Data["forwards"], "default", 1)

			got, err := f.s.controlStore.readForwardSnapshot(ctx, "default", refs[0])
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("retry changed data", err)
			}
		})
	}
}

func TestForwardStorageRejectsInlineHistory(t *testing.T) {
	ctx := context.Background()
	f, r, _ := forwardFixture(t)
	ds, _ := forwardHistory(r.pointer.Data["forwards"], "default", 2)

	data, err := f.s.controlStore.readForwardSnapshot(ctx, "default", ds[0])
	if err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal([]map[string]any{{"snapshot": data, "podUID": "pod-uid"}})
	cm := f.durable(t)

	cm.Data["forwards"] = string(raw)
	if err := f.api.Client.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}

	f.s.rollouts = nil
	if _, err := f.s.rolloutFor(ctx, f.index); err == nil {
		t.Fatal("inline history accepted")
	}
}

func TestForwardStorageBoundsAndIntegrity(t *testing.T) {
	ctx := context.Background()
	f, r, digest := forwardFixture(t)
	ds, _ := forwardHistory(r.pointer.Data["forwards"], "default", 2)

	original, err := f.s.controlStore.readForwardSnapshot(ctx, "default", ds[0])
	if err != nil {
		t.Fatal(err)
	}

	ref := *ds[0].Ref
	for _, size := range []int{0, -1, forwardSnapshotBytes + 1} {
		bad := ref
		bad.Size = size

		raw, _ := json.Marshal([]forwardDecision{{Ref: &bad, PodUID: "pod-uid"}})
		if _, err := forwardHistory(string(raw), "default", 2); err == nil {
			t.Fatal("unbounded reference read", size)
		}
	}

	full := []forwardDecision{}

	const payloads = forwardPayloadBytes / forwardSnapshotBytes
	for i := 0; i <= payloads; i++ {
		x := ref
		x.Digest, x.Size = fmt.Sprintf("%064x", i), forwardSnapshotBytes
		full = append(full, forwardDecision{Ref: &x, PodUID: "pod-uid"})
	}

	if _, err := encodeForwards(full[:payloads]); err != nil {
		t.Fatal("payload boundary rejected", err)
	}

	if _, err := encodeForwards(full); err == nil {
		t.Fatal("aggregate payload cap ignored")
	}
	// 512 boots share bytes; wildcard reservations still consume metadata slots.
	boots := make([]forwardDecision, catchupLimit)
	for i := range boots {
		boots[i] = forwardDecision{Ref: full[0].Ref, PodUID: "pod-uid", Boot: fmt.Sprintf("%064x", i+1)}
	}

	if raw, err := encodeForwards(boots); err != nil || len(raw) > forwardBytes {
		t.Fatal("boot payloads were counted repeatedly", err)
	}

	boots[0].Boot = ""

	boots[0].Ref = full[1].Ref
	if _, err := encodeForwards(boots); err == nil {
		t.Fatal("missing first-boot reservation")
	}

	if _, err := encodeForwards([]forwardDecision{{Ref: &ref, PodUID: strings.Repeat("x", forwardBytes)}}); err == nil {
		t.Fatal("metadata byte cap ignored")
	}

	bad := ref

	bad.Node = strings.Repeat("ef", 32)
	if _, err := f.s.controlStore.readForwardSnapshot(ctx, "default", forwardDecision{Ref: &bad}); err == nil {
		t.Fatal("metadata not bound to payload")
	}

	key := client.ObjectKey{Namespace: "state", Name: forwardChunkName(stateName("default"), &ref, 0)}

	part := &corev1.ConfigMap{}
	if err := f.api.Get(ctx, key, part); err != nil {
		t.Fatal(err)
	}
	// Delete/recreate is possible even for immutable ConfigMaps. Digest validation
	// must detect byte corruption before binding or delivering a skip grant.
	if err := f.api.Delete(ctx, part); err != nil {
		t.Fatal(err)
	}

	if _, err := f.s.controlStore.readForwardSnapshot(ctx, "default", ds[0]); err == nil {
		t.Fatal("missing chunk accepted")
	}

	part.ResourceVersion = ""

	part.BinaryData["snapshot"][len(part.BinaryData["snapshot"])-1] ^= 1
	if err := f.api.Create(ctx, part); err != nil {
		t.Fatal(err)
	}

	e, grant, _, err := f.s.forward(ctx, r, "default", f.node, "pod-uid", strings.Repeat("ab", 32), digest, digest, 0)
	if err == nil || e != nil || grant != nil {
		t.Fatal("corrupt bytes granted authority")
	}
	// A colliding immutable object is never overwritten, including retries.
	// Reconstruct the full snapshot; production geometry spans many chunks.
	if err := f.s.controlStore.putForwardSnapshot(ctx, r.pointer.Name, forwardDecision{Snapshot: original}); err == nil {
		t.Fatal("colliding chunk reused")
	}
}

func TestForwardStorageGC(t *testing.T) {
	ctx := context.Background()
	f, r, _ := forwardFixture(t)
	ds, _ := forwardHistory(r.pointer.Data["forwards"], "default", 2)
	ref := ds[0].Ref
	name := forwardChunkName(stateName("default"), ref, 0)
	check := func(want bool) {
		t.Helper()

		err := f.api.Get(ctx, client.ObjectKey{Namespace: "state", Name: name}, &corev1.ConfigMap{})
		if want && err != nil || !want && !apierrors.IsNotFound(err) {
			t.Fatal("GC reachability", want, err)
		}
	}
	commit := func() {
		t.Helper()

		g, cm, err := f.s.controlStore.load(ctx, "default")
		if err != nil {
			t.Fatal(err)
		}

		g.Revision++
		if err := f.s.controlStore.commit(ctx, g, cm); err != nil {
			t.Fatal(err)
		}
	}
	commit()
	check(true)
	// Interrupted writes are not ledger authority and are reclaimable.
	orphan, _ := marshalSnapshot(f.index.snapshot(f.node))

	orphanDecision := forwardDecision{Snapshot: orphan}
	if err := f.s.controlStore.putForwardSnapshot(ctx, r.pointer.Name, orphanDecision); err != nil {
		t.Fatal(err)
	}

	orphanName := forwardChunkName(stateName("default"), orphanDecision.snapshotRef(), 0)

	commit()

	if err := f.api.Get(ctx, client.ObjectKey{Namespace: "state", Name: orphanName}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatal("interrupted write orphan retained", err)
	}

	check(true)

	cm := f.durable(t)

	cm.Data["forwards"] = "malformed"
	if err := f.api.Client.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}

	commit()
	check(true) // Invalid ledger never authorizes collection.

	cm = f.durable(t)

	cm.Data["forwards"] = "[]"
	if err := f.api.Client.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}

	commit()
	check(false) // Unreferenced payloads/orphans are collected on next commit.
}

// Rollout lifecycle, restart and process incarnation.

func TestRolloutDurableBarriersRestartAndIncarnation(t *testing.T) {
	ctx := context.Background()
	n, p, svc := fixtures()
	p.UID = "pod-uid"
	kube := tokenClient{fakeKube(n, p, svc)}

	g, _, err := buildCacheFixture("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, svc)
	if err != nil {
		t.Fatal(err)
	}

	g.Revision = 1

	store := stateStore{client: kube, namespace: "state"}
	if err = store.commit(ctx, g, nil); err != nil {
		t.Fatal(err)
	}

	index, _ := indexGeneration(g)

	s := &Server{controlStore: store}
	if err = s.install(index); err != nil {
		t.Fatal(err)
	}

	node := g.Nodes[n.Name].ID
	digest := ""
	call := func(phase, boot string, code int) *pb.ControlCommand {
		t.Helper()

		req := httptest.NewRequest("GET", "/v3/"+identity("universe", "default")+"/"+node, nil)
		req.SetPathValue("universe", identity("universe", "default"))
		req.SetPathValue("node", node)
		controlTLS(req, "pod-uid")
		req.Header.Set("X-Racer-Boot", strings.Repeat(boot, 32))
		req.Header.Set("X-Racer-Profile", "1")
		req.Header.Set("X-Racer-Phase", phase)
		req.Header.Set("X-Racer-Digest", digest)

		w := httptest.NewRecorder()
		s.control(w, req)

		if w.Code != code {
			t.Fatalf("code %d: %s", w.Code, w.Body.String())
		}

		if code != 200 {
			return nil
		}

		var command pb.ControlCommand

		if err := proto.Unmarshal(w.Body.Bytes(), &command); err != nil {
			t.Fatal(err)
		}

		return &command
	}

	first := call("0", "ab", 200)
	if first.Phase != 1 || first.Configuration == nil {
		t.Fatal("missing prepare")
	}

	digest = hex.EncodeToString(first.SnapshotDigest)

	if got := call("3", "ab", 200); got.Phase != 1 {
		t.Fatal("future acknowledgment bypassed prepare")
	}

	if got := call("1", "ab", 200); got.Phase != 2 || got.Configuration != nil {
		t.Fatal("prepare barrier/status-only command")
	}

	call("2", "cd", 409)
	// A leader restart reloads the receive commitment but no cached acknowledgments.
	s = &Server{controlStore: store}
	s.install(index)

	if got := call("0", "ab", 200); got.Phase != 2 {
		t.Fatal("lost durable receive decision")
	}

	if got := call("2", "ab", 200); got.Phase != 3 {
		t.Fatal("receive barrier")
	}

	if got := call("3", "ab", 200); got.Phase != 4 {
		t.Fatal("activate barrier")
	}

	if busy, err := s.rolloutBusy(ctx, index); err != nil || !busy {
		t.Fatal("retired too early", err)
	}

	call("4", "ab", 200)

	if busy, err := s.rolloutBusy(ctx, index); err != nil || busy {
		t.Fatal("retirement blocked", err)
	}
	// Restarted process must establish its own state; old status is not inherited.
	s.rollouts["default"].acks[node] = rolloutAck{boot: strings.Repeat("ab", 32), phase: 4, seen: time.Now().Add(-time.Minute)}

	if got := call("0", "cd", 200); got.Phase != 4 {
		t.Fatal("catch-up decision missing")
	}

	if s.rollouts["default"].acks[node].phase != 0 {
		t.Fatal("inherited old process status")
	}
}

func TestRolloutMissingPodAndEmptyTarget(t *testing.T) {
	for _, phase := range []uint32{1, 2, 3} {
		t.Run(string(rune('0'+phase)), func(t *testing.T) {
			ctx := context.Background()
			n, p, svc := fixtures()
			p.UID = "pod-uid"
			kube := fakeKube(n, p, svc)

			g, _, err := buildCacheFixture("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, svc)
			if err != nil {
				t.Fatal(err)
			}

			g.Revision = 1

			store := stateStore{client: kube, namespace: "state"}
			if err := store.commit(ctx, g, nil); err != nil {
				t.Fatal(err)
			}

			index, _ := indexGeneration(g)

			s := &Server{controlStore: store}
			if err := s.install(index); err != nil {
				t.Fatal(err)
			}

			r, err := s.rolloutFor(ctx, index)
			if err != nil {
				t.Fatal(err)
			}

			if err = s.persistPhase(ctx, g.Universe, r, phase); err != nil {
				t.Fatal(err)
			}

			if err = kube.Delete(ctx, p); err != nil {
				t.Fatal(err)
			}

			if busy, err := s.rolloutBusy(ctx, index); err != nil || busy {
				t.Fatal(busy, err)
			}

			if phase == 1 && r.phase != 5 {
				t.Fatal("must abort before serving")
			}

			if phase > 1 && (r.phase != 4 || !r.recovering) {
				t.Fatal("must recover forward")
			}
		})
	}

	ctx := context.Background()
	store := stateStore{client: fakeKube(), namespace: "state"}

	g := &generation{Universe: "default", Revision: 1, Nodes: map[string]member{}}
	if err := store.commit(ctx, g, nil); err != nil {
		t.Fatal(err)
	}

	index, _ := indexGeneration(g)

	s := &Server{controlStore: store}
	if err := s.install(index); err != nil {
		t.Fatal(err)
	}

	if busy, err := s.rolloutBusy(ctx, index); err != nil || busy {
		t.Fatal("empty target blocked", err)
	}
}
