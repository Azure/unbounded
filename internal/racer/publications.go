// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// VersionRecord is the only persisted topology bookkeeping. Hashes cover
// canonical content excluding counters. No member history or publication bytes
// are stored. ResourceVersion CAS must precede installing a publication.
type VersionRecord struct {
	Cluster           wire.ClusterID         `json:"cluster"`
	Sequence          wire.Sequence          `json:"sequence,string"`
	MembershipVersion wire.MembershipVersion `json:"membership_version,string"`
	ContentHash       string                 `json:"content_hash"`
	MembershipHash    string                 `json:"membership_hash"`
}

// PreparedPublication owns encoded candidate bytes. Publishers require CAS;
// replicas require canonical validation and an authoritative durable confirmation.
type PreparedPublication struct {
	owner           *Publications
	previous        VersionRecord
	resourceVersion string
	record          VersionRecord
	encoded         string
	delta           string
	deltaBase       string
}

type CommittedPublication struct {
	owner      *Publications
	record     VersionRecord
	encoded    string
	delta      string
	deltaBase  string
	leadership context.Context
	authority  context.Context
}

// publicationResponse is a response-only view, never installable state. The
// caller owns one admitted image context through both write and flush.
type publicationResponse struct{ encoded string }

func (p publicationResponse) writeTo(ctx context.Context, w io.Writer) (int64, error) {
	var written int64

	for remaining := p.encoded; remaining != ""; {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		// ResponseWriter need not implement StringWriter. Limit conversion scratch
		// to 32 KiB rather than allocating a full publication for every response.
		chunk := remaining[:min(len(remaining), 32*1024)]
		n, err := io.WriteString(requestWriter{ctx: ctx, writer: w}, chunk)

		written += int64(n)
		if err != nil {
			return written, err
		}

		if n != len(chunk) {
			return written, io.ErrShortWrite
		}

		remaining = remaining[n:]
	}

	return written, ctx.Err()
}

// writeContext pins one response to its image's revocable authority and the
// freshness deadline at write admission. Later confirmations cannot extend an
// in-flight response, and supersession or suspension permanently revokes it.
func (p *CommittedPublication) writeContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if p == nil || p.owner == nil {
		return nil, nil, wire.Unavailable
	}

	owner := p.owner
	owner.mu.Lock()
	defer owner.mu.Unlock()

	if _, err := owner.currentLocked(); err != nil {
		return nil, nil, err
	}

	if p.authority == nil || p.authority != owner.current.authority || p.authority.Err() != nil {
		return nil, nil, wire.Unavailable
	}

	ctx, cancel := context.WithDeadline(parent, owner.confirmed.Add(owner.maxAge))
	stop := context.AfterFunc(p.authority, cancel)

	return publicationWriteContext{Context: ctx, authority: p.authority}, func() { stop(); cancel() }, nil
}

// Cancellation callbacks close transports asynchronously. Check the image's
// authority synchronously too, so an unblocked writer cannot race revocation.
type publicationWriteContext struct {
	context.Context
	authority context.Context
}

func (c publicationWriteContext) Err() error {
	if err := c.authority.Err(); err != nil {
		return err
	}

	if deadline, ok := c.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}

	return c.Context.Err()
}

// Publications owns only the current immutable publication and one broadcast
// notification. Older state belongs to dataplanes; poll admission belongs to Server.
type Publications struct {
	mu        sync.Mutex
	current   *CommittedPublication
	changed   chan struct{}
	suspended bool
	process   context.Context
	confirmed time.Time
	maxAge    time.Duration
	observed  VersionRecord
	revoke    context.CancelFunc
}

func NewPublications() *Publications {
	return &Publications{changed: make(chan struct{}), maxAge: 30 * time.Second}
}

func (p *Publications) bindProcess(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.process = ctx
}

func (p *Publications) notifyLocked() { close(p.changed); p.changed = make(chan struct{}) }

