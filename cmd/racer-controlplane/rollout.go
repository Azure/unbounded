// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/Azure/unbounded/api/racer"
)

// Durable rollout decisions and receiver barriers.

type rolloutAck struct {
	boot  string
	phase uint32
	seen  time.Time
}
type rollout struct {
	revision   uint64
	phase      uint32
	pointer    *corev1.ConfigMap
	acks       map[string]rolloutAck
	since      time.Time
	recovering bool
	invalid    bool // An uncertain write forbids decisions until an uncached reload.
	forwards   *validatedForwardHistory
}

func (r *rollout) invalidate() {
	r.invalid = true
	r.forwards = nil
}

// Decisions, unlike heartbeat acknowledgments, are durable before delivery.
// On restart all barriers are recollected from the actual selected processes.
func (s *Server) rolloutFor(ctx context.Context, t *topologyIndex) (*rollout, error) {
	if s.rollouts == nil {
		s.rollouts = map[string]*rollout{}
	}

	previous := s.rollouts[t.g.Universe]
	if r := previous; r != nil && r.revision == t.g.Revision && !r.invalid {
		return r, nil
	}

	r := &rollout{revision: t.g.Revision, phase: 1, acks: map[string]rolloutAck{}, since: time.Now()}
	cm := &corev1.ConfigMap{}

	err := s.controlStore.client.Get(ctx, types.NamespacedName{Namespace: s.controlStore.namespace, Name: stateName(t.g.Universe) + "-rollout"}, cm)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}

	if apierrors.IsNotFound(err) && previous != nil && previous.revision == r.revision && previous.pointer != nil {
		return nil, fmt.Errorf("durable rollout disappeared")
	}

	if err == nil {
		revision, e := strconv.ParseUint(cm.Data["revision"], 10, 64)
		if e != nil || revision == 0 || revision > r.revision {
			return nil, fmt.Errorf("invalid or newer durable rollout revision")
		}

		phase, e := strconv.ParseUint(cm.Data["phase"], 10, 32)
		if e != nil || phase < 1 || phase > 5 {
			return nil, fmt.Errorf("invalid durable rollout phase")
		}

		recovering, e := strconv.ParseBool(cm.Data["recovering"])
		if e != nil || (recovering && phase != 4) {
			return nil, fmt.Errorf("invalid durable rollout recovery")
		}

		since, e := time.Parse(time.RFC3339Nano, cm.Data["since"])
		if e != nil {
			return nil, fmt.Errorf("invalid durable rollout timestamp")
		}

		if previous != nil && previous.revision == r.revision && previous.pointer != nil {
			if revision != r.revision || phase < uint64(previous.phase) ||
				(previous.phase >= 2 && previous.phase <= 4 && phase == 5) ||
				(previous.recovering && !recovering) {
				return nil, fmt.Errorf("durable rollout regressed")
			}
		}

		r.pointer = cm
		if revision == r.revision {
			r.phase = uint32(phase)
			r.recovering = recovering
			r.since = since
		}

		if _, err := removalHistory(cm.Data["removals"], t.g.Universe, r.revision); err != nil {
			return nil, err
		}

		if _, err := r.forwardHistory(t.g.Universe); err != nil {
			return nil, err
		}
	}

	if r.pointer != nil {
		ds, err := r.forwardHistory(t.g.Universe)
		if err != nil {
			return nil, err
		}

		kept := committedForwards(t, ds)
		if len(ds) != 0 {
			// t is loaded from, or installed after, the durable topology commit.
			// Repeat this GC on restart if the post-commit write was lost.
			if err := s.saveForwards(ctx, r, kept); err != nil {
				return nil, err
			}
		}
	}

	if r.pointer == nil || r.pointer.Data["revision"] != strconv.FormatUint(r.revision, 10) {
		if err := s.persistPhase(ctx, t.g.Universe, r, r.phase); err != nil {
			return nil, err
		}
	}

	s.rollouts[t.g.Universe] = r

	return r, nil
}

func (s *Server) persistPhase(ctx context.Context, universe string, r *rollout, phase uint32) error {
	return s.persistRollout(ctx, universe, r, phase, r.recovering)
}

func (s *Server) persistRollout(ctx context.Context, universe string, r *rollout, phase uint32, recovering bool) (err error) {
	if r.invalid {
		return fmt.Errorf("rollout requires uncached reload")
	}
	// Even a timeout or conflict can hide a committed decision. Never retry a
	// proposal by merely refreshing its RV: reload revision AND phase first.
	defer func() {
		if err != nil {
			r.invalidate()
		}
	}()

	cm := r.pointer
	if cm == nil {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: s.controlStore.namespace, Name: stateName(universe) + "-rollout"}}
	} else {
		cm = cm.DeepCopy()
	}

	now := time.Now()

	removals, err := s.advanceRemovals(universe, r, phase, cm.Data["removals"])
	if err != nil {
		return err
	}

	serving := cm.Data["serving"]

	if phase >= 2 && phase <= 4 {
		pointer := &corev1.ConfigMap{}
		if err := s.controlStore.client.Get(ctx, types.NamespacedName{Namespace: s.controlStore.namespace, Name: stateName(universe)}, pointer); err != nil {
			return err
		}

		serving = pointer.Data["manifest"]

		var m manifest
		if err := json.Unmarshal([]byte(serving), &m); err != nil || m.Universe != universe {
			return fmt.Errorf("invalid serving manifest")
		}
	}

	forwards := cm.Data["forwards"]
	cm.Data = map[string]string{"revision": strconv.FormatUint(r.revision, 10), "phase": strconv.Itoa(int(phase)), "since": now.Format(time.RFC3339Nano), "recovering": strconv.FormatBool(recovering)}
	cm.Data["forwards"] = forwards
	cm.Data["serving"] = serving

	cm.Data["removals"] = removals
	if r.pointer == nil {
		err = s.controlStore.client.Create(ctx, cm)
	} else {
		err = s.controlStore.client.Update(ctx, cm)
	}

	if err != nil {
		return err
	}

	r.pointer = cm
	r.phase = phase
	r.since = now
	r.recovering = recovering

	return nil
}

