// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
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

// parseTaints mirrors Kubernetes' pkg/util/taints parsing and validation logic,
// scoped to additions because MachineConfiguration creation does not remove taints.
func parseTaints(spec []string) ([]corev1.Taint, error) {
	taints := make([]corev1.Taint, 0, len(spec))
	uniqueTaints := map[corev1.TaintEffect]map[string]struct{}{}

	for _, taintSpec := range spec {
		if strings.HasSuffix(taintSpec, "-") {
			return nil, fmt.Errorf("invalid taint spec: %v, removing taints is not supported", taintSpec)
		}

		newTaint, err := parseTaint(taintSpec)
		if err != nil {
			return nil, err
		}

		if len(newTaint.Effect) == 0 {
			return nil, fmt.Errorf("invalid taint spec: %v", taintSpec)
		}

		if uniqueTaints[newTaint.Effect] == nil {
			uniqueTaints[newTaint.Effect] = map[string]struct{}{}
		}

		if _, ok := uniqueTaints[newTaint.Effect][newTaint.Key]; ok {
			return nil, fmt.Errorf("duplicated taints with the same key and effect: %v", newTaint)
		}

		uniqueTaints[newTaint.Effect][newTaint.Key] = struct{}{}
		taints = append(taints, newTaint)
	}

	return taints, nil
}

// parseTaint parses a taint from a string, whose form must be either
// '<key>=<value>:<effect>', '<key>:<effect>', or '<key>'.
func parseTaint(st string) (corev1.Taint, error) {
	var taint corev1.Taint

	var (
		key    string
		value  string
		effect corev1.TaintEffect
	)

	parts := strings.Split(st, ":")
	switch len(parts) {
	case 1:
		key = parts[0]
	case 2:
		effect = corev1.TaintEffect(parts[1])
		if err := validateTaintEffect(effect); err != nil {
			return taint, err
		}

		partsKV := strings.Split(parts[0], "=")
		if len(partsKV) > 2 {
			return taint, fmt.Errorf("invalid taint spec: %v", st)
		}

		key = partsKV[0]
		if len(partsKV) == 2 {
			value = partsKV[1]
			if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
				return taint, fmt.Errorf("invalid taint spec: %v, %s", st, strings.Join(errs, "; "))
			}
		}
	default:
		return taint, fmt.Errorf("invalid taint spec: %v", st)
	}

	if errs := validation.IsQualifiedName(key); len(errs) > 0 {
		return taint, fmt.Errorf("invalid taint spec: %v, %s", st, strings.Join(errs, "; "))
	}

	taint.Key = key
	taint.Value = value
	taint.Effect = effect

	return taint, nil
}

func validateTaintEffect(effect corev1.TaintEffect) error {
	if effect != corev1.TaintEffectNoSchedule && effect != corev1.TaintEffectPreferNoSchedule && effect != corev1.TaintEffectNoExecute {
		return fmt.Errorf("invalid taint effect: %v, unsupported taint effect", effect)
	}

	return nil
}

// formatTaint formats a corev1.Taint as key=value:Effect.
func formatTaint(t corev1.Taint) string {
	if t.Value == "" {
		return fmt.Sprintf("%s:%s", t.Key, t.Effect)
	}

	return fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect)
}
