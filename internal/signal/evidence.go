// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type EvidenceStore interface {
	Put(context.Context, string, []byte) (string, string, error)
	Get(context.Context, string) ([]byte, string, string, bool, error)
}

type KubernetesEvidenceStore struct {
	kube      client.Client
	namespace string
}

const maximumEvidenceChunkBytes = 700 * 1024

type evidenceChunkManifest struct {
	Version          string `json:"version"`
	Generation       string `json:"generation"`
	Chunks           int    `json:"chunks"`
	CompressedDigest string `json:"compressedDigest"`
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
	name := evidenceName(path)
	if name == "" {
		return "", "", fmt.Errorf("evidence path is required")
	}

	digest := digestBytes(data)
	generation := strings.TrimPrefix(digest, "sha256:")

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
		Immutable:  &immutable,
		BinaryData: map[string][]byte{},
	}
	uriKey := "evidence.json.gz"

	if len(compressed) <= maximumEvidenceChunkBytes {
		evidence.BinaryData["evidence.json.gz"] = compressed
	} else {
		uriKey = "manifest.json"

		chunkCount := (len(compressed) + maximumEvidenceChunkBytes - 1) / maximumEvidenceChunkBytes
		for index := range chunkCount {
			start := index * maximumEvidenceChunkBytes
			end := min(start+maximumEvidenceChunkBytes, len(compressed))

			chunk := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-evidence-%s-%03d", name, generation, index),
					Namespace: s.namespace,
					Labels: map[string]string{
						SnapshotRunLabel: name,
					},
					Annotations: map[string]string{
						"signal.unbounded-cloud.io/evidence-digest": digest,
						"signal.unbounded-cloud.io/chunk-index":     strconv.Itoa(index),
					},
				},
				Immutable: &immutable,
				BinaryData: map[string][]byte{
					"chunk": compressed[start:end],
				},
			}
			if err := s.createOrVerifyEvidenceConfigMap(ctx, chunk); err != nil {
				return "", "", err
			}
		}

		manifest, err := json.Marshal(evidenceChunkManifest{
			Version:          "v2",
			Generation:       generation,
			Chunks:           chunkCount,
			CompressedDigest: digestBytes(compressed),
		})
		if err != nil {
			return "", "", fmt.Errorf("encode evidence chunk manifest: %w", err)
		}

		evidence.BinaryData["manifest.json"] = manifest
	}

	if err := s.createOrVerifyEvidenceConfigMap(ctx, evidence); err != nil {
		return "", "", err
	}

	return evidenceURI(s.namespace, evidence.Name, uriKey), digest, nil
}

