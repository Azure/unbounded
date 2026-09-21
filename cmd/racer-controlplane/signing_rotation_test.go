// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSigningRingLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	p := rotationPolicy{24 * time.Hour, 10 * time.Minute}

	r, err := newSigningRing(now)
	if err != nil {
		t.Fatal(err)
	}

	first := r.Active
	step := func(at time.Time, want bool) {
		t.Helper()

		changed, _, err := r.advance(at, p)
		if err != nil || changed != want {
			t.Fatalf("advance changed=%v err=%v", changed, err)
		}
		// Every transition survives serialization and a new controller instance.
		data, err := r.data(true)
		if err != nil {
			t.Fatal(err)
		}

		r, err = readSigningRing(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: peerSigningSecret}, Data: data})
		if err != nil {
			t.Fatal(err)
		}
	}
	step(now.Add(23*time.Hour), false)
	step(now.Add(p.Interval-p.Grace), true)

	if r.Active != first || r.ActivateAfter != nil {
		t.Fatal("staging activated signer or backdated grace")
	}

	pending := *r.Pending
	if b := r.bundle(true); b.Seed != first.Seed || len(b.Public) != 2 {
		t.Fatal("pending seed exposed")
	}

	if b := r.bundle(false); b.Seed != "" {
		t.Fatal("config seed exposed")
	}
	// Publication was committed, but the writer lost its response and restarted
	// an hour later. Confirmation starts a fresh full grace period.
	confirmed := now.Add(25 * time.Hour)
	step(confirmed, true)

	p.Grace = time.Second // flag changes cannot shorten persisted grace

	step(confirmed.Add(9*time.Minute), false)
	step(confirmed.Add(10*time.Minute), true)

	if r.Active != pending || r.Previous != first.Public {
		t.Fatal("wrong active/previous")
	}

	step(confirmed.Add(10*time.Minute), false) // no catch-up rotations

	second := r.Active
	stage := r.ActivatedAt.Add(p.Interval - p.Grace)
	step(stage, true)

	if len(r.bundle(false).Public) != 3 {
		t.Fatal("missing staged overlap")
	}

	step(stage, true)
	step(stage.Add(p.Grace), true)

	if r.Previous != second.Public || len(r.bundle(false).Public) != 2 {
		t.Fatal("did not retire n-2")
	}

	for _, public := range r.bundle(false).Public {
		if public == first.Public {
			t.Fatal("retained n-2")
		}
	}
}

func TestSigningRingValidation(t *testing.T) {
	r, _ := newSigningRing(time.Now())
	for _, mutate := range []func(*signingRing){
		func(r *signingRing) { r.Version++ },
		func(r *signingRing) { r.Generation = 0 },
		func(r *signingRing) { r.Active.Seed = "00" },
		func(r *signingRing) { r.Previous = r.Active.Public },
		func(r *signingRing) { r.Pending = &r.Active; r.Grace = time.Minute },
		func(r *signingRing) { r.ActivateAfter = &r.ActivatedAt },
	} {
		copy := *r
		mutate(&copy)

		if copy.validate() == nil {
			t.Fatal("accepted malformed ring")
		}
	}

	data, _ := r.data(false)

	var bundle signingBundle
	if err := json.Unmarshal(data[bundleFile], &bundle); err != nil {
		t.Fatal(err)
	}

	bundle.Seed = r.Active.Seed

	data[bundleFile], _ = json.Marshal(bundle)
	if _, err := readSigningRing(&corev1.Secret{Data: data}); err == nil {
		t.Fatal("accepted inconsistent consumer bundle")
	}
}
