// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestKubernetesRecoveryGateVerifiesBoundImmutableReceipt(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_000, 0)
	run := recoveryGateRun()
	workflow := recoveryGateWorkflow()
	bundle := []byte(`{"sealed":"evidence"}`)
	receipt := signedRecoveryReceipt(t, privateKey, run, workflow, bundle, now)
	immutable := true
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "receipt", Namespace: run.Namespace},
		Immutable:  &immutable,
		Data: map[string]string{
			"receipt.json": string(receipt),
			"bundle.json":  string(bundle),
		},
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	gate, err := NewKubernetesRecoveryGate(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build(),
		publicKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	gate.now = func() time.Time { return now }

	if err := gate.Verify(context.Background(), run, workflow); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
}

func TestKubernetesRecoveryGateRejectsUntrustedOrUnboundReceipts(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_000, 0)
	run := recoveryGateRun()
	workflow := recoveryGateWorkflow()
	bundle := []byte(`{"sealed":"evidence"}`)
	validReceipt := signedRecoveryReceipt(t, privateKey, run, workflow, bundle, now)
	immutable := true

	tests := map[string]func(*corev1.ConfigMap){
		"mutable": func(configMap *corev1.ConfigMap) {
			configMap.Immutable = nil
		},
		"changed bundle": func(configMap *corev1.ConfigMap) {
			configMap.Data["bundle.json"] = `{"sealed":"different"}`
		},
		"changed signature": func(configMap *corev1.ConfigMap) {
			var receipt map[string]any
			if err := json.Unmarshal([]byte(configMap.Data["receipt.json"]), &receipt); err != nil {
				t.Fatal(err)
			}
			receipt["runUID"] = "attacker"
			encoded, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			configMap.Data["receipt.json"] = string(encoded)
		},
		"duplicate field": func(configMap *corev1.ConfigMap) {
			configMap.Data["receipt.json"] = `{"schema":"a","schema":"b"}`
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "receipt", Namespace: run.Namespace},
				Immutable:  &immutable,
				Data: map[string]string{
					"receipt.json": string(validReceipt),
					"bundle.json":  string(bundle),
				},
			}
			mutate(configMap)

			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}

			gate, err := NewKubernetesRecoveryGate(
				fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build(),
				publicKey,
			)
			if err != nil {
				t.Fatal(err)
			}
			gate.now = func() time.Time { return now }

			if err := gate.Verify(context.Background(), run, workflow); err == nil {
				t.Fatal("Verify() unexpectedly succeeded")
			}
		})
	}
}

func recoveryGateRun() *signalv1.CertificationRun {
	return &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dapple-run",
			Namespace: "project-signal",
			UID:       types.UID("run-uid"),
		},
	}
}

func recoveryGateWorkflow() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name":      "dapple-run-a1",
			"namespace": "project-signal",
			"uid":       "workflow-uid",
		},
		"status": map[string]any{
			"outputs": map[string]any{
				"parameters": []any{
					map[string]any{
						"name":  recoveryReceiptParameter,
						"value": "receipt",
					},
				},
			},
		},
	}}
}

func signedRecoveryReceipt(
	t *testing.T,
	privateKey ed25519.PrivateKey,
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	bundle []byte,
	now time.Time,
) []byte {
	t.Helper()

	sum := sha256.Sum256(bundle)
	unsigned := map[string]any{
		"schema":                recoveryReceiptSchema,
		"runName":               run.Name,
		"runUID":                string(run.UID),
		"workflowUID":           string(workflow.GetUID()),
		"bundleSHA256":          hex.EncodeToString(sum[:]),
		"patchApplied":          "YES",
		"terminalRecordPresent": true,
		"issuedAtEpoch":         now.Unix(),
		"expiresAtEpoch":        now.Add(5 * time.Minute).Unix(),
	}
	canonical, err := json.Marshal(unsigned)
	if err != nil {
		t.Fatal(err)
	}

	receipt := make(map[string]any, len(unsigned)+1)
	for key, value := range unsigned {
		receipt[key] = value
	}
	receipt["signatureBase64"] = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, canonical))

	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}

	return encoded
}
