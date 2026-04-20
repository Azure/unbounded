// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Operation represents a single requested operation parsed from the
// ConfigMap data. This struct is deliberately decoupled from the ConfigMap
// format so that migrating to a dedicated CRD only requires replacing the
// shim layer (this file and opwatch.go).
type Operation struct {
	Type    string `json:"type"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// Operation state constants.
const (
	OpStatePending    = "pending"
	OpStateInProgress = "in_progress"
	OpStateCompleted  = "completed"
	OpStateFailed     = "failed"
)

// Operation type constants.
const (
	OpTypeReboot = "reboot"
)

// ConfigMap constants for the operation shim. When migrating to a dedicated
// CRD, these constants and the functions below are the only pieces that need
// to change.
const (
	operationNamespace = "unbounded-system"
	operationLabelKey  = "unbounded.io/agent-op"
	operationDataKey   = "operations"
)

// parseOperations reads the operations JSON from a ConfigMap's data field.
func parseOperations(cm *corev1.ConfigMap) ([]Operation, error) {
	raw, ok := cm.Data[operationDataKey]
	if !ok {
		return nil, fmt.Errorf("ConfigMap %s/%s missing %q data key", cm.Namespace, cm.Name, operationDataKey)
	}

	var ops []Operation
	if err := json.Unmarshal([]byte(raw), &ops); err != nil {
		return nil, fmt.Errorf("parse operations JSON from ConfigMap %s/%s: %w", cm.Namespace, cm.Name, err)
	}

	return ops, nil
}

// serializeOperations writes the operations list back into the ConfigMap's
// data field as JSON.
func serializeOperations(cm *corev1.ConfigMap, ops []Operation) error {
	data, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("marshal operations: %w", err)
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}

	cm.Data[operationDataKey] = string(data)

	return nil
}

// hasPendingOperations returns true if the operations list contains at
// least one operation in a retriable state (pending or in_progress).
// Operations left in_progress by a crashed process are treated as
// retriable to ensure restart safety.
func hasPendingOperations(ops []Operation) bool {
	for i := range ops {
		if ops[i].State == OpStatePending || ops[i].State == OpStateInProgress {
			return true
		}
	}

	return false
}