func (p *Publications) Prepare(previous VersionRecord, resourceVersion string, members AcceptedMembers, caches []wire.CacheDefinition) (*PreparedPublication, error) {
	if !previous.valid() || resourceVersion == "" {
		return nil, wire.InvalidRequest
	}

	v := wire.Publication{SchemaVersion: wire.SchemaVersion, Cluster: previous.Cluster, Caches: caches, Members: make([]wire.Member, 0, len(members))}
	for id, member := range members {
		if id != member.Node {
			return nil, wire.InvalidRequest
		}

		v.Members = append(v.Members, member)
	}

	candidate, err := wire.NewCanonicalCandidate(v)
	if err != nil {
		return nil, err
	}

	content, membership, err := candidate.ContentHashes()
	if err != nil {
		return nil, err
	}

	record := previous
	if content != previous.ContentHash {
		if record.Sequence == ^wire.Sequence(0) {
			return nil, wire.Unavailable
		}

		record.Sequence++
	}

	if membership != previous.MembershipHash {
		if content == previous.ContentHash || record.MembershipVersion == ^wire.MembershipVersion(0) {
			return nil, wire.Unavailable
		}

		record.MembershipVersion++
	}

	record.ContentHash, record.MembershipHash = content, membership

	encoded, err := candidate.EncodePublication(record.Sequence, record.MembershipVersion)
	if err != nil {
		return nil, err
	}

	prepared := &PreparedPublication{owner: p, previous: previous, resourceVersion: resourceVersion, record: record, encoded: string(encoded)}
	v.Sequence, v.MembershipVersion = record.Sequence, record.MembershipVersion
	// Capture immutable base under the lock, then diff/encode entirely outside it.
	if current, err := p.Current(); err == nil && current.record == previous && record.Sequence > previous.Sequence {
		if base, err := wire.DecodePublication(strings.NewReader(current.encoded)); err == nil {
			if delta, err := wire.EncodeDelta(base, v); err == nil && len(delta) < len(encoded) {
				prepared.delta, prepared.deltaBase = string(delta), previous.ContentHash
			}
		}
	}

	return prepared, nil
}

// CommitVersion mints publisher installable state after a resource-version CAS.
// Even unchanged content is CAS-confirmed; its counters and bytes remain identical.
func (r *TopologyReconciler) CommitVersion(ctx context.Context, p *PreparedPublication) (*CommittedPublication, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if p == nil || p.owner != r.Publications || p.previous.Cluster != r.Config.Cluster {
		return nil, wire.InvalidRequest
	}

	cm, previous, err := readVersion(ctx, r.APIReader, r.Config)
	if err != nil {
		r.suspendInvalidAuthority(err)
		return nil, err
	}

	if cm.ResourceVersion != p.resourceVersion || previous != p.previous {
		return nil, apierrors.NewConflict(corev1.Resource("configmaps"), cm.Name, wire.Conflict)
	}

	cm.Data = versionData(p.record)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := r.Update(ctx, cm); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return &CommittedPublication{owner: p.owner, record: p.record, encoded: p.encoded, delta: p.delta, deltaBase: p.deltaBase, leadership: ctx}, nil
}

// ForBase returns a shared bounded delta only for the exact authenticated cursor.
// Coalesced/skipped updates and controller restarts automatically use the full image.
func (p *CommittedPublication) ForBase(hash string) publicationResponse {
	if hash == "" || hash != p.deltaBase || p.delta == "" {
		return publicationResponse{encoded: p.encoded}
	}

	return publicationResponse{encoded: p.delta}
}

func (p *Publications) Install(next *CommittedPublication) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if next == nil || next.owner != p || next.leadership == nil || !next.record.valid() || next.encoded == "" {
		return wire.InvalidRequest
	}

	if err := next.leadership.Err(); err != nil {
		return err
	}

	if err := p.observeLocked(next.record); err != nil {
		return err
	}

	if p.process != nil {
		copy := *next
		copy.leadership = p.process
		next = &copy
	}

	if current := p.current; current != nil {
		// observeLocked is the sole counter/hash high-water guard. Installed
		// state can only lag that observation, never exceed it.
		if next.record.Sequence == current.record.Sequence {
			if next.record != current.record || next.encoded != current.encoded {
				return wire.Conflict
			}

			p.confirmed = time.Now()
			if current.leadership.Err() != nil || p.suspended {
				next = p.authorizeLocked(next)
				p.current = next
			}

			if p.suspended {
				p.suspended = false
				p.notifyLocked()
			}

			return nil
		}
	}

	p.current = p.authorizeLocked(next)
	p.confirmed = time.Now()
	p.suspended = false
	p.notifyLocked()

	return nil
}

func (p *Publications) authorizeLocked(next *CommittedPublication) *CommittedPublication {
	if p.revoke != nil {
		p.revoke()
	}

	copy := *next
	copy.authority, p.revoke = context.WithCancel(copy.leadership)

	return &copy
}

// confirm never promotes a hash to an image. It only renews the freshness of an
// already validated image when all durable counters and hashes still match.
func (p *Publications) confirm(record VersionRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.observeLocked(record); err != nil {
		return err
	}

	if p.current != nil && record == p.current.record && !p.suspended {
		p.confirmed = time.Now()
	}

	return nil
}

