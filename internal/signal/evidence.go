// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

type EvidenceStore interface {
	Put(context.Context, string, []byte) (string, string, error)
}

type KubernetesEvidenceStore struct {
	namespace string
}

func NewKubernetesEvidenceStore(namespace string) (*KubernetesEvidenceStore, error) {
	if namespace == "" {
		return nil, fmt.Errorf("evidence namespace is required")
	}

	return &KubernetesEvidenceStore{namespace: namespace}, nil
}

func (s *KubernetesEvidenceStore) Put(_ context.Context, path string, data []byte) (string, string, error) {
	name := strings.TrimSuffix(url.PathEscape(strings.TrimSuffix(path[strings.LastIndex(path, "/")+1:], ".json")), ".json")
	if name == "" {
		return "", "", fmt.Errorf("evidence path is required")
	}

	sum := sha256.Sum256(data)

	return fmt.Sprintf("kubernetes://%s/certificationruns/%s", s.namespace, name),
		"sha256:" + hex.EncodeToString(sum[:]),
		nil
}