func (s *Server) rolloutBusy(ctx context.Context, t *topologyIndex) (bool, error) {
	// Inventory I/O must not hold the subscription lock: a slow API LIST would
	// prevent every receiver from reporting the acknowledgments needed to finish
	// this rollout. Load the decision only after reacquiring the lock below.
	// A disappeared target cannot finish the barrier. Before serving, abort;
	// afterwards persist forward recovery and finish surviving processes before
	// admitting a replacement. A control-network partition alone is not failure.
	var pods corev1.PodList
	if err := s.controlStore.client.List(ctx, &pods, client.MatchingLabels{dataplaneLabel: "true"}); err != nil {
		return true, err
	}

	live := map[string]bool{}

	var nodes corev1.NodeList
	if err := s.controlStore.client.List(ctx, &nodes); err != nil {
		return true, err
	}

	ready := map[string]bool{}
	for _, node := range nodes.Items {
		ready[node.Name] = nodeReady(&node)
	}

	for _, pod := range pods.Items {
		if podAvailable(&pod) && ready[pod.Spec.NodeName] {
			live[string(pod.UID)] = true
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Inventory was collected for t, not for a successor installed while the
	// reads were in flight. An uncertain topology commit can also unpublish t.
	if s.source != nil {
		current := s.source.topologies[identityBytes("universe", t.g.Universe)]
		if current == nil || current.g.Revision != t.g.Revision {
			return true, fmt.Errorf("rollout topology changed during inventory read")
		}
	}

	r, err := s.rolloutFor(ctx, t)
	if err != nil {
		return true, err
	}

	missing := false
	targets := 0

	for _, m := range t.g.Nodes {
		if m.IP != "" {
			targets++
		}

		if m.IP != "" && !live[m.PodUID] {
			missing = true
		}
	}
	// An empty replacement has no target receivers. Historical recipients still
	// obtain the removal command on reconnect, without blocking later membership.
	if targets == 0 && r.phase < 4 {
		if err := s.persistPhase(ctx, t.g.Universe, r, 4); err != nil {
			return true, err
		}
	}

	if missing && r.phase != 5 {
		phase := uint32(5)

		recovering := r.recovering
		if r.phase >= 2 {
			phase = 4
			recovering = true
		}

		if phase != r.phase || recovering != r.recovering {
			if err := s.persistRollout(ctx, t.g.Universe, r, phase, recovering); err != nil {
				return true, err
			}
		}
	}

	if r.phase == 1 && time.Since(r.since) > 5*time.Minute {
		if err := s.persistPhase(ctx, t.g.Universe, r, 5); err != nil {
			return true, err
		}
	}

	if r.phase == 5 {
		return false, nil
	}

	if r.phase != 4 {
		return true, nil
	}

	for _, m := range t.g.Nodes {
		if m.IP != "" && (!r.recovering || live[m.PodUID]) && r.acks[m.ID].phase < 4 {
			return true, nil
		}
	}

	return false, nil
}

// Authenticated control delivery and acknowledgment collection.

// Pod-bound service-account tokens identify the selected process's Pod. A boot
// nonce binds commands and acknowledgments to this subscription incarnation.
func (s *Server) control(w http.ResponseWriter, req *http.Request) {
	fail := func(err error, code int) { http.Error(w, err.Error(), code) }

	u, e := hex.DecodeString(req.PathValue("universe"))
	if e != nil || len(u) != 32 {
		fail(fmt.Errorf("invalid universe"), 400)
		return
	}

	n, e := hex.DecodeString(req.PathValue("node"))
	if e != nil || len(n) != 32 {
		fail(fmt.Errorf("invalid node"), 400)
		return
	}

	boot, e := hex.DecodeString(req.Header.Get("X-Racer-Boot"))
	if e != nil || len(boot) != 32 || req.Header.Get("X-Racer-Profile") != "1" {
		fail(fmt.Errorf("unsupported incarnation/profile"), 400)
		return
	}

	token, bearer := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !bearer {
		fail(errInvalidCredential, 403)
		return
	}

	kube := s.reviewClient
	if kube == nil {
		kube = s.controlStore.client
	}

	podUID, err := s.credentials.authenticate(req.Context(), kube, token, controlAudience)
	if err != nil {
		code := http.StatusServiceUnavailable
		if err == errInvalidCredential {
			code = http.StatusForbidden
		}

		fail(err, code)

		return
	}

	s.mu.Lock()
	if req.Context().Err() != nil {
		s.mu.Unlock()
		return
	}

	if s.signer == nil || s.source == nil {
		s.mu.Unlock()
		fail(fmt.Errorf("signed controller unavailable"), 503)

		return
	}

	locked := true

	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()

	key := recipient{[32]byte(u), [32]byte(n)}

	t := s.source.topologies[key.universe]
	if t == nil {
		fail(fmt.Errorf("unknown universe"), 404)
		return
	}

	nodeID := hex.EncodeToString(n)
	name, ok := t.byID[nodeID]
	// Authorization is deliberately outside the credential cache: every heartbeat
	// checks the current committed selection, including phase 4 and cache hits.
	if !ok || t.g.Nodes[name].PodUID != podUID {
		fail(fmt.Errorf("pod is not selected for node"), 403)
		return
	}

	r, err := s.rolloutFor(req.Context(), t)
	if err != nil {
		fail(err, 503)
		return
	}

	entry, err := s.current(key)
	if err != nil || entry == nil {
		fail(fmt.Errorf("snapshot unavailable"), 503)
		return
	}

	digest := sha256.Sum256(entry.snapshot)

	if old, ok := r.acks[nodeID]; ok && old.boot != hex.EncodeToString(boot) && time.Since(old.seen) < 15*time.Second {
		fail(fmt.Errorf("another process incarnation is registered"), 409)
		return
	}

	var phase uint64
	if raw := req.Header.Get("X-Racer-Phase"); raw != "" {
		phase, err = strconv.ParseUint(raw, 10, 32)
		if err != nil {
			fail(fmt.Errorf("invalid phase"), 400)
			return
		}
	}

	forwardEntry, forwardDigest, forwardRevision, err := s.forward(req.Context(), r, t.g.Universe, nodeID, podUID, hex.EncodeToString(boot), req.Header.Get("X-Racer-Digest"), req.Header.Get("X-Racer-Forward-Eligible"), phase)
	if err != nil {
		fail(err, 503)
		return
	}

	prior, priorPhase, err := s.catchup(req.Context(), r, t.g.Universe, nodeID, podUID, hex.EncodeToString(boot), req.Header.Get("X-Racer-Digest"), phase)
	if err != nil {
		fail(err, 503)
		return
	}

	if forwardEntry != nil {
		prior, priorPhase = forwardEntry, 4
	}

	if prior != nil {
		r.acks[nodeID] = rolloutAck{hex.EncodeToString(boot), 0, time.Now()}
		entry = prior
		digest = sha256.Sum256(entry.snapshot)
	} else {
		if req.Header.Get("X-Racer-Digest") == hex.EncodeToString(digest[:]) && phase <= uint64(r.phase) && phase <= 4 {
			if phase > 0 {
				if err := s.collectForwards(req.Context(), r, t.g.Universe, nodeID, podUID, hex.EncodeToString(boot)); err != nil {
					fail(err, 503)
					return
				}

				if err := s.collectRemovals(req.Context(), r, t.g.Universe, nodeID, hex.EncodeToString(boot)); err != nil {
					fail(err, 503)
					return
				}
			}

			r.acks[nodeID] = rolloutAck{hex.EncodeToString(boot), uint32(phase), time.Now()}
		} else {
			r.acks[nodeID] = rolloutAck{hex.EncodeToString(boot), 0, time.Now()}
		}

		all := true

		for _, member := range t.g.Nodes {
			if member.IP == "" {
				continue
			}

			ack := r.acks[member.ID]
			if ack.phase < r.phase || time.Since(ack.seen) > 15*time.Second {
				all = false
				break
			}
		}

		if all && r.phase < 4 {
			if err = s.persistPhase(req.Context(), t.g.Universe, r, r.phase+1); err != nil {
				fail(err, 503)
				return
			}
		}

		if r.phase == 1 && time.Since(r.since) > 5*time.Minute {
			if err = s.persistPhase(req.Context(), t.g.Universe, r, 5); err != nil {
				fail(err, 503)
				return
			}
		}
	}

	if prior == nil {
		// Register an empty command's boot durably BEFORE its first delivery,
		// including a phase transition performed by this request.
		if _, _, err := s.catchup(req.Context(), r, t.g.Universe, nodeID, podUID, hex.EncodeToString(boot), hex.EncodeToString(digest[:]), 0); err != nil {
			fail(err, 503)
			return
		}
	}

	var config *pb.Configuration
	if req.Header.Get("X-Racer-Digest") != hex.EncodeToString(digest[:]) || req.Header.Get("X-Racer-Needs-Config") == "1" {
		config = &pb.Configuration{}
		if err = proto.Unmarshal(entry.body, config); err != nil {
			fail(err, 500)
			return
		}
	}

	commandPhase := r.phase
	if prior != nil {
		commandPhase = priorPhase
	}

	command := &pb.ControlCommand{Universe: u, Node: n, Incarnation: boot, SnapshotDigest: digest[:], Revision: entry.revision, Phase: commandPhase, Profile: 1, Configuration: config}
	command.ForwardDigest, command.ForwardRevision, command.PodUid = forwardDigest, forwardRevision, podUID
	command.StoragePolicy = s.storageCommand(req, key, podUID)

	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(command)
	if err != nil {
		fail(err, 500)
		return
	}

	body, err := proto.Marshal(&pb.SignedControlCommand{Command: raw, Signature: s.signer.signDomain("racer/control/v1", raw)})
	if err != nil {
		fail(err, 500)
		return
	}
	s.mu.Unlock()

	locked = false

	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))

	if _, err := w.Write(body); err != nil {
		return // The next heartbeat retries the durably recorded command.
	}
}

