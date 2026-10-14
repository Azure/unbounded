// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
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

	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Fatalf("unexpected digest %q", digest)
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

	recovered, recoveredURI, recoveredDigest, err := store.Get(context.Background(), "project-signal/example/run-a.json")
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(recovered, data) || recoveredURI != uri || recoveredDigest != digest {
		t.Fatalf("recovered evidence = %q, %q, %q", recovered, recoveredURI, recoveredDigest)
	}

	if _, _, err := store.Put(context.Background(), "project-signal/example/run-a.json", data); err != nil {
		t.Fatalf("identical evidence was not idempotent: %v", err)
	}

	if _, _, err := store.Put(context.Background(), "project-signal/example/run-a.json", []byte(`{"verdict":"Fail"}`)); err == nil {
		t.Fatal("immutable evidence was replaced")
	}
}

func TestEvidenceReaderRejectsMutableCorruptAndWrongDigestObjects(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*corev1.ConfigMap)
	}{
		{"mutable", func(evidence *corev1.ConfigMap) { evidence.Immutable = nil }},
		{"corrupt compression", func(evidence *corev1.ConfigMap) { evidence.BinaryData["evidence.json.gz"] = []byte("not gzip") }},
		{"wrong digest", func(evidence *corev1.ConfigMap) {
			evidence.Annotations["signal.unbounded-cloud.io/evidence-digest"] = "sha256:wrong"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}

			kube := fake.NewClientBuilder().WithScheme(scheme).Build()

			store, err := NewKubernetesEvidenceStore(kube, "signal")
			if err != nil {
				t.Fatal(err)
			}

			if _, _, err := store.Put(context.Background(), "run-a.json", []byte(`{"verdict":"Pass"}`)); err != nil {
				t.Fatal(err)
			}

			var evidence corev1.ConfigMap
			if err := kube.Get(context.Background(), client.ObjectKey{Namespace: "signal", Name: "run-a-evidence"}, &evidence); err != nil {
				t.Fatal(err)
			}

			tc.change(&evidence)

			if err := kube.Update(context.Background(), &evidence); err != nil {
				t.Fatal(err)
			}

			if _, _, _, err := store.Get(context.Background(), "run-a.json"); err == nil {
				t.Fatal("invalid evidence was accepted")
			}
		})
	}
}
