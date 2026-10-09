// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"errors"
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ValidateOverride keeps controller configuration in sync with trusted bootstrap.
func ValidateOverride(original, candidate *unstructured.Unstructured) error {
	controller := func(workload *unstructured.Unstructured) (map[string]any, int) {
		var found map[string]any

		count := 0

		containers, _, err := unstructured.NestedSlice(workload.Object, "spec", "template", "spec", "containers")
		if err != nil {
			return nil, 0
		}

		for _, entry := range containers {
			container, ok := entry.(map[string]any)
			if ok && container["name"] == "controller" {
				found = container
				count++
			}
		}

		return found, count
	}

	before, beforeCount := controller(original)

	after, afterCount := controller(candidate)
	if beforeCount != 1 || afterCount != 1 {
		return errors.New("racer Deployment must retain exactly one canonical controller container; use ConfigMap racer-config for configuration tuning")
	}

	for _, field := range []string{"env", "envFrom"} {
		beforeValue, beforePresent := before[field]

		afterValue, afterPresent := after[field]
		if beforePresent != afterPresent || !reflect.DeepEqual(beforeValue, afterValue) {
			return fmt.Errorf("racer Deployment container controller %s must match the canonical configuration exactly for trusted bootstrap parity; use ConfigMap racer-config for configuration tuning", field)
		}
	}

	return nil
}