// Removal catch-up: retain unresolved empty-recipient decisions across revisions.

// Fixed per-universe budget, not a rolling window: never evict an unresolved
// receive decision to make space. Backpressure requires retirement or Pod replacement.
const (
	catchupLimit = 512
	catchupBytes = 512 * 1024
)

type removalDecision struct {
	Snapshot []byte `json:"snapshot"`
	PodUID   string `json:"podUID"`
	Boot     string `json:"boot,omitempty"`
	Phase    uint32 `json:"phase"`
	Done     bool   `json:"done,omitempty"`
}

func removalHistory(raw, universe string, revision uint64) ([]removalDecision, error) {
	var entries []removalDecision

	if raw != "" {
		if len(raw) > catchupBytes {
			return nil, fmt.Errorf("removal history exceeds byte budget")
		}

		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return nil, err
		}
	}

	if len(entries) > catchupLimit {
		return nil, fmt.Errorf("removal history exceeds entry budget")
	}

	seen := map[string]bool{}

	for _, d := range entries {
		var snap pb.Snapshot

		boot, err := hex.DecodeString(d.Boot)
		if proto.Unmarshal(d.Snapshot, &snap) != nil || snap.Idle || len(snap.Volumes) != 0 || len(snap.Peers) != 0 ||
			hex.EncodeToString(snap.Universe) != identity("universe", universe) || len(snap.Node) != 32 ||
			snap.Revision == 0 || snap.Revision > revision || d.PodUID == "" ||
			d.Phase < 1 || d.Phase > 4 || (d.Boot != "" && (err != nil || len(boot) != 32)) {
			return nil, fmt.Errorf("invalid durable removal decision")
		}

		hash := sha256.Sum256(d.Snapshot)

		key := hex.EncodeToString(hash[:]) + "/" + d.Boot
		if seen[key] {
			return nil, fmt.Errorf("duplicate durable removal decision")
		}

		seen[key] = true
	}

	return entries, nil
}

