// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// ConfigMapPayloadHash hashes the complete ConfigMap payload. JSON provides a
// deterministic encoding for string-keyed maps, including binary values.
func ConfigMapPayloadHash(config *corev1.ConfigMap) string {
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

// ConfigMapPayloadChanged reports whether two ConfigMaps differ in payload.
func ConfigMapPayloadChanged(oldConfig, newConfig *corev1.ConfigMap) bool {
	return ConfigMapPayloadHash(oldConfig) != ConfigMapPayloadHash(newConfig)
}
