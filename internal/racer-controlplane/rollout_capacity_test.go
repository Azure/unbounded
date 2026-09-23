// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/Azure/unbounded/api/racer"
)

// Model the recovery dependency rather than injecting retirement acknowledgments:
// replacing failed Pods authorizes terminal delivery for the CURRENT revision,
// even while successor admission is over capacity. Workers below are simulated;
// the commands and acknowledgments use the production authenticated handler.
func TestForwardCapacityPhaseTwoPodReplacementRecovery(t *testing.T) {
	const nodesCount, selected, replaced = 1500, 1495, 35

	ctx := t.Context()
	nodes := make([]corev1.Node, nodesCount)
	pods := make([]corev1.Pod, nodesCount)

	objects := make([]client.Object, 0, nodesCount+selected+1)
	for i := range nodesCount {
		n, p, _ := fixtures()
		n.Name = fmt.Sprintf("node-%04d", i)
		n.UID = types.UID(n.Name)
		p.Name, p.Spec.NodeName = n.Name, n.Name
		p.UID = types.UID(fmt.Sprintf("00000000-0000-0000-0000-%012d", i))
		p.Status.PodIP = fmt.Sprintf("10.1.%d.%d", i/250, i%250+1)
		nodes[i], pods[i] = *n, *p

		objects = append(objects, n)
		if i < selected {
			objects = append(objects, p)
		}
	}

	_, _, cache := fixtures()
	objects = append(objects, cache)
	kube := fakeKube(objects...)
	store := stateStore{client: kube, namespace: "state"}

	g, _, err := buildCacheFixture("default", nil, nodes, pods[:selected], cache)
	if err != nil {
		t.Fatal(err)
	}

	g.Revision = 29
	if g.Volume.Slots != defaultSlots || len(g.Nodes) != nodesCount {
		t.Fatal("not production geometry")
	}

	if err := store.commit(ctx, g, nil); err != nil {
		t.Fatal(err)
	}

	index, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	rec := newTestReconciler(kube)

	s := rec.server
	if err := s.install(index); err != nil {
		t.Fatal(err)
	}

	r, err := s.rolloutFor(ctx, index)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.persistPhase(ctx, "default", r, 2); err != nil {
		t.Fatal(err)
	}

	// Retain an unknown-boot obligation for one replaced Pod and one survivor.
	for _, i := range []int{0, replaced} {
		m := g.Nodes[nodes[i].Name]
		snapshot := index.snapshot(m.ID)
		snapshot.Revision = 28

		data, err := marshalSnapshot(snapshot)
		if err != nil {
			t.Fatal(err)
		}

		ds, err := r.forwardHistory("default")
		if err != nil {
			t.Fatal(err)
		}

		if err := s.saveForwards(ctx, r, append(ds, forwardDecision{Snapshot: data, PodUID: m.PodUID})); err != nil {
			t.Fatal(err)
		}
	}

	history := r.pointer.Data["forwards"]
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
	call := func(i int, uid string, phase uint32, digest string, wantCode int) *pb.ControlCommand {
		t.Helper()

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.SetPathValue("universe", identity("universe", "default"))
		req.SetPathValue("node", g.Nodes[nodes[i].Name].ID)
		controlTLS(req, uid)
		req.Header.Set("X-Racer-Boot", fmt.Sprintf("%064x", i+1))
		req.Header.Set("X-Racer-Profile", "1")
		req.Header.Set("X-Racer-Phase", strconv.Itoa(int(phase)))
		req.Header.Set("X-Racer-Digest", digest)

		w := httptest.NewRecorder()
		s.control(w, req)

		if w.Code != wantCode {
			t.Fatalf("node %d phase %d: HTTP %d want %d: %s", i, phase, w.Code, wantCode, w.Body.String())
		}

		if wantCode != http.StatusOK {
			return nil
		}

		var command pb.ControlCommand
		if err := proto.Unmarshal(w.Body.Bytes(), &command); err != nil {
			t.Fatal(err)
		}

		return &command
	}

	// New Pods on the five placeholders cannot change the receive decision or
	// enroll themselves. While every selected Pod is present, capacity blocks R30.
	for i := selected; i < nodesCount; i++ {
		if err := kube.Create(ctx, &pods[i]); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := rec.Reconcile(ctx, request); err == nil || !strings.Contains(err.Error(), "capacity exhausted") {
		t.Fatal("expected phase-2 capacity backpressure", err)
	}

	if s.rollouts["default"].phase != 2 {
		t.Fatal("placeholder authorized terminal recovery")
	}

	call(selected, string(pods[selected].UID), 0, "", http.StatusForbidden)

	// Replace all 35 affected selected Pods with fresh UIDs. There are still
	// 1460 live survivors at phase 2; none has a phase-4 acknowledgment.
	for i := range replaced {
		if err := kube.Delete(ctx, &pods[i]); err != nil {
			t.Fatal(err)
		}

		p := pods[i].DeepCopy()
		p.ResourceVersion = ""

		p.UID = types.UID(fmt.Sprintf("replacement-%d", i))
		if err := kube.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// Include controller restart while still over capacity. The terminal current
	// decision must survive both the failed admission and process restart.
	for attempt := range 2 {
		if attempt != 0 {
			rec = newTestReconciler(kube)
			s = rec.server
		}

		if _, err := rec.Reconcile(ctx, request); err == nil || !strings.Contains(err.Error(), "capacity exhausted") {
			t.Fatal("expected successor capacity backpressure", err)
		}

		current, _, err := store.load(ctx, "default")
		if err != nil || !reflect.DeepEqual(g, current) {
			t.Fatal("failed admission changed topology", err)
		}

		r = s.rollouts["default"]
		if r.phase != 4 || !r.recovering || r.pointer.Data["phase"] != "4" || r.pointer.Data["forwards"] != history {
			t.Fatal("failed admission lost terminal recovery or collected history")
		}
	}

	call(0, "replacement-0", 0, "", http.StatusForbidden)

	digests := make([]string, selected)
	for i := replaced; i < selected; i++ {
		command := call(i, string(pods[i].UID), 2, "", http.StatusOK)
		if command.Revision != 29 || command.Phase != 4 || command.Configuration == nil {
			t.Fatalf("survivor needs successor to retire: node=%d revision=%d phase=%d", i, command.Revision, command.Phase)
		}

		digests[i] = hex.EncodeToString(command.SnapshotDigest)
	}
	// Simulate worker activation then actual retirement in response to R29/4.
	// No successor is needed, and phase-3 reports do not release the barrier.
	for _, phase := range []uint32{3, 4} {
		for i := replaced; i < selected; i++ {
			command := call(i, string(pods[i].UID), phase, digests[i], http.StatusOK)
			if command.Revision != 29 || command.Phase != 4 || command.Configuration != nil {
				t.Fatal("current terminal delivery changed while draining")
			}
		}

		busy, err := s.rolloutBusy(ctx, s.source.topologies[identityBytes("universe", "default")])
		if err != nil || busy != (phase != 4) {
			t.Fatalf("phase %d recovery barrier: busy=%t err=%v", phase, busy, err)
		}
	}

	if _, err := rec.Reconcile(ctx, request); err != nil {
		t.Fatal("retired survivors did not admit replacement", err)
	}

	current, _, err := store.load(ctx, "default")
	if err != nil || current.Revision != 30 {
		t.Fatal("successor was not committed", err)
	}
	// The replaced UID loses authority only after commit. All 40 newly selected
	// Pods now receive R30 prepare, which is also the enrollment selection gate.
	call(0, string(pods[0].UID), 0, "", http.StatusForbidden)

	for i := range nodesCount {
		if i >= replaced && i < selected {
			continue
		}

		uid := string(pods[i].UID)
		if i < replaced {
			uid = fmt.Sprintf("replacement-%d", i)
		}

		if current.Nodes[nodes[i].Name].PodUID != uid {
			t.Fatal("successor did not select new Pod")
		}

		command := call(i, uid, 0, "", http.StatusOK)
		if command.Revision != 30 || command.Phase != 1 {
			t.Fatal("new Pod cannot prepare successor")
		}
	}

	r = s.rollouts["default"]

	ds, err := r.forwardHistory("default")
	if err != nil || len(ds) != 1 || ds[0].PodUID != string(pods[replaced].UID) || ds[0].Boot != "" || ds[0].Ref.Revision != 28 {
		t.Fatal("post-commit GC lost live unknown-boot history or retained replaced Pod", err)
	}

	// Complete the successor's normal fleet-wide receive-before-send barriers.
	// Warm every digest first so the bounded payload LRU need not retain bodies
	// for the subsequent config-free acknowledgment sweeps.
	nextDigests := make([]string, nodesCount)

	uidFor := func(i int) string {
		if i < replaced {
			return fmt.Sprintf("replacement-%d", i)
		}

		return string(pods[i].UID)
	}
	for i := range nodesCount {
		command := call(i, uidFor(i), 0, "", http.StatusOK)
		if command.Revision != 30 || command.Phase != 1 {
			t.Fatal("successor bypassed prepare")
		}

		nextDigests[i] = hex.EncodeToString(command.SnapshotDigest)
	}

	for phase := uint32(1); phase <= 4; phase++ {
		for i := range nodesCount {
			command := call(i, uidFor(i), phase, nextDigests[i], http.StatusOK)
			if command.Revision != 30 || command.Phase < phase || command.Phase > min(phase+1, 4) {
				t.Fatal("successor barrier violated")
			}
		}

		if s.rollouts["default"].phase != min(phase+1, 4) {
			t.Fatalf("successor stuck after phase %d", phase)
		}
	}

	if busy, err := s.rolloutBusy(ctx, s.source.topologies[identityBytes("universe", "default")]); err != nil || busy {
		t.Fatal("successor did not finish retirement", err)
	}
}

// Real slot geometry, realistic identities, and enough unresolved recipients to
// exceed the former payload cap while fitting the reserved metadata budget.
func TestForwardCapacityProductionGeometry(t *testing.T) {
	ctx := t.Context()
	g := testGeneration(defaultSlots, 1500)

	g.Revision = 29
	for name, m := range g.Nodes {
		m.PodUID = identity("pod", name)[:36]
		g.Nodes[name] = m
	}

	index, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	kube := fakeKube()
	api := &forwardCapacityAPI{Client: kube}

	store := stateStore{client: api, namespace: "state"}
	if err := store.commit(ctx, g, nil); err != nil {
		t.Fatal(err)
	}

	s := &Server{controlStore: store}
	if err := s.install(index); err != nil {
		t.Fatal(err)
	}

	r, err := s.rolloutFor(ctx, index)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.persistPhase(ctx, "default", r, 2); err != nil {
		t.Fatal(err)
	}

	// First prove that an overfull correction does not write any chunks or ledger.
	if err := s.planForward(ctx, index, index); err == nil || !strings.Contains(err.Error(), "capacity exhausted") {
		t.Fatalf("unbounded survivor set admitted: %v", err)
	}

	if api.payloads != 0 || r.pointer.Data["forwards"] != "" {
		t.Fatal("failed preflight wrote handoff state")
	}

	const unresolved = 228
	for i := unresolved; i < 1500; i++ {
		m := g.Nodes[fmt.Sprintf("node-%06d", i)]
		r.acks[m.ID] = rolloutAck{phase: 4}
	}

	// An uncertain chunk create cannot publish a ledger; reload and retry must
	// verify/reuse the orphan before the complete reference-only CAS.
	api.fail = true

	if err := s.planForward(ctx, index, index); err == nil || !r.invalid {
		t.Fatalf("chunk failure did not invalidate planning: %v", err)
	}

	if r.pointer.Data["forwards"] != "" {
		t.Fatal("partial staging published history")
	}

	acks := r.acks

	r, err = s.rolloutFor(ctx, index)
	if err != nil {
		t.Fatal(err)
	}

	r.acks = acks

	if err := s.planForward(ctx, index, index); err != nil {
		t.Fatal(err)
	}

	ds, err := r.forwardHistory("default")
	if err != nil || len(ds) != unresolved {
		t.Fatalf("lost unresolved obligations: %d %v", len(ds), err)
	}

	total := 0
	for _, d := range ds {
		total += d.Ref.Size
		if len(d.Snapshot) != 0 || d.Boot != "" || d.Grant != 0 || d.Ref.Revision != 29 {
			t.Fatal("changed wildcard binding")
		}
	}

	if total <= 256*1024*1024 || total > forwardPayloadBytes {
		t.Fatalf("fixture missed production capacity regression: %d bytes", total)
	}

	t.Logf("slots=%d nodes=%d unresolved=%d payload bytes=%d ledger bytes=%d", defaultSlots, len(g.Nodes), len(ds), total, len(r.pointer.Data["forwards"]))

	// Commit a successor, then restart. GC must use bounded metadata pages and
	// retain the exact old bytes, including unknown-boot obligations.
	next, pointer, err := store.load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}

	next.Revision++
	if err := store.commit(ctx, next, pointer); err != nil {
		t.Fatal(err)
	}

	if api.unboundedGC || api.gcLists < 2 {
		t.Fatal("GC did not use bounded metadata requests")
	}

	index, err = indexGeneration(next)
	if err != nil {
		t.Fatal(err)
	}

	s = &Server{controlStore: store}
	if err := s.install(index); err != nil {
		t.Fatal(err)
	}

	r, err = s.rolloutFor(ctx, index)
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.forwardHistory("default")
	if err != nil || !reflect.DeepEqual(ds, got) {
		t.Fatal("restart lost handoff obligations", err)
	}

	d := ds[len(ds)-1]

	original, err := store.readForwardSnapshot(ctx, "default", d)
	if err != nil {
		t.Fatal(err)
	}

	hash := sha256.Sum256(original)
	if hex.EncodeToString(hash[:]) != d.Ref.Digest {
		t.Fatal("GC changed payload")
	}

	boot := strings.Repeat("ab", 32)

	e, _, _, err := s.forward(ctx, r, "default", d.Ref.Node, d.PodUID, boot, d.Ref.Digest, "", 2)
	if err != nil || e == nil || !bytes.Equal(e.snapshot, original) {
		t.Fatal("historical receive decision not replayable", err)
	}

	if err := s.collectForwards(ctx, r, "default", d.Ref.Node, d.PodUID, boot); err != nil {
		t.Fatal(err)
	}

	got, err = r.forwardHistory("default")
	if err != nil || !reflect.DeepEqual(ds, got) {
		t.Fatal("retiring one boot lost unknown-boot obligations", err)
	}
}

type forwardCapacityAPI struct {
	client.Client
	payloads    int
	fail        bool
	gcLists     int
	unboundedGC bool
}

func (c *forwardCapacityAPI) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	err := c.Client.Create(ctx, obj, opts...)
	if obj.GetLabels()[stateLabel] == "forward" {
		c.payloads++
		if c.fail && err == nil {
			c.fail = false
			return errors.New("lost chunk create response")
		}
	}

	return err
}