func encodeRemovals(entries []removalDecision) (string, error) {
	data, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	// Reserve the eventual boot binding and retirement marker's encoded size,
	// so filling an admitted placeholder cannot exhaust the byte budget later.
	reserved := append([]removalDecision(nil), entries...)
	for i := range reserved {
		if reserved[i].Boot == "" {
			reserved[i].Boot = fmt.Sprintf("%064x", 0)
		}

		reserved[i].Done = true
	}

	budget, err := json.Marshal(reserved)
	if err != nil {
		return "", err
	}

	if len(entries) > catchupLimit || len(budget) > catchupBytes {
		return "", fmt.Errorf("removal catch-up budget exhausted; retire recipients or replace their Pods")
	}

	return string(data), nil
}

// Part of the same CAS as the global decision. Snapshots are self-contained and
// survive topology chunk GC. Only excluded, entirely empty recipients qualify.
func (s *Server) advanceRemovals(universe string, r *rollout, phase uint32, raw string) (string, error) {
	entries, err := removalHistory(raw, universe, r.revision)
	if err != nil {
		return "", err
	}

	u := identityBytes("universe", universe)

	var t *topologyIndex
	if s.source != nil {
		t = s.source.topologies[[32]byte(u)]
	}

	if t == nil || t.g.Revision != r.revision {
		return "", fmt.Errorf("missing removal topology")
	}

	return planRemovals(t, r, phase, entries)
}

func planRemovals(t *topologyIndex, r *rollout, phase uint32, entries []removalDecision) (string, error) {
	kept := entries[:0]
	for _, d := range entries {
		var snap pb.Snapshot

		if err := proto.Unmarshal(d.Snapshot, &snap); err != nil {
			return "", err
		}

		if snap.Revision == r.revision && phase == 5 {
			continue
		}

		name, ok := t.byID[hex.EncodeToString(snap.Node)]
		// The production subscription already rejects an old Pod UID. A new
		// Pod has no process-local commitment belonging to the replaced Pod.
		if !ok || t.g.Nodes[name].PodUID != d.PodUID {
			continue
		}

		if snap.Revision < r.revision && (d.Done || d.Boot == "") {
			continue // Never delivered: every new delivery binds a boot first.
		}

		if snap.Revision == r.revision && phase >= 2 && phase <= 4 {
			d.Phase = phase
		}

		kept = append(kept, d)
	}

	entries = kept

	if phase >= 1 && phase <= 4 {
		names := make([]string, 0, len(t.g.Nodes))
		for name := range t.g.Nodes {
			names = append(names, name)
		}

		sort.Strings(names)

		for _, name := range names {
			m := t.g.Nodes[name]
			if m.IP != "" || m.PodUID == "" {
				continue
			}

			snap := t.snapshot(m.ID)
			if snap == nil || snap.Idle || len(snap.Volumes) != 0 || len(snap.Peers) != 0 {
				return "", fmt.Errorf("excluded recipient is not empty")
			}

			data, err := marshalSnapshot(snap)
			if err != nil {
				return "", err
			}

			found := false

			for _, d := range entries {
				if string(d.Snapshot) == string(data) {
					found = true
					break
				}
			}

			if !found {
				entries = append(entries, removalDecision{Snapshot: data, PodUID: m.PodUID, Phase: phase})
			}
		}
	}

	return encodeRemovals(entries)
}