// observed is independent of installed bytes, including before the first image.
// Suspension never forgets this high-water mark. Skipped versions may return to
// earlier hashes, but counters and unchanged membership versions must agree.
func (p *Publications) observeLocked(record VersionRecord) error {
	if !record.valid() {
		p.suspendLocked()
		return wire.Conflict
	}

	if old := p.observed; old.Sequence != 0 {
		rollback := record.Cluster != old.Cluster || record.Sequence < old.Sequence || record.MembershipVersion < old.MembershipVersion
		conflictingReplay := record.Sequence == old.Sequence && record != old
		changedMembership := record.MembershipVersion == old.MembershipVersion && record.MembershipHash != old.MembershipHash
		// Check progression only after ruling out rollback, before subtracting
		// unsigned counters. Each membership change requires a publication change.
		if rollback || conflictingReplay || changedMembership || uint64(record.MembershipVersion-old.MembershipVersion) > uint64(record.Sequence-old.Sequence) {
			p.suspendLocked()
			return wire.Conflict
		}
	}

	p.observed = record

	return nil
}

func (p *Publications) Current() (*CommittedPublication, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.currentLocked()
}

// CurrentAndSubscribe atomically reads the current publication and subscribes to
// changes, including when unavailable. The channel closes on install or suspension;
// leadership cancellation must be observed separately by the caller.
func (p *Publications) CurrentAndSubscribe() (*CommittedPublication, <-chan struct{}, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current, err := p.currentLocked()

	return current, p.changed, err
}

func (p *Publications) currentLocked() (*CommittedPublication, error) {
	if p.current == nil || p.suspended || time.Since(p.confirmed) >= p.maxAge {
		return nil, wire.Unavailable
	}

	if err := p.current.leadership.Err(); err != nil {
		return nil, err
	}

	return p.current, nil
}

// Suspend withdraws readiness and wakes polls after failure to validate durable
// authority. Keep bytes/history for a later successful CAS, never serve them until
// then. Invalid desired inputs alone do not suspend the last valid publication.
func (p *Publications) Suspend() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.suspendLocked()
}

func (p *Publications) suspendLocked() {
	if p.revoke != nil {
		p.revoke()
	}

	if !p.suspended {
		p.suspended = true
		p.notifyLocked()
	}
}

// Wait rejects invalid identities/cursors and honors context cancellation and
// certificate expiration. It shares publication bytes and broadcast notifications;
// callers own admission for the full response lifetime, including writes and flush.
func (p *Publications) Wait(ctx context.Context, identity NodeIdentity, after *wire.Sequence) (*CommittedPublication, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !wire.ValidUUID(string(identity.node)) || !time.Now().Before(identity.expires) {
		return nil, wire.Unauthenticated
	}

	current, changed, err := p.CurrentAndSubscribe()
	if err != nil {
		return nil, err
	}

	if identity.cluster != current.record.Cluster {
		return nil, wire.Forbidden
	}

	if after != nil && *after == 0 {
		return nil, wire.Conflict
	}

	if after != nil && *after > current.record.Sequence {
		return nil, wire.Unavailable
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !time.Now().Before(identity.expires) {
		return nil, wire.Unauthenticated
	}

	if after == nil || current.record.Sequence > *after {
		if err := current.leadership.Err(); err != nil {
			return nil, err
		}

		return current, nil
	}

	timer := time.NewTimer(wire.PollWait)
	defer timer.Stop()

	expiration := time.NewTimer(time.Until(identity.expires))
	defer expiration.Stop()

	freshness := time.NewTicker(min(p.maxAge, time.Second))
	defer freshness.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-current.leadership.Done():
			return nil, current.leadership.Err()
		case <-expiration.C:
			return nil, wire.Unauthenticated
		case <-freshness.C:
			if _, err := p.Current(); err != nil {
				return nil, err
			}

			continue
		case <-timer.C:
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			if err := current.leadership.Err(); err != nil {
				return nil, err
			}

			if !time.Now().Before(identity.expires) {
				return nil, wire.Unauthenticated
			}

			return nil, nil
		case <-changed:
		}

		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !time.Now().Before(identity.expires) {
			return nil, wire.Unauthenticated
		}

		current, changed, err = p.CurrentAndSubscribe()
		if err != nil {
			return nil, err
		}

		if current.record.Sequence > *after {
			return current, nil
		}
	}
}

func (p *Publications) Ready(_ *http.Request) error { _, err := p.Current(); return err }

const installationUIDAnnotation = "racer.unbounded-cloud.io/installation-uid"

// Initialize consumes a new installation's permanent marker before the sole
// counter Create attempt. It does not run controllers or acquire serving authority.
func Initialize(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return err
	}

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	return initializeVersion(ctx, c, c, cfg)
}

