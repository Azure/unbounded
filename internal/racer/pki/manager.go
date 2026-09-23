// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Manager struct {
	client        client.Client
	namespace     string
	options       Options
	mu            sync.RWMutex
	fence         string
	leaderContext context.Context
	// Serialize local commits and garbage collection. Kubernetes CAS still
	// fences concurrent managers, including a paused former leader.
	storeMu      *semaphore.Weighted
	cacheMu      sync.Mutex
	shardCache   map[string]cachedShard
	observations map[string]memberObservation
	localState   atomic.Pointer[state] // immutable metadata, published only after commit
}

const publicationFence = "racer.unbounded.cloud/pki-fence"

// RotationAnnotation requests rotation using a unique, nonempty operator nonce.
const RotationAnnotation = "racer.unbounded-cloud.io/rotate-ca"

// Reserve space below the Kubernetes object limit. Participant records are
// sharded separately; this limit applies to each object, never to the fleet.
const maxStateBytes = 900 * 1024

func encodeState(s *state) ([]byte, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}

	if len(data) > maxStateBytes {
		return nil, errors.New("PKI durable state capacity exhausted; refusing to omit members or leaf records")
	}

	return data, nil
}

// Publish repairs trust publication without advancing rotation. Call this before
// the initial authoritative admission sweep, then Reconcile after that sweep.
func (m *Manager) Publish(ctx context.Context) error { return m.publish(ctx) }

func (m *Manager) checkBootstrap(ctx context.Context) error {
	var states corev1.ConfigMapList
	if err := m.client.List(ctx, &states, client.InNamespace(m.namespace)); err != nil {
		return err
	}

	for _, cm := range states.Items {
		if cm.Labels["racer.unbounded-cloud.io/state"] == "commit" || (strings.HasPrefix(cm.Name, "racer-replica-") && (cm.Data["certificate"] != "" || cm.Data["proof-certificate"] != "")) {
			return ErrLostState
		}
	}

	return nil
}

func New(c client.Client, namespace string, options Options) (*Manager, error) {
	options = options.defaults()

	if c == nil || len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, errors.New("client and valid namespace required")
	}

	if options.LeafLifetime <= 0 || options.ClockSkew < 0 || options.RotateAfter <= 0 || options.ProofLifetime <= 0 || options.ReconcileInterval <= 0 || options.CALifetime <= options.RotateAfter+options.LeafLifetime+2*options.ClockSkew {
		return nil, errors.New("invalid PKI lifetimes")
	}

	return &Manager{client: c, namespace: namespace, options: options, storeMu: semaphore.NewWeighted(1)}, nil
}

func (m *Manager) objectKey(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: m.namespace, Name: name}
}

func (m *Manager) readMetadata(ctx context.Context) (*corev1.Secret, *state, error) {
	secret := &corev1.Secret{}
	if err := m.client.Get(ctx, m.objectKey(SecretName), secret); err != nil {
		return nil, nil, err
	}

	var s state
	if err := strictJSON(secret.Data[StateKey], &s); err != nil {
		return nil, nil, fmt.Errorf("invalid persisted CA state: %w", err)
	}

	if err := validateState(&s); err != nil {
		return nil, nil, err
	}

	return secret, &s, nil
}

func (m *Manager) read(ctx context.Context) (*corev1.Secret, *state, error) {
	secret, s, err := m.readMetadata(ctx)
	if err != nil {
		return nil, nil, err
	}

	if err := m.loadParticipants(ctx, s, ""); err != nil {
		return nil, nil, err
	}

	m.applyObservations(s)

	return secret, s, nil
}