// Accepting any newer candidate proves this boot cleared its earlier local
// commitment. This also collects lost retirement acknowledgments. It says
// nothing about another boot.
func (s *Server) collectRemovals(ctx context.Context, r *rollout, universe, node, boot string) error {
	entries, err := removalHistory(r.pointer.Data["removals"], universe, r.revision)
	if err != nil {
		return err
	}

	kept := make([]removalDecision, 0, len(entries))
	for _, d := range entries {
		var snap pb.Snapshot

		if err := proto.Unmarshal(d.Snapshot, &snap); err != nil {
			return err
		}

		if d.Boot == boot && hex.EncodeToString(snap.Node) == node && snap.Revision < r.revision {
			continue
		}

		kept = append(kept, d)
	}

	if len(kept) != len(entries) {
		return s.saveRemovals(ctx, r, kept)
	}

	return nil
}

// All history mutations use the rollout's RV and B14 invalidation rule. An
// ambiguous write cannot authorize a command or a latest-revision acknowledgment.
func (s *Server) saveRemovals(ctx context.Context, r *rollout, entries []removalDecision) error {
	if r.invalid {
		return fmt.Errorf("rollout requires uncached reload")
	}

	raw, err := encodeRemovals(entries)
	if err != nil {
		return err
	}

	cm := r.pointer.DeepCopy()

	cm.Data["removals"] = raw
	if err := s.controlStore.client.Update(ctx, cm); err != nil {
		r.invalidate()
		return err
	}

	r.pointer = cm

	return nil
}

// A requested digest can only select a retained decision for this exact node,
// Pod and boot. Old acknowledgments never enter the latest rollout's barriers.
func (s *Server) catchup(ctx context.Context, r *rollout, universe, node, pod, boot, digest string, ack uint64) (*entry, uint32, error) {
	entries, err := removalHistory(r.pointer.Data["removals"], universe, r.revision)
	if err != nil {
		return nil, 0, err
	}

	var another *removalDecision

	for i, d := range entries {
		hash := sha256.Sum256(d.Snapshot)
		if digest != hex.EncodeToString(hash[:]) {
			continue
		}

		var snap pb.Snapshot

		if err := proto.Unmarshal(d.Snapshot, &snap); err != nil {
			return nil, 0, err
		}

		if hex.EncodeToString(snap.Node) != node || d.PodUID != pod {
			return nil, 0, fmt.Errorf("removal decision identity mismatch")
		}

		if d.Boot != "" && d.Boot != boot {
			copy := d
			another = &copy

			continue
		}

		if d.Boot == "" {
			entries[i].Boot = boot
			if err := s.saveRemovals(ctx, r, entries); err != nil {
				return nil, 0, err
			}
			// Bind first; a claimed retirement from an unregistered boot is not GC.
		} else if ack == 4 && d.Phase == 4 && snap.Revision < r.revision {
			entries = append(entries[:i], entries[i+1:]...)
			if err := s.saveRemovals(ctx, r, entries); err != nil {
				return nil, 0, err
			}

			return nil, 0, nil
		} else if ack == 4 && r.phase == 4 && !d.Done {
			entries[i].Done = true
			if err := s.saveRemovals(ctx, r, entries); err != nil {
				return nil, 0, err
			}
		}

		if snap.Revision < r.revision {
			if d.Phase < 3 {
				return nil, 0, fmt.Errorf("removal decision is not terminal")
			}

			e, err := newEntry(d.Snapshot, snap.Revision, s.signer)

			return e, d.Phase, err
		}

		return nil, 0, nil // Exact current-boot match wins over earlier other boots.
	}

	if another != nil {
		var snap pb.Snapshot

		if err := proto.Unmarshal(another.Snapshot, &snap); err != nil {
			return nil, 0, err
		}

		if snap.Revision < r.revision {
			return nil, 0, fmt.Errorf("removal decision boot mismatch")
		}
		// A restarted process may stage the current empty candidate. Retain the
		// original boot's obligation until its own retirement or Pod replacement.
		another.Boot = boot

		another.Done = false
		if err := s.saveRemovals(ctx, r, append(entries, *another)); err != nil {
			return nil, 0, err
		}
	}

	return nil, 0, nil
}

// Forward recovery: durable obligations, boot grants and retirement.

// Unknown boots remain obligations. Never evict by age or boot.
type forwardDecision struct {
	Snapshot []byte           `json:"-"` // Staged payload, never persisted inline.
	Ref      *forwardSnapshot `json:"ref,omitempty"`
	PodUID   string           `json:"podUID"`
	Boot     string           `json:"boot,omitempty"`
	Grant    uint64           `json:"grant,omitempty"`
}

const forwardBytes = 256 * 1024 // room for B13 and the serving manifest

// Only a successful validation of exact durable bytes can populate this cache.
// All access is under Server.mu. Returned decisions are private deep copies:
// planning, binding and collection must never mutate the validated cache.
type validatedForwardHistory struct {
	pointer  *corev1.ConfigMap
	raw      string
	rv       string
	universe string
	revision uint64
	entries  []forwardDecision
}

