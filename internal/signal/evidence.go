// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type EvidenceStore interface {
	Put(context.Context, string, []byte) (string, string, error)
}

type KubernetesEvidenceStore struct {
	kube      client.Client
	namespace string
}

func NewKubernetesEvidenceStore(kube client.Client, namespace string) (*KubernetesEvidenceStore, error) {
	if kube == nil {
		return nil, fmt.Errorf("kubernetes client is required")
	}

	if namespace == "" {
		return nil, fmt.Errorf("evidence namespace is required")
	}

	return &KubernetesEvidenceStore{kube: kube, namespace: namespace}, nil
}

func (s *KubernetesEvidenceStore) Put(ctx context.Context, path string, data []byte) (string, string, error) {
	name := strings.TrimSuffix(url.PathEscape(strings.TrimSuffix(path[strings.LastIndex(path, "/")+1:], ".json")), ".json")
	if name == "" {
		return "", "", fmt.Errorf("evidence path is required")
	}

	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	compressed, err := compressEvidence(data)
	if err != nil {
		return "", "", err
	}

	immutable := true

	evidence := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-evidence",
			Namespace: s.namespace,
			Labels: map[string]string{
				SnapshotRunLabel: name,
			},
			Annotations: map[string]string{
				"signal.unbounded-cloud.io/evidence-digest": digest,
			},
		},
		Immutable: &immutable,
		BinaryData: map[string][]byte{
			"evidence.json.gz": compressed,
		},
	}
	if err := s.kube.Create(ctx, evidence); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", "", fmt.Errorf("create evidence ConfigMap: %w", err)
		}

		var existing corev1.ConfigMap
		if getErr := s.kube.Get(ctx, client.ObjectKeyFromObject(evidence), &existing); getErr != nil {
			return "", "", fmt.Errorf("get existing evidence ConfigMap: %w", getErr)
		}

		if existing.Annotations["signal.unbounded-cloud.io/evidence-digest"] != digest ||
			!bytes.Equal(existing.BinaryData["evidence.json.gz"], compressed) {
			return "", "", fmt.Errorf("evidence ConfigMap %s already exists with different content", evidence.Name)
		}
	}

	return fmt.Sprintf("kubernetes://%s/configmaps/%s/evidence.json.gz", s.namespace, evidence.Name), digest, nil
}

func compressEvidence(data []byte) ([]byte, error) {
	var compressed bytes.Buffer

	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(data); err != nil {
		return nil, fmt.Errorf("compress evidence: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finish evidence compression: %w", err)
	}

	return compressed.Bytes(), nil
}