func validateState(s *state) error {
	if (s.Version != 1 && s.Version != 2) || s.Fence == "" || s.FenceAt.IsZero() || s.Generation == 0 || len(s.Authorities) < 1 || len(s.Authorities) > 2 || s.Members == nil || s.Retired == nil || s.NextRotation.IsZero() {
		return errors.New("invalid persisted CA state metadata")
	}

	if err := validateShardReferences(s); err != nil {
		return err
	}

	if (s.Phase == "stable" && len(s.Authorities) != 1) || ((s.Phase == "overlap" || s.Phase == "switched") && len(s.Authorities) != 2) {
		return errors.New("invalid rotation authority count")
	}

	if s.Phase != "stable" && s.Phase != "overlap" && s.Phase != "switched" {
		return errors.New("invalid rotation phase")
	}

	for _, ca := range s.Authorities {
		cert, _, err := parseAuthority(ca)
		if err != nil {
			return err
		}

		if ca.LastIssuedExpiry.After(cert.NotAfter) {
			return errors.New("invalid issued expiry watermark")
		}
	}

	if _, err := ParseBundle(s.bundle().JSON()); err != nil {
		return err
	}

	expected := s.Authorities[0].Digest
	if s.Phase == "switched" {
		expected = s.Authorities[1].Digest
	}

	if s.Active != expected {
		return errors.New("active authority disagrees with rotation phase")
	}

	for key, p := range s.Members {
		if p == nil || key != p.Identity.Key().String() || s.Retired[key] || p.Leaves == nil {
			return errors.New("invalid durable member")
		}

		if _, err := p.Identity.URI(); err != nil {
			return err
		}

		for fingerprint, leaf := range p.Leaves {
			if !hexID.MatchString(fingerprint) || !hexID.MatchString(leaf.Root) || leaf.Expiry.IsZero() {
				return errors.New("invalid durable leaf record")
			}

			for _, ca := range s.Authorities {
				if ca.Digest == leaf.Root && leaf.Expiry.After(ca.LastIssuedExpiry) {
					return errors.New("leaf exceeds expiry watermark")
				}
			}
		}
	}

	return nil
}

// Load validates both persisted objects without creating or changing them.
func (m *Manager) Load(ctx context.Context) error {
	_, s, err := m.read(ctx)
	if err != nil {
		return err
	}

	b, err := m.Bundle(ctx)
	if err != nil {
		return err
	}

	if b.Digest() != s.bundle().Digest() {
		return ErrNotReady
	}

	return nil
}

func (m *Manager) Bundle(ctx context.Context) (TrustBundle, error) {
	cm := &corev1.ConfigMap{}
	if err := m.client.Get(ctx, m.objectKey(ConfigMapName), cm); err != nil {
		return TrustBundle{}, err
	}

	return ParseBundle([]byte(cm.Data[BundleKey]))
}

// AcquireLeadership must only be called after external leader election grants
// leadership. token must be globally unique for this leadership lifetime.
// Context cancellation immediately disables further mutations by this manager.
func (m *Manager) AcquireLeadership(ctx context.Context, token string) error {
	if token == "" || ctx.Err() != nil {
		return ErrNotLeader
	}

	if err := m.storeMu.Acquire(ctx, 1); err != nil {
		return err
	}
	defer m.storeMu.Release(1)

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.fence != "" {
		return errors.New("manager already acquired leadership; create a new manager for a new term")
	}

	secret, s, err := m.readMetadata(ctx)
	if apierrors.IsNotFound(err) {
		// The public object is also a bootstrap tombstone: losing private state
		// must never silently create a new trust domain.
		if err := m.checkBootstrap(ctx); err != nil {
			return err
		}

		cm := &corev1.ConfigMap{}
		if getErr := m.client.Get(ctx, m.objectKey(ConfigMapName), cm); !apierrors.IsNotFound(getErr) {
			if getErr != nil {
				return getErr
			}

			return ErrLostState
		}

		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: m.namespace}, Data: map[string]string{}}
		if err = m.client.Create(ctx, cm); err != nil {
			return err
		}

		ca, caErr := makeCA(m.options.Now(), m.options)
		if caErr != nil {
			return caErr
		}

		s = &state{Version: 1, Fence: token, FenceAt: m.options.Now(), Generation: 1, Active: ca.Digest, Phase: "stable", Authorities: []authority{ca}, Members: map[string]*member{}, Retired: map[string]bool{}, NextRotation: m.options.Now().Add(m.options.RotateAfter)}

		data, marshalErr := encodeState(s)
		if marshalErr != nil {
			return marshalErr
		}

		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: m.namespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{StateKey: data}}
		if err = m.client.Create(ctx, secret); err != nil {
			return err
		}
	} else {
		if err != nil {
			return err
		}

		if s.Fence == token {
			return errors.New("leadership token must not be reused")
		}

		s.Fence = token
		s.FenceAt = m.options.Now()
		// Old process observations never count as fresh proofs after takeover.
		for _, p := range s.Members {
			p.ProofFence = ""
			p.Drained = false
		}

		data, marshalErr := encodeState(s)
		if marshalErr != nil {
			return marshalErr
		}

		secret.Data[StateKey] = data
		if err = m.client.Update(ctx, secret); err != nil {
			return err
		}
	}
	// Fence the second object before returning leadership. An earlier publisher
	// that already captured the old ConfigMap version now loses its CAS; one
	// that reads later sees this fence and cannot publish at all.
	if err = m.claimPublication(ctx, token); err != nil {
		return err
	}

	m.fence = token
	m.leaderContext = ctx
	m.localState.Store(s)

	return nil
}