func (r *rollout) forwardHistory(universe string) ([]forwardDecision, error) {
	if r.invalid {
		r.forwards = nil
		return nil, fmt.Errorf("rollout requires uncached reload")
	}

	raw := r.pointer.Data["forwards"]

	c := r.forwards
	if c == nil || c.pointer != r.pointer || c.raw != raw || c.rv != r.pointer.ResourceVersion || c.universe != universe || c.revision != r.revision {
		r.forwards = nil

		entries, err := forwardHistory(raw, universe, r.revision)
		if err != nil {
			return nil, err
		}

		c = &validatedForwardHistory{r.pointer, raw, r.pointer.ResourceVersion, universe, r.revision, entries}
		r.forwards = c
	}

	entries := make([]forwardDecision, len(c.entries))

	refs := make([]forwardSnapshot, len(c.entries))
	for i, d := range c.entries {
		entries[i] = d
		refs[i] = *d.Ref // forwardHistory accepts references only, never inline payloads.
		entries[i].Ref = &refs[i]
	}

	return entries, nil
}

func forwardHistory(raw, universe string, revision uint64) ([]forwardDecision, error) {
	var ds []forwardDecision

	if len(raw) > forwardBytes {
		return nil, fmt.Errorf("forward history byte budget")
	}

	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &ds); err != nil {
			return nil, err
		}
	}

	if len(ds) > catchupLimit {
		return nil, fmt.Errorf("forward history entry budget")
	}

	seen := map[string]bool{}

	for _, d := range ds {
		boot, err := hex.DecodeString(d.Boot)
		if d.Ref == nil {
			return nil, fmt.Errorf("forward snapshot reference required")
		}

		if err := d.validate(universe, revision); err != nil {
			return nil, err
		}

		if d.PodUID == "" || (d.Boot != "" && (err != nil || len(boot) != 32)) || (d.Grant != 0 && (d.Boot == "" || d.Grant <= d.snapshotRef().Revision || d.Grant > revision)) {
			return nil, fmt.Errorf("invalid forward decision")
		}

		key := d.snapshotRef().Digest + "/" + d.Boot
		if seen[key] {
			return nil, fmt.Errorf("duplicate forward decision")
		}

		seen[key] = true
	}

	if err := forwardPayloadBudget(ds); err != nil {
		return nil, err
	}

	return ds, nil
}

func encodeForwards(ds []forwardDecision) (string, error) {
	if err := forwardPayloadBudget(ds); err != nil {
		return "", err
	}

	ds = append([]forwardDecision(nil), ds...)
	for i := range ds {
		ds[i].Ref = ds[i].snapshotRef()
		ds[i].Snapshot = nil
	}

	data, err := json.Marshal(ds)
	if err != nil {
		return "", err
	}

	reserved := append([]forwardDecision(nil), ds...)
	// Reserve a first bound entry for each wildcard before topology commit.
	for _, d := range ds {
		if d.Boot != "" {
			continue
		}

		bound := false

		for _, b := range ds {
			if b.Boot != "" && b.Ref.Digest == d.Ref.Digest {
				bound = true
				break
			}
		}

		if !bound {
			reserved = append(reserved, d)
		}
	}

	for i := range reserved {
		reserved[i].Boot = fmt.Sprintf("%064x", 0)
		reserved[i].Grant = ^uint64(0)
	}

	b, err := json.Marshal(reserved)
	if err != nil {
		return "", err
	}

	if len(reserved) > catchupLimit || len(b) > forwardBytes {
		return "", fmt.Errorf("forward history capacity exhausted")
	}

	return string(data), nil
}

func (s *Server) saveForwards(ctx context.Context, r *rollout, ds []forwardDecision) error {
	if r.invalid {
		return fmt.Errorf("rollout requires uncached reload")
	}

	raw, err := encodeForwards(ds)
	if err != nil {
		return err
	}

	if raw == r.pointer.Data["forwards"] {
		return nil
	}
	// Immutable payloads precede the sole authority/CAS point. Failed creates
	// leave only orphans; retries verify existing bytes rather than overwrite them.
	written := map[string]bool{}
	for _, d := range ds {
		if len(d.Snapshot) != 0 && !written[d.snapshotRef().Digest] {
			if err := s.controlStore.putForwardSnapshot(ctx, r.pointer.Name, d); err != nil {
				r.invalidate()
				return err
			}

			written[d.snapshotRef().Digest] = true
		}
	}

	cm := r.pointer.DeepCopy()

	cm.Data["forwards"] = raw
	if err = s.controlStore.client.Update(ctx, cm); err != nil {
		r.invalidate()
		return err
	}

	r.pointer = cm

	return nil
}

// Persist obligations BEFORE topology commit. Intent alone grants no terminal
// phase: delivery starts only after a strictly newer durable topology is installed.
func (s *Server) planForward(ctx context.Context, old, next *topologyIndex) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.planForwardLocked(ctx, old, next)
}

