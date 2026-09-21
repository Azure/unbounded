// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	ringFile   = "ring.json"
	bundleFile = "bundle.json"
)

type rotationPolicy struct{ Interval, Grace time.Duration }

func (p rotationPolicy) validate() error {
	if p.Grace <= 0 || p.Interval <= p.Grace {
		return fmt.Errorf("signing rotation interval must exceed positive propagation delay")
	}

	return nil
}

// Seeds exist only in controller state and the active peer consumer bundle.
// Public identities use lowercase hex in both languages.
type ringKey struct {
	Seed   string `json:"seed"`
	Public string `json:"public"`
}
type signingRing struct {
	Version     uint32    `json:"version"`
	Generation  uint64    `json:"generation"`
	Active      ringKey   `json:"active"`
	ActivatedAt time.Time `json:"activatedAt"`
	Previous    string    `json:"previous,omitempty"`
	Pending     *ringKey  `json:"pending,omitempty"`
	// Zero until an uncached read confirms publication. This additional commit
	// makes even a timed-out successful publication receive a full grace period.
	ActivateAfter *time.Time    `json:"activateAfter,omitempty"`
	Grace         time.Duration `json:"grace,omitempty"`
}
type signingBundle struct {
	Version    uint32   `json:"version"`
	Generation uint64   `json:"generation"`
	Active     string   `json:"active"`
	Seed       string   `json:"seed,omitempty"`
	Public     []string `json:"public"`
}

func newRingKey() (ringKey, error) {
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		return ringKey{}, err
	}
	defer clear(key)

	seed := key.Seed()
	defer clear(seed)

	return ringKey{Seed: hex.EncodeToString(seed), Public: hex.EncodeToString(pub)}, nil
}

func (k ringKey) signer() (*signer, error) {
	seed, err := decodeRingKey(k.Seed)
	if err != nil {
		return nil, err
	}
	defer clear(seed)

	s, err := readSigner(bytes.NewReader(seed))
	if err != nil {
		return nil, err
	}

	if hex.EncodeToString(s.key[32:]) != k.Public {
		clear(s.key)
		return nil, fmt.Errorf("ring seed/public mismatch")
	}

	return s, nil
}

func decodeRingKey(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 || hex.EncodeToString(b) != s {
		return nil, fmt.Errorf("ring key must be 32 bytes of lowercase hex")
	}

	return b, nil
}

func newSigningRing(now time.Time) (*signingRing, error) {
	key, err := newRingKey()
	if err != nil {
		return nil, err
	}

	return &signingRing{Version: 1, Generation: 1, Active: key, ActivatedAt: now.UTC()}, nil
}

func (r *signingRing) validate() error {
	if r.Version != 1 || r.Generation == 0 || r.ActivatedAt.IsZero() {
		return fmt.Errorf("invalid signing ring version, generation or activation")
	}

	seen := map[string]bool{}

	for _, k := range []*ringKey{&r.Active, r.Pending} {
		if k == nil {
			continue
		}

		s, err := k.signer()
		if err != nil {
			return err
		}

		clear(s.key)

		if seen[k.Public] {
			return fmt.Errorf("duplicate ring identity")
		}

		seen[k.Public] = true
	}

	if r.Previous != "" {
		if _, err := decodeRingKey(r.Previous); err != nil {
			return err
		}

		if seen[r.Previous] {
			return fmt.Errorf("duplicate previous identity")
		}
	}

	if r.Pending == nil && (r.ActivateAfter != nil || r.Grace != 0) || r.Pending != nil && r.Grace <= 0 {
		return fmt.Errorf("invalid pending rotation")
	}

	if r.ActivateAfter != nil && !r.ActivateAfter.After(r.ActivatedAt) {
		return fmt.Errorf("invalid activation deadline")
	}

	return nil
}

func (r *signingRing) bundle(peer bool) signingBundle {
	b := signingBundle{Version: 1, Generation: r.Generation, Active: r.Active.Public, Public: []string{r.Active.Public}}
	if peer {
		b.Seed = r.Active.Seed
	}

	if r.Previous != "" {
		b.Public = append(b.Public, r.Previous)
	}

	if r.Pending != nil {
		b.Public = append(b.Public, r.Pending.Public)
	}

	return b
}

func (r *signingRing) data(peer bool) (map[string][]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}

	state, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}

	bundle, err := json.Marshal(r.bundle(peer))

	return map[string][]byte{ringFile: state, bundleFile: bundle}, err
}

func readSigningRing(s *corev1.Secret) (*signingRing, error) {
	if s.DeletionTimestamp != nil {
		return nil, fmt.Errorf("secret is being deleted")
	}

	var r signingRing

	d := json.NewDecoder(bytes.NewReader(s.Data[ringFile]))
	d.DisallowUnknownFields()

	if err := d.Decode(&r); err != nil {
		return nil, err
	}

	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing ring data")
	}

	data, err := r.data(s.Name == peerSigningSecret)
	if err != nil {
		return nil, err
	}

	if !bytes.Equal(data[bundleFile], s.Data[bundleFile]) {
		return nil, fmt.Errorf("consumer bundle does not match ring")
	}

	return &r, nil
}

// advance is called only on state freshly read from the API. Each true result
// requires one successful resourceVersion-checked commit before another step.
func (r *signingRing) advance(now time.Time, p rotationPolicy) (bool, time.Duration, error) {
	if err := p.validate(); err != nil {
		return false, 0, err
	}

	if err := r.validate(); err != nil {
		return false, 0, err
	}

	if r.Generation == ^uint64(0) {
		return false, 0, fmt.Errorf("signing generation exhausted")
	}

	now = now.UTC()

	if r.Pending == nil {
		at := r.ActivatedAt.Add(p.Interval - p.Grace)
		if now.Before(at) {
			return false, at.Sub(now), nil
		}

		key, err := newRingKey()
		if err != nil {
			return false, 0, err
		}

		r.Pending, r.Grace = &key, p.Grace
	} else if r.ActivateAfter == nil {
		at := now.Add(r.Grace)
		r.ActivateAfter = &at
	} else {
		if now.Before(*r.ActivateAfter) {
			return false, r.ActivateAfter.Sub(now), nil
		}

		r.Previous, r.Active = r.Active.Public, *r.Pending
		r.ActivatedAt, r.Pending, r.ActivateAfter, r.Grace = now, nil, nil, 0
	}

	r.Generation++

	return true, 0, nil
}