func (c *forwardCapacityAPI) List(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) error {
	options := (&client.ListOptions{}).ApplyOptions(opts)
	if options.LabelSelector != nil && strings.Contains(options.LabelSelector.String(), stateOwnerLabel) {
		c.gcLists++
		if _, ok := obj.(*metav1.PartialObjectMetadataList); !ok || options.Limit != 100 {
			c.unboundedGC = true
			return errors.New("GC must list bounded metadata pages")
		}
	}

	return c.Client.List(ctx, obj, opts...)
}

// Model the reported rollout geometry with real reconciliation and durable
// history. Worker acknowledgments are synthetic; this does not model live worker
// retirement or prove that a particular cluster can reach the retirement barrier.
func TestForwardCapacityLargeUniversePodRollout(t *testing.T) {
	ctx := context.Background()

	const participants, retained = 1500, 126

	var (
		nodes   []corev1.Node
		pods    []corev1.Pod
		objects []client.Object
	)

	for i := range participants {
		n, p, _ := fixtures()
		n.Name = fmt.Sprintf("node-%04d", i)
		n.UID = types.UID(n.Name)
		p.Name, p.UID, p.Spec.NodeName = n.Name, types.UID("pod-"+n.Name), n.Name
		p.Status.PodIP = fmt.Sprintf("10.1.%d.%d", i/250, i%250+1)

		nodes, pods = append(nodes, *n), append(pods, *p)
		objects = append(objects, n, p)
	}

	_, _, svc := fixtures()
	objects = append(objects, svc)
	kube := fakeKube(objects...)
	store := stateStore{client: kube, namespace: "state"}

	g, _, err := buildCacheFixture("default", nil, nodes, pods, svc)
	if err != nil {
		t.Fatal(err)
	}

	g.Revision = 5
	if err := store.commit(ctx, g, nil); err != nil {
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

	r, err := s.rolloutFor(ctx, index)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.persistPhase(ctx, "default", r, 4); err != nil {
		t.Fatal(err)
	}

	var history []forwardDecision

	for i := range retained {
		m := g.Nodes[nodes[i].Name]
		snap := index.snapshot(m.ID)
		snap.Revision, snap.Epoch = 4, 4

		data, err := marshalSnapshot(snap)
		if err != nil {
			t.Fatal(err)
		}

		history = append(history, forwardDecision{Snapshot: data, PodUID: m.PodUID})
	}

	if err := s.saveForwards(ctx, r, history); err != nil {
		t.Fatal(err)
	}

	before := r.pointer.Data["forwards"]
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
	// Replace a recipient with retained history, then one without it. Each
	// controller restart must recollect actual process retirement acknowledgments.
	for iteration, replacement := range []int{0, participants - 1} {
		p := &corev1.Pod{}
		if err := kube.Get(ctx, client.ObjectKeyFromObject(&pods[replacement]), p); err != nil {
			t.Fatal(err)
		}

		if err := kube.Delete(ctx, p); err != nil {
			t.Fatal(err)
		}

		p.ResourceVersion = ""

		p.UID = types.UID(fmt.Sprintf("replacement-%d", iteration))
		if err := kube.Create(ctx, p); err != nil {
			t.Fatal(err)
		}

		rec := newTestReconciler(kube)

		for _, phase := range []uint32{0, 3, 4} {
			if phase != 0 {
				roll := rec.server.rollouts["default"]
				for _, m := range g.Nodes {
					roll.acks[m.ID] = rolloutAck{phase: phase}
				}
			}

			_, reconcileErr := rec.Reconcile(ctx, request)

			durable, pointer, err := store.load(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}

			cm := &corev1.ConfigMap{}
			if err := kube.Get(ctx, client.ObjectKey{Namespace: "state", Name: pointer.Name + "-rollout"}, cm); err != nil {
				t.Fatal(err)
			}

			if phase != 4 {
				if reconcileErr == nil || (!strings.Contains(reconcileErr.Error(), "forward history capacity exhausted") && !strings.Contains(reconcileErr.Error(), "forward payload capacity exhausted")) {
					t.Fatalf("phase %d: expected forward metadata backpressure, got %v", phase, reconcileErr)
				}

				if !reflect.DeepEqual(durable, g) || cm.Data["forwards"] != before {
					t.Fatal("failed admission changed durable topology or history")
				}

				continue
			}

			if reconcileErr != nil || durable.Revision != g.Revision+1 || durable.Nodes[p.Spec.NodeName].PodUID != string(p.UID) {
				t.Fatalf("retired survivors did not admit replacement: revision=%d error=%v", durable.Revision, reconcileErr)
			}

			if cm.Data["forwards"] != before {
				t.Fatal("planner collected replacement history before post-commit reload")
			}

			g = durable
		}

		// Reload from durable state, exercising only the existing post-commit GC.
		index, err := indexGeneration(g)
		if err != nil {
			t.Fatal(err)
		}

		s = &Server{controlStore: store}
		if err := s.install(index); err != nil {
			t.Fatal(err)
		}

		r, err = s.rolloutFor(ctx, index)
		if err != nil {
			t.Fatal(err)
		}

		remaining, err := forwardHistory(r.pointer.Data["forwards"], "default", g.Revision)
		if err != nil || len(remaining) != retained-1 {
			t.Fatalf("unexpected post-commit history: count=%d error=%v", len(remaining), err)
		}

		for _, d := range remaining {
			if d.PodUID == string(pods[0].UID) || d.Boot != "" || d.Ref.Revision != 4 {
				t.Fatal("replacement GC lost or rebound another Pod's unknown-boot obligation")
			}
		}

		before = r.pointer.Data["forwards"]
		if err := s.persistPhase(ctx, "default", r, 4); err != nil {
			t.Fatal(err)
		}
	}
}