func (s *Server) planForwardLocked(ctx context.Context, old, next *topologyIndex) error {
	r, err := s.rolloutFor(ctx, old)
	if err != nil {
		return err
	}

	if r.phase < 2 || r.phase == 5 {
		return fmt.Errorf("prepare rollout must complete or time out")
	}

	ds, err := r.forwardHistory(old.g.Universe)
	if err != nil {
		return err
	}
	// Only the installed, committed selection can make an obligation inaccessible.
	// The proposal may fail to commit or disappear after this ledger write.
	ds = committedForwards(old, ds)

	names := make([]string, 0, len(old.g.Nodes))
	for n := range old.g.Nodes {
		names = append(names, n)
	}

	sort.Strings(names)

	for _, n := range names {
		m := old.g.Nodes[n]
		if r.acks[m.ID].phase == 4 {
			continue
		}

		if m.IP == "" || m.PodUID == "" {
			continue
		}

		name, ok := next.byID[m.ID]
		if !ok || next.g.Nodes[name].PodUID != m.PodUID {
			continue
		}

		data, err := marshalSnapshot(old.snapshot(m.ID))
		if err != nil {
			return err
		}

		found := false
		h := sha256.Sum256(data)

		digest := hex.EncodeToString(h[:])
		for _, d := range ds {
			if d.snapshotRef().Digest == digest {
				found = true
			}
		}

		if !found {
			ds = append(ds, forwardDecision{Snapshot: data, PodUID: m.PodUID})
			// Bound planning memory too; do not accumulate an arbitrarily large
			// universe's recipient bodies before discovering it cannot fit.
			if _, err := encodeForwards(ds); err != nil {
				return err
			}
		}
	}

	if _, err := encodeForwards(ds); err != nil {
		return err
	}

	rem, err := removalHistory(r.pointer.Data["removals"], old.g.Universe, r.revision)
	if err != nil {
		return err
	}

	proposal := *next.g
	if proposal.Revision == r.revision {
		proposal.Revision++
	}

	t, err := indexGeneration(&proposal)
	if err != nil {
		return err
	}

	if _, err := planRemovals(t, &rollout{revision: proposal.Revision, phase: 1}, 1, rem); err != nil {
		return err
	}

	return s.saveForwards(ctx, r, ds)
}

func committedForwards(t *topologyIndex, ds []forwardDecision) []forwardDecision {
	kept := make([]forwardDecision, 0, len(ds))
	for _, d := range ds {
		name, ok := t.byID[d.snapshotRef().Node]
		if ok && t.g.Nodes[name].PodUID == d.PodUID {
			kept = append(kept, d)
		}
	}

	return kept
}

// A phase-0 report alone never qualifies. Rust atomically rechecks eligibility.
func (s *Server) forward(ctx context.Context, r *rollout, universe, node, pod, boot, digest, eligible string, ack uint64) (*entry, []byte, uint64, error) {
	ds, err := r.forwardHistory(universe)
	if err != nil {
		return nil, nil, 0, err
	}

	for i, d := range ds {
		ref := d.snapshotRef()
		if ref.Node != node || d.PodUID != pod || ref.Revision >= r.revision {
			continue
		}

		if digest != ref.Digest {
			continue
		}

		if d.Boot != "" && d.Boot != boot {
			continue
		}

		if d.Boot == "" {
			found := false

			for _, exact := range ds {
				if exact.snapshotRef().Digest == ref.Digest && exact.Boot == boot {
					found = true
				}
			}

			if found {
				continue
			}

			copy := d
			copy.Boot = boot
			ds = append(ds, copy)
			i = len(ds) - 1
		}

		data, err := s.controlStore.readForwardSnapshot(ctx, universe, d)
		if err != nil {
			return nil, nil, 0, err
		}

		if ack == 4 && d.Boot == boot {
			ds = append(ds[:i], ds[i+1:]...)
			err = s.saveForwards(ctx, r, ds)

			return nil, nil, 0, err
		}

		if eligible == digest {
			ds[i].Grant = r.revision
			if err = s.saveForwards(ctx, r, ds); err != nil {
				return nil, nil, 0, err
			}

			h, err := hex.DecodeString(ref.Digest)
			if err != nil {
				return nil, nil, 0, err
			}

			return nil, h, ref.Revision, nil
		}

		if err = s.saveForwards(ctx, r, ds); err != nil {
			return nil, nil, 0, err
		}

		e, err := newEntry(data, ref.Revision, s.signer)

		return e, nil, 0, err
	}

	return nil, nil, 0, nil
}

func (s *Server) collectForwards(ctx context.Context, r *rollout, universe, node, pod, boot string) error {
	ds, err := r.forwardHistory(universe)
	if err != nil {
		return err
	}

	if len(ds) == 0 {
		return nil
	}

	kept := ds[:0]
	for _, d := range ds {
		ref := d.snapshotRef()
		if d.Boot == boot && d.PodUID == pod && ref.Node == node && ref.Revision < r.revision {
			continue
		}

		kept = append(kept, d)
	}

	if len(kept) == len(ds) {
		return nil
	}

	return s.saveForwards(ctx, r, kept)
}

// Immutable forward payloads: bounded references, chunk storage and GC protection.

// Bound both individual reads and cumulative retained payloads independently of
// the ledger's metadata budget. Boots/reservations share one immutable payload.
const (
	forwardSnapshotBytes = 64 * 1024 * 1024
	forwardPayloadBytes  = 256 * 1024 * 1024
)

type forwardSnapshot struct {
	Digest   string `json:"digest"`
	Universe string `json:"universe"`
	Node     string `json:"node"`
	Revision uint64 `json:"revision"`
	Size     int    `json:"size"`
}