func (m *Manager) claimPublication(ctx context.Context, token string) error {
	cm := &corev1.ConfigMap{}
	err := m.client.Get(ctx, m.objectKey(ConfigMapName), cm)

	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return err
	}

	_, s, err := m.readMetadata(ctx)
	if err != nil {
		return err
	}

	if s.Fence != token || ctx.Err() != nil {
		return ErrNotLeader
	}

	if missing {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: m.namespace}, Data: map[string]string{BundleKey: string(s.bundle().JSON())}}
	}

	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}

	cm.Annotations[publicationFence] = token
	if missing {
		return m.client.Create(ctx, cm)
	}

	return m.client.Update(ctx, cm)
}

func (m *Manager) leader() (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.fence == "" || m.leaderContext == nil || m.leaderContext.Err() != nil {
		return "", ErrNotLeader
	}

	return m.fence, nil
}

// mutate retries only resource-version conflicts, re-reading and checking the
// persisted fence each time. The callback may run repeatedly and must be pure
// apart from preparing the returned state/result.
func (m *Manager) mutate(ctx context.Context, fn func(*state) error) error {
	return m.mutateParticipants(ctx, "", fn)
}

func (m *Manager) mutateParticipants(ctx context.Context, key string, fn func(*state) error) error {
	// Expired enrollment requests must leave the queue without waiting for API
	// I/O ahead of them, or retries accumulate behind work nobody can receive.
	if err := m.storeMu.Acquire(ctx, 1); err != nil {
		return err
	}
	defer m.storeMu.Release(1)

	for attempt := 0; attempt < 8; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		fence, err := m.leader()
		if err != nil {
			return err
		}

		secret, s, err := m.readMetadata(ctx)
		if err != nil {
			return err
		}

		if s.Fence != fence {
			return ErrNotLeader
		}

		if err := m.loadParticipants(ctx, s, key); err != nil {
			return err
		}

		m.applyObservations(s)

		before, err := snapshotParticipants(s)
		if err != nil {
			return err
		}

		if err = fn(s); err != nil {
			return err
		}

		if _, err = m.leader(); err != nil {
			return err
		}

		data, err := m.prepareCommit(ctx, s, before)
		if err != nil {
			return err
		}

		if bytes.Equal(data, secret.Data[StateKey]) {
			return nil
		}

		secret.Data[StateKey] = data

		err = m.client.Update(ctx, secret)
		if err == nil {
			// Serialize publication with observation validation, never with API I/O.
			m.cacheMu.Lock()
			m.localState.Store(s)
			m.cacheMu.Unlock()
		}

		if !apierrors.IsConflict(err) {
			return err
		}
	}

	return errors.New("PKI state CAS conflict retry limit reached")
}

func (m *Manager) publish(ctx context.Context) error {
	fence, err := m.leader()
	if err != nil {
		return err
	}

	_, s, err := m.readMetadata(ctx)
	if err != nil {
		return err
	}

	if s.Fence != fence {
		return ErrNotLeader
	}

	wanted := s.bundle()
	cm := &corev1.ConfigMap{}

	err = m.client.Get(ctx, m.objectKey(ConfigMapName), cm)
	if apierrors.IsNotFound(err) {
		return m.claimPublication(ctx, fence)
	}

	if err != nil {
		return err
	}

	if cm.Annotations[publicationFence] != fence {
		return ErrNotLeader
	}

	if raw := cm.Data[BundleKey]; raw != "" {
		current, parseErr := ParseBundle([]byte(raw))
		if parseErr != nil {
			return parseErr
		}

		if current.Generation > wanted.Generation || (current.Generation == wanted.Generation && current.Digest() != wanted.Digest()) {
			return errors.New("trust bundle diverges from persisted CA state")
		}

		if current.Digest() == wanted.Digest() {
			return nil
		}
	}
	// Confirm the fence again after reading the publication object's CAS version.
	_, latest, err := m.readMetadata(ctx)
	if err != nil {
		return err
	}

	if latest.Fence != fence {
		return ErrNotLeader
	}

	if latest.bundle().Digest() != wanted.Digest() {
		return ErrNotReady
	}

	if _, err = m.leader(); err != nil {
		return err
	}

	if cm.Data == nil {
		cm.Data = map[string]string{}
	}

	cm.Data[BundleKey] = string(wanted.JSON())

	return m.client.Update(ctx, cm)
}