func (s *KubernetesEvidenceStore) Get(
	ctx context.Context,
	path string,
) ([]byte, string, string, bool, error) {
	name := evidenceName(path)
	if name == "" {
		return nil, "", "", false, fmt.Errorf("evidence path is required")
	}

	var evidence corev1.ConfigMap
	if err := s.kube.Get(
		ctx,
		client.ObjectKey{Namespace: s.namespace, Name: name + "-evidence"},
		&evidence,
	); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", "", false, nil
		}

		return nil, "", "", false, fmt.Errorf("get evidence ConfigMap: %w", err)
	}

	if evidence.Immutable == nil || !*evidence.Immutable {
		return nil, "", "", false, fmt.Errorf("evidence ConfigMap %s is mutable", evidence.Name)
	}

	compressed, exists := evidence.BinaryData["evidence.json.gz"]

	uriKey := "evidence.json.gz"
	if !exists {
		uriKey = "manifest.json"

		var manifest evidenceChunkManifest
		if err := json.Unmarshal(evidence.BinaryData["manifest.json"], &manifest); err != nil {
			return nil, "", "", false, fmt.Errorf("decode evidence ConfigMap %s chunk manifest: %w", evidence.Name, err)
		}

		if manifest.Version != "v2" || manifest.Generation == "" || manifest.Chunks < 1 {
			return nil, "", "", false, fmt.Errorf("evidence ConfigMap %s has an invalid chunk manifest", evidence.Name)
		}

		if "sha256:"+manifest.Generation != evidence.Annotations["signal.unbounded-cloud.io/evidence-digest"] {
			return nil, "", "", false, fmt.Errorf("evidence ConfigMap %s chunk generation does not match its digest", evidence.Name)
		}

		compressed = make([]byte, 0, manifest.Chunks*maximumEvidenceChunkBytes)
		for index := range manifest.Chunks {
			var chunk corev1.ConfigMap
			if err := s.kube.Get(
				ctx,
				client.ObjectKey{
					Namespace: s.namespace,
					Name:      fmt.Sprintf("%s-%s-%03d", evidence.Name, manifest.Generation, index),
				},
				&chunk,
			); err != nil {
				return nil, "", "", false, fmt.Errorf("get evidence chunk %d: %w", index, err)
			}

			if chunk.Immutable == nil || !*chunk.Immutable {
				return nil, "", "", false, fmt.Errorf("evidence chunk %s is mutable", chunk.Name)
			}

			if chunk.Annotations["signal.unbounded-cloud.io/evidence-digest"] !=
				evidence.Annotations["signal.unbounded-cloud.io/evidence-digest"] ||
				chunk.Annotations["signal.unbounded-cloud.io/chunk-index"] != strconv.Itoa(index) {
				return nil, "", "", false, fmt.Errorf("evidence chunk %s identity does not match its manifest", chunk.Name)
			}

			compressed = append(compressed, chunk.BinaryData["chunk"]...)
		}

		if digestBytes(compressed) != manifest.CompressedDigest {
			return nil, "", "", false, fmt.Errorf("evidence ConfigMap %s chunks do not match their manifest", evidence.Name)
		}
	}

	data, err := decompressEvidence(compressed)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("decompress evidence ConfigMap %s: %w", evidence.Name, err)
	}

	digest := digestBytes(data)
	if evidence.Annotations["signal.unbounded-cloud.io/evidence-digest"] != digest {
		return nil, "", "", false, fmt.Errorf("evidence ConfigMap %s digest does not match its content", evidence.Name)
	}

	uri := evidenceURI(s.namespace, evidence.Name, uriKey)

	return data, uri, digest, true, nil
}

func evidenceURI(namespace, name, key string) string {
	return fmt.Sprintf("kubernetes://%s/configmaps/%s/%s", namespace, name, key)
}

func (s *KubernetesEvidenceStore) createOrVerifyEvidenceConfigMap(
	ctx context.Context,
	evidence *corev1.ConfigMap,
) error {
	if err := s.kube.Create(ctx, evidence); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create evidence ConfigMap %s: %w", evidence.Name, err)
		}

		var existing corev1.ConfigMap
		if getErr := s.kube.Get(ctx, client.ObjectKeyFromObject(evidence), &existing); getErr != nil {
			return fmt.Errorf("get existing evidence ConfigMap %s: %w", evidence.Name, getErr)
		}

		if existing.Immutable == nil || !*existing.Immutable {
			return fmt.Errorf("evidence ConfigMap %s already exists but is mutable", evidence.Name)
		}

		if !maps.Equal(existing.Labels, evidence.Labels) ||
			!maps.Equal(existing.Annotations, evidence.Annotations) ||
			!maps.EqualFunc(existing.BinaryData, evidence.BinaryData, bytes.Equal) {
			return fmt.Errorf("evidence ConfigMap %s already exists with different content", evidence.Name)
		}
	}

	return nil
}

func evidenceName(path string) string {
	return strings.TrimSuffix(
		url.PathEscape(strings.TrimSuffix(path[strings.LastIndex(path, "/")+1:], ".json")),
		".json",
	)
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

func decompressEvidence(compressed []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}

	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()

	if readErr != nil {
		return nil, readErr
	}

	if closeErr != nil {
		return nil, closeErr
	}

	return data, nil
}