func readInstallation(ctx context.Context, reader client.Reader, cfg Config, fresh bool) (*corev1.ConfigMap, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cm := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName}, cm); err != nil {
		return nil, authorityReadFailure(err)
	}

	state := "consumed"
	if fresh {
		state = "fresh"
	}

	immutable := cm.Immutable != nil && *cm.Immutable
	if cm.UID == "" || cm.ResourceVersion == "" || cm.DeletionTimestamp != nil || cm.Data["cluster"] != string(cfg.Cluster) || cm.Data["version_configmap"] != cfg.VersionConfigMapName || cm.Data["state"] != state || immutable == fresh {
		return nil, fmt.Errorf("installation marker invalid or already consumed: %w", wire.Unavailable)
	}

	return cm, nil
}

// InitializeVersion never retries marker CAS or counter creation, including
// ambiguous transport failures.
func (r *TopologyReconciler) InitializeVersion(ctx context.Context) error {
	return initializeVersion(ctx, r.Client, r.APIReader, r.Config)
}

func initializeVersion(ctx context.Context, writer client.Writer, reader client.Reader, cfg Config) error {
	if !wire.ValidUUID(string(cfg.Cluster)) {
		return wire.InvalidRequest
	}

	marker, err := readInstallation(ctx, reader, cfg, true)
	if err != nil {
		return err
	}

	key := client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.VersionConfigMapName}
	if err := reader.Get(ctx, key, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}

		return fmt.Errorf("version state already exists: %w", wire.Conflict)
	}

	content, membership, err := wire.ContentHashes(wire.Publication{SchemaVersion: wire.SchemaVersion, Cluster: cfg.Cluster})
	if err != nil {
		return err
	}

	marker.Data["state"] = "consumed"
	immutable := true
	marker.Immutable = &immutable

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := writer.Update(ctx, marker); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	return writer.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Annotations: map[string]string{installationUIDAnnotation: string(marker.UID)}},
		Data:       versionData(VersionRecord{Cluster: cfg.Cluster, Sequence: 1, MembershipVersion: 1, ContentHash: content, MembershipHash: membership}),
	})
}

func versionData(v VersionRecord) map[string]string {
	return map[string]string{"cluster": string(v.Cluster), "sequence": strconv.FormatUint(uint64(v.Sequence), 10), "membership_version": strconv.FormatUint(uint64(v.MembershipVersion), 10), "content_hash": v.ContentHash, "membership_hash": v.MembershipHash}
}

func validHash(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

func (v VersionRecord) valid() bool {
	return wire.ValidUUID(string(v.Cluster)) && v.Sequence > 0 && v.MembershipVersion > 0 && uint64(v.MembershipVersion) <= uint64(v.Sequence) && validHash(v.ContentHash) && validHash(v.MembershipHash)
}

func parseVersion(cm *corev1.ConfigMap, cluster wire.ClusterID, markerUID types.UID) (VersionRecord, error) {
	sequence, e1 := strconv.ParseUint(cm.Data["sequence"], 10, 64)
	membership, e2 := strconv.ParseUint(cm.Data["membership_version"], 10, 64)

	v := VersionRecord{Cluster: wire.ClusterID(cm.Data["cluster"]), Sequence: wire.Sequence(sequence), MembershipVersion: wire.MembershipVersion(membership), ContentHash: cm.Data["content_hash"], MembershipHash: cm.Data["membership_hash"]}
	if e1 != nil || e2 != nil || !v.valid() || v.Cluster != cluster || cm.ResourceVersion == "" || cm.DeletionTimestamp != nil || cm.Annotations[installationUIDAnnotation] != string(markerUID) || strconv.FormatUint(sequence, 10) != cm.Data["sequence"] || strconv.FormatUint(membership, 10) != cm.Data["membership_version"] {
		return VersionRecord{}, fmt.Errorf("durable version state invalid; explicit new-cluster rebootstrap required: %w", wire.Unavailable)
	}

	return v, nil
}

func readVersion(ctx context.Context, reader client.Reader, cfg Config) (*corev1.ConfigMap, VersionRecord, error) {
	marker, err := readInstallation(ctx, reader, cfg, false)
	if err != nil {
		return nil, VersionRecord{}, err
	}

	cm := &corev1.ConfigMap{}

	if err := ctx.Err(); err != nil {
		return nil, VersionRecord{}, err
	}

	if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.VersionConfigMapName}, cm); err != nil {
		return nil, VersionRecord{}, authorityReadFailure(err)
	}

	v, err := parseVersion(cm, cfg.Cluster, marker.UID)

	return cm, v, err
}

// ValidateInstallation checks the durable marker and counter binding required by
// normal startup without modifying state. Provisioners use it before deployment.
func ValidateInstallation(ctx context.Context, reader client.Reader, namespace, cluster string) error {
	_, _, err := readVersion(ctx, reader, Config{
		Namespace: namespace, Cluster: wire.ClusterID(cluster),
		InstallationConfigMapName: "racer-installation", VersionConfigMapName: "racer-version",
	})

	return err
}
