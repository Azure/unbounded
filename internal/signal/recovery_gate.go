// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	recoveryReceiptSchema    = "signal.unbounded-cloud.io/recovery-receipt/v1"
	recoveryReceiptParameter = "recovery-receipt-configmap"
	requireRecoveryParameter = "require-recovery-gate"
	maxRecoveryReceiptBytes  = 16 * 1024
	maxRecoveryBundleBytes   = 16 * 1024 * 1024
)

type RecoveryGate interface {
	Verify(context.Context, *signalv1.CertificationRun, *unstructured.Unstructured) error
}

type KubernetesRecoveryGate struct {
	kube      client.Reader
	publicKey ed25519.PublicKey
	now       func() time.Time
}

type recoveryReceipt struct {
	Schema                string `json:"schema"`
	RunName               string `json:"runName"`
	RunUID                string `json:"runUID"`
	WorkflowUID           string `json:"workflowUID"`
	BundleSHA256          string `json:"bundleSHA256"`
	PatchApplied          string `json:"patchApplied"`
	TerminalRecordPresent bool   `json:"terminalRecordPresent"`
	TerminalOutcome       string `json:"terminalOutcome"`
	IssuedAtEpoch         int64  `json:"issuedAtEpoch"`
	ExpiresAtEpoch        int64  `json:"expiresAtEpoch"`
	SignatureBase64       string `json:"signatureBase64"`
}

func NewKubernetesRecoveryGate(
	kube client.Reader,
	publicKey ed25519.PublicKey,
) (*KubernetesRecoveryGate, error) {
	if kube == nil {
		return nil, fmt.Errorf("recovery gate Kubernetes reader is required")
	}

	if len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("recovery verifier public key must be %d bytes", ed25519.PublicKeySize)
	}

	return &KubernetesRecoveryGate{
		kube:      kube,
		publicKey: append(ed25519.PublicKey(nil), publicKey...),
	}, nil
}

func LoadRecoveryVerifierPublicKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read recovery verifier public key: %w", err)
	}

	decoded, err := base64.StdEncoding.Strict().DecodeString(string(bytes.TrimSpace(data)))
	if err != nil {
		return nil, fmt.Errorf("decode recovery verifier public key: %w", err)
	}

	if len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("recovery verifier public key must be %d bytes", ed25519.PublicKeySize)
	}

	return ed25519.PublicKey(decoded), nil
}

func (g *KubernetesRecoveryGate) Verify(
	ctx context.Context,
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
) error {
	if workflow == nil {
		return fmt.Errorf("recovery-gated pass requires a Workflow")
	}

	name := workflowParameter(workflow, recoveryReceiptParameter)
	if name == "" {
		return fmt.Errorf("workflow did not publish %s", recoveryReceiptParameter)
	}

	var evidence corev1.ConfigMap
	if err := g.kube.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: name}, &evidence); err != nil {
		return fmt.Errorf("get recovery receipt ConfigMap: %w", err)
	}

	if evidence.Immutable == nil || !*evidence.Immutable {
		return fmt.Errorf("recovery receipt ConfigMap must be immutable")
	}

	receiptJSON := []byte(evidence.Data["receipt.json"])
	bundleJSON := []byte(evidence.Data["bundle.json"])

	if len(receiptJSON) == 0 || len(receiptJSON) > maxRecoveryReceiptBytes {
		return fmt.Errorf("recovery receipt size is invalid")
	}

	if len(bundleJSON) == 0 || len(bundleJSON) > maxRecoveryBundleBytes {
		return fmt.Errorf("recovery bundle size is invalid")
	}

	receipt, err := parseRecoveryReceipt(receiptJSON)
	if err != nil {
		return fmt.Errorf("decode recovery receipt: %w", err)
	}

	sum := sha256.Sum256(bundleJSON)
	if receipt.Schema != recoveryReceiptSchema ||
		receipt.RunName != run.Name ||
		receipt.RunUID != string(run.UID) ||
		receipt.WorkflowUID != string(workflow.GetUID()) ||
		receipt.BundleSHA256 != hex.EncodeToString(sum[:]) ||
		receipt.PatchApplied != "YES" ||
		!receipt.TerminalRecordPresent ||
		receipt.TerminalOutcome != "PASS" {
		return fmt.Errorf("recovery receipt binding mismatch")
	}

	now := time.Now
	if g.now != nil {
		now = g.now
	}

	nowEpoch := now().Unix()
	if receipt.IssuedAtEpoch > nowEpoch ||
		receipt.ExpiresAtEpoch < nowEpoch ||
		receipt.ExpiresAtEpoch-receipt.IssuedAtEpoch > 300 {
		return fmt.Errorf("recovery receipt is outside its validity window")
	}

	signature, err := base64.StdEncoding.Strict().DecodeString(receipt.SignatureBase64)
	if err != nil {
		return fmt.Errorf("decode recovery receipt signature: %w", err)
	}

	unsigned := map[string]any{
		"schema":                receipt.Schema,
		"runName":               receipt.RunName,
		"runUID":                receipt.RunUID,
		"workflowUID":           receipt.WorkflowUID,
		"bundleSHA256":          receipt.BundleSHA256,
		"patchApplied":          receipt.PatchApplied,
		"terminalRecordPresent": receipt.TerminalRecordPresent,
		"terminalOutcome":       receipt.TerminalOutcome,
		"issuedAtEpoch":         receipt.IssuedAtEpoch,
		"expiresAtEpoch":        receipt.ExpiresAtEpoch,
	}

	canonical, err := json.Marshal(unsigned)
	if err != nil {
		return fmt.Errorf("encode recovery receipt: %w", err)
	}

	if !ed25519.Verify(g.publicKey, canonical, signature) {
		return fmt.Errorf("recovery receipt signature is invalid")
	}

	return nil
}

func parseRecoveryReceipt(data []byte) (recoveryReceipt, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))

	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return recoveryReceipt{}, fmt.Errorf("expected JSON object")
	}

	fields := make(map[string]json.RawMessage)

	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return recoveryReceipt{}, err
		}

		key, ok := token.(string)
		if !ok {
			return recoveryReceipt{}, fmt.Errorf("expected JSON object key")
		}

		if _, exists := fields[key]; exists {
			return recoveryReceipt{}, fmt.Errorf("duplicate JSON key %q", key)
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return recoveryReceipt{}, err
		}

		fields[key] = value
	}

	if _, err := decoder.Token(); err != nil {
		return recoveryReceipt{}, err
	}

	if _, err := decoder.Token(); err != io.EOF {
		return recoveryReceipt{}, fmt.Errorf("trailing JSON data")
	}

	normalized, err := json.Marshal(fields)
	if err != nil {
		return recoveryReceipt{}, err
	}

	var receipt recoveryReceipt

	strict := json.NewDecoder(bytes.NewReader(normalized))
	strict.DisallowUnknownFields()

	if err := strict.Decode(&receipt); err != nil {
		return recoveryReceipt{}, err
	}

	return receipt, nil
}
