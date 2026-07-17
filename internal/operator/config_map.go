// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

const (
	machinaConfigHashAnnotation = "unbounded-cloud.io/machina-config-hash"
	netConfigHashAnnotation     = "unbounded-cloud.io/net-config-hash"
	storageConfigHashAnnotation = "unbounded-cloud.io/storage-config-hash"
)

// configMapPayloadHash hashes the complete ConfigMap payload. JSON provides a
// deterministic encoding for string-keyed maps, including binary values.
func configMapPayloadHash(config *corev1.ConfigMap) string {
	payload, err := json.Marshal(struct {
		Data       map[string]string `json:"data"`
		BinaryData map[string][]byte `json:"binaryData"`
	}{
		Data:       config.Data,
		BinaryData: config.BinaryData,
	})
	if err != nil {
		panic(fmt.Sprintf("encode ConfigMap payload: %v", err))
	}

	sum := sha256.Sum256(payload)

	return hex.EncodeToString(sum[:])
}

func configMapPayloadChanged(oldConfig, newConfig *corev1.ConfigMap) bool {
	return configMapPayloadHash(oldConfig) != configMapPayloadHash(newConfig)
}
