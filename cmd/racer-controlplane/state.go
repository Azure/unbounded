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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	stateLabel      = annotationPrefix + "state"
	stateOwnerLabel = annotationPrefix + "state-owner"
	stateChunkSize  = 512 * 1024
)

type stateStore struct {
	client    client.Client
	namespace string
}
type manifest struct {
	Universe string   `json:"universe"`
	Digest   string   `json:"digest"`
	Chunks   []string `json:"chunks"`
}

func stateName(universe string) string { return "racer-" + identity("universe", universe)[:40] }

func (s stateStore) load(ctx context.Context, name string) (*generation, *corev1.ConfigMap, error) {
	pointer := &corev1.ConfigMap{}

	err := s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: stateName(name)}, pointer)
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}

	if err != nil {
		return nil, nil, err
	}

	if pointer.Labels[stateLabel] != "commit" {
		return nil, nil, fmt.Errorf("state name collision: %s", pointer.Name)
	}

	var m manifest
	if err := json.Unmarshal([]byte(pointer.Data["manifest"]), &m); err != nil {
		return nil, nil, err
	}

	if m.Universe != name || len(m.Chunks) == 0 {
		return nil, nil, fmt.Errorf("invalid state manifest")
	}

	var data bytes.Buffer

	for _, name := range m.Chunks {
		part := &corev1.ConfigMap{}
		if err := s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: name}, part); err != nil {
			return nil, nil, err
		}

		data.Write(part.BinaryData["state"])
	}

	digest := sha256.Sum256(data.Bytes())
	if hex.EncodeToString(digest[:]) != m.Digest {
		return nil, nil, fmt.Errorf("state digest mismatch")
	}

	var g generation
	if err := json.Unmarshal(data.Bytes(), &g); err != nil {
		return nil, nil, err
	}

	if g.Format != generationFormat {
		return nil, nil, fmt.Errorf("incompatible persisted generation format %d; deploy with fresh controller state", g.Format)
	}

	if g.Universe != name || g.Revision == 0 {
		return nil, nil, fmt.Errorf("invalid persisted generation")
	}

	return &g, pointer, nil
}

// Persist all immutable chunks before a single resourceVersion-checked pointer
// update. A crash or conflict cannot expose a partial topology. Only the elected
// leader writes or serves generations. Retain current and previous chunks for
// recovery; orphan chunks from interrupted writes are collected on next commit.
func (s stateStore) commit(ctx context.Context, g *generation, pointer *corev1.ConfigMap) error {
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}

	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	m := manifest{Universe: g.Universe, Digest: hash}

	base := stateName(g.Universe)
	for offset := 0; offset < len(data); offset += stateChunkSize {
		name := fmt.Sprintf("%s-%s-%d", base, hash[:12], offset/stateChunkSize)
		immutable := true

		part := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: s.namespace, Name: name, Labels: map[string]string{stateLabel: "chunk", stateOwnerLabel: base}}, Immutable: &immutable, BinaryData: map[string][]byte{"state": data[offset:min(offset+stateChunkSize, len(data))]}}
		if err := s.client.Create(ctx, part); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return err
			}

			existing := &corev1.ConfigMap{}
			if err := s.client.Get(ctx, client.ObjectKeyFromObject(part), existing); err != nil {
				return err
			}

			if existing.Labels[stateLabel] != "chunk" || !bytes.Equal(existing.BinaryData["state"], part.BinaryData["state"]) {
				return fmt.Errorf("state chunk collision: %s", name)
			}
		}

		m.Chunks = append(m.Chunks, name)
	}

	encoded, err := json.Marshal(m)
	if err != nil {
		return err
	}

	var previous manifest

	if pointer == nil {
		pointer = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: s.namespace, Name: base, Labels: map[string]string{stateLabel: "commit"}}, Data: map[string]string{"manifest": string(encoded)}}
		err = s.client.Create(ctx, pointer)
	} else {
		_ = json.Unmarshal([]byte(pointer.Data["manifest"]), &previous)
		pointer = pointer.DeepCopy()
		pointer.Data = map[string]string{"manifest": string(encoded)}
		err = s.client.Update(ctx, pointer)
	}

	if err != nil {
		return err
	}
	// GC is best effort after the commit point, never an excuse to roll back a
	// successfully committed generation or leave it unpublished.
	keep := map[string]bool{}
	for _, name := range append(m.Chunks, previous.Chunks...) {
		keep[name] = true
	}
	// Aborted candidates must not collect the last serving generation.
	rollout := &corev1.ConfigMap{}
	if e := s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: base + "-rollout"}, rollout); e == nil {
		if protectForwardChunks(rollout.Data["forwards"], g.Universe, keep) != nil {
			return nil
		}

		var serving manifest
		if raw := rollout.Data["serving"]; raw != "" {
			if json.Unmarshal([]byte(raw), &serving) != nil {
				return nil
			}

			for _, name := range serving.Chunks {
				keep[name] = true
			}
		}
	} else if !apierrors.IsNotFound(e) {
		return nil
	}

	var chunks corev1.ConfigMapList
	if s.client.List(ctx, &chunks, client.InNamespace(s.namespace), client.MatchingLabels{stateOwnerLabel: base}) == nil {
		for i := range chunks.Items {
			if !keep[chunks.Items[i].Name] {
				_ = s.client.Delete(ctx, &chunks.Items[i])
			}
		}
	}

	return nil
}
