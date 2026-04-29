// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

func getKubeconfigPath(p string) string {
	if !isEmpty(p) {
		return p
	}

	if env := os.Getenv("KUBECONFIG"); !isEmpty(env) {
		return env
	}

	// Fall back to the default kubectl kubeconfig location.
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".kube", "config")
	}

	return ""
}

// parseTaints converts taint strings in key=value:Effect format to
// corev1.Taint values.
func parseTaints(ss []string) []corev1.Taint {
	taints := make([]corev1.Taint, 0, len(ss))
	for _, s := range ss {
		t := parseTaint(s)
		taints = append(taints, t)
	}
	return taints
}

// parseTaint parses a single taint string. Accepted formats:
//   - key=value:Effect
//   - key:Effect
func parseTaint(s string) corev1.Taint {
	colonIdx := strings.LastIndex(s, ":")
	if colonIdx < 0 {
		return corev1.Taint{Key: s}
	}
	effect := corev1.TaintEffect(s[colonIdx+1:])
	keyVal := s[:colonIdx]

	if eqIdx := strings.Index(keyVal, "="); eqIdx >= 0 {
		return corev1.Taint{Key: keyVal[:eqIdx], Value: keyVal[eqIdx+1:], Effect: effect}
	}
	return corev1.Taint{Key: keyVal, Effect: effect}
}

// formatTaint formats a corev1.Taint as key=value:Effect.
func formatTaint(t corev1.Taint) string {
	if t.Value == "" {
		return fmt.Sprintf("%s:%s", t.Key, t.Effect)
	}
	return fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect)
}