func nextGeneration(s *state) error {
	if s.Generation == math.MaxUint64 {
		return errors.New("trust generation exhausted")
	}

	s.Generation++

	return nil
}

func (m *Manager) ready(ctx context.Context, s *state) error {
	b, err := m.Bundle(ctx)
	if err != nil {
		return err
	}

	if b.Digest() != s.bundle().Digest() {
		return ErrNotReady
	}

	return nil
}

// TriggerRotation persists the new root before publishing the overlap. Reconcile
// repairs an interrupted publication. Repeated triggers during rotation are safe.
func (m *Manager) TriggerRotation(ctx context.Context) error {
	return m.triggerRotation(ctx, "")
}

func (m *Manager) triggerRotation(ctx context.Context, nonce string) error {
	err := m.mutate(ctx, func(s *state) error {
		if s.Phase != "stable" || (nonce != "" && nonce == s.RotationNonce) {
			return nil
		}

		if err := m.ready(ctx, s); err != nil {
			return err
		}

		ca, err := makeCA(m.options.Now(), m.options)
		if err != nil {
			return err
		}

		if err = nextGeneration(s); err != nil {
			return err
		}

		s.Authorities = append(s.Authorities, ca)

		s.Phase = "overlap"
		if nonce != "" {
			s.RotationNonce = nonce
		}

		return nil
	})
	if err != nil {
		return err
	}

	return m.publish(ctx)
}

func (m *Manager) allProven(s *state, drained bool) bool {
	b := s.bundle()
	bundleDigest := b.Digest()

	now := m.options.Now()
	for _, p := range s.Members {
		if p.Ack.Generation != b.Generation || p.Ack.Digest != bundleDigest || p.ProofGeneration != b.Generation || p.ProofDigest != bundleDigest || p.ProofRoot != s.proofRoot() || p.ProofFence != s.Fence || p.ProofAt.After(now) || now.Sub(p.ProofAt) > m.options.ProofLifetime || (drained && !p.Drained) {
			return false
		}
	}

	return true
}

func (m *Manager) Reconcile(ctx context.Context) error {
	if err := m.publish(ctx); err != nil {
		return err
	}

	_, s, err := m.readMetadata(ctx)
	if err != nil {
		return err
	}

	var cm corev1.ConfigMap
	if err := m.client.Get(ctx, m.objectKey(ConfigMapName), &cm); err != nil {
		return err
	}

	nonce := cm.Annotations[RotationAnnotation]
	if s.Phase == "stable" && nonce != "" && nonce != s.RotationNonce {
		return m.triggerRotation(ctx, nonce)
	}

	if s.Phase == "stable" && !m.options.Now().Before(s.NextRotation) {
		return m.TriggerRotation(ctx)
	}

	if s.Phase == "stable" {
		return nil
	}

	err = m.mutate(ctx, func(s *state) error {
		if err := m.ready(ctx, s); err != nil {
			return err
		}

		switch s.Phase {
		case "overlap":
			if m.allProven(s, false) {
				if err := nextGeneration(s); err != nil {
					return err
				}

				s.Active = s.Authorities[1].Digest
				s.Phase = "switched"
			}
		case "switched":
			if m.allProven(s, true) && !m.options.Now().Before(s.Authorities[0].LastIssuedExpiry.Add(m.options.ClockSkew)) {
				if err := nextGeneration(s); err != nil {
					return err
				}

				s.Authorities = s.Authorities[1:]
				s.Phase = "stable"
				s.NextRotation = m.options.Now().Add(m.options.RotateAfter)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	return m.publish(ctx)
}

func (m *Manager) NeedLeaderElection() bool { return true }

// Start is a controller-runtime leader-elected runnable. Errors stop the runnable
// so the parent manager can relinquish leadership and report failed readiness.
func (m *Manager) Start(ctx context.Context) error {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}

	if err := m.AcquireLeadership(ctx, hex.EncodeToString(nonce[:])); err != nil {
		return err
	}

	ticker := time.NewTicker(m.options.ReconcileInterval)
	defer ticker.Stop()

	for {
		if err := m.Reconcile(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