func (d forwardDecision) snapshotRef() *forwardSnapshot {
	if len(d.Snapshot) == 0 {
		return d.Ref
	}

	var snap pb.Snapshot

	if err := proto.Unmarshal(d.Snapshot, &snap); err != nil {
		return nil
	}

	h := sha256.Sum256(d.Snapshot)

	return &forwardSnapshot{hex.EncodeToString(h[:]), hex.EncodeToString(snap.Universe), hex.EncodeToString(snap.Node), snap.Revision, len(d.Snapshot)}
}

func (d forwardDecision) validate(universe string, revision uint64) error {
	if d.Ref != nil && len(d.Snapshot) != 0 {
		return fmt.Errorf("mixed inline/reference forward snapshot")
	}

	if len(d.Snapshot) != 0 {
		var snap pb.Snapshot
		if proto.Unmarshal(d.Snapshot, &snap) != nil {
			return fmt.Errorf("invalid inline forward snapshot")
		}
	}

	r := d.snapshotRef()

	validHash := func(s string) bool {
		b, err := hex.DecodeString(s)
		return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
	}
	if r == nil || !validHash(r.Digest) || !validHash(r.Node) || r.Universe != identity("universe", universe) || r.Revision == 0 || r.Revision > revision || r.Size <= 0 || r.Size > forwardSnapshotBytes {
		return fmt.Errorf("invalid forward snapshot reference")
	}

	return nil
}

func forwardPayloadBudget(ds []forwardDecision) error {
	seen := map[string]forwardSnapshot{}
	total := 0

	for _, d := range ds {
		r := d.snapshotRef()
		if r == nil || r.Size <= 0 || r.Size > forwardSnapshotBytes {
			return fmt.Errorf("forward snapshot capacity exhausted")
		}

		if prior, ok := seen[r.Digest]; ok {
			if prior != *r {
				return fmt.Errorf("inconsistent forward snapshot reference")
			}

			continue
		}

		seen[r.Digest] = *r

		total += r.Size
		if total > forwardPayloadBytes {
			return fmt.Errorf("forward payload capacity exhausted")
		}
	}

	return nil
}

func forwardChunkName(base string, r *forwardSnapshot, index int) string {
	return fmt.Sprintf("%s-f-%s-%d", base, r.Digest, index)
}

func validForwardChunk(part *corev1.ConfigMap, base string, size int) bool {
	return part.Labels[stateLabel] == "forward" && part.Labels[stateOwnerLabel] == base && part.Immutable != nil && *part.Immutable && len(part.BinaryData) == 1 && len(part.Data) == 0 && len(part.BinaryData["snapshot"]) == size
}

func (s stateStore) putForwardSnapshot(ctx context.Context, rolloutName string, d forwardDecision) error {
	r := d.snapshotRef()
	base := strings.TrimSuffix(rolloutName, "-rollout")

	for offset := 0; offset < r.Size; offset += stateChunkSize {
		immutable := true
		payload := d.Snapshot[offset:min(offset+stateChunkSize, r.Size)]

		part := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: s.namespace, Name: forwardChunkName(base, r, offset/stateChunkSize), Labels: map[string]string{stateLabel: "forward", stateOwnerLabel: base}}, Immutable: &immutable, BinaryData: map[string][]byte{"snapshot": payload}}
		if err := s.client.Create(ctx, part); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return err
			}

			existing := &corev1.ConfigMap{}
			if err := s.client.Get(ctx, client.ObjectKeyFromObject(part), existing); err != nil {
				return err
			}

			if !validForwardChunk(existing, base, len(payload)) || !bytes.Equal(existing.BinaryData["snapshot"], payload) {
				return fmt.Errorf("forward chunk collision: %s", part.Name)
			}
		}
	}

	return nil
}

func (s stateStore) readForwardSnapshot(ctx context.Context, universe string, d forwardDecision) ([]byte, error) {
	if err := d.validate(universe, ^uint64(0)); err != nil {
		return nil, err
	}

	if d.Ref == nil {
		return d.Snapshot, nil
	}

	r := d.Ref
	base := stateName(universe)

	data := make([]byte, 0, r.Size)
	for offset := 0; offset < r.Size; offset += stateChunkSize {
		part := &corev1.ConfigMap{}

		name := forwardChunkName(base, r, offset/stateChunkSize)
		if err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: name}, part); err != nil {
			return nil, err
		}

		if !validForwardChunk(part, base, min(stateChunkSize, r.Size-offset)) {
			return nil, fmt.Errorf("invalid forward chunk: %s", name)
		}

		data = append(data, part.BinaryData["snapshot"]...)
	}

	inline := forwardDecision{Snapshot: data}
	if err := inline.validate(universe, r.Revision); err != nil {
		return nil, err
	}

	if *inline.snapshotRef() != *r {
		return nil, fmt.Errorf("forward snapshot digest/binding mismatch")
	}

	return data, nil
}

// GC uses the durable ledger, never a proposed selection or cached rollout. A
// malformed/unreadable ledger prevents collection. Caller holds the topology /
// subscription writer lock (the elected leader is the only writer).
func protectForwardChunks(raw, universe string, keep map[string]bool) error {
	ds, err := forwardHistory(raw, universe, ^uint64(0))
	if err != nil {
		return err
	}

	for _, d := range ds {
		if d.Ref != nil {
			for offset := 0; offset < d.Ref.Size; offset += stateChunkSize {
				keep[forwardChunkName(stateName(universe), d.Ref, offset/stateChunkSize)] = true
			}
		}
	}

	return nil
}
