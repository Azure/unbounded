// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesEvidenceStoreReturnsQueryableRunURIAndDigest(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	kube := fake.NewClientBuilder().WithScheme(scheme).Build()

	store, err := NewKubernetesEvidenceStore(kube, "project-signal")
	if err != nil {
		t.Fatal(err)
	}

	data := []byte(`{"verdict":"Pass"}`)

	uri, digest, err := store.Put(context.Background(), "project-signal/example/run-a.json", data)
	if err != nil {
		t.Fatal(err)
	}

	if uri != "kubernetes://project-signal/configmaps/run-a-evidence/evidence.json.gz" {
		t.Fatalf("unexpected URI %q", uri)
	}

	if digest != digestBytes(data) {
		t.Fatalf("digest = %q, want %q", digest, digestBytes(data))
	}

	var evidence corev1.ConfigMap
	if err := kube.Get(
		context.Background(),
		client.ObjectKey{Namespace: "project-signal", Name: "run-a-evidence"},
		&evidence,
	); err != nil {
		t.Fatal(err)
	}

	if evidence.Immutable == nil || !*evidence.Immutable {
		t.Fatal("evidence ConfigMap is mutable")
	}

	reader, err := gzip.NewReader(bytes.NewReader(evidence.BinaryData["evidence.json.gz"]))
	if err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}

	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	if string(got) != string(data) {
		t.Fatalf("persisted evidence = %q", got)
	}

	recovered, recoveredURI, recoveredDigest, found, err := store.Get(
		context.Background(),
		"project-signal/example/run-a.json",
	)
	if err != nil {
		t.Fatal(err)
	}

	if !found ||
		recoveredURI != uri ||
		recoveredDigest != digest ||
		!bytes.Equal(recovered, data) {
		t.Fatalf(
			"recovered evidence = found:%t uri:%q digest:%q data:%q",
			found,
			recoveredURI,
			recoveredDigest,
			recovered,
		)
	}
}

func TestKubernetesEvidenceStoreChunksLargeEvidence(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	kube := fake.NewClientBuilder().WithScheme(scheme).Build()

	store, err := NewKubernetesEvidenceStore(kube, "project-signal")
	if err != nil {
		t.Fatal(err)
	}

	data := make([]byte, 2*maximumEvidenceChunkBytes)

	state := uint32(1)
	for index := range data {
		state = state*1664525 + 1013904223
		data[index] = byte(state >> 24)
	}

	uri, digest, err := store.Put(context.Background(), "project-signal/example/run-large.json", data)
	if err != nil {
		t.Fatal(err)
	}

	if uri != "kubernetes://project-signal/configmaps/run-large-evidence/manifest.json" {
		t.Fatalf("unexpected chunked URI %q", uri)
	}

	var manifest corev1.ConfigMap
	if err := kube.Get(
		context.Background(),
		client.ObjectKey{Namespace: "project-signal", Name: "run-large-evidence"},
		&manifest,
	); err != nil {
		t.Fatal(err)
	}

	if len(manifest.BinaryData["manifest.json"]) == 0 {
		t.Fatal("large evidence did not use a chunk manifest")
	}

	recovered, recoveredURI, recoveredDigest, found, err := store.Get(
		context.Background(),
		"project-signal/example/run-large.json",
	)
	if err != nil {
		t.Fatal(err)
	}

	if !found ||
		recoveredURI != uri ||
		recoveredDigest != digest ||
		!bytes.Equal(recovered, data) {
		t.Fatalf(
			"recovered large evidence = found:%t uri:%q digest:%q bytes:%d",
			found,
			recoveredURI,
			recoveredDigest,
			len(recovered),
		)
	}
}
